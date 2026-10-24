-- 0014_artifact_blob_pointers: object-storage pointer columns for run
-- archives (KB-317 artifact blob plan). Rows become pointers: the archive
-- record grows blob_uri, blob_sha256 and blob_size so a record row can
-- reference an uploaded blob instead of embedding it. Columns are
-- NOT NULL DEFAULT '' so all existing rows stay valid pointers-to-nothing
-- until the uploader leg lands; storing a blob is a later workstream.
ALTER TABLE run_archive ADD COLUMN IF NOT EXISTS blob_uri    TEXT NOT NULL DEFAULT '';
ALTER TABLE run_archive ADD COLUMN IF NOT EXISTS blob_sha256 TEXT NOT NULL DEFAULT '';
ALTER TABLE run_archive ADD COLUMN IF NOT EXISTS blob_size   BIGINT NOT NULL DEFAULT 0;