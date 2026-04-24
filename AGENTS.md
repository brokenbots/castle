# AGENTS.md

Repository guidance for AI coding agents working in this workspace.

## Scope and priorities

- Keep changes small and targeted; avoid broad refactors unless requested.
- Treat `api/` contracts as the source of truth for cross-component behavior.
- Prefer linking existing docs over duplicating details.

## Quick start commands

- Bootstrap dependencies: `make bootstrap`
- Build all components: `make build`
- Run all Go tests: `make test`
- Validate all example workflows: `make validate`

Component-specific:

- Run Castle (dev): `make dev-castle`
- Run Overseer (dev example): `make dev-overseer`
- Run Parapet dev server: `make dev-parapet`
- Build UI only: `cd parapet && npm run build`

## Project map

- Contracts (Phase 1+): `proto/overlord/v1/*.proto` (source of truth; generated Go in `shared/pb/`, TS in `parapet/src/gen/`). Managed with `buf`.
- Legacy contracts (Phase 0, being retired in 1.1): [api/README.md](api/README.md), [api/openapi.yaml](api/openapi.yaml), [api/events.schema.json](api/events.schema.json)
- Castle server (Connect/gRPC + SQLite): [castle/](castle)
- Overseer CLI and execution engine: [overseer/](overseer)
- Workflow parser/compiler (HCL -> FSM): [workflow/](workflow)
- Shared event types (Phase 0 hand-written; being replaced by generated protobuf): [shared/events/types.go](shared/events/types.go)
- Frontend app (Vite/React/TS, `@connectrpc/connect-web`): [parapet/](parapet)
- Architecture and phase details: [README.md](README.md), [PLAN.md](PLAN.md), [WORKSTREAM.md](WORKSTREAM.md)

## Conventions agents should follow

- Go workspace uses multiple modules via [go.work](go.work); run commands from repo root using `make` targets when possible.
- **Wire contract changes (Phase 1+)**: edit `proto/overlord/v1/*.proto` first; run `make proto` to regenerate Go and TS clients; then update Castle handlers, Overseer client, and Parapet call sites.
- Keep logs structured (`slog` JSON style in backend entrypoints).
- Preserve existing adapter boundaries in Overseer (`internal/adapter`, `internal/adapters/*`, `internal/dispatcher`).

## Common pitfalls

- Do not assume Copilot adapter code is active by default; real implementation is build-tagged (`copilot`) and default builds use a stub.
- Castle run/event ordering depends on server-assigned monotonic `seq` per `run_id`; avoid client-side ordering assumptions.
- Prefer `make test` over ad-hoc partial test runs unless task scope is clearly limited.
- Avoid introducing CGO-only SQLite dependencies; current storage uses pure-Go `modernc.org/sqlite`.

## High-value files for orientation

- Build and dev workflows: [Makefile](Makefile)
- Castle entrypoint: [castle/cmd/castle/main.go](castle/cmd/castle/main.go)
- Overseer entrypoint: [overseer/cmd/overseer/main.go](overseer/cmd/overseer/main.go)
- Workflow schema/compiler surface: [workflow/schema.go](workflow/schema.go)
- UI scripts/deps: [parapet/package.json](parapet/package.json)
