CREATE TABLE IF NOT EXISTS overseers (
    id           TEXT PRIMARY KEY,
    name         TEXT NOT NULL,
    hostname     TEXT,
    version      TEXT,
    token_hash   TEXT NOT NULL,
    status       TEXT NOT NULL DEFAULT 'online',
    created_at   TEXT NOT NULL,
    last_seen_at TEXT NOT NULL
);

CREATE TABLE IF NOT EXISTS runs (
    id            TEXT PRIMARY KEY,
    overseer_id   TEXT NOT NULL REFERENCES overseers(id),
    workflow_name TEXT NOT NULL,
    workflow_hcl  TEXT NOT NULL,
    status        TEXT NOT NULL,
    current_step  TEXT NOT NULL,
    last_seq      INTEGER NOT NULL DEFAULT 0,
    created_at    TEXT NOT NULL,
    ended_at      TEXT
);

CREATE INDEX IF NOT EXISTS idx_runs_overseer ON runs(overseer_id);
CREATE INDEX IF NOT EXISTS idx_runs_status ON runs(status);

CREATE TABLE IF NOT EXISTS events (
    run_id        TEXT NOT NULL,
    seq           INTEGER NOT NULL,
    type          TEXT NOT NULL,
    ts            TEXT NOT NULL,
    correlation_id TEXT,
    payload       TEXT NOT NULL,
    PRIMARY KEY (run_id, seq)
);
