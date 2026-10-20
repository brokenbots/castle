-- Run archive records (KB-308): the pre-retention terminal-run summary the
-- criteria-k8s operator writes so run history outlives CriteriaRun retention
-- (#309). One row per archive key — "run-archive/<run_id>" for terminal CRs
-- with a castle overseer run, or an explicit CR-derived key (the record's
-- archive_key, e.g. "cr-uid/<uid>") for CRs with no castle run — so a
-- terminal CR is NEVER unarchivable. Re-archiving the same key is an
-- idempotent overwrite (latest wins), so a retried archive leg never
-- duplicates history; overwriting with a different identity than the first
-- writer is refused (store.ErrArchiveOperatorConflict). record holds the
-- JSON archive record (operator-owned, evolving schema); operator_id is the
-- authenticated token identity that wrote it and archived_at the
-- server-side stamp.
CREATE TABLE run_archive (
    archive_key TEXT PRIMARY KEY,
    run_id      TEXT NOT NULL DEFAULT '',
    job_name    TEXT NOT NULL DEFAULT '',
    final_phase TEXT NOT NULL DEFAULT '',
    ticket      TEXT NOT NULL DEFAULT '',
    record      TEXT NOT NULL,
    operator_id TEXT NOT NULL,
    archived_at TEXT NOT NULL
);
