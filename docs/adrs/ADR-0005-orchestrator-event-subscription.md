# ADR-0005 — Orchestrator run-event subscription: polling over a server stream

**Status:** Accepted
**Date:** 2026-09-12
**Ticket:** [CRI-133](https://linear.app/brokenbots/issue/CRI-133/castle-orchestrator-event-subscription-api-control-plane-prerequisite)
**Related:** CRI-78 (SQLite writer serialization), CRI-115 (per-scope adapter pod reconcile), CRI-131 (operator republish)

## Context

Since the k8s port, runner Jobs execute `criteria apply` in local mode and write `events.ndjson` to the runner PVC. The operator (criteria-k8s operator) reads those files, interprets run and adapter lifecycle, and republishes metadata into Castle (CRI-131). This demotes Castle from control plane to a database: the operator sits in the event path, learns run state from the PVC rather than from Castle, and planned agent behavior that saves state to Castle is blocked because agents hold no Castle identity.

CRI-133 asks for an orchestrator-facing read path on Castle so the operator can observe run lifecycle directly from Castle. Two options were evaluated:

1. **A ConnectRPC server stream** (e.g. `OrchestratorService.SubscribeRunEvents`) that replays the event stream from the store for a run (or all active runs) to a subscribed orchestrator identity, with backpressure and replay-from-cursor semantics.
2. **A polling API** (`ListRunEvents(since_seq)`), simpler and fitting the operator reconcile-loop model.

Both options must satisfy the same requirements:

- The operator authenticates as an **orchestrator identity** using accept-token auth, distinct from agent tokens.
- The operator observes run lifecycle from Castle within a reconcile interval, without touching the PVC.
- The read path carries adapter lifecycle events (`adapter.lifecycle.provision_wanted` / `adapter.lifecycle.released`) with `scope_instance_id`, `shim_listen_address`, and `token_ref` — the CRI-115 contract that drives per-scope adapter pod reconcile.
- The read path carries run lifecycle events (`RunStarted` / `RunCompleted` / `RunFailed`) so the operator can stamp CriteriaRun status without touching the PVC.
- Replay tolerates operator restarts via a cursor (since-seq) mechanism.
- Concurrency respects the CRI-78 SQLite writer serialization (single write connection).
- Auth separation: an orchestrator identity cannot be used to write agent-owned data.

## Decision

**We build option 2: polling.** Castle adds a dedicated `criteria.v1.OrchestratorService` with two read-only RPCs:

- `SubscribeRunEvents(run_id, since_seq, limit)` — returns the persisted events for one run with `seq > since_seq` in ascending seq order, plus `last_seq` and `next_since_seq` continuation cursors. Polling semantics: one bounded page per call; the operator re-polls within its reconcile interval.
- `ListActiveRuns()` — returns runs that have not reached a terminal state (`pending` / `running` / `paused`), the discovery step of the operator reconcile loop.

The operator reconcile contract is: `ListActiveRuns` → for each active run, poll `SubscribeRunEvents(since_seq)` → advance its per-run cursor to `last_seq` after consuming a **non-empty** page (persisting `next_since_seq` on truncated pages) → apply run and adapter lifecycle to its own reconcile state. An **empty page leaves the cursor unchanged**: `last_seq` is 0 on empty pages and must not be persisted, otherwise an idle run would reset its cursor to 0 and replay from the beginning on every reconcile.

### Rationale

- **Operator model.** The consumer is a reconcile loop (Kubernetes-controller shaped): periodic, stateless, restart-tolerant. Polling with an exclusive seq cursor is the natural primitive; the operator already persists reconcile state per run and can persist the event cursor the same way. A long-lived stream would force the operator to manage stream lifetime, reconnect, and resume as a separate state machine layered on top of the reconcile loop.
- **Single-replica SQLite (CRI-78).** Castle runs one SQLite writer connection. Streams are durable only insofar as cursors are written; `WatchRun` already shows the cost — per-stream cursor writers with busy-retry and flush bookkeeping. Polling performs zero writes on the read path, so a restart-tolerant read costs nothing against the writer budget. (Operator-side cursor persistence is free: it is the operator's own state store.)
- **Replay correctness.** `AppendEvent` assigns per-run monotonic `seq` inside a single transaction and is idempotent on `(run_id, correlation_id)`. A poll anchored at `since_seq` therefore yields a gapless, duplicate-free replay under interleaved writers, verified by tests that interleave concurrent `SubmitEvents` streams with repeated polling.
- **Push is already available where it matters.** `ServerService.WatchRun` streams a single run with replay and cursor persistence. The orchestrator can use it opportunistically for a single run; multi-run observation does not need a new stream.
- **Option 1's added complexity is not paid for here.** Backpressure tuning (buffer sizes, slow-consumer eviction) and cross-run subscription ("all active runs" multiplexed over one stream) would both be new subsystems. The polling option needs none of it and satisfies every required behavior.

### Consequences

- **Operator cursor persistence is the operator's job.** Castle stays stateless on the read path: each poll carries `since_seq` and returns `next_since_seq` when the page is full. A fresh operator replays from `since_seq=0`; a restarted operator resumes from its persisted cursor. Replay is complete because events are durable in SQLite until the operator has consumed them.
- **Auth boundary.** Orchestrator identities are provisioned from a configured accept-token (`--orchestrator-token` / `CASTLE_ORCHESTRATOR_TOKEN`), stored hashed, and authenticated like agent tokens (same headers). The interceptor enforces that orchestrator identities may invoke only the read-only ServerService procedures and `OrchestratorService`; every `CriteriaService` procedure (agent-owned writes: `CreateRun`, `SubmitEvents`, `Control`, `ReattachRun`, `Heartbeat`, `Resume`) and every `ServerService` write (`StopRun`, `PauseRun`, `ResumeRun`, `SendPrompt`, `SubmitWorkflowAssignment`) is rejected with `PermissionDenied`. Agent identities may read `OrchestratorService` (it is read-only), mirroring `ServerService` reads.
- **Revocation.** Provisioning and revocation are both driven by the configured token: a non-empty token upserts (rotating) the `orchestrator-operator` identity, and an **empty** token deletes it, so a retired credential stops authenticating at the next castle restart. Re-configuring a token re-provisions the identity.
- **Adapter lifecycle wire contract (CRI-115).** Two new agent-emitted envelope payloads — `adapter.lifecycle.provision_wanted` and `adapter.lifecycle.released`, each carrying `scope_instance_id`, `shim_listen_address`, and `token_ref` — are added to `events.proto` with permanent field numbers (35, 36). Castle persists and fans them out like any other envelope; the orchestrator observes them through `SubscribeRunEvents` and reconciles adapter pods per `scope_instance_id`. Castle does not interpret the fields (agent-side contract ownership stays in Criteria).
- **Anonymous reads unchanged.** In dev mode (`--allow-anon-reads`), the `OrchestratorService` read RPCs are exempted like the other read-only `ServerService` procedures so local tooling keeps working. Production TLS deployments keep them authenticated.

## Alternatives considered

- **Option 1 (server stream with backpressure and replay-from-cursor).** Rejected for this phase: it duplicates the operator's reconnect/resume state machine, adds per-stream cursor write load against the single SQLite writer, and requires a new backpressure subsystem, while the reconcile-loop consumer needs none of it. `WatchRun` remains the stream-shaped tool for single-run tailing.
- **Reusing `ServerService.ListRunEvents` with no new service.** Rejected: `ServerService` is the UI/tool surface whose read RPCs are anonymously readable in dev mode; the orchestrator contract deserves its own service boundary that Castle can evolve for the runner cutover (accept-token auth, no mTLS) without touching UI semantics.
- **A cross-run "poll all active runs" batching RPC.** Rejected: per-run seq numbers are independent, so a single `since_seq` cannot span runs; it would force per-run cursor maps into the request/response or server-side cursor writes. `ListActiveRuns` + per-run polls keep the cursor model simple and per-run.