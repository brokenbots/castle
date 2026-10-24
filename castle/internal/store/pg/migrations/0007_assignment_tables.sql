-- 0007_assignment_tables: workflow assignments. The sqlite version recreates
-- runs so overseer_id becomes nullable (un-parked runs reattach); Postgres
-- gets the same shape via ALTER (no table rebuild needed) — pg-native column
-- order matches the CREATE order sqlite converged on.
ALTER TABLE runs ALTER COLUMN overseer_id DROP NOT NULL;
ALTER TABLE runs ADD COLUMN IF NOT EXISTS labels TEXT;

-- Agent labels used to match eligible assignments.
ALTER TABLE overseers ADD COLUMN IF NOT EXISTS labels TEXT;

CREATE TABLE IF NOT EXISTS workflow_assignments (
    id               TEXT PRIMARY KEY,
    owner_criteria_id TEXT NOT NULL,
    run_id           TEXT NOT NULL UNIQUE REFERENCES runs(id),
    workflow_name    TEXT NOT NULL,
    workflow_source  TEXT,
    lockfile_source  TEXT,
    idempotency_key  TEXT NOT NULL,
    state            TEXT NOT NULL DEFAULT 'queued',
    terminal_reason  TEXT,
    leased_criteria_id TEXT,
    lease_expires_at TIMESTAMPTZ,
    created_at       TIMESTAMPTZ NOT NULL,
    updated_at       TIMESTAMPTZ NOT NULL,
    UNIQUE (owner_criteria_id, idempotency_key)
);

CREATE INDEX IF NOT EXISTS idx_workflow_assignments_state ON workflow_assignments(state);

CREATE TABLE IF NOT EXISTS workflow_assignment_labels (
    assignment_id TEXT NOT NULL REFERENCES workflow_assignments(id) ON DELETE CASCADE,
    key           TEXT NOT NULL,
    value         TEXT NOT NULL,
    PRIMARY KEY (assignment_id, key)
);

CREATE INDEX IF NOT EXISTS idx_workflow_assignment_labels_lookup
    ON workflow_assignment_labels(assignment_id, key, value);

CREATE TABLE IF NOT EXISTS workflow_assignment_leases (
    id            TEXT PRIMARY KEY,
    assignment_id TEXT NOT NULL REFERENCES workflow_assignments(id),
    criteria_id   TEXT NOT NULL REFERENCES overseers(id),
    created_at    TIMESTAMPTZ NOT NULL,
    expires_at    TIMESTAMPTZ NOT NULL
);

CREATE INDEX IF NOT EXISTS idx_workflow_assignment_leases_assignment
    ON workflow_assignment_leases(assignment_id);

-- Lease attempts (one row per attempted lease).
CREATE TABLE IF NOT EXISTS workflow_assignment_attempts (
    assignment_id TEXT NOT NULL REFERENCES workflow_assignments(id),
    attempt       BIGINT NOT NULL,
    criteria_id   TEXT,
    created_at    TIMESTAMPTZ NOT NULL,
    completed_at  TIMESTAMPTZ,
    outcome       TEXT,
    PRIMARY KEY (assignment_id, attempt)
);