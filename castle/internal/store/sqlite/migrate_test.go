package sqlite

import (
	"context"
	"database/sql"
	"embed"
	"sort"
	"strings"
	"testing"

	_ "modernc.org/sqlite"
)

//go:embed testdata/migrations_ok/*.sql
var testMigrationsOK embed.FS

//go:embed testdata/migrations_gap/*.sql
var testMigrationsGap embed.FS

//go:embed testdata/migrations_bad/*.sql
var testMigrationsBad embed.FS

func newMigrationTestDB(t *testing.T) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite", ":memory:")
	if err != nil {
		t.Fatalf("open in-memory sqlite: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db
}

func TestMigrate_FreshDB_AppliesAllMigrations(t *testing.T) {
	db := newMigrationTestDB(t)
	ctx := context.Background()
	wantVersions := discoveredRuntimeMigrationVersions(t)

	if err := Migrate(ctx, db); err != nil {
		t.Fatalf("Migrate failed: %v", err)
	}

	gotVersions := appliedVersionsSlice(t, db)
	if !equalIntSlices(gotVersions, wantVersions) {
		t.Fatalf("applied versions mismatch, want=%v got=%v", wantVersions, gotVersions)
	}

	var idxCount int
	if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM sqlite_master WHERE type='index' AND name='idx_events_run_correlation'`).Scan(&idxCount); err != nil {
		t.Fatalf("query sqlite_master index: %v", err)
	}
	if idxCount != 1 {
		t.Fatalf("expected idx_events_run_correlation to exist, got count=%d", idxCount)
	}
}

func TestMigrate_AssignmentQueueSchema(t *testing.T) {
	db := newMigrationTestDB(t)
	ctx := context.Background()

	if err := Migrate(ctx, db); err != nil {
		t.Fatalf("Migrate failed: %v", err)
	}

	expectedTables := []string{
		"workflow_assignments",
		"workflow_assignment_labels",
		"workflow_assignment_leases",
		"workflow_assignment_attempts",
	}
	for _, name := range expectedTables {
		var count int
		if err := db.QueryRowContext(ctx,
			`SELECT COUNT(*) FROM sqlite_master WHERE type='table' AND name=?`, name).Scan(&count); err != nil {
			t.Fatalf("query table %s: %v", name, err)
		}
		if count != 1 {
			t.Fatalf("expected table %s to exist, got count=%d", name, count)
		}
	}

	// runs.overseer_id must be nullable so queued runs can exist before leasing.
	var overseerNotNull int
	if err := db.QueryRowContext(ctx, `SELECT "notnull" FROM pragma_table_info('runs') WHERE name='overseer_id'`).Scan(&overseerNotNull); err != nil {
		t.Fatalf("query runs columns: %v", err)
	}
	if overseerNotNull != 0 {
		t.Fatalf("expected runs.overseer_id to be nullable, got notnull=%d", overseerNotNull)
	}

	// overseers.labels must exist to store eligibility labels.
	var labelsCount int
	if err := db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM pragma_table_info('overseers') WHERE name='labels'`).Scan(&labelsCount); err != nil {
		t.Fatalf("query overseers columns: %v", err)
	}
	if labelsCount != 1 {
		t.Fatalf("expected overseers.labels column to exist, got count=%d", labelsCount)
	}

	var idempCount int
	if err := db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM sqlite_master WHERE type='index' AND name='idx_workflow_assignments_idempotency'`).Scan(&idempCount); err != nil {
		t.Fatalf("query idempotency index: %v", err)
	}
	if idempCount != 1 {
		t.Fatalf("expected idempotency unique index, got count=%d", idempCount)
	}
}

func TestMigrate_AlreadyApplied_NoOp(t *testing.T) {
	db := newMigrationTestDB(t)
	ctx := context.Background()

	if err := Migrate(ctx, db); err != nil {
		t.Fatalf("first Migrate failed: %v", err)
	}
	var firstCount int
	if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM schema_migrations`).Scan(&firstCount); err != nil {
		t.Fatalf("count migrations after first run: %v", err)
	}

	if err := Migrate(ctx, db); err != nil {
		t.Fatalf("second Migrate failed: %v", err)
	}
	var secondCount int
	if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM schema_migrations`).Scan(&secondCount); err != nil {
		t.Fatalf("count migrations after second run: %v", err)
	}
	if secondCount != firstCount {
		t.Fatalf("expected no new migration rows on second run, first=%d second=%d", firstCount, secondCount)
	}
}

func TestMigrate_PreExistingSchema_AdoptsVersion1(t *testing.T) {
	db := newMigrationTestDB(t)
	ctx := context.Background()
	wantVersions := discoveredRuntimeMigrationVersions(t)

	// Simulate a pre-migrator database where the v1 schema already exists.
	if _, err := db.ExecContext(ctx, `
		CREATE TABLE overseers (
			id TEXT PRIMARY KEY,
			name TEXT NOT NULL,
			hostname TEXT,
			version TEXT,
			token_hash TEXT NOT NULL,
			status TEXT NOT NULL DEFAULT 'online',
			created_at TEXT NOT NULL,
			last_seen_at TEXT NOT NULL
		);
		CREATE TABLE runs (
			id TEXT PRIMARY KEY,
			overseer_id TEXT NOT NULL REFERENCES overseers(id),
			workflow_name TEXT NOT NULL,
			workflow_hcl TEXT NOT NULL,
			status TEXT NOT NULL,
			current_step TEXT NOT NULL,
			last_seq INTEGER NOT NULL DEFAULT 0,
			created_at TEXT NOT NULL,
			ended_at TEXT
		);
		CREATE INDEX idx_runs_overseer ON runs(overseer_id);
		CREATE INDEX idx_runs_status ON runs(status);
		CREATE TABLE events (
			run_id TEXT NOT NULL,
			seq INTEGER NOT NULL,
			type TEXT NOT NULL,
			ts TEXT NOT NULL,
			correlation_id TEXT,
			payload TEXT NOT NULL,
			PRIMARY KEY (run_id, seq)
		);
	`); err != nil {
		t.Fatalf("pre-seed schema: %v", err)
	}
	if _, err := db.ExecContext(ctx, `
		INSERT INTO overseers(id, name, hostname, version, token_hash, status, created_at, last_seen_at)
		VALUES('o1', 'alice', 'host-1', 'v0', 'h', 'online', '2026-01-01T00:00:00Z', '2026-01-01T00:00:00Z');
		INSERT INTO runs(id, overseer_id, workflow_name, workflow_hcl, status, current_step, last_seq, created_at)
		VALUES('r1', 'o1', 'wf', 'hcl', 'pending', 'step1', 0, '2026-01-01T00:00:00Z');
		INSERT INTO events(run_id, seq, type, ts, correlation_id, payload)
		VALUES('r1', 1, 'run.started', '2026-01-01T00:00:00Z', 'corr-1', '{}');
	`); err != nil {
		t.Fatalf("seed sample rows: %v", err)
	}

	if err := Migrate(ctx, db); err != nil {
		t.Fatalf("Migrate failed: %v", err)
	}

	gotVersions := appliedVersionsSlice(t, db)
	if !equalIntSlices(gotVersions, wantVersions) {
		t.Fatalf("applied versions mismatch on pre-existing schema, want=%v got=%v", wantVersions, gotVersions)
	}

	var rowCount int
	if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM overseers WHERE id='o1'`).Scan(&rowCount); err != nil {
		t.Fatalf("query overseers after migrate: %v", err)
	}
	if rowCount != 1 {
		t.Fatalf("expected pre-existing overseer row to remain, got count=%d", rowCount)
	}

	if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM events WHERE run_id='r1'`).Scan(&rowCount); err != nil {
		t.Fatalf("query events after migrate: %v", err)
	}
	if rowCount != 1 {
		t.Fatalf("expected pre-existing event row to remain, got count=%d", rowCount)
	}
}

func TestMigrate_OutOfOrderVersion_Rejected(t *testing.T) {
	db := newMigrationTestDB(t)
	ctx := context.Background()

	err := migrateWithFS(ctx, db, testMigrationsGap)
	if err == nil {
		t.Fatalf("expected migration gap error, got nil")
	}
	if !strings.Contains(err.Error(), "missing 0002") {
		t.Fatalf("expected error to mention missing 0002, got: %v", err)
	}
}

func TestMigrate_TransactionRollback(t *testing.T) {
	db := newMigrationTestDB(t)
	ctx := context.Background()

	err := migrateWithFS(ctx, db, testMigrationsBad)
	if err == nil {
		t.Fatalf("expected migration failure for invalid SQL, got nil")
	}

	var version1Count int
	if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM schema_migrations WHERE version = 1`).Scan(&version1Count); err != nil {
		t.Fatalf("query version 1: %v", err)
	}
	if version1Count != 1 {
		t.Fatalf("expected version 1 to remain recorded, got %d", version1Count)
	}

	var version2Count int
	if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM schema_migrations WHERE version = 2`).Scan(&version2Count); err != nil {
		t.Fatalf("query version 2: %v", err)
	}
	if version2Count != 0 {
		t.Fatalf("expected failed migration version 2 to remain unrecorded, got %d", version2Count)
	}

	var badTableCount int
	if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM sqlite_master WHERE type='table' AND name='broken_table'`).Scan(&badTableCount); err != nil {
		t.Fatalf("query sqlite_master for broken_table: %v", err)
	}
	if badTableCount != 0 {
		t.Fatalf("expected no partial table from failed migration, got count=%d", badTableCount)
	}
}

func discoveredRuntimeMigrationVersions(t *testing.T) []int {
	t.Helper()

	migrations, err := pendingMigrations(migrationsFS, map[int]struct{}{})
	if err != nil {
		t.Fatalf("discover runtime migrations: %v", err)
	}

	versions := make([]int, 0, len(migrations))
	for _, m := range migrations {
		versions = append(versions, m.version)
	}
	sort.Ints(versions)
	return versions
}

func appliedVersionsSlice(t *testing.T, db *sql.DB) []int {
	t.Helper()

	rows, err := db.Query(`SELECT version FROM schema_migrations ORDER BY version`)
	if err != nil {
		t.Fatalf("query applied versions: %v", err)
	}
	defer rows.Close()

	var out []int
	for rows.Next() {
		var version int
		if err := rows.Scan(&version); err != nil {
			t.Fatalf("scan applied version: %v", err)
		}
		out = append(out, version)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate applied versions: %v", err)
	}

	return out
}

func equalIntSlices(a, b []int) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
