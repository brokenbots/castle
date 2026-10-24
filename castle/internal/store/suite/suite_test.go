package suite

import (
	"context"
	"flag"
	"path/filepath"
	"sync"
	"testing"

	"github.com/brokenbots/castle/castle/internal/store"
	"github.com/brokenbots/castle/castle/internal/store/pg/pgtest"
	"github.com/brokenbots/castle/castle/internal/store/sqlite"
)

// Engine parity is enforced here, not hoped for: every behavioral test in
// this package runs once per engine below. The sqlite profile keeps its own
// engine-specific tests (reader split, fault injection, raw-file checks); the
// suite pins the behavior contract both engines must satisfy (KB-317).
var skipPG = flag.Bool("suite.skip-pg", false, "skip the Postgres engine (for sandboxed runs without the embedded-binary cache)")

type engine struct {
	name   string
	open   func(t *testing.T) store.Store
	reopen func(t *testing.T) store.Store
}

func engines(t *testing.T) []engine {
	eng := []engine{{name: "sqlite", open: openSQLite, reopen: reopenSQLite}}
	if !*skipPG {
		eng = append(eng, engine{
			name:   "pg",
			open:   func(t *testing.T) store.Store { return pgtest.Store(t) },
			reopen: func(t *testing.T) store.Store { return pgtest.Reopen(t) },
		})
	}
	t.Logf("suite engines: %d", len(eng))
	return eng
}

var sqlitePaths sync.Map // path cache keyed by *testing.T

func openSQLite(t *testing.T) store.Store {
	t.Helper()
	p := filepath.Join(t.TempDir(), "test.db")
	sqlitePaths.Store(t, p)
	s, err := sqlite.Open(p)
	if err != nil {
		t.Fatalf("open sqlite store: %v", err)
	}
	return s
}

// reopenSQLite reopens the same database file the test's open call created,
// simulating a Castle restart against persistent storage.
func reopenSQLite(t *testing.T) store.Store {
	t.Helper()
	p, ok := sqlitePaths.Load(t)
	if !ok {
		t.Fatalf("reopen before open for this test")
	}
	s, err := sqlite.Open(p.(string))
	if err != nil {
		t.Fatalf("reopen sqlite store: %v", err)
	}
	return s
}

// run executes check against every engine, each on its own empty store.
func run(t *testing.T, check func(t *testing.T, s store.Store)) {
	t.Helper()
	for _, e := range engines(t) {
		e := e
		t.Run(e.name, func(t *testing.T) {
			s := e.open(t)
			t.Cleanup(func() { _ = s.Close() })
			check(t, s)
		})
	}
}

// runWithCtx is run but hands the check a context.
func runWithCtx(t *testing.T, check func(t *testing.T, ctx context.Context, s store.Store)) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	for _, e := range engines(t) {
		e := e
		t.Run(e.name, func(t *testing.T) {
			s := e.open(t)
			t.Cleanup(func() { _ = s.Close() })
			check(t, ctx, s)
		})
	}
}
