-- 0005_run_variable_scope: per-run variable scope blob (KB-150). NULL =
-- unset, mirroring the sqlite profile where the column is added nullable.
ALTER TABLE runs ADD COLUMN IF NOT EXISTS variable_scope TEXT;