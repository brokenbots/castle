package main

import (
	"bytes"
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/brokenbots/castle/castle/internal/store"
	"github.com/brokenbots/castle/castle/internal/store/pg"
	"github.com/brokenbots/castle/castle/internal/store/pg/pgtest"
	"github.com/brokenbots/castle/castle/internal/store/relocate"
	"github.com/brokenbots/castle/castle/internal/store/sqlite"
)

// seedSource writes one overseer and one agent run through the sqlite store
// API so every registry table exists (some empty) and the import has data.
func seedSource(t *testing.T, path string) string {
	t.Helper()
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Microsecond)

	st, err := sqlite.Open(path)
	if err != nil {
		t.Fatalf("open sqlite store: %v", err)
	}
	defer func() { _ = st.Close() }()

	ov := &store.Overseer{
		ID: "ov-cli", Name: "agent-a", Hostname: "host-a", Version: "v1",
		TokenHash: "hash-a", Status: "online", CreatedAt: now, LastSeenAt: now,
	}
	if err := st.CreateOverseer(ctx, ov); err != nil {
		t.Fatalf("CreateOverseer: %v", err)
	}
	r := &store.Run{
		ID: "run-cli", OverseerID: ov.ID, WorkflowName: "wf", WorkflowHCL: "job {}",
		Status: "running", CreatedAt: now, StartedAt: &now,
	}
	if err := st.CreateRun(ctx, r); err != nil {
		t.Fatalf("CreateRun: %v", err)
	}
	return path
}

func TestRunRequiresPGDSN(t *testing.T) {
	var out, errOut bytes.Buffer
	code := run([]string{"--db", "castle.db"}, &out, &errOut)
	if code != 2 {
		t.Fatalf("missing --pg-dsn: want exit 2, got %d", code)
	}
	if !strings.Contains(errOut.String(), "--pg-dsn") {
		t.Errorf("stderr should name --pg-dsn, got: %q", errOut.String())
	}
	if out.Len() != 0 {
		t.Errorf("no report should be printed on usage error, got: %q", out.String())
	}
}

func TestRunRejectsBadFlagsAndPositionals(t *testing.T) {
	for _, args := range [][]string{
		{"--nope"},
		{"--pg-dsn", "postgres://u:p@h/db", "stray"},
	} {
		var out, errOut bytes.Buffer
		code := run(args, &out, &errOut)
		if code == 0 {
			t.Fatalf("args %v: want non-zero, got 0", args)
		}
		if code != 2 {
			t.Errorf("args %v: usage error should exit 2, got %d", args, code)
		}
		if errOut.Len() == 0 {
			t.Errorf("args %v: want an explanatory usage error on stderr", args)
		}
	}
}

func TestRunImportsAndVerifies(t *testing.T) {
	ctx := context.Background()
	srcPath := seedSource(t, filepath.Join(t.TempDir(), "castle.db"))
	pgDSN := pgtest.DSN(t)

	var out, errOut bytes.Buffer
	code := run([]string{"--db", srcPath, "--pg-dsn", pgDSN, "--verify-rows", "100"}, &out, &errOut)
	if code != 0 {
		t.Fatalf("import: want exit 0, got %d\nstderr: %s", code, errOut.String())
	}
	// One report line per registry table, all verified. (table's fields are
	// unexported, so the CLI test asserts the row contract by count+marker;
	// the names are pinned by the relocate package's own tests.)
	rows := 0
	for _, line := range strings.Split(strings.TrimSpace(out.String()), "\n") {
		if strings.HasSuffix(line, " ok") {
			rows++
		}
	}
	if rows != len(relocate.Tables) {
		t.Errorf("want %d verified table rows, got %d\nstdout:\n%s", len(relocate.Tables), rows, out.String())
	}
	if !strings.Contains(out.String(), "castle-import: all tables imported and verified") {
		t.Errorf("stdout should end with the success line, got:\n%s", out.String())
	}

	// The pg store reads back what the CLI wrote.
	pst, err := pg.Open(pgDSN)
	if err != nil {
		t.Fatalf("reopen pg store: %v", err)
	}
	defer func() { _ = pst.Close() }()
	runs, _, err := pst.ListRuns(ctx, "", "", 10, "")
	if err != nil {
		t.Fatalf("ListRuns through pg store: %v", err)
	}
	if len(runs) != 1 || runs[0].ID != "run-cli" {
		t.Fatalf("ListRuns: want [run-cli], got %+v", runs)
	}
}

func TestRunFailsOnVerification(t *testing.T) {
	srcPath := seedSource(t, filepath.Join(t.TempDir(), "castle.db"))
	pgDSN := pgtest.DSN(t)

	var out, errOut bytes.Buffer
	if code := run([]string{"--db", srcPath, "--pg-dsn", pgDSN}, &out, &errOut); code != 0 {
		t.Fatalf("first import: want exit 0, got %d\nstderr: %s", code, errOut.String())
	}

	// Tamper with the target behind the importer's back, then re-run with the
	// override: the re-run heals the deletion, which verification must catch.
	dst, err := relocate.OpenTarget(pgDSN)
	if err != nil {
		t.Fatalf("OpenTarget: %v", err)
	}
	if _, err := dst.ExecContext(context.Background(), `DELETE FROM runs WHERE id = $1`, "run-cli"); err != nil {
		t.Fatalf("delete target run: %v", err)
	}
	if err := dst.Close(); err != nil {
		t.Fatalf("close target handle: %v", err)
	}

	out.Reset()
	errOut.Reset()
	code := run([]string{"--db", srcPath, "--pg-dsn", pgDSN, "--allow-non-empty-pg"}, &out, &errOut)
	if code != 1 {
		t.Fatalf("verification failure: want exit 1, got %d\nstdout:\n%s", code, out.String())
	}
	if !strings.Contains(errOut.String(), "verification FAILED") {
		t.Errorf("stderr should announce the failure, got: %q", errOut.String())
	}
	if !strings.Contains(out.String(), "runs") || !strings.Contains(out.String(), "FAILED") {
		t.Errorf("report should mark the tampered table FAILED, got:\n%s", out.String())
	}
}

func TestRunRefusesNonEmptyTarget(t *testing.T) {
	srcPath := seedSource(t, filepath.Join(t.TempDir(), "castle.db"))
	pgDSN := pgtest.DSN(t)

	var out, errOut bytes.Buffer
	if code := run([]string{"--db", srcPath, "--pg-dsn", pgDSN}, &out, &errOut); code != 0 {
		t.Fatalf("first import: want exit 0, got %d\nstderr: %s", code, errOut.String())
	}
	out.Reset()
	errOut.Reset()
	code := run([]string{"--db", srcPath, "--pg-dsn", pgDSN}, &out, &errOut)
	if code != 1 {
		t.Fatalf("non-empty re-run: want exit 1, got %d", code)
	}
	if !strings.Contains(errOut.String(), "empty pg database only") {
		t.Errorf("stderr should carry the refusal, got: %q", errOut.String())
	}
}

func TestRunEnvFallbacks(t *testing.T) {
	srcPath := seedSource(t, filepath.Join(t.TempDir(), "castle.db"))
	pgDSN := pgtest.DSN(t)
	t.Setenv("CASTLE_DB_PATH", srcPath)
	t.Setenv("CASTLE_PG_DSN", pgDSN)

	var out, errOut bytes.Buffer
	code := run(nil, &out, &errOut)
	if code != 0 {
		t.Fatalf("env-only invocation: want exit 0, got %d\nstderr: %s", code, errOut.String())
	}
}
