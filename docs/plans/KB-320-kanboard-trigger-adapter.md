# KB-320 — Kanboard trigger adapter (thin) + board mirror

Workstream: P0.3 of the castle-based orchestrator option-B scope. The referenced
hermes plan document (`option-b-castle-orchestrator-scope.md`) is not accessible
from this environment (same situation the KB-318 plan document explicitly
records, docs/plans/KB-318-project-entity.md); the workstream task text and the
board's own evidence are authoritative. The design decisions below are
therefore derived from the workstream text, the repository's own completed
precedents (CRI-133/CRI-195 identity boundaries, KB-308, KB-318 project entity
and assignment conventions), and the behavior the current criteria-k8s
kanboard-watcher exhibits (the "full card lifecycle equal" target).

## Summary

The kanboard-watcher (criteria-k8s) today turns board columns into runs by
matching cards against its routes ConfigMap and creating CriteriaRuns in the
operator's namespace. Under option B, dispatch ownership moves to castle: a
thin **kanboard trigger adapter** running next to castle

1. polls Kanboard exactly like the watcher's poll loop (consumer discipline:
   one listing read per poll, no per-ticket reads on the steady-state path),
2. fires **assignments** at castle — route NAME + project + payload — with
   **no routing logic in the adapter** (no route resolution, no workflow
   library lookup, no per-card workflow-tag override resolution: each
   configured lane maps a trigger-column entry to exactly ONE static route
   name supplied by lane config),
3. mirrors the **castle event stream** (`WatchRun`) back onto board columns,
   replacing the watcher's CriteriaRun-phase reconcile loop,
4. holds its own **least-privilege castle identity** — submit + own-record
   disposition + event observation only.

## Lane model (the adapter's config, not routing logic)

The adapter's unit of configuration is the **lane**: one route name, its
trigger columns, and its workflow source. Lanes are declared statically
(JSON config file); the adapter performs no matching beyond the minimum any
trigger needs (trigger-tag gate + entry memo, below). One adapter process
can serve several lanes because the board mirror must reconcile a ticket's
runs **across** lanes (the watcher's rule: the ticket's most recent run is
the reconciliation authority — a fresh triage verdict and an in-flight dev
run interact on one card).

Lane config fields (per lane):

- `route_name` — the assignment's `workflow_name` ("route NAME"). Static:
  no selection, no override, nothing resolves beyond what is written here.
- `class` — `triage` or `develop`, driving the succeed-verdict handling
  (KB-9) when a run settles.
- `trigger_columns` — board columns whose ENTRY (KB-302 firing discipline)
  makes the lane eligible to fire a card.
- `workflow_source`, `lockfile_source` — pass-through assignment content.
  Castle requires a non-empty `workflow_source` (upstream
  criteria.v1 contract), so the lane carries its workflow source the same
  way the routes ConfigMap carried it pre-cutover. A lane without source
  fails closed at boot; it must never submit an assignment castle would
  reject for a missing source.
- `inputs` — static pass-through `input_values` (lane-level defaults).

Global config: Kanboard JSON-RPC endpoint + app token, castle Connect
endpoint + trigger identity token, castle project id for assignments,
board project name, trigger tag (default `k8s-run`), review and done
column names (defaults `Review`/`Done`), poll interval, state file path.

## Firing (board events → assignment)

Direct port of the watcher's firing loop with route resolution removed:

1. Poll: one `getAllTasks` listing per poll with per-task tag hydration
   (`getTaskTags`), per-project name/column caches (same request-frugality
   contract, CRI-252 applied to Kanboard).
2. **In-flight abstention**: a ticket with an active (non-terminal) run on
   any lane never fires.
3. **Entry memo, KB-302**: a card fires only on ENTRY into one of the
   lane's trigger columns (first sighting counts as entry, matching the
   watcher: a card already parked on the trigger column when the adapter
   boots is treated as entered-once). The memo is **durable** (state file,
   atomic JSON writes) — the watcher's in-memory memo loses its anti-refire
   protection (and the fire keys' idempotency alignment) on restart; a
   state file preserves it. Losing the state file degrades safely: keys
   are derived from a per-ticket fire sequence, and a re-derived key
   collides with the previously submitted assignment instead of creating a
   spurious duplicate run.
4. `k8s-run` trigger-tag gate (the "tag-lane tag"); cards without the tag
   never fire.
5. Repo binding: `repo:<owner>/<name>` tag; optional lane default;
   fail closed without either (watcher parity).
6. Submit: `castle.v1.ProjectService/SubmitProjectAssignment` —
   `project_id` (configured castle project; exists-check at submit time
   protects against a typo silently queueing into a void), `workflow_name`
   (lane route name), `workflow_source`/`lockfile_source` (lane config),
   `idempotency_key` (`kanboard:<board-project>:<ticket>:<fire-seq>`),
   `input_values` (payload, KB-318 conventions): `ticket_id`
   (`KB-<id>` plus board context keys) merged over the lane's static
   inputs, delivered downstream as `criteria.input.*` labels. The resolved
   repo binding is delivered as `repo_url` input: per-card `repo:` tag >
   lane `repo_default` > config `repo_default`, and the per-card resolved
   value overrides a static lane `inputs["repo_url"]` entry. The
   castle.v1 project-scoped surface is used because the workstream's
   "(route NAME + project + payload)" is exactly SubmitProjectAssignment's
   shape — the criteria.v1 `SubmitWorkflowAssignment` has no project or
   payload field.
7. On `created=false` (assignment exists — boot verification or state loss)
   the adapter adopts the returned run id for the mirror instead of firing
   again; a settled existing run means the fire is a no-op for the card's
   current entry only when combined with the durable entry memo.

What the adapter deliberately does NOT do (no-routing boundary): no routes
ConfigMap resolution, no workflow override semantics for ungrouped tags
(the watcher's tag-group machinery retires with it), no namespace /
cutover binding logic, no ticket→project routing table.

## Board mirror (castle events → column transitions)

Replaces watcher reconcile. For every run the adapter tracks (submits it
made, plus boot adoption), a per-run `WatchRun` stream
(`subscriber_id = "kanboard-trigger-adapter:<instance>"; server-persisted
cursor gives reconnect-resume) feeds a phase model:

- non-terminal envelopes keep the run active — **in flight ⇒ column moves
  suppressed** (KB-302 park-only mechanics);
- `RunFailed{Reason}` ⇒ verdict `failed`, reason recorded;
- `RunCompleted{FinalState, Success}` ⇒ verdict `final_state`;
- a terminal envelope ends the stream; run is settled.

Column transitions replicate the reconcile table exactly:

| condition | move |
|---|---|
| any active run for the ticket | none (in-flight park) |
| latest run failed (Failed envelope, or succeeded with verdict in the human-handoff set: `failed`, `need_assistance`, `awaiting_human`) | card → Review + one failure comment, once per run (comment fires at the move) |
| latest run succeeded, dev class, verdict `handler_complete` | card → Done (the verdict's PR evidence is the move authority, KB-50) |
| latest run succeeded, dev class, empty/unrecognized verdict | leave (abstain) |
| latest run succeeded, triage class (its own verdict set, incl. `handler_complete`) | leave (re-arm handoff stays the workflow's, KB-9) |
| card already in Review from a previous failure | no second move/comment |

The failed-run repair path is included: a run's failure verdict observed
while its card sits in Done repairs the card back to Review (the Done
stamp predates the verdict).

## Castle-side: submit-only (trigger) identity

New identity kind, parallel to orchestrator (CRI-133) and console (CRI-195):

- Store: `triggers` table (id, `criteria_id` unique, token_hash,
  rotated_at) — migration `0016` in both SQLite and Postgres engines.
- Store surface: `UpsertTrigger`/`ListTriggers`/`DeleteTrigger` and token
  resolution in `auth`.
- Interceptor: token resolution order becomes agent → orchestrator →
  trigger → console (distinct random material, mirroring the existing
  precedence note); a new `authorizeTriggerProcedure` restricts a caller
  holding a trigger id to exactly the adapter's set:
  `SubmitWorkflowAssignment`, `SubmitProjectAssignment`,
  `GetAssignmentDisposition`, `ListRunEvents`, `WatchRun`. Nothing else —
  no agent-owned CriteriaService, no Register (the interceptor's
  console-style denial extends to trigger identities), no run-control,
  no project mutations.
- Handlers: both assignment submit handlers and the disposition reader
  accept the trigger identity as the assignment owner
  (`OwnerCriteriaID`); every other project/handler entry point stays
  agent-only (`requireProjectCaller` rejects trigger ids).
- Provisioning (main.go): `--trigger-token` / `CASTLE_TRIGGER_TOKEN`
  with the orchestrator precedent's semantics — set ⇒ upsert+rotate,
  empty ⇒ revoke (delete).

## Tests

- auth: boundary contract tests (map membership, token resolution order,
  allowlist enforcement end-to-end through the interceptor).
- store: trigger upsert/rotate/delete, migration applied on both engines.
- adapter: firing-gate unit tests (entry discipline, trigger tag,
  in-flight abstention, repo fail-closed, idempotency key stability),
  mirror transition unit tests (full verdict table incl. KB-302 park,
  KB-9 triage abstention, KB-23 verdict spelling variants and KB-50 Done
  repair), and an integration test running castle + the adapter against a
  fake Kanboard: tag-lane tag → assignment → operator run (event submit)
  → column mirror.

## Out of scope (sibling/aftermath work, tracked on the criteria-k8s side)

- Retiring the criteria-k8s kanboard-watcher after shadow-run parity.
- Shrinking routes-sync to refs-only.
- Any operator-side change; the PROXY PRINCIPLE stays intact — run pods
  keep talking to the operator, castle only dispatches and records.