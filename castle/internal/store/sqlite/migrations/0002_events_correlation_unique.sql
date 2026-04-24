-- Enforce at-most-once persistence per (run_id, correlation_id).
-- Overseer clients replay unacked envelopes on reconnect with the same
-- correlation_id, so Castle must not double-persist them.
-- NULL or empty correlation_id values are excluded so legacy rows (Phase 0)
-- that lacked a correlation id do not collide.
CREATE UNIQUE INDEX IF NOT EXISTS idx_events_run_correlation
    ON events(run_id, correlation_id)
    WHERE correlation_id IS NOT NULL AND correlation_id <> '';
