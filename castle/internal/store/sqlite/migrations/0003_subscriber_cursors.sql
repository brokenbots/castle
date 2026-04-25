CREATE TABLE IF NOT EXISTS run_subscriptions (
    subscriber_id TEXT NOT NULL,
    run_id        TEXT NOT NULL,
    last_seq      INTEGER NOT NULL,
    updated_at    TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP,
    PRIMARY KEY (subscriber_id, run_id)
);

CREATE INDEX IF NOT EXISTS idx_run_subscriptions_updated
    ON run_subscriptions(updated_at);
