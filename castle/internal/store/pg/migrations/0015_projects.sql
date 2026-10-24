-- 0015_projects: first-class project entity (KB-318). Projects scope
-- workflow assignment dispatch: the store's lease path only offers project
-- assignments to agents in the project's member set. Also persists the agent
-- identity block on overseer rows (registered at Register time) and stamps it
-- onto runs for exact-build traceability.

CREATE TABLE IF NOT EXISTS projects (
    id                    TEXT PRIMARY KEY,
    membership_policy     TEXT NOT NULL,
    default_refire_policy TEXT NOT NULL DEFAULT '',
    queues                TEXT NOT NULL DEFAULT '',
    settings              TEXT NOT NULL DEFAULT '',
    info                  TEXT NOT NULL DEFAULT '',
    created_at            TIMESTAMPTZ NOT NULL,
    updated_at            TIMESTAMPTZ NOT NULL
);

-- Membership is enforced at the store layer: the lease scan consults this
-- table directly when matching project-scoped assignments.
CREATE TABLE IF NOT EXISTS project_members (
    project_id TEXT NOT NULL REFERENCES projects(id) ON DELETE CASCADE,
    member_id  TEXT NOT NULL,
    kind       TEXT NOT NULL,
    created_at TIMESTAMPTZ NOT NULL,
    PRIMARY KEY (project_id, member_id)
);

CREATE INDEX IF NOT EXISTS idx_project_members_member
    ON project_members(member_id);

-- Agent identity block (KB-318): the {version, repo_url, build_date,
-- sdk_versions, extra} block presented at registration, persisted verbatim.
ALTER TABLE overseers ADD COLUMN IF NOT EXISTS identity TEXT;

-- Project scoping of queued work: an empty project_id is unscoped global
-- work (pre-project behavior).
ALTER TABLE workflow_assignments ADD COLUMN IF NOT EXISTS project_id TEXT REFERENCES projects(id);
-- Workflow-generic assignment conventions: shape-validated string maps
-- castle stores and delivers verbatim but never interprets.
ALTER TABLE workflow_assignments ADD COLUMN IF NOT EXISTS input_values TEXT;
ALTER TABLE workflow_assignments ADD COLUMN IF NOT EXISTS target_metadata TEXT;

-- Exact-build traceability: the identity block of the agent executing the
-- run, stamped at lease time (and at CreateRun for agent-owned runs).
ALTER TABLE runs ADD COLUMN IF NOT EXISTS agent_identity TEXT NOT NULL DEFAULT '';

-- Assignment project scoping lookup.
CREATE INDEX IF NOT EXISTS idx_workflow_assignments_project
    ON workflow_assignments(project_id);