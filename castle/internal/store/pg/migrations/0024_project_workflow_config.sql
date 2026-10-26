-- 0024_project_workflow_config: the project's ONE workflow config record
-- (KB-339). Project config — routes, workflow catalog entries, configLibrary
-- entries, the namespace binding payload, and castle-only digests — lives in
-- the record doc on the projects row: castle is the config home and the
-- ConfigMaps render FROM it (single writer). The doc is canonical JSON of a
-- criteria.brokenbots.dev/v1 ProjectWorkflowConfig record; shape validation
-- is the rpc layer's contract. The etag is sha256 over the canonical form of
-- the stored doc, first 32 hex chars; an absent doc carries the empty etag,
-- so the very first swap may pass expected = "".

ALTER TABLE projects ADD COLUMN IF NOT EXISTS workflow_config TEXT NOT NULL DEFAULT '';
ALTER TABLE projects ADD COLUMN IF NOT EXISTS workflow_config_etag TEXT NOT NULL DEFAULT '';