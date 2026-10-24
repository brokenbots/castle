-- 0019_runs_project_id: stamp every run with the project it belongs to
-- (KB-324 S1). Runs minted by assignment dispatch inherit the assignment's
-- project_id; standalone agent runs stay unscoped (NULL) and remain
-- console-operator-visible only. The membership read scope is the same
-- project_members predicate the assignment lease scan consults.

ALTER TABLE runs ADD COLUMN IF NOT EXISTS project_id TEXT NULL REFERENCES projects(id);

CREATE INDEX IF NOT EXISTS idx_runs_project_id ON runs(project_id);
