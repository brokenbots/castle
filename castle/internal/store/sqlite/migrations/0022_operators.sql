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
    -- The login namespace: unique among password-carrying rows only (see
    -- the partial unique index below). Token-only operators may share a
    -- name with a password operator — alias rows keep their legacy
    -- class-local names, and login resolves against password rows only.
    name          TEXT NOT NULL,
    display_name  TEXT NOT NULL DEFAULT '',
    -- Password carries the bcrypt hash of password-authenticated operators
    -- (console logins); token-only operators (agents, orchestrators,
    -- triggers) have no hash and can never log in with a password.
    password_hash TEXT,
    -- Privilege + classification attributes (e.g. {"role":"admin"}). Stored
    -- as a JSON object document; "{}" means "no attributes" (least
    -- privilege).
    attributes    TEXT NOT NULL DEFAULT '{}',
    created_at    TEXT NOT NULL,
    updated_at    TEXT NOT NULL
);

-- Login-name uniqueness applies among password-carrying operators only.
CREATE UNIQUE INDEX operators_name_login_uq
    ON operators(name) WHERE password_hash IS NOT NULL;

-- Operator-issued bearer tokens (KB-330): the credential space for tokens
-- castle mints directly. Legacy class tokens keep resolving through their
-- registries; this table only grows when an operator token is issued.
CREATE TABLE operator_tokens (
    id          TEXT PRIMARY KEY,
    operator_id TEXT NOT NULL REFERENCES operators(id) ON DELETE CASCADE,
    token_hash  TEXT NOT NULL UNIQUE,
    created_at  TEXT NOT NULL
);

-- Seed password operators from console_users: same id (the console
-- identity column keeps its value), username becomes the operator name,
-- admin role (the console login's effective scope: see-all reads, project
-- administration, run control).
INSERT INTO operators(id,name,display_name,password_hash,attributes,created_at,updated_at)
    SELECT id, username, username, password_hash, '{"role":"admin"}', created_at, updated_at
    FROM console_users;

-- Seed token-only alias operators from the legacy class registries, in
-- first-writer-wins order (a shared id across classes would have carried
-- an ambiguous role anyway). Triggers have no name column; the id names
-- the alias operator.
INSERT INTO operators(id,name,display_name,password_hash,attributes,created_at,updated_at)
    SELECT id, name, name, NULL, '{"role":"agent"}', created_at, created_at
    FROM overseers
    WHERE true
    ON CONFLICT(id) DO NOTHING;

INSERT INTO operators(id,name,display_name,password_hash,attributes,created_at,updated_at)
    SELECT id, name, name, NULL, '{"role":"orchestrator"}', created_at, created_at
    FROM orchestrators
    WHERE true
    ON CONFLICT(id) DO NOTHING;

INSERT INTO operators(id,name,display_name,password_hash,attributes,created_at,updated_at)
    SELECT id, id, id, NULL, '{"role":"trigger"}', created_at, created_at
    FROM triggers
    WHERE true
    ON CONFLICT(id) DO NOTHING;

-- Alias columns: every legacy identity row now points at its operator row.
ALTER TABLE overseers ADD COLUMN operator_id TEXT REFERENCES operators(id);
UPDATE overseers SET operator_id = id;
ALTER TABLE orchestrators ADD COLUMN operator_id TEXT REFERENCES operators(id);
UPDATE orchestrators SET operator_id = id;
ALTER TABLE triggers ADD COLUMN operator_id TEXT REFERENCES operators(id);
UPDATE triggers SET operator_id = id;

-- Re-key the login session ledger onto operators. Sessions survive: every
-- user_id already resolves to the seeded operator with the same id.
CREATE TABLE console_sessions_new (
    id          TEXT PRIMARY KEY,
    operator_id TEXT NOT NULL REFERENCES operators(id) ON DELETE CASCADE,
    token_hash  TEXT NOT NULL UNIQUE,
    created_at  TEXT NOT NULL,
    revoked_at  TEXT,
    membership  TEXT NOT NULL DEFAULT ''
);
INSERT INTO console_sessions_new(id, operator_id, token_hash, created_at, revoked_at, membership)
    SELECT id, user_id, token_hash, created_at, revoked_at, membership
    FROM console_sessions;
DROP TABLE console_sessions;
ALTER TABLE console_sessions_new RENAME TO console_sessions;

-- console_users is superseded by operators: password logins resolve there
-- now. Dropped last, so the old session FK never dangles during re-keying.
DROP TABLE console_users;