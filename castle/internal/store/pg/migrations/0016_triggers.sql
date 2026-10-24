-- 0016_triggers: trigger identity registry (KB-320). Submit-only
-- board-trigger identities (e.g. the Kanboard trigger adapter): they submit
-- workflow assignments and read castle run events; every other surface stays
-- denied by the auth interceptor's least-privilege allowlist.
CREATE TABLE IF NOT EXISTS triggers (
    id         TEXT PRIMARY KEY,
    token_hash TEXT NOT NULL UNIQUE,
    created_at TIMESTAMPTZ NOT NULL
);