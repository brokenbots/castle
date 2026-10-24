-- 0017_refire_policies: refire/classification policy as data (KB-321). Policy
-- rows are keyed (project_id, route) — ('', '') is the global default,
-- (project, '') the per-project assignment default, ('', route) a cross-project
-- route override, and (project, route) a per-route override. The policy column
-- holds the refire.MarshalPolicy JSON document verbatim.

CREATE TABLE IF NOT EXISTS refire_policies (
    project_id TEXT NOT NULL,
    route      TEXT NOT NULL,
    policy     TEXT NOT NULL,
    created_at TEXT NOT NULL,
    updated_at TEXT NOT NULL,
    PRIMARY KEY (project_id, route)
);

-- Attempt lineage for refired runs: the family root's run id (original
-- attempt; originals carry '') and the family attempt number (originals
-- carry 1).
ALTER TABLE workflow_assignments ADD COLUMN refire_root_run_id TEXT NOT NULL DEFAULT '';
ALTER TABLE workflow_assignments ADD COLUMN refire_attempt INTEGER NOT NULL DEFAULT 1;