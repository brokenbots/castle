# Overlord

![Go](https://img.shields.io/badge/Go-1.24+-00ADD8?style=flat&logo=go)
![License](https://img.shields.io/badge/license-TBD-lightgrey)
![Status](https://img.shields.io/badge/status-Phase%200%20prototype-orange)

Overlord is an agent management platform for orchestrating complex AI-assisted and automated workflows in a secure, scalable, and deterministic manner.

---

## Overview

Modern AI-driven development workflows involve multiple agents, scripts, and validation steps that need to be coordinated reliably. Overlord provides a declarative, state-driven framework for defining what a workflow needs to accomplish and how to get there — while keeping humans in control of the outcomes.

Workflows are described in **HCL** (HashiCorp Configuration Language), defining an initial state, a target state, and the steps to transition between them. Execution is managed by a lightweight local controller called the **Overseer**, which runs each workflow as a finite state machine (FSM). A central server — the **Castle** — tracks the state of all running workflows and provides APIs for tooling and interfaces. A separate web application — **Parapet** — provides the real-time user interface.

---

## Repository Layout

```
overlord/
├── api/                  # Source-of-truth wire contracts (OpenAPI + JSON Schema)
├── castle/               # Castle server (Go)
├── examples/             # Example workflow .hcl files
├── overseer/             # Overseer CLI (Go)
├── parapet/              # Web UI (Vite + React + TS + RTK + Tailwind)
├── shared/               # Shared Go packages (events wire types)
├── workflow/             # HCL workflow parser + FSM compiler
├── go.work               # Go workspace tying all Go modules together
├── Makefile              # Build / test / dev-* / validate targets
├── PLAN.md               # Phased roadmap
└── README.md
```

The four Go modules (`shared`, `workflow`, `overseer`, `castle`) are stitched together by `go.work` and use `replace` directives during prototype development.

---

## Core Concepts

### Job

A Job is the unit of work in Overlord. It is declared as:

- **Initial state** — the current condition of the system or codebase
- **Target state** — the desired outcome
- **Workflow** — the HCL-defined graph of steps, transitions, and outcomes to move between states

### Overseer

The Overseer is the local workflow executor. It:

- Runs as a **single cross-platform Go binary**
- Executes the workflow as a **finite state machine (FSM)** graph, ensuring deterministic outcomes
- Prevents runaway agent loops by enforcing state transition rules and a `MaxTotalSteps` ceiling
- Gates on external actions (e.g., running tests) before advancing state
- Uses a **pluggable adapter architecture** to interact with existing agent harnesses — it wraps them rather than replacing them

Adapter status:
- `shell` — implemented (Phase 0)
- `copilot` — implemented behind the `copilot` build tag, using the GitHub Copilot Go SDK (`github.com/github/copilot-sdk/go`); each developer authenticates locally via `gh auth login`/Copilot CLI
- `claude`, `gemini` — Phase 1

### Castle

The Castle is the central coordination server. It:

- Maintains a registry of connected Overseers and their workflow runs in **SQLite** (pure-Go `modernc.org/sqlite`, no CGO)
- Exposes a versioned **REST API** under `/api/v0` (chi router) for control-plane operations
- Exposes two **WebSocket** endpoints: one bidirectional channel per connected Overseer at `/api/v0/ws`, plus per-run client streams at `/api/v0/runs/{id}/stream`
- Assigns the run ID and a monotonic `seq` to every event, providing a single source of truth ordering
- Is designed to scale up to HA deployment in Phase 2

### Parapet

Parapet is the web user interface. It:

- Is a separate **Vite + React + TypeScript** application
- Uses **Redux Toolkit Query** to talk to Castle's REST API and **WebSocket** for live event streams
- Styled with **Tailwind CSS**
- Displays workflow status, step history, and live updates

---

## Architecture

```
                  ┌────────────────────────────┐
                  │          Parapet           │
                  │   (React + RTK + Tailwind) │
                  └─────────────┬──────────────┘
                                │ HTTP + WebSocket
┌───────────────────────────────▼────────────────────────────┐
│                           Castle                            │
│                                                             │
│  ┌──────────────┐   ┌──────────────┐  ┌─────────────────┐  │
│  │   REST API   │   │  WS server   │  │  Per-run hub    │  │
│  │ (chi router) │   │ (coder/ws)   │  │  fan-out        │  │
│  └──────┬───────┘   └──────┬───────┘  └────────┬────────┘  │
│         │                  │                   │           │
│  ┌──────▼──────────────────▼───────────────────▼────────┐  │
│  │  store.Store interface — SQLite impl (Phase 0)       │  │
│  │  overseers, runs, events (PK run_id+seq)             │  │
│  └──────────────────────────────────────────────────────┘  │
└─────────────────────────────┬──────────────────────────────┘
                              │  REST + single bidi WS per Overseer
              ┌───────────────┼────────────────┐
              │               │                │
       ┌──────▼─────┐  ┌──────▼─────┐  ┌───────▼────┐
       │  Overseer  │  │  Overseer  │  │  Overseer  │
       │  FSM eng.  │  │  FSM eng.  │  │  FSM eng.  │
       └──┬─────────┘  └────────────┘  └────────────┘
          │
          ├─► shell adapter
          └─► copilot adapter (build tag)
```

### Wire contracts

The contracts are frozen up front so Castle, Overseer and Parapet can be built in parallel:

- **REST**: [api/openapi.yaml](api/openapi.yaml) (`/api/v0`)
- **WebSocket events**: [api/events.schema.json](api/events.schema.json) — every envelope has `schema_version`, `run_id`, `seq`, `type`, `ts`, `correlation_id`, `payload`. Castle assigns `seq` (monotonic per run).

---

## Technology Stack

| Component        | Technology                                                |
|------------------|-----------------------------------------------------------|
| Overseer         | Go 1.24 — single cross-platform binary, cobra CLI         |
| Castle Server    | Go 1.24 — chi router, coder/websocket, modernc.org/sqlite |
| Workflow DSL     | HCL v2 (`github.com/hashicorp/hcl/v2`)                    |
| Castle REST API  | HTTP/JSON under `/api/v0`                                 |
| Castle Events    | WebSocket (one bidi per Overseer, plus per-run streams)   |
| Castle Storage   | SQLite (pure-Go, no CGO) behind `store.Store` interface   |
| Parapet UI       | Vite + React 18 + TypeScript + Redux Toolkit (RTKQ) + Tailwind |
| Build/dev tasks  | GNU `make` (`Makefile`)                                   |

---

## Design Principles

**Deterministic execution.** The FSM enforces explicit state transitions. Agents cannot loop indefinitely or skip validation gates. A `MaxTotalSteps` policy puts a hard ceiling on every run.

**Pluggable agent backends.** Overlord wraps existing agent harnesses (Copilot, Claude, Gemini, etc.) rather than replacing them. Adapters implement a small `Adapter` interface and are registered with the dispatcher.

**Declarative workflows.** Workflows are described in HCL: readable, version-controllable, auditable. A standalone `overseer validate` command verifies a workflow without running it.

**Castle is the source of truth.** Run IDs, event sequence numbers, and run status all originate at the Castle. Overseers may be restarted without losing the canonical history.

**Frozen wire contracts.** OpenAPI + JSON Schema are checked into `api/` and treated as authoritative. Generated code or hand-written clients in any language must conform.

**Simple deployment.** The Overseer is a single binary. The Castle is a single binary plus a SQLite file.

---

## Prototype Scope (v0)

Phase 0 establishes the core loop end-to-end:

- HCL workflow definition with FSM compiler and reachability/terminal validation
- `shell` adapter executes commands, streams stdout/stderr line-by-line into events
- `copilot` adapter (build tag) drives the GitHub Copilot agent SDK
- Castle REST + WS protocol versioned at `/api/v0`, persisted in SQLite
- Castle assigns run IDs and monotonic per-run `seq`
- Parapet: run list, run detail with live event tail, overseer list

Communication is **plain HTTP/WS** in Phase 0; mTLS / SSH tunnels are Phase 1.

---

## Getting Started

### Prerequisites

- Go 1.24+
- Node.js 20+ (for Parapet)
- GNU `make`

### Build everything

```bash
make build           # builds all Go modules + Parapet
# or, manually:
( cd castle  && go build -o castle  ./cmd/castle  )
( cd overseer && go build -o overseer ./cmd/overseer )
( cd parapet && npm install && npm run build )
```

### Run the smoke test

```bash
# Terminal 1 — start the Castle
./castle/castle --addr :8080 --db ./castle.db

# Terminal 2 — run the hello workflow against it
./overseer/overseer run --workflow examples/hello.hcl --castle http://localhost:8080

# Terminal 3 — start the UI (proxies /api to localhost:8080)
( cd parapet && npm run dev )
# open http://localhost:5173
```

### Validate workflows without running them

```bash
./overseer/overseer validate examples/*.hcl
```

### Build with the Copilot adapter

```bash
( cd overseer && go build -tags copilot -o overseer ./cmd/overseer )
```

---

## Contributing

See [PLAN.md](PLAN.md) for the phased roadmap and current focus areas.

## License

TBD
