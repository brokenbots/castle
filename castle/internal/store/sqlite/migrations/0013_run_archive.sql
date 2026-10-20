-- Run archive records (KB-308): the pre-retention terminal-run summary the
-- criteria-k8s operator writes so run history outlives CriteriaRun retention
-- (#309). One row per castle run id — re-archiving the same run is an
-- idempotent overwrite (latest wins), so a retried archive leg never
-- duplicates history. record holds the JSON archive record (operator-owned,
-- evolving schema); operator_id is the authenticated token identity that
-- wrote it and archived_at the server-side stamp.
CREATE TABLE run_archive (
    run_id      TEXT PRIMARY KEY,
    job_name    TEXT NOT NULL DEFAULT '',
    final_phase TEXT NOT NULL DEFAULT '',
    ticket      TEXT NOT NULL DEFAULT '',
    record      TEXT NOT NULL,
    operator_id TEXT NOT NULL,
    archived_at TEXT NOT NULL
);

-- Coverage/backfill listing walks run-id ascending for stable keyset pages
-- (KB-308 scope item 5).
CREATE INDEX idx_run_archive_archived_at ON run_archive(archived_at DESC);
CREATE INDEX idx_run_archive_owner ON run_archive(operator_id);