-- 0008_run_metadata: ticket linkage columns (KB-180).
ALTER TABLE runs ADD COLUMN IF NOT EXISTS ticket   TEXT;
ALTER TABLE runs ADD COLUMN IF NOT EXISTS repo_url TEXT;
ALTER TABLE runs ADD COLUMN IF NOT EXISTS pr_url   TEXT;