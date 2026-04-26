-- 0006_wait_signal: add pending_signal and paused_at columns to runs (W05)
ALTER TABLE runs ADD COLUMN pending_signal TEXT NULL;
ALTER TABLE runs ADD COLUMN paused_at TEXT NULL;
