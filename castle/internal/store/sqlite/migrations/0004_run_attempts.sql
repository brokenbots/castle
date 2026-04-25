CREATE TABLE IF NOT EXISTS run_attempts (
    run_id       TEXT      NOT NULL,
    step         TEXT      NOT NULL,
    attempt      INTEGER   NOT NULL,
    started_at   TIMESTAMP NOT NULL,
    completed_at TIMESTAMP,
    outcome      TEXT,
    PRIMARY KEY (run_id, step, attempt)
);

CREATE INDEX IF NOT EXISTS idx_run_attempts_run_step
    ON run_attempts(run_id, step);
