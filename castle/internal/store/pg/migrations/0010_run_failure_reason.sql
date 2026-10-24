-- 0010_run_failure_reason: human-facing failure reason column (KB-209).
ALTER TABLE runs ADD COLUMN IF NOT EXISTS failure_reason TEXT;