-- 0003_subscriptions: per-subscriber replay cursors.
CREATE TABLE IF NOT EXISTS run_subscriptions (
    subscriber_id TEXT NOT NULL,
    run_id        TEXT NOT NULL REFERENCES runs(id),
    last_seq      BIGINT NOT NULL,
    updated_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
    PRIMARY KEY (subscriber_id, run_id)
);