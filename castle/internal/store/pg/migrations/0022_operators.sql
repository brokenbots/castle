-- 0022_operators: ONE operator model (KB-330). Every credential class
-- collapses into a single operators identity: humans logging into the
-- console (password-carrying rows, role=admin), agents, orchestrators and
-- triggers (token-only alias rows carrying the role their credential class
-- already had). Privileges derive from the operator's attributes, never
-- from the credential class; the legacy tables stay as alias/token
-- registries so every existing wire identity keeps authenticating with its
-- current effective scope until explicitly re-registered.
--
-- Legacy identities keep their ids: the operator row seeded from a
-- console_users/overseers/orchestrators/triggers row carries the SAME id,
-- and the legacy table gains an operator_id alias column backfilled to it,
-- so wire identities on the identity columns keep resolving.

CREATE TABLE operators (
    id            TEXT PRIMARY KEY,
    name          TEXT NOT NULL UNIQUE,
    display_name  TEXT NOT NULL DEFAULT '',
    -- Password carries the bcrypt hash of password-authenticated operators
    -- (console logins); token-only operators (agents, orchestrators,
    -- triggers) have no hash and can never log in with a password.
    password_hash TEXT,
    -- Privilege + classification attributes (e.g. {"role":"admin"}). Stored
    -- as a JSONB document; {} means "no attributes" (least privilege).
    attributes    JSONB NOT NULL DEFAULT '{}'::jsonb,
    created_at    TIMESTAMPTZ NOT NULL,
    updated_at    TIMESTAMPTZ NOT NULL
);

-- Operator-issued bearer tokens (KB-330): the credential space for tokens
-- castle mints directly. Legacy class tokens keep resolving through their
-- registries; this table only grows when an operator token is issued.
CREATE TABLE operator_tokens (
    id          TEXT PRIMARY KEY,
    operator_id TEXT NOT NULL REFERENCES operators(id) ON DELETE CASCADE,
    token_hash  TEXT NOT NULL UNIQUE,
    created_at  TIMESTAMPTZ NOT NULL
);

-- Seed password operators from console_users: same id (the console
-- identity column keeps its value), username becomes the operator name,
-- admin role (the console login's effective scope: see-all reads, project
-- administration, run control).
INSERT INTO operators(id,name,display_name,password_hash,attributes,created_at,updated_at)
    SELECT id, username, username, password_hash, '{"role":"admin"}'::jsonb, created_at, updated_at
    FROM console_users;

-- Seed token-only alias operators from the legacy class registries, in
-- first-writer-wins order (a shared id across classes would have carried
-- an ambiguous role anyway). Triggers have no name column; the id names
-- the alias operator.
INSERT INTO operators(id,name,display_name,password_hash,attributes,created_at,updated_at)
    SELECT id, name, name, NULL, '{"role":"agent"}'::jsonb, created_at, created_at
    FROM overseers
    ON CONFLICT(id) DO NOTHING;

INSERT INTO operators(id,name,display_name,password_hash,attributes,created_at,updated_at)
    SELECT id, name, name, NULL, '{"role":"orchestrator"}'::jsonb, created_at, created_at
    FROM orchestrators
    ON CONFLICT(id) DO NOTHING;

INSERT INTO operators(id,name,display_name,password_hash,attributes,created_at,updated_at)
    SELECT id, id, id, NULL, '{"role":"trigger"}'::jsonb, created_at, created_at
    FROM triggers
    ON CONFLICT(id) DO NOTHING;

-- Alias columns: every legacy identity row now points at its operator row.
ALTER TABLE overseers ADD COLUMN IF NOT EXISTS operator_id TEXT REFERENCES operators(id);
UPDATE overseers SET operator_id = id WHERE operator_id IS NULL;
ALTER TABLE orchestrators ADD COLUMN IF NOT EXISTS operator_id TEXT REFERENCES operators(id);
UPDATE orchestrators SET operator_id = id WHERE operator_id IS NULL;
ALTER TABLE triggers ADD COLUMN IF NOT EXISTS operator_id TEXT REFERENCES operators(id);
UPDATE triggers SET operator_id = id WHERE operator_id IS NULL;

-- Re-key the login session ledger onto operators. Sessions survive: every
-- user_id already resolves to the seeded operator with the same id.
ALTER TABLE console_sessions RENAME COLUMN user_id TO operator_id;
ALTER TABLE console_sessions DROP CONSTRAINT IF EXISTS console_sessions_user_id_fkey;
ALTER TABLE console_sessions DROP CONSTRAINT IF EXISTS console_sessions_operator_id_fkey;
ALTER TABLE console_sessions
    ADD CONSTRAINT console_sessions_operator_id_fkey
    FOREIGN KEY (operator_id) REFERENCES operators(id) ON DELETE CASCADE;

-- console_users is superseded by operators: password logins resolve there
-- now.
DROP TABLE console_users;