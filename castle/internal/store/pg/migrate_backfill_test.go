package pg

// KB-332 cutover backfill coverage for the pg engine. These tests stay
// in-package because they stage a realistic pre-0023 database from the pg
// engine's embedded migration fileset (migrationsFS), which external pg_test
// suites cannot reach while pgtest itself imports this package.

import (
	"context"
	"database/sql"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"slices"
	"sync"
	"testing"
	"time"

	embeddedpostgres "github.com/fergusstrange/embedded-postgres"

	"github.com/brokenbots/castle/castle/internal/store"
	"github.com/brokenbots/castle/castle/internal/store/migrate"
)

// pgtestCacheDir mirrors pgtest.DefaultPostgresCacheDir so an already-warm
// embedded-postgres cache is shared between both harnesses instead of
// re-downloading: <userHome>/.embedded-postgres-go, the library's default.
func pgtestCacheDir(t *testing.T) string {
	t.Helper()
	home, err := os.UserHomeDir()
	if err != nil {
		return ".embedded-postgres-go"
	}
	return filepath.Join(home, ".embedded-postgres-go")
}

var (
	ephemeralMu      sync.Mutex
	ephemeralServer  *embeddedpostgres.EmbeddedPostgres
	ephemeralBootstrap *sql.DB
	ephemeralPort    uint32
	ephemeralStarted bool
	ephemeralCounter int
)

// bootEphemeralPG starts the shared embedded PostgreSQL server for the test
// binary (pinned at V18, same cache dir convention as pgtest) and returns a
// connection to the bootstrap database used for CREATE/DROP DATABASE. The
// server belongs to the first test's cleanup, which suits the sequential
// migration tests here.
func bootEphemeralPG(t *testing.T) *sql.DB {
	t.Helper()
	ephemeralMu.Lock()
	defer ephemeralMu.Unlock()

	if ephemeralStarted {
		return ephemeralBootstrap
	}

	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("reserve ephemeral pg port: %v", err)
	}
	ephemeralPort = uint32(l.Addr().(*net.TCPAddr).Port)
	l.Close()

	dataDir, err := os.MkdirTemp("", "castle-pg-backfill-data-")
	if err != nil {
		t.Fatalf("ephemeral pg data dir: %v", err)
	}
	runtimeDir, err := os.MkdirTemp("", "castle-pg-backfill-runtime-")
	if err != nil {
		t.Fatalf("ephemeral pg runtime dir: %v", err)
	}

	bootBackoff := []time.Duration{2 * time.Second, 4 * time.Second, 8 * time.Second, 16 * time.Second}
	var bootErr error
	for attempt := 1; attempt <= len(bootBackoff)+1; attempt++ {
		srv := embeddedpostgres.NewDatabase(embeddedpostgres.DefaultConfig().
			Version(embeddedpostgres.V18).
			CachePath(pgtestCacheDir(t)).
			Username("postgres").
			Password("postgres").
			Port(ephemeralPort).
			RuntimePath(runtimeDir).
			DataPath(filepath.Join(dataDir, "pgdata")).
			StartTimeout(2 * time.Minute))
		if bootErr = srv.Start(); bootErr == nil {
			ephemeralServer = srv
			break
		}
		if attempt <= len(bootBackoff) {
			time.Sleep(bootBackoff[attempt-1])
		}
	}
	if bootErr != nil {
		t.Fatalf("boot ephemeral pg after %d attempts: %v", len(bootBackoff)+1, bootErr)
	}
	ephemeralStarted = true

	dsn := fmt.Sprintf("postgres://postgres:postgres@127.0.0.1:%d/postgres?sslmode=disable", ephemeralPort)
	ephemeralBootstrap, err = sql.Open("pgx", dsn)
	if err != nil {
		t.Fatalf("open ephemeral pg bootstrap: %v", err)
	}
	if err := ephemeralBootstrap.Ping(); err != nil {
		t.Fatalf("ping ephemeral pg bootstrap: %v", err)
	}
	return ephemeralBootstrap
}

// TestMain tears the shared ephemeral server down after the whole test
// binary finishes. The server is booted lazily — only the backfill tests
// below use it — so ordinary package pg runs never pay the boot cost.
func TestMain(m *testing.M) {
	code := m.Run()
	ephemeralMu.Lock()
	if ephemeralStarted && ephemeralServer != nil {
		_ = ephemeralServer.Stop()
	}
	if ephemeralBootstrap != nil {
		_ = ephemeralBootstrap.Close()
	}
	ephemeralMu.Unlock()
	os.Exit(code)
}

func pgTestDSN(t *testing.T) string {
	t.Helper()
	base := bootEphemeralPG(t)

	ephemeralMu.Lock()
	defer ephemeralMu.Unlock()
	ephemeralCounter++
	name := fmt.Sprintf("castle_backfill_%d_%d", os.Getpid(), ephemeralCounter)
	t.Cleanup(func() {
		if _, err := base.Exec(`DROP DATABASE IF EXISTS ` + name + ` WITH (FORCE)`); err != nil {
			t.Errorf("drop ephemeral pg database %s: %v", name, err)
		}
	})
	if _, err := base.Exec(`DROP DATABASE IF EXISTS ` + name); err != nil {
		t.Fatalf("prune ephemeral pg database %s: %v", name, err)
	}
	if _, err := base.Exec(`CREATE DATABASE ` + name); err != nil {
		t.Fatalf("create ephemeral pg database %s: %v", name, err)
	}
	return fmt.Sprintf("postgres://postgres:postgres@127.0.0.1:%d/%s?sslmode=disable", ephemeralPort, name)
}

// seedPreBackfillPG stages the 0022 schema and a realistic pre-0019 dispatch
// history on a fresh test database, recording every applied version so only
// 0023 stays pending. Mirrors the sqlite backfill test seed (KB-332).
func seedPreBackfillPG(t *testing.T, db *sql.DB, seedProject bool) {
	t.Helper()
	ctx := context.Background()

	migs, err := migrate.Discover(migrationsFS)
	if err != nil {
		t.Fatalf("discover pg migrations: %v", err)
	}
	if _, err := db.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS schema_migrations (
		version INTEGER PRIMARY KEY,
		applied_at TIMESTAMPTZ NOT NULL DEFAULT now()
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
		if _, err := db.ExecContext(ctx,
			`INSERT INTO schema_migrations(version) VALUES ($1)`, m.Version); err != nil {
			t.Fatalf("record %s: %v", m.Name, err)
		}
	}

	now := "2026-10-25 00:00:00+00"
	seed := []string{
		`INSERT INTO overseers(id,name,token_hash,status,created_at,last_seen_at)
		 VALUES ('ovs-kb332','worker','agent-hash','online',$1,$1)`,
	}
	if seedProject {
		seed = append(seed, `INSERT INTO projects(id,membership_policy,created_at,updated_at)
		 VALUES ('kanboard-tickets','open',$1,$1)`)
	}
	for _, r := range []struct{ id, workflow string }{
		{"run-kbd-dev-1", "kanboard_develop_v1"},
		{"run-kbd-tri-1", "kanboard_triage_v1"},
		{"run-lin-1", "linear_intake_v1"},
		{"run-arc-1", "arc_shepherd_v1"},
	} {
		seed = append(seed, `INSERT INTO runs(id,overseer_id,workflow_name,workflow_hcl,status,current_step,created_at)
		 VALUES ('`+r.id+`','ovs-kb332','`+r.workflow+`','workflow "kb-332" {}','succeeded','done',$1)`)
	}
	for _, q := range seed {
		if _, err := db.ExecContext(ctx, q, now); err != nil {
			t.Fatalf("seed: %v: %q", err, q)
		}
	}
}

func runProjectID(t *testing.T, db *sql.DB, runID string) sql.NullString {
	t.Helper()
	var projectID sql.NullString
	err := db.QueryRowContext(context.Background(),
		`SELECT project_id FROM runs WHERE id = $1`, runID).Scan(&projectID)
	if err != nil {
		t.Fatalf("project_id of %s: %v", runID, err)
	}
	return projectID
}

func assertBackfillStamps(t *testing.T, db *sql.DB) {
	t.Helper()
	for _, r := range []struct {
		id       string
		wantNull bool
	}{
		{id: "run-kbd-dev-1", wantNull: false},
		{id: "run-kbd-tri-1", wantNull: false},
		{id: "run-lin-1", wantNull: true},
		{id: "run-arc-1", wantNull: true},
	} {
		got := runProjectID(t, db, r.id)
		if r.wantNull != !got.Valid {
			t.Fatalf("run %s project_id = %v, want null=%v", r.id, got, r.wantNull)
		}
		if got.Valid && got.String != "kanboard-tickets" {
			t.Fatalf("run %s project_id = %q, want kanboard-tickets", r.id, got.String)
		}
	}
}

// TestMigrate_ProjectBackfill_AssignsKanboardDispatches drives the 0023
// cutover backfill on the pg engine: kanboard_* dispatch history joins
// kanboard-tickets through pg.Open's pending-migration step, foreign runs
// stay unscoped, and the version ledger records the one-shot migration.
func TestMigrate_ProjectBackfill_AssignsKanboardDispatches(t *testing.T) {
	ds := pgTestDSN(t)
	db, err := sql.Open("pgx", ds)
	if err != nil {
		t.Fatalf("open legacy pg database: %v", err)
	}
	defer db.Close()
	seedPreBackfillPG(t, db, true)

	// pg.Open applies only the pending 0023 over the staged 0022 schema.
	s, err := Open(ds)
	if err != nil {
		t.Fatalf("Open (applies 0023): %v", err)
	}
	defer s.Close()

	assertBackfillStamps(t, db)

	ctx := context.Background()
	var applied int
	if err := db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM schema_migrations WHERE version = 23`).Scan(&applied); err != nil {
		t.Fatalf("version ledger: %v", err)
	}
	if applied != 1 {
		t.Fatalf("0023 stamped %d times, want 1", applied)
	}

	// A no-op second pass must succeed: the one-shot migration is never
	// replayed.
	if err := Migrate(ctx, db); err != nil {
		t.Fatalf("re-migrate after Open: %v", err)
	}
	if err := db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM schema_migrations WHERE version = 23`).Scan(&applied); err != nil {
		t.Fatalf("ledger after re-migrate: %v", err)
	}
	if applied != 1 {
		t.Fatalf("0023 stamped %d times after re-migrate, want 1", applied)
	}
}

// TestMigrate_ProjectBackfill_NoProjectRow_KeepsRunsUnscoped pins the pg
// guard: without the kanboard-tickets project row every run stays unscoped.
func TestMigrate_ProjectBackfill_NoProjectRow_KeepsRunsUnscoped(t *testing.T) {
	ds := pgTestDSN(t)
	db, err := sql.Open("pgx", ds)
	if err != nil {
		t.Fatalf("open legacy pg database: %v", err)
	}
	defer db.Close()
	seedPreBackfillPG(t, db, false)

	s, err := Open(ds)
	if err != nil {
		t.Fatalf("Open (applies 0023): %v", err)
	}
	defer s.Close()

	for _, id := range []string{"run-kbd-dev-1", "run-kbd-tri-1", "run-lin-1", "run-arc-1"} {
		if got := runProjectID(t, db, id); got.Valid {
			t.Fatalf("run %s project_id = %q, want NULL (no project row)", id, got.String)
		}
	}
}

// TestMigrate_ProjectBackfill_ScopedReadsSeeRealSpace is the pg twin of the
// KB-332 verification leg: after pg.Open applies the backfill, a project
// member's scoped reads surface the kanboard dispatch history, outsiders see
// none, unscoped lists lose the backfilled runs, and console operators see
// all of them.
func TestMigrate_ProjectBackfill_ScopedReadsSeeRealSpace(t *testing.T) {
	ds := pgTestDSN(t)
	db, err := sql.Open("pgx", ds)
	if err != nil {
		t.Fatalf("open legacy pg database: %v", err)
	}
	defer db.Close()
	seedPreBackfillPG(t, db, true)

	s, err := Open(ds)
	if err != nil {
		t.Fatalf("Open (applies 0023): %v", err)
	}
	defer s.Close()

	ctx := context.Background()
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