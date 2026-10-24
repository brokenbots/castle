-- 0009_orchestrators: orchestrator registry (KB-196).
CREATE TABLE IF NOT EXISTS orchestrators (
    id         TEXT PRIMARY KEY,
    name       TEXT NOT NULL UNIQUE,
    token_hash TEXT NOT NULL,
    created_at TIMESTAMPTZ NOT NULL
);