# KB-308 — Pre-retention run archive leg (the plan-of-record while the workstream is the spec)

Working branch: KB-308. Sibling card repo: brokenbots/workflow-example
(criteria-k8s operator). This repo (brokenbots/castle) owns the archive STORE surface.

## Contract (the part castle owns): schema.v1.json

The archive record is a nested JSON document. One record per terminal CriteriaRun.
Top-level fields:

| field                 | type     | meaning                                                |
|-----------------------|----------|--------------------------------------------------------|
| schema_version        | int      | record schema version; 1 today                          |
| run_id                | string   | castle run id ("" when the CR ran without a castle run) |
| job_name              | string   | runner job name; "" when the job never became a job     |
| ticket                | string   | ticket id (KB ticket column)                            |
| repo                  | string   | repository the workflow operated on                     |
| workflow_name         | string   | workflow name; "" unavailable                           |
| workflow_ref          | string   | workflow ref (branch/sha); "" unavailable               |
| tree_digest           | string   | tree digest of the workflow tree; "" unavailable        |
| binding_cm_checksum   | string   | checksum of the binding ConfigMap used ("": unknown)    |
| project_cm_checksum   | string   | checksum of the project ConfigMap ("": unknown)         |
| operator_version      | string   | criteria-k8s version that wrote the record              |
| final_phase           | string   | CR terminal phase: Succeeded / Failed                   |
| conditions            | object[] | `{type,status,reason,message,last_transition_time}`     |
| failure_reason        | string   | workflow verdict text; "" on success                    |
| steps                 | object[] | `{name, outcome}` for every step the run visited        |
| restarts              | int      | pod restart count observed before terminal              |
| backoff_evictions     | int      | eviction count observed before terminal                 |
| terminal_cleanup      | string   | "deferred"/"cleared"/"" from the TerminalCleanup condition |
| started_at            | string   | RFC3339; "" when the run never started                  |
| ended_at              | string   | RFC3339; "" when not terminal (never today)             |
| archive_key           | string   | explicit CR-derived record key (e.g. "cr-uid/<uid>") for records with NO castle run; "" for run-backed records |

One record per ARCHIVE KEY: the effective key is archive_key when set, else
run_id (both empty are rejected). Records with a castle run key on that
run_id; run-id-less terminal CRs archive under an explicit CR-derived
`archive_key` (e.g. "cr-uid/<metadata.uid>", the only namespace agents may
use) so a terminal CR is never unarchivable. `archived_at` is NOT an
operator field: castle
stamps it server-side and returns it in `ArchiveRunResponse`/`GetRunArchiveResponse`
(and per-row in `ArchiveSummary`). Wire bodies are protojson lowerCamelCase
(e.g. `schemaVersion`, `bindingCmChecksum`); the table above uses the field
names for reference.

## Storage design

`run_archive` table: `archive_key TEXT PRIMARY KEY` — the effective archive
key ("run-archive/<run_id>" for run-backed records, the explicit CR key for
run-id-less records); `run_id` is a plain column ("" for run-id-less records).
Re-archiving the same key UPDATEs in place (latest wins, never duplicates);
the first non-empty writer identity locks the row's `operator_id`
(`store.ErrArchiveOperatorConflict` → permission_denied for other identities).

Store API (interface + sqlite impl):
- `PutRunArchiveRecord(ctx, rec *RunArchiveRecord) (created bool, err error)`
  idempotent PUT keyed on rec.Key; rec.Key and rec.Record are required; a
  cross-identity overwrite fails with `ErrArchiveOperatorConflict`.
- `GetRunArchiveRecord(ctx, key) (*RunArchiveRecord, error)`
- `ListRunArchives(ctx, keyPrefix, afterKey string, limit int, pageToken string) ([]*RunArchiveRecord, string, error)`
  ascending archive_key keyset pages (default page 100, max 1000).

## Wire (proto)

`proto/castle/v1/archive.proto` — package castle.v1, new service `RunArchiveService`
with `ArchiveRun`, `GetRunArchive`, `ListRunArchives`. Generated Go goes under
castle/gen/castle/v1. TS generation for the console is not needed in this card:
the run-viewer UI is not part of this leg; castle-side TS bindings regenerate anyway
if a consumer needs them (out of scope).

## Auth model (scope item 2: token auth, like the podstate feed)

Operator uses its existing token: the criteria agent token (CASTLE_TOKEN) — the
podstate feed's exact token path (SubmitEvents with a criteria agent token). The
operator's CriteriaRun controller already authenticates this way for per-run event
ingest and per-run status stamps. The service handler reads the same
auth.CallerCriteriaID identity; the operator may write a record only for a run
whose overseer (agent identity) it is — enforced end-to-end against the
EFFECTIVE key (the key the stored row hangs off: archive_key when set, else
run_id), because validating one run while keying another would let a caller
forge run-attributed history or squat a key the legitimate owner can never
archive to. A key naming an existing castle run requires the run to resolve
and be owned by the caller AND the record's run_id to equal that key
(unknown run: not_found; another identity's run: permission_denied; keying
after an owned run with a foreign/ghost/empty run_id in the body:
invalid_argument); run_id and archive_key must agree when both are set
(invalid_argument); a run-id-less record must keep the CR-derived "cr-uid/"
namespace (invalid_argument otherwise). Cross-writer protection for keys no
castle run backs is castle's first-writer-wins provenance gate
(permission_denied to a second identity) on top of that namespace rule.
This follows the criteria-k8s identity model:
the operator already owns a criteria identity (podstate feed), and every other
operator→castle write in this codebase (SubmitEvents, run.metadata stamps) uses it.

## Operator-side checklist in workflow-example (sibling card — the card in that repo)

- criteria-k8s/internal/castle/archive.go: PutArchive REST client (one endpoint).
- criteria-k8s/internal/controller/criteriarun_controller.go: terminal-leg hook
  with TerminalArchivePending condition and backoff.
- criteria-k8s/internal/controller/backfill_archive.go: one-shot seed for the 435
  CRs already terminal (idempotent, uses the same wire PUT).
- criteria-k8s/api/v1/criteriarun_types.go: `Status.ArchiveID`,
  `Status.ArchivePending` fields.

## Order of work on castle

- [x] Read-only survey (done): interceptor paths, console pattern, migrations, gen.
- [x] Plan doc (this file)
- [x] proto/castle/v1/archive.proto + gen
- [x] migration 0013_run_archive.sql
- [x] store surface + sqlite impl + tests
- [x] RPC service + handlers + tests
- [x] main.go wiring
- [x] full `make ci` green

## What is deliberately NOT in this leg

- No Parapet UI changes (nothing visible in the viewer changes with this leg).
- No criteria.v1 schema changes (the record is castle-owned; the criteria mirror stays
  verbatim-by-rule).
- No operator code (that goes to the workflow-example sibling card).
- No retention changes (#309 owns deletion; the archive leg only guarantees the record
  exists before deletion).