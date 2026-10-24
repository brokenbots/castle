-- 0006_run_signal_pause: pending signal + paused stamping (KB-160).
ALTER TABLE runs ADD COLUMN IF NOT EXISTS pending_signal TEXT;
ALTER TABLE runs ADD COLUMN IF NOT EXISTS paused_at    TIMESTAMPTZ;