package pg_test

import (
	"context"
	"database/sql"
	"testing"

	"github.com/brokenbots/castle/castle/internal/store/pg"
	"github.com/brokenbots/castle/castle/internal/store/pg/pgtest"
)

// TestMigrateLaneEvidenceIndexes pins that the 0026 lane-evidence indexes
// exist after the pg store's migrations apply: the never-started reaper's
// sibling arms filter workflow_assignments.leased_criteria_id and the
// lease-anchor arm filters workflow_assignment_leases(criteria_id,
// expires_at), and both engines must carry the same indexes.
func TestMigrateLaneEvidenceIndexes(t *testing.T) {
	dsn := pgtest.DSN(t)
	s, err := pg.Open(dsn)
	if err != nil {
		t.Fatalf("pg.Open: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	raw, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatalf("open raw handle: %v", err)
	}
	t.Cleanup(func() { _ = raw.Close() })

	for _, idx := range []string{
		"idx_workflow_assignments_leased_criteria_id",
		"idx_workflow_assignment_leases_criteria_expires",
	} {
		var cnt int
		if err := raw.QueryRowContext(context.Background(),
			`SELECT COUNT(*) FROM pg_indexes WHERE indexname = $1`, idx).Scan(&cnt); err != nil {
			t.Fatalf("query pg_indexes for %s: %v", idx, err)
		}
		if cnt != 1 {
			t.Fatalf("expected index %s after pg migrations, got count=%d", idx, cnt)
		}
	}
}
