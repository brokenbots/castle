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

## Container Image and Cluster Deployment

Castle lives at `https://github.com/brokenbots/castle` and the Castle server
image is built from the repo-root `Dockerfile.castle`: a multi-stage build
that compiles `./cmd/castle` with Go and ships it as a non-root Alpine image
with a persistent `/data` volume.

```sh
# Build and publish to the cluster-local registry consumed by the
# Kubernetes deployment (see below):
docker build -f Dockerfile.castle -t localhost:5000/castle:dev .
docker push localhost:5000/castle:dev
```

`make docker-build` builds the same image locally as `castle:dev`, and
`make compose-up` runs it through `compose.local.yml`. Images are published
to the local registry as `localhost:5000/castle` using a moving `dev` tag
plus numbered release tags (`v3` through `v10` to date).

Castle is a **hard deployment requirement** for the workflow runner
deployment in
[github.com/brokenbots/workflow-example](https://github.com/brokenbots/workflow-example):
its `k8s/04-castle.yaml` runs this image as a single-replica Deployment
(pinned to `localhost:5000/castle:dev`), and its `criteria-k8s` operator
reconciles per-scope adapter pods from the Castle event stream and fails
closed when Castle is not reachable. Without `--castle-addr`, per-scope
adapter reconcile stays disabled:
`castle observation disabled; per-scope adapter reconcile requires
--castle-addr (castle is the only lifecycle source since CRI-135)`
(`criteriarun_controller.go:322`). `provision_wanted` adapter-lifecycle
events reach the operator only through Castle since the `events.ndjson`
dual-write was retired, so runs with per-scope sessions cannot progress
without a reachable Castle; non-per-scope runs tolerate a disabled Castle.

## Target Architecture

Castle will implement `criteria.v1.CriteriaService` for long-lived agents and `criteria.v1.ServerService` for Parapet and operator clients. Workflow assignments remain on Criteria's Control stream. The local Compose acceptance system will run one Castle container, two labeled Criteria agent containers, and an independent submission/watch client.
