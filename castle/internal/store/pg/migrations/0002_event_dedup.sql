-- 0002_event_dedup: at-most-once correlated event appends, same semantics as
-- the sqlite partial unique index. Carries the partition key because a
-- partial unique index on a partitioned table must include it.
CREATE UNIQUE INDEX IF NOT EXISTS idx_events_run_correlation
    ON events (run_id, correlation_id, ts)
    WHERE correlation_id IS NOT NULL AND correlation_id <> '';