# KB-318 — Project entity + agent identity block + project-scoped lease path

Design decisions for the castle-side delivery of KB-318. Source: Kanboard task
KB-318 (the referenced hermes plan doc `option-b-castle-orchestrator-scope.md`
is not accessible in this environment; the task text is authoritative).

## Constraint: the criteria.v1 wire contract is immutable here

`proto/criteria/v1/*.proto` are an exact mirror of the released
`github.com/brokenbots/criteria/sdk` (repo rule), and the pinned SDK's
`RegisterRequest`, `Agent`, `WorkflowAssignment`, and
`SubmitWorkflowAssignmentRequest` have no KB-318 fields. Upstream criteria
landed nothing for this task. Therefore every KB-318 wire addition must ride
castle-owned surfaces:

- **identity block in** — connect metadata on `Register` (User-Agent pattern),
  parsed by `AuthInterceptor.handleRegister` and injected into ctx, mirroring
  the caller-ID injection. Headers: `x-criteria-agent-version`,
  `x-criteria-agent-repo-url`, `x-criteria-agent-build-date`,
  `x-criteria-agent-sdk-version` (repeated `name:version`),
  `x-criteria-agent-extra` (repeated `key:value`),
  `x-criteria-agent-project` (project to join at registration).
- **project assignment submission in** — castle.v1 `ProjectService`
  (proto/castle/v1/projects.proto), following the CRI-195/KB-308 precedent of
  castle-owned services in `proto/castle/v1`.
- **conventions out to the agent** — the pinned control `WorkflowAssignment`
  message cannot gain fields, so `input_values` / `target_metadata` are
  delivered alongside routing labels under reserved prefixes
  (`criteria.input.<name>`, `criteria.target.<key>`) in the control message
  only; they stay structured in the store. When upstream adds first-class
  fields, the reserved-prefix delivery is dropped.

## Store model

- `projects(id, membership_policy open|closed, default_refire_policy, queues
  [json], settings [json], info [json], created_at, updated_at)`;
  `project_members(project_id, member_id, kind ai|human, created_at)`.
- `WorkflowAssignment` gains `ProjectID` (empty = unscoped global work),
  `InputValues`, `TargetMetadata` (json columns).
- `Overseer` gains `Identity` (json column, nullable) — the identity block
  persisted at Register.
- `Run` gains `AgentIdentity` (json, "" = unknown) — stamped at `CreateRun`
  (agent-owned calls) and at lease time from the leased overseer row, so each
  assignment→run records the exact build that executed it.
- Membership is enforced in `LeaseWorkflowAssignment` itself (both engines):
  an assignment's project is visible to the scan only when the leasing agent
  is a `project_members` row of that project. Labels continue to partition
  within the pool. Non-members get ErrNotFound (the same signal as
  "nothing eligible").
- `AddProjectMember` (operator-managed, any policy) vs `JoinProject`
  (self-join; open policy only, `ErrProjectClosed` otherwise). Removing a
  member demotes future lease eligibility only; leases already granted are
  unaffected.

## castle.v1 ProjectService boundary

Implemented posture (pre-user-auth; member management is strictly
self-service until authenticated operators exist):

- `CreateProject`: any registered caller may create (operators self-serve
  project bootstrap; operator identities are agents today).
- `AddProjectMember`: self-join only (`member_id` must equal the caller) and
  only for OPEN projects — `closed` admission is operator-managed and has no
  self-service path (PermissionDenied). Self-adds record kind `ai`;
  human-kind membership is operator-managed and arrives with user auth.
- `RemoveProjectMember`: self-leave only (`member_id` must equal the caller,
  PermissionDenied otherwise, decided on caller identity without disclosing
  whether the target exists).
- `GetProject` / `ListProjects` / `ListProjectMembers`: readable by
  registered callers.
- `SubmitProjectAssignment`: agent identity = owner (same rule as
  criteria.v1 `SubmitWorkflowAssignment`); carries `project_id`,
  `input_values`, `target_metadata` (shape-validated only — sizes capped,
  castle never interprets values and does not resolve them against the
  tree's interface configSlots; that resolution is criteria territory),
  and composes idempotency per domain as owner + idempotency_key.
- Project ids are caller-supplied slugs (`[a-z0-9][a-z0-9-_.]{0,127}`).

## Acceptance mapping

- project-scoping store tests (both engines): member leases project work,
  non-member is rejected, unscoped work unaffected, labels partition inside
  the project pool, identity stamped on run, conventions round-trip.
- rpc wire tests: register with identity block + project join (headers),
  identity echoed on ListAgents/GetAgent (`identity` label, JSON block),
  member assignment leased + control delivered with conventions, non-member
  agent never leases it, run/events flow.
- PROXY PRINCIPLE recorded: docs + code comments at the dispatch boundary.

## PROXY PRINCIPLE

Operator owns the run lifecycle; run pods talk to the operator, never to
castle. Castle observes progress only through agent-submitted events and
exerts control only through agent-visible lifecycle calls (cancel/stop/pause)
and assignment delivery.