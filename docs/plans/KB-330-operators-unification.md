# KB-330 — operators unification (one operator model)

Spec: /data/intake/KB-330/workstreams/KB-330.md (immutable).

## Design

Collapse the four credential classes (console_users, overseers/agents,
orchestrators, triggers) into ONE `operators` table with attribute-driven
privileges, while preserving every existing wire identity, token, and
effective scope.

- `operators`: id PK, name UNIQUE, display_name, password_hash (NULL for
  token-only), attributes (JSONB / sqlite TEXT JSON), created_at, updated_at.
- `operator_tokens`: id PK, operator_id FK, token_hash UNIQUE, created_at —
  the bearer space for NEW operator-issued tokens.
- Migration 0022 (both engines): console_users seed operator rows carrying
  the SAME id (`console-default`), name=username, password_hash and
  attributes `{"role":"admin"}`; overseers/orchestrators/triggers seed
  mirror operator rows (roles `agent`/`orchestrator`/`trigger`) and gain a
  `operator_id` alias column backfilled to the legacy id. console_sessions
  re-keys user_id → operator_id (FK to operators). console_users is dropped.
- Legacy tables STAY as alias/token registries: overseer/orchestrator/trigger
  tokens keep resolving through their current tables and map onto operator
  context via the alias. Wire identities are unchanged.

## Privileges (deliverable 3)

Interceptor authorization switches from identity-kind maps to operator
attributes: role=admin → full console surface (see-all reads, project admin,
run control); role=orchestrator → orchestrator surface; role=trigger →
trigger surface; role=agent/unknown → agent surface + membership-scoped run
reads. Legacy direct-call paths (no operator context, tests) keep the old
kind-key fallbacks. Handlers (projects.go require* helpers, castle.go
membershipScopedCaller) consult `auth.CallerOperator`.

## New token issuance

`--operator-token` / `CASTLE_OPERATOR_TOKEN` provisions an admin operator
(`operator-admin`) + operator token; empty value revokes (mirrors the
orchestrator/trigger flag pattern).

## Steps

1. store: Operator/OperatorToken types, attributes helpers, Store methods,
   ConsoleSession.UserID→OperatorID, engines, relocate registry.
2. migrations 0022 (sqlite + pg) + data-seed tests (pre-0022 fixture).
3. auth: operator.go context + resolution, interceptor attribute checks.
4. rpc: attribute-driven scoping + project posture helpers.
5. main.go provisioning.
6. tests: suite parity, auth interceptor, rpc wire; then full gates.

## Tests

- store/suite: operator + token CRUD/rotation/cascade, session operator key.
- sqlite/pg migration tests: fresh + seeded (console→operators, alias ids).
- auth: admin-operator token allowed on console surface, denied on
  agent-owned write; legacy token compat (operator overlay present).
- rpc: admin operator sees all runs incl. foreign project-scoped runs;
  trigger-role operator denied project surfaces.