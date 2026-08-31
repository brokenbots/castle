# Castle Contributor Guide

## Scope

This repository owns the Castle control-plane server and the Parapet web UI. Criteria owns workflow execution, adapters, and the agent-side protocol SDK.

## Commands

- Bootstrap: `make bootstrap`
- Full local gate: `make ci`
- Castle tests: `cd castle && go test -race ./...`
- Parapet tests: `cd parapet && npm run test:run`
- Regenerate bindings: `make proto`
- Container smoke: `make compose-up`

## Boundaries

- Keep persistence and orchestration logic independent of generated protobuf types where practical.
- Treat `github.com/brokenbots/criteria/sdk` as the Criteria wire-contract source of truth.
- Do not add Overseer executor, workflow-engine, or adapter implementations to this repository.
- Update protobuf sources before generated Go or TypeScript bindings.
- Do not hand-edit generated files under `shared/pb` or `parapet/src/gen`.
- Keep Castle compatible with a single-replica SQLite deployment until an external-store project explicitly changes that constraint.

## Migration Baseline

The extracted `overlord.v1` schema and `shared/sdk/overseer` package are temporary scaffolding. Replace them through reviewed Criteria compatibility work; do not extend them as a public API.
