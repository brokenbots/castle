# Overlord

![Go](https://img.shields.io/badge/Go-1.26+-00ADD8?style=flat&logo=go)
![License](https://img.shields.io/badge/license-TBD-lightgrey)
![Status](https://img.shields.io/badge/status-prototype-orange)

Overlord is an agent management platform for orchestrating complex AI-assisted and automated workflows in a secure, scalable, and deterministic manner.

---

## Overview

Modern AI-driven development workflows involve multiple agents, scripts, and validation steps that need to be coordinated reliably. Overlord provides a declarative, state-driven framework for defining what a workflow needs to accomplish and how to get there — while keeping humans in control of the outcomes.

Workflows are described in **HCL** (HashiCorp Configuration Language), defining an initial state, a target state, and the steps to transition between them. Execution is managed by a lightweight local controller called the **Overseer**, which runs each workflow as a finite state machine (FSM). A central server — the **Castle** — tracks the state of all running workflows and provides APIs for tooling and interfaces. A separate web application — **Parapet** — provides the real-time user interface.

---

## Core Concepts

### Job

A Job is the unit of work in Overlord. It is declared as:

- **Initial state** — the current condition of the system or codebase
- **Target state** — the desired outcome
- **Workflow** — the HCL-defined graph of steps, transitions, and outcomes to move between states

### Overseer

The Overseer is the local workflow executor. It:

- Runs as a **single cross-platform Go binary** — easy to deploy anywhere
- Executes the workflow as a **finite state machine (FSM)** graph, ensuring deterministic outcomes
- Prevents runaway agent loops by enforcing state transition rules
- Gates on external actions (e.g., running tests) before advancing state
- Uses a **pluggable adapter architecture** to interact with existing agent harnesses — it wraps them rather than replacing them

Supported agent backends (planned):
- Shell / script execution
- Claude Code
- GitHub Copilot CLI
- Gemini CLI

### Castle

The Castle is the central coordination server. It:

- Maintains a registry of connected Overseers and their workflow states
- Exposes a **Connect / gRPC API** (`connectrpc.com/connect`) for all control-plane and streaming operations; the same handlers speak gRPC, gRPC-Web, and the Connect protocol, so browsers, Go clients, and `curl` all work against one endpoint
- Serves wire payloads as **protobuf or JSON** depending on the client's requested codec
- Streams live workflow events to subscribers via server-streaming RPCs
- Is designed for **high availability** deployment

### Parapet

Parapet is the web user interface for Overlord. It:

- Is a separate **React/TypeScript** application
- Talks to Castle over HTTPS using generated Connect clients (`@connectrpc/connect-web`) — no WebSocket or separate proxy required
- Displays workflow status, step history, and live updates from Overseers via `CastleService.WatchRun` server-streaming RPCs

---

## Architecture

```
             ┌────────────────────────────┐
             │          Parapet           │
             │      (React / TypeScript)  │
             └─────────────┬──────────────┘
                           │ HTTPS (Connect: JSON or protobuf)
┌──────────────────────────▼──────────────────────────┐
│                        Castle                       │
│                                                      │
│  ┌───────────────────────────────────┐                     │
│  │ Connect / gRPC handlers       │                     │
│  │ (unary + server-stream + bidi)│                     │
│  └────────────────────────┬─────────────┘                     │
│                           │                             │
│  ┌─────────────────────────▼───────────┐                │
│  │        Workflow State Store     │                │
│  │        (SQLite)                 │                │
│  └─────────────────────────────────┘                │
└──────────────────────┬───────────────────────────────┘
                       │  gRPC over HTTP/2 (TLS or mTLS)
           ┌───────────┼───────────┐
           │           │           │
    ┌──────▼──┐  ┌─────▼───┐  ┌───▼─────┐
    │Overseer │  │Overseer │  │Overseer │
    │  (FSM)  │  │  (FSM)  │  │  (FSM)  │
    └──┬──┬───┘  └─────────┘  └─────────┘
       │  │
       │  └──── Scripts / Tests
       │
    ┌──▼──────────────────────┐
    │   Agent Adapters        │
    │  Claude Code            │
    │  Copilot CLI            │
    │  Gemini CLI             │
    └─────────────────────────┘
```

---

## Technology Stack

| Component          | Technology                                                       |
|--------------------|------------------------------------------------------------------|
| Overseer           | Go — single cross-platform binary                                |
| Castle Server      | Go                                                               |
| Workflow DSL       | HCL (HashiCorp Configuration Language)                           |
| Wire schema        | Protocol Buffers (`proto/overlord/v1`), managed with `buf`       |
| Castle API         | Connect / gRPC / gRPC-Web (HTTPS, optional mTLS)                 |
| Castle Events      | Server-streaming RPC (`CastleService.WatchRun`)                  |
| Wire codec         | Runtime-selectable: JSON or binary protobuf                      |
| Castle Storage     | Embedded DB (SQLite via pure-Go `modernc.org/sqlite`)            |
| Parapet UI         | React / TypeScript + `@connectrpc/connect-web`                   |

---

## Design Principles

**Deterministic execution** — The FSM enforces explicit state transitions. Agents cannot loop indefinitely or skip validation gates.

**Pluggable agent backends** — Overlord wraps existing agent harnesses (Claude Code, Copilot CLI, Gemini, etc.) rather than replacing them. Adapters are swappable.

**Declarative workflows** — Workflows are described in HCL, making them readable, version-controllable, and auditable.

**Simple deployment** — The Overseer is a single binary. No agent-side infrastructure required. Overseers can self-register to any reachable Castle given the right credentials.

**Standard protocols** — Connect / gRPC / gRPC-Web over HTTPS allow any compatible client (Go, browser, `grpcurl`, `curl`) to interact with the Castle from a single schema, enabling diverse interfaces and integrations.

---

## Prototype Scope (v0)

The initial prototype establishes the core communication loop and workflow visibility:

- A simple HCL workflow definition that controls an agent and executes shell commands via the Overseer
- Unsecured (plain HTTP/WS) communication between the Overseer and Castle for development convenience
- The Castle tracks and stores current workflow state (active step, transitions)
- A minimal Parapet UI (React/TypeScript) that queries Castle APIs and displays live workflow state

---

## Getting Started

### Prerequisites

- Go 1.26+
- Node.js 20+ (for Parapet)
- Docker (optional, for local container workflow)

### Bootstrap

```bash
make bootstrap
```

### Build

```bash
# Build all components
make build
```

### Run (Native)

```bash
# Start the Castle
make dev-castle

# Start an Overseer and register it to the Castle
make dev-overseer

# Optional: run the UI
make dev-parapet
```

### Run (Docker Compose)

```bash
# Build and start Castle + Overseer
make compose-up

# Stop and remove containers
make compose-down
```

The compose stack uses `compose.local.yml` and starts:
- `castle` on `http://localhost:8080`
- `overseer` configured to run `examples/hello.hcl`

The default Overseer runtime image is intentionally minimal and contains only the Overseer binary plus example workflow files.

### Runtime Configuration

Both binaries support environment variables as additive fallback to existing flags.
Precedence is:

1. CLI flag
2. Environment variable
3. Built-in default

Castle runtime variables:

| Variable | Flag | Default |
|---|---|---|
| `CASTLE_ADDR` | `--addr` | `:8080` |
| `CASTLE_DB_PATH` | `--db` | `./castle.db` |

Overseer runtime variables:

| Variable | Flag | Default |
|---|---|---|
| `OVERSEER_CASTLE_URL` | `--castle` | `http://localhost:8080` |
| `OVERSEER_WORKFLOW` | `--workflow` | _(required if flag not set)_ |
| `OVERSEER_NAME` | `--name` | hostname |

Example env-based startup:

```bash
CASTLE_ADDR=:8080 CASTLE_DB_PATH=./castle.dev.db go run ./castle/cmd/castle

OVERSEER_CASTLE_URL=http://localhost:8080 \
OVERSEER_WORKFLOW=./examples/build_and_test.hcl \
go run ./overseer/cmd/overseer run
```

---

## Contributing

Contribution guidelines will be added once the project structure is established.

## License

TBD
