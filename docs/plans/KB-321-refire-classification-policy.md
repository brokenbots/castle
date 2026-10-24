# KB-321 — P0.4 refire/classification policy engine (retire the cron shepherd)

Design decision record for the refire/classification policy engine. The
referenced design doc (`option-b-castle-orchestrator-scope.md`) is not
reachable from the workstream evidence directory (same as KB-308/KB-318/KB-320);
the workstream task text and this repo's existing conventions are authoritative.

## Policy as data

- New refire policy table `refire_policies` (migration 0017, sqlite + pg in
  lockstep), keyed `(project_id, route)`:
  - `(project_id, route)` — per-route override for one project.
  - `(project_id, '')` — the project's per-assignment default.
  - `('', route)` — cross-project route override.
  - `('', '')` — the global default row.
- `policy` column holds the policy document verbatim as JSON
  (`refire.MarshalPolicy`/`ParsePolicy` in package `castle/internal/store/refire`):
  `max_attempts` (total family attempts including the original; 0 disables
  refiring), `backoff_seconds` (delay from the failed attempt's ended_at
  before the next attempt may fire), and the three classification
  vocabularies as lowercase substring lists: `transient` (refire),
  `parks` (human-gate, never refire without human ack),
  `cosmetics` (known-benign, no refire).
- Resolution order: route row > project default row > global route row >
  global default row > compiled-in default. Rows are full specifications —
  no field-level merging between layers. The compiled-in default is also a
  row: boot seeds `('', '')` insert-if-missing, so the default is always
  visible and editable as data.
- `--refire-policy-file` (or `CASTLE_REFIRE_POLICY_FILE`) is the operator
  seed path: a JSON array of `{project_id, route, policy}` documents
  upserted at boot. Absent file leaves rows untouched (rows are the
  authority; the file is one editor among others).
- `projects.default_refire_policy` (KB-318) stays criteria-owned vocabulary
  persisted verbatim and does not enter resolution: its document format was
  never defined, and interpreting another owner's freeform field would guess
  semantics. If upstream later defines it, a policy row is the drop-in
  translation point.

## Built-in default policy

```
max_attempts       3
backoff_seconds    30
transient          stall, heartbeat, timeout, deadline exceeded, unavailable,
                   rate limit, transient, connection refused, connection reset,
                   created_never_started
parks              need_help, need_assistance, awaiting_human
cosmetics          pod-state emit race, pod state emit race, pod phase race,
                   emit race
```

Castle's own deterministic failure stamps ("agent heartbeat lost" heartbeat
reaper = the provider stall; "created_never_started" KB-233 rule 2) both sit
in the transient vocabulary. Unclassified failures default to NO refire
(fail-safe). Cancelled runs never requeue (operator intent), rejected
assignments never refire (no work ever ran).

## Classification

Evaluated per terminal-failed run family, highest precedence first:

1. **park** — gate check (KB-226/KB-270): the run's latest persisted event
   parks under `reap.LastEventParksHumanGate` (awaiting_human /
   need_assistance / need_help run.completed final states, approval
   requests, signal waits) or the policy `parks` vocabulary matches a
   `run.completed` final state. The failed run is reclassified to the
   KB-226 parked shape — status `awaiting_human`, no ended_at, no
   failure_reason, no workflow-assignment change — and never refires. The
   human ack is the already-built park path (CRI-196 deliver an approval
   decision / board re-entry via the KB-320 trigger adapter), which is
   outside the automatic refire scan by construction.
2. **cosmetic** — pod-state emit-race style reasons: leave terminal-failed,
   no refire, no park.
3. **transient** — refire.
4. **none** — unclassified reason: leave terminal-failed (fail-safe).

## Attempts and requeue

- `workflow_assignments` gains `refire_root_run_id` (family root: the
  original attempt's run id; originals carry '') and `refire_attempt`
  (family attempt number, original = 1). The lease-attempt rows in
  `workflow_assignment_attempts` are dispatch-level retries and are NOT the
  policy's attempt accounting.
- Refiring creates a fresh run + queued assignment via the same transaction
  path assignment submission uses. The successor's idempotency key is
  `<base #attempt-N stripped>#attempt-{attempt+1}` — the idempotency key
  includes the attempt count, so the unique (owner, key) index makes every
  reconcile pass idempotent (crash-safe dedupe).
- Only the family's highest-attempt failed run is ever a candidate; refires
  stop when any newer attempt exists, when the policy's attempt budget is
  exhausted, or before the backoff deadline. Refired runs go through the
  normal dispatch (leased to a connected eligible agent) so the assignment
  queue stays the single delivery path.

## Where the loop runs

- Store pass `RefireFailedRuns(now, limit)` (both engines, shared SQL via
  the reap Dialect pattern) does scan + classify + requeue + park in one
  write transaction.
- `ServerServer.RefireFailedRunsNow` wraps the store pass and dispatches
  every refired assignment; `main.go` runs it on a 15s ticker
  (`--refire-interval`, 0 disables) next to the reapers.
- Policy CRUD (`UpsertRefirePolicy` / `ListRefirePolicies` /
  `DeleteRefirePolicy`) — rows are data, no proto/RPC surface change.

## Retiring the cron shepherd

The `adr-307-arc-shepherd` cronjob lived in the workflow-example repo's
k8s manifests (out of castle repo scope by the contributor guide's
boundaries). Castle now implements the cron's behavior under policy: the
cron's prompt-encoded stall classification becomes `transient`/`parks`/
`cosmetics` policy rows, evaluated by the dispatch reconcile loop instead of
a timer that re-fired everything indiscriminately.

## Acceptance mapping

- stall-failed run auto-refires: "agent heartbeat lost" /
  "created_never_started"/ stall reasons classify transient and requeue
  under attempt/backoff limits.
- parked run never refires without human ack: awaiting_human runs are
  never candidates; designed-park `run.completed` endings are reclassified
  parked by the gate check before any refire decision.
- cron gone: the engine in this repo replaces it (cron manifest removal is
  a workflow-example-repo change, tracked there).
- policy editable as data: policy rows (boot seed + SQL editability), no
  code changes needed to retune attempts/backoff/vocabulary.