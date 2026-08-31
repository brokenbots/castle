-- 0007_assignment_queue: durable workflow assignment queue and idempotent submission.
-- Creates tables for queued submissions, eligibility labels, leases, attempts,
-- assignment ownership, idempotency keys, and terminal disposition. Relaxes
-- runs.overseer_id so queued runs can exist before an agent leases them, and
-- adds JSON agent labels to overseers for eligibility matching.

-- Queued runs are created without an owning agent; an agent is assigned
-- atomically when it leases the work. Existing CreateRun-created rows still
-- have a real overseer_id. SQLite does not support ALTER COLUMN, so recreate
-- the table preserving all columns and data added by prior migrations.
ALTER TABLE runs RENAME TO runs_old;

CREATE TABLE runs (
    id             TEXT PRIMARY KEY,
    overseer_id    TEXT REFERENCES overseers(id),
    workflow_name  TEXT NOT NULL,
    workflow_hcl   TEXT NOT NULL,
    status         TEXT NOT NULL,
    current_step   TEXT NOT NULL,
    last_seq       INTEGER NOT NULL DEFAULT 0,
    created_at     TEXT NOT NULL,
    ended_at       TEXT,
    variable_scope TEXT,
    pending_signal TEXT,
    paused_at      TEXT
);

INSERT INTO runs (id, overseer_id, workflow_name, workflow_hcl, status, current_step, last_seq, created_at, ended_at, variable_scope, pending_signal, paused_at)
SELECT id, overseer_id, workflow_name, workflow_hcl, status, current_step, last_seq, created_at, ended_at, variable_scope, pending_signal, paused_at
FROM runs_old;

DROP TABLE runs_old;

CREATE INDEX IF NOT EXISTS idx_runs_overseer ON runs(overseer_id);
CREATE INDEX IF NOT EXISTS idx_runs_status ON runs(status);

-- Agent labels used to match eligible assignments.
ALTER TABLE overseers ADD COLUMN labels TEXT NULL;

-- Workflow assignments: durable, idempotent submission queue.
CREATE TABLE IF NOT EXISTS workflow_assignments (
    id                  TEXT PRIMARY KEY,
    owner_criteria_id   TEXT NOT NULL,
    run_id              TEXT NOT NULL UNIQUE REFERENCES runs(id),
    workflow_name       TEXT NOT NULL,
    workflow_source     TEXT NOT NULL,
    lockfile_source     TEXT,
    idempotency_key     TEXT NOT NULL,
    state               TEXT NOT NULL DEFAULT 'queued',
    terminal_reason     TEXT,
    leased_criteria_id  TEXT,
    lease_expires_at    TEXT,
    created_at          TEXT NOT NULL,
    updated_at          TEXT NOT NULL
);

-- Idempotency is scoped to the owning caller.
CREATE UNIQUE INDEX IF NOT EXISTS idx_workflow_assignments_idempotency
    ON workflow_assignments(owner_criteria_id, idempotency_key);

CREATE INDEX IF NOT EXISTS idx_workflow_assignments_state
    ON workflow_assignments(state);

-- Eligibility labels for matching assignments to agents.
CREATE TABLE IF NOT EXISTS workflow_assignment_labels (
    assignment_id TEXT NOT NULL REFERENCES workflow_assignments(id) ON DELETE CASCADE,
    key           TEXT NOT NULL,
    value         TEXT NOT NULL,
    PRIMARY KEY (assignment_id, key)
);

CREATE INDEX IF NOT EXISTS idx_workflow_assignment_labels_lookup
    ON workflow_assignment_labels(assignment_id, key, value);

-- Lease records for an assignment.
CREATE TABLE IF NOT EXISTS workflow_assignment_leases (
    id            TEXT PRIMARY KEY,
    assignment_id TEXT NOT NULL REFERENCES workflow_assignments(id),
    criteria_id   TEXT NOT NULL REFERENCES overseers(id),
    created_at    TEXT NOT NULL,
    expires_at    TEXT NOT NULL
);

CREATE INDEX IF NOT EXISTS idx_workflow_assignment_leases_assignment
    ON workflow_assignment_leases(assignment_id);

-- Lease attempts (one row per attempted lease).
CREATE TABLE IF NOT EXISTS workflow_assignment_attempts (
    assignment_id TEXT NOT NULL REFERENCES workflow_assignments(id),
    attempt       INTEGER NOT NULL,
    criteria_id   TEXT,
    created_at    TEXT NOT NULL,
    completed_at  TEXT,
    outcome       TEXT,
    PRIMARY KEY (assignment_id, attempt)
);
