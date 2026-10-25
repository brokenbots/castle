# Castle environment variables

Castle is configured with CLI flags; every flag has an environment-variable
equivalent (the flag usage text names it). This page documents the variables
and their current defaults. KB-326 changed the anonymous-read defaults.

## Anonymous reads

`--allow-anon-reads` governs what an unauthenticated (anonymous) caller may
read from the ServerService: run reads (`GetRun`, `InspectRun`,
`ListRunEvents`, `WatchRun`) and the observation surfaces (agent/orchestrator
reads, run-archive reads). **The default is `false` since KB-326 (S3): a
fresh castle refuses anonymous read RPCs.**

| Variable | Default | Meaning |
| --- | --- | --- |
| `CASTLE_ALLOW_ANON_READS` | off | When `true`, enables anonymous reads without the CLI flag. Precedence: CLI flag > env > off. |

Semantics when enabled:

- Project-scoped runs (runs stamped with a project id) are **never**
  anonymously readable — listed, read, or streamed — regardless of the flag
  or any project setting. An anonymous caller refusing at the run gate
  answers `not_found` so denied runs do not leak their existence.
- Legacy unscoped runs (no project stamp) are anonymously readable while the
  dev override is on.
- Without the dev override, a run is anonymously readable only if some
  project in the owning agent's membership sets the per-project
  **allow_anonymous_reads** setting (below).

## Per-project allow_anonymous_reads setting

The project settings doc (`SetProjectSettings`) is a map of convention-keyed
JSON scalars. Castle persists other keys verbatim; it interprets exactly one
key itself (all keys are validated at set time — JSON scalars only):

| Key | Value | Meaning |
| --- | --- | --- |
| `allow_anonymous_reads` | JSON boolean (`true` / `false`) | Grants anonymous read callers visibility of this project's agents' **legacy unscoped runs** (runs without a project stamp) even without the global dev override. Absent or `false` grants nothing. |

Scope discipline:

- The setting only ever widens the *legacy unscoped* space. It cannot make a
  project-scoped run anonymously readable.
- Multiple projects may count the same agent as a member; any one project's
  `allow_anonymous_reads=true` grants the point-read (union semantics).
- The grant applies to run reads (`GetRun`, `InspectRun`, `ListRunEvents`,
  `WatchRun`). Anonymous list surfaces remain dev-override-only
  (`CASTLE_ALLOW_ANON_READS`) and show legacy unscoped runs.

## Other configuration

| Variable | Default | Meaning |
| --- | --- | --- |
| `CASTLE_ADDR` | `:8080` | listen address |
| `CASTLE_DB_PATH` | `./castle.db` | SQLite database path (profile `sqlite`) |
| `CASTLE_STORE` | `sqlite` | storage engine profile: `sqlite` (local/dev) or `pg` (KB-317) |
| `CASTLE_PG_DSN` | *(empty)* | Postgres DSN for `--store pg` |
| `CASTLE_TLS_CERT` / `CASTLE_TLS_KEY` / `CASTLE_TLS_CA` / `CASTLE_TLS_CLIENT_CA` | *(empty)* | TLS material; loading a server certificate enables TLS mode |
| `CASTLE_GRPC_REFLECTION` | `true` without TLS, `false` with TLS | gRPC reflection |
| `CASTLE_ORCHESTRATOR_TOKEN` | *(empty)* | orchestrator accept token provisioning; empty revokes the identity |
| `CASTLE_TRIGGER_TOKEN` | *(empty)* | kanboard trigger accept token provisioning; empty revokes the identity |
| `OVERLORD_CASTLE_BOOTSTRAP_TOKEN` | *(empty)* | agent Register bootstrap token; empty disables Register |
| `CASTLE_CONSOLE_USER` / `CASTLE_CONSOLE_PASSWORD` | *(empty)* | console login provisioning; both must be set (empty revokes console login) |
| `CASTLE_EVENT_BUFFER_CAPACITY` | hub default | in-memory events retained per run for WatchRun replay |
| `CASTLE_AGENT_HEARTBEAT_INTERVAL` | `10s` | nominal agent heartbeat cadence; the reaper derives staleness from it |
| `CASTLE_RUN_REAP_MULTIPLIER` | `6` | reap pending/running runs whose heartbeat is older than this multiple of the heartbeat interval; `0` disables |
| `CASTLE_CREATED_NEVER_STARTED_WINDOW` | `10m` | reap runs that never started this long after creation; `0` disables |
| `CASTLE_REFIRE_INTERVAL` | `15s` | refire/classification policy pass cadence; `0` disables |
| `CASTLE_REFIRE_POLICY_FILE` | *(empty)* | refire policy file upserted at boot (KB-321) |