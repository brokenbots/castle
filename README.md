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
- Unsecured (cleartext HTTP/2 via h2c) Connect/gRPC communication between the Overseer and Castle for development convenience
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

### Running your first workflow

Overlord workflows are HCL files that describe steps, transitions, and outcomes. The Overseer CLI provides three main commands:

- **`overseer compile`** — parse and validate a workflow, output the compiled JSON or DOT graph
- **`overseer plan`** — preview what the workflow will do (variables, agents, steps, transitions)
- **`overseer apply`** — execute the workflow locally (no Castle required) or against a Castle instance

```bash
# Build core binaries and plugin adapters
make build && make plugins

# Install adapters into the default discovery location
mkdir -p ~/.overlord/plugins
cp ./bin/overlord-adapter-* ~/.overlord/plugins/
chmod +x ~/.overlord/plugins/overlord-adapter-*

# Preview a workflow
./bin/overseer plan examples/hello.hcl

# Run locally (no Castle required)
./bin/overseer apply examples/hello.hcl

# Run against a Castle instance (requires Castle to be running)
./bin/overseer apply examples/agent_hello.hcl --castle http://localhost:8080
```

The `overseer run` command is deprecated but preserved as an alias for `apply --castle`.

### Workflow language at a glance

Workflows support:
- **Variables** (`variable "name" { ... }`) with type and default value
- **Step outputs** — adapters return key-value outputs that feed into downstream steps or branching logic
- **Wait nodes** — pause for a duration (`wait { duration = "30s" }`) or an external signal (`wait { signal = "..." }`)
- **Approval gates** — `approval { ... }` pauses until a user approves or rejects via Parapet
- **Branching** — `branch { when ... { transition_to = "..." } }` evaluates HCL expressions to choose the next step
- **Iteration** — `for_each { items = [...]; do = "step" }` runs a step multiple times with `each.value` and `each.index` bound

The full language reference is in [docs/workflow.md](docs/workflow.md). Plugin and adapter contracts are documented in [docs/plugins.md](docs/plugins.md).

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

### Crash Recovery

If an Overseer crashes mid-run, restart it against the same Castle and it will
attempt to pick up where it left off by re-running the step that was in
flight.

Crash recovery behavior notes:

- Resume re-executes the in-flight step from the beginning; partial step replay
   is not supported.
- Re-execution counts against `policy.max_step_retries`.
- This model assumes workflow steps are idempotent.

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
