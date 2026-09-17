-- Console human users and their login sessions (CRI-195). Passwords are
-- stored as bcrypt hashes — never plaintext or reversibly — and session
-- tokens as SHA-256 hex digests like agent and orchestrator tokens.
CREATE TABLE console_users (
    id            TEXT PRIMARY KEY,
    username      TEXT NOT NULL UNIQUE,
    password_hash TEXT NOT NULL,
    created_at    TEXT NOT NULL,
    updated_at    TEXT NOT NULL
);

CREATE TABLE console_sessions (
    id         TEXT PRIMARY KEY,
    user_id    TEXT NOT NULL REFERENCES console_users(id) ON DELETE CASCADE,
    token_hash TEXT NOT NULL UNIQUE,
    created_at TEXT NOT NULL
);