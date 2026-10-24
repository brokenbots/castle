-- 0004_attempts: step attempt ledger. started_at/completed_at are nullable
-- so the in-flight start leg can be recorded before completion.
CREATE TABLE IF NOT EXISTS run_attempts (
    run_id       TEXT NOT NULL REFERENCES runs(id),
    step         TEXT NOT NULL,
    attempt      BIGINT NOT NULL,
    started_at   TIMESTAMPTZ,
    completed_at TIMESTAMPTZ,
    outcome      TEXT,
    PRIMARY KEY (run_id, step, attempt)
);

CREATE INDEX IF NOT EXISTS idx_run_attempts_run_step
    ON run_attempts(run_id, step);