-- Trigger identities (KB-320): submit-only board-trigger identities. The
-- Kanboard trigger adapter submits workflow assignments (route name, project,
-- payload) on board events and reads castle run events to mirror outcomes back
-- onto the board; every other surface stays denied by the auth interceptor's
-- least-privilege allowlist. Token is stored as a SHA-256 hex digest like
-- agent tokens; it is never persisted in the clear.
CREATE TABLE triggers (
    id         TEXT PRIMARY KEY,
    token_hash TEXT NOT NULL UNIQUE,
    created_at TEXT NOT NULL
);
