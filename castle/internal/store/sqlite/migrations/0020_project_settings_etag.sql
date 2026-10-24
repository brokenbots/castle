-- 0020_project_settings_etag: content-addressed etag for the project
-- settings doc (KB-324 S1). The settings doc is the ONE convention-keyed
-- JSON map per project (target_metadata defaults, serialization defaults,
-- retention, auth knobs); castle persists it verbatim and never interprets
-- it. The etag is sha256 over the canonical JSON of the sorted map, first
-- 32 hex chars; an absent or empty doc carries the empty etag, so the very
-- first swap may pass expected = "".

ALTER TABLE projects ADD COLUMN settings_etag TEXT NOT NULL DEFAULT '';
