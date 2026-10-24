package suite

import (
	"context"
	"flag"
	"path/filepath"
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
	name string
	open func(t *testing.T) store.Store
}

func engines(t *testing.T) []engine {
	eng := []engine{{name: "sqlite", open: openSQLite}}
	if !*skipPG {
		eng = append(eng, engine{name: "pg", open: func(t *testing.T) store.Store { return pgtest.Store(t) }})
	}
	t.Logf("suite engines: %d", len(eng))
	return eng
}

func openSQLite(t *testing.T) store.Store {
	t.Helper()
	s, err := sqlite.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("open sqlite store: %v", err)
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
