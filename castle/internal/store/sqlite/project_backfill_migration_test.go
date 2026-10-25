package sqlite

import (
	"context"
	"database/sql"
	"slices"
	"testing"
	"time"

	"github.com/brokenbots/castle/castle/internal/store"
	"github.com/brokenbots/castle/castle/internal/store/migrate"
)

// seedPreBackfillDB rebuilds the live 0022 schema and seeds a realistic
// pre-0019 dispatch history for the KB-332 project backfill tests: one
// overseer, legacy kanboard_* and foreign-harness runs with project_id NULL
// (the column is added by 0019 and left untouched), plus — when seedProject
// is true — the kanboard-tickets project row created by the live seeding.
func seedPreBackfillDB(t *testing.T, db *sql.DB, seedProject bool) {
	t.Helper()
	ctx := context.Background()

	migs, err := migrate.Discover(migrationsFS)
	if err != nil {
		t.Fatalf("discover runtime migrations: %v", err)
	}
	if _, err := db.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS schema_migrations (
		version INTEGER PRIMARY KEY,
		applied_at TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP
	)`); err != nil {
		t.Fatalf("create schema_migrations: %v", err)
	}
	for _, m := range migs {
		if m.Version >= 23 {
			break
		}
		if _, err := db.ExecContext(ctx, string(m.SQL)); err != nil {
			t.Fatalf("apply %s: %v", m.Name, err)
		}
		// Record the version so the runner below treats the pre-0023
		// schema as already applied and runs only 0023.
		if _, err := db.ExecContext(ctx,
			`INSERT INTO schema_migrations(version) VALUES(?)`, m.Version); err != nil {
			t.Fatalf("record %s: %v", m.Name, err)
		}
	}

	now := time.Date(2026, 10, 25, 0, 0, 0, 0, time.UTC).Format(store.TimestampLayout)
	seed := []string{
		`INSERT INTO overseers(id,name,token_hash,status,created_at,last_seen_at)
		 VALUES ('ovs-kb332','worker','agent-hash','online','` + now + `','` + now + `')`,
	}
	if seedProject {
		seed = append(seed, `INSERT INTO projects(id,membership_policy,created_at,updated_at)
		 VALUES ('kanboard-tickets','open','`+now+`','`+now+`')`)
	}
	for _, r := range []struct{ id, workflow string }{
		{"run-kbd-dev-1", "kanboard_develop_v1"},
		{"run-kbd-tri-1", "kanboard_triage_v1"},
		{"run-lin-1", "linear_intake_v1"},
		{"run-arc-1", "arc_shepherd_v1"},
	} {
		seed = append(seed, `INSERT INTO runs(id,overseer_id,workflow_name,workflow_hcl,status,current_step,created_at)
		 VALUES ('`+r.id+`','ovs-kb332','`+r.workflow+`','workflow "kb-332" {}','succeeded','done','`+now+`')`)
	}
	for _, q := range seed {
		if _, err := db.ExecContext(ctx, q); err != nil {
			t.Fatalf("seed: %v: %q", err, q)
		}
	}
}

// projectIDsOfRun fetches the stamped project_id for a run.
func projectIDsOfRun(t *testing.T, db *sql.DB, runID string) sql.NullString {
	t.Helper()
	var projectID sql.NullString
	if err := db.QueryRowContext(context.Background(),
		`SELECT project_id FROM runs WHERE id = ?`, runID).Scan(&projectID); err != nil {
		t.Fatalf("project_id of %s: %v", runID, err)
	}
	return projectID
}

// TestMigrate_ProjectBackfill_AssignsKanboardDispatches drives the KB-332
// cutover backfill over a realistic pre-0023 database: kanboard_* dispatch
// history joins the kanboard-tickets project, foreign harness/legacy runs
// stay unscoped, and the version ledger records the one-shot migration.
func TestMigrate_ProjectBackfill_AssignsKanboardDispatches(t *testing.T) {
	db := newMigrationTestDB(t)
	seedPreBackfillDB(t, db, true)
	ctx := context.Background()

	if err := Migrate(ctx, db); err != nil {
		t.Fatalf("Migrate: %v", err)
	}

	for _, r := range []struct {
		id       string
		wantNull bool
	}{
		{id: "run-kbd-dev-1", wantNull: false},
		{id: "run-kbd-tri-1", wantNull: false},
		{id: "run-lin-1", wantNull: true},
		{id: "run-arc-1", wantNull: true},
	} {
		got := projectIDsOfRun(t, db, r.id)
		if r.wantNull != !got.Valid {
			t.Fatalf("run %s project_id = %v, want null=%v", r.id, got, r.wantNull)
		}
		if got.Valid && got.String != "kanboard-tickets" {
			t.Fatalf("run %s project_id = %q, want kanboard-tickets", r.id, got.String)
		}
	}

	var applied int
	if err := db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM schema_migrations WHERE version = 23`).Scan(&applied); err != nil {
		t.Fatalf("version ledger: %v", err)
	}
	if applied != 1 {
		t.Fatalf("0023 stamped %d times, want 1", applied)
	}

	// One-shot: a second pass does not rewrite (or double-apply) anything.
	if err := Migrate(ctx, db); err != nil {
		t.Fatalf("second Migrate: %v", err)
	}
	if err := db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM schema_migrations WHERE version = 23`).Scan(&applied); err != nil {
		t.Fatalf("ledger after re-migrate: %v", err)
	}
	if applied != 1 {
		t.Fatalf("0023 stamped %d times after re-migrate, want 1", applied)
	}
}

// TestMigrate_ProjectBackfill_NoProjectRow_KeepsRunsUnscoped pins the guard:
// without the kanboard-tickets project row the migration leaves every run
// unscoped instead of stamping dangling references.
func TestMigrate_ProjectBackfill_NoProjectRow_KeepsRunsUnscoped(t *testing.T) {
	db := newMigrationTestDB(t)
	seedPreBackfillDB(t, db, false)
	ctx := context.Background()

	if err := Migrate(ctx, db); err != nil {
		t.Fatalf("Migrate: %v", err)
	}
	for _, id := range []string{"run-kbd-dev-1", "run-kbd-tri-1", "run-lin-1", "run-arc-1"} {
		if got := projectIDsOfRun(t, db, id); got.Valid {
			t.Fatalf("run %s project_id = %q, want NULL (no project row)", id, got.String)
		}
	}
}

// TestMigrate_ProjectBackfill_ScopedReadsSeeRealSpace exercises the KB-332
// verification leg over the migrated store: a project member's scoped reads
// surface the backfilled kanboard dispatch history, non-members see none of
// it, unscoped lists lose the backfilled runs, and console operators still
// see all of them.
func TestMigrate_ProjectBackfill_ScopedReadsSeeRealSpace(t *testing.T) {
	db := newMigrationTestDB(t)
	seedPreBackfillDB(t, db, true)
	ctx := context.Background()

	if err := Migrate(ctx, db); err != nil {
		t.Fatalf("Migrate: %v", err)
	}
	s := &Store{db: db, reader: db}

	if err := s.AddProjectMember(ctx, &store.ProjectMember{
		ProjectID: "kanboard-tickets",
		MemberID:  "criteria-k8s-operator",
		Kind:      store.ProjectMemberKindAI,
	}); err != nil {
		t.Fatalf("AddProjectMember: %v", err)
	}

	listIDs := func(runs []*store.Run) []string {
		ids := make([]string, 0, len(runs))
		for _, r := range runs {
			ids = append(ids, r.ID)
		}
		return ids
	}

	// The member sees the backfilled kanboard dispatch history.
	runs, _, err := s.ListRunsVisibleTo(ctx, "criteria-k8s-operator", "", "", 0, "")
	if err != nil {
		t.Fatalf("ListRunsVisibleTo(member): %v", err)
	}
	got := listIDs(runs)
	slices.Sort(got)
	if !slices.Equal(got, []string{"run-kbd-dev-1", "run-kbd-tri-1"}) {
		t.Fatalf("member saw %v, want the kanboard dispatch history", got)
	}

	// A non-member agent correctly sees none of it.
	if runs, _, err = s.ListRunsVisibleTo(ctx, "outsider-agent", "", "", 0, ""); err != nil {
		t.Fatalf("ListRunsVisibleTo(outsider): %v", err)
	}
	if len(runs) != 0 {
		t.Fatalf("outsider saw %v, want none", listIDs(runs))
	}

	// Unscoped listings only retain the foreign/legacy runs.
	if runs, _, err = s.ListRunsUnscoped(ctx, "", "", 0, ""); err != nil {
		t.Fatalf("ListRunsUnscoped: %v", err)
	}
	if got := listIDs(runs); !slices.Equal(got, []string{"run-lin-1", "run-arc-1"}) {
		t.Fatalf("unscoped listing = %v, want only the foreign runs", got)
	}

	// Console operators still see everything.
	if runs, _, err = s.ListRuns(ctx, "", "", 0, ""); err != nil {
		t.Fatalf("ListRuns: %v", err)
	}
	got = listIDs(runs)
	slices.Sort(got)
	if !slices.Equal(got, []string{"run-arc-1", "run-kbd-dev-1", "run-kbd-tri-1", "run-lin-1"}) {
		t.Fatalf("console listing = %v, want all runs", got)
	}

	// Point reads agree with the list scoping.
	for _, tc := range []struct {
		runID, criteriaID string
		want              bool
	}{
		{"run-kbd-dev-1", "criteria-k8s-operator", true},
		{"run-lin-1", "criteria-k8s-operator", false},
		{"run-kbd-dev-1", "outsider-agent", false},
	} {
		visible, err := s.IsRunVisibleTo(ctx, tc.runID, tc.criteriaID)
		if err != nil {
			t.Fatalf("IsRunVisibleTo(%s,%s): %v", tc.runID, tc.criteriaID, err)
		}
		if visible != tc.want {
			t.Fatalf("IsRunVisibleTo(%s,%s) = %v, want %v", tc.runID, tc.criteriaID, visible, tc.want)
		}
	}
}