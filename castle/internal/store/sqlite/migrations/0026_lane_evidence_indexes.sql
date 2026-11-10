-- 0026_lane_evidence_indexes: index the columns the KB-371 queued-arm
-- lane-evidence predicates filter on (castle/internal/store/reap/reap.go).
-- The sibling arms filter workflow_assignments.leased_criteria_id and the
-- lane-anchor arm filters workflow_assignment_leases.criteria_id with
-- expires_at; lease rows are insert-only and never pruned, and the same
-- predicates re-run inside the writer transaction, so the reaper would scan
-- unindexed per eligible overseer exactly when candidates are numerous.
CREATE INDEX IF NOT EXISTS idx_workflow_assignments_leased_criteria_id ON workflow_assignments(leased_criteria_id);
CREATE INDEX IF NOT EXISTS idx_workflow_assignment_leases_criteria_expires ON workflow_assignment_leases(criteria_id, expires_at);
