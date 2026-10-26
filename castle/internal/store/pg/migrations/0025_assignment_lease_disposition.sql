-- 0025_assignment_lease_disposition: dispatch-scan disposition columns on the
-- assignment row (KB-343). The Option B failure-story contract makes a
-- non-granting lease scan queryable: lease_attempt counts the lease scans
-- that visited the row and last_lease_rejection names the dispatch rule that
-- most recently declined it (bare snake word; NULL on a granted lease or a
-- row never visited).
ALTER TABLE workflow_assignments ADD COLUMN IF NOT EXISTS lease_attempt BIGINT NOT NULL DEFAULT 0;
ALTER TABLE workflow_assignments ADD COLUMN IF NOT EXISTS last_lease_rejection TEXT;