-- 0005_run_variable_scope: per-run variable scope blob (KB-150).
ALTER TABLE runs ADD COLUMN IF NOT EXISTS variable_scope TEXT NOT NULL DEFAULT '';