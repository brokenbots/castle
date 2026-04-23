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

- Contracts: [api/README.md](api/README.md), [api/openapi.yaml](api/openapi.yaml), [api/events.schema.json](api/events.schema.json)
- Castle server (REST + WS + SQLite): [castle/](castle)
- Overseer CLI and execution engine: [overseer/](overseer)
- Workflow parser/compiler (HCL -> FSM): [workflow/](workflow)
- Shared event types: [shared/events/types.go](shared/events/types.go)
- Frontend app (Vite/React/TS): [parapet/](parapet)
- Architecture and phase details: [README.md](README.md), [PLAN.md](PLAN.md)

## Conventions agents should follow

- Go workspace uses multiple modules via [go.work](go.work); run commands from repo root using `make` targets when possible.
- Keep API changes synchronized across contracts and implementations:
  - Update `api/*` first.
  - Then update Castle handlers and WS/event handling.
  - Then update shared Go types and frontend API usage.
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
