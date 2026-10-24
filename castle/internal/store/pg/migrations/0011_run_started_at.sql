-- 0011_run_started_at: started-at stamp + created/id ordering index (KB-219,
-- CRI-202 cursor ordering parity).
ALTER TABLE runs ADD COLUMN IF NOT EXISTS started_at TIMESTAMPTZ;
CREATE INDEX IF NOT EXISTS idx_runs_created_id ON runs(created_at DESC, id DESC);