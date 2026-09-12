-- Orchestrator identities (CRI-133): operator-facing accept-token identities
-- with read-only authority on the run-event read path. Token is stored as a
-- SHA-256 hex digest like agent tokens; it is never persisted in the clear.
CREATE TABLE orchestrators (
    id         TEXT PRIMARY KEY,
    name       TEXT NOT NULL,
    token_hash TEXT NOT NULL UNIQUE,
    created_at TEXT NOT NULL
);
