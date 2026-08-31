# Castle

Castle is the central server and web interface for Criteria workflow agents.

This repository contains:

- `castle/`: a Go Connect/gRPC server backed by SQLite.
- `parapet/`: a React and TypeScript web UI for run observation and control.
- `compose.local.yml`: the local persistent Castle deployment.

## Migration Status

Castle and Parapet were extracted with their relevant history from the unpublished Overlord prototype. Executor, adapter, and workflow-engine code has been removed because Criteria replaces Overseer.

The retained `overlord.v1` protobuf and `shared/sdk/overseer` packages are temporary migration scaffolding. The Criteria-on-Castle project will replace them with the immutable `github.com/brokenbots/criteria/sdk` contract, currently pinned conceptually to Criteria commit `8ef3c05514ede491a48a1e7a9715acf29d11c43b` (`v0.0.0-20260831005623-8ef3c05514ed`). They are not a compatibility commitment.

## Development

Requirements:

- Go 1.26+
- Node.js 20+
- Buf
- Docker for container validation

```sh
make bootstrap
make ci
```

Run Castle locally:

```sh
make dev-castle
```

Run Castle in Docker with persistent SQLite storage:

```sh
make compose-up
```

Parapet can be started separately with `make dev-parapet`.

## Target Architecture

Castle will implement `criteria.v1.CriteriaService` for long-lived agents and `criteria.v1.ServerService` for Parapet and operator clients. Workflow assignments remain on Criteria's Control stream. The local Compose acceptance system will run one Castle container, two labeled Criteria agent containers, and an independent submission/watch client.
