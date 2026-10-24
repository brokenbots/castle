-- 0013_run_archive: terminal-run archive (KB-308). archive_key uses binary
-- collation ("C") so prefix-range pagination is byte-ordered, matching the
-- sqlite profile's BINARY collation semantics that store.PrefixUpperBound
-- and keyset pagination rely on. Record is a JSON text blob here; blobs move
-- to object storage with rows holding pointers in a later workstream.
CREATE TABLE IF NOT EXISTS run_archive (
    archive_key TEXT COLLATE "C" PRIMARY KEY,
    run_id      TEXT NOT NULL DEFAULT '',
    job_name    TEXT NOT NULL DEFAULT '',
    final_phase TEXT NOT NULL DEFAULT '',
    ticket      TEXT NOT NULL DEFAULT '',
    record      TEXT NOT NULL,
    operator_id TEXT NOT NULL DEFAULT '',
    archived_at TIMESTAMPTZ NOT NULL
);