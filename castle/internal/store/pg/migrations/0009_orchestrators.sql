-- 0009_orchestrators: orchestrator registry (KB-196).
CREATE TABLE IF NOT EXISTS orchestrators (
    id         TEXT PRIMARY KEY,
    name       TEXT NOT NULL,
    token_hash TEXT NOT NULL UNIQUE,
    created_at TIMESTAMPTZ NOT NULL
);