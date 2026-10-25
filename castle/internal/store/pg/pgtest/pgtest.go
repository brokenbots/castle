// Package pgtest boots an ephemeral embedded PostgreSQL server for tests
// that exercise the pg-backed store.Store against a real engine. Binaries
// are fetched once into an OS-wide cache; every call to Store hands back a
// fresh, isolated database that is dropped when the test finishes.
package pgtest

import (
	"context"
	"database/sql"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"regexp"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	embeddedpostgres "github.com/fergusstrange/embedded-postgres"
	// The pgx stdlib registration is what the rest of the tree opens with
	// ("pgx"); lib/pq must stay an indirect dependency.
	_ "github.com/jackc/pgx/v5/stdlib"

	"github.com/brokenbots/castle/castle/internal/store/pg"
)

const (
	testUser     = "postgres"
	testPassword = "postgres"
)

var (
	mu   sync.Mutex
	srv  *embeddedpostgres.EmbeddedPostgres
	base *sql.DB
	port uint32

	dbCounter atomic.Uint32
	// perTest tracks the database name and open handles of each test's
	// isolated database so Reopen can hand back a second Store over the
	// same data (restart semantics).
	perTest sync.Map // testing.TB -> *suiteDB
)

type suiteDB struct {
	dsn    string
	stores []*pg.Store
}

var dbNameRE = regexp.MustCompile(`^[a-z][a-z0-9_]{0,62}$`)

// shared boots the per-process ephemeral server on first use and hands back
// a connection to the bootstrap database, used for CREATE/DROP DATABASE.
func shared(t *testing.T) *sql.DB {
	t.Helper()
	mu.Lock()
	defer mu.Unlock()
	if base != nil {
		return base
	}

	// Provision the pinned binary archive deterministically and
	// single-flight before booting: with a warm cache the server starts
	// with zero network access, and otherwise exactly one downloads while
	// the parallel test binaries poll.
	cacheDir := DefaultPostgresCacheDir()
	if _, err := EnsurePostgresBinaryIfNeeded(context.Background(), DefaultBinaryRepoBase, cacheDir); err != nil {
		t.Fatalf("pgtest: provision embedded postgres binary: %v", err)
	}

	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("pgtest: reserving a free port: %v", err)
	}
	port = uint32(l.Addr().(*net.TCPAddr).Port)
	l.Close()

	dataDir, err := os.MkdirTemp("", "castle-pgtest-data-")
	if err != nil {
		t.Fatalf("pgtest: data dir: %v", err)
	}
	// Per-process runtime dir: extraction is deterministic and cheap, and an
	// isolated directory keeps parallel test binaries from racing on the
	// extraction step. The downloaded archive itself always shares the
	// explicit cache dir below.
	runtimeDir, err := os.MkdirTemp("", "castle-pgtest-runtime-")
	if err != nil {
		t.Fatalf("pgtest: runtime dir: %v", err)
	}

	// Bounded boot retry: the same mirror flakiness that corrupted old
	// fetches aside, Start's sub-steps (extraction, initdb, first boot) can
	// each fail transiently on a contended runner. Each attempt builds a
	// fresh server value; Start removes the runtime dir itself, so the
	// retries converge on clean state. The test fails only after the final
	// attempt.
	bootBackoff := []time.Duration{2 * time.Second, 4 * time.Second, 8 * time.Second, 16 * time.Second}
	var bootErr error
	for attempt := 1; attempt <= len(bootBackoff)+1; attempt++ {
		srv = embeddedpostgres.NewDatabase(embeddedpostgres.DefaultConfig().
			// The version and cache are pinned explicitly (not inherited
			// from DefaultConfig) so a library upgrade can never silently
			// change which binary is booted or which cache file is read.
			Version(embeddedpostgres.V18).
			CachePath(cacheDir).
			Username(testUser).
			Password(testPassword).
			Port(port).
			RuntimePath(runtimeDir).
			DataPath(filepath.Join(dataDir, "pgdata")).
			StartTimeout(2 * time.Minute))
		if bootErr = srv.Start(); bootErr == nil {
			break
		}
		if attempt <= len(bootBackoff) {
			time.Sleep(bootBackoff[attempt-1])
		}
	}
	if bootErr != nil {
		t.Fatalf("pgtest: boot embedded postgres after %d attempts: %v", len(bootBackoff)+1, bootErr)
	}

	base, err = sql.Open("pgx", dsn("postgres"))
	if err != nil {
		t.Fatalf("pgtest: open bootstrap connection: %v", err)
	}
	if err := base.Ping(); err != nil {
		t.Fatalf("pgtest: ping bootstrap connection: %v", err)
	}
	return base
}

func dsn(db string) string {
	return fmt.Sprintf("postgres://%s:%s@127.0.0.1:%d/%s?sslmode=disable", testUser, testPassword, port, db)
}

// Store opens a fresh, fully migrated pg-backed store.Store on an isolated
// database of the shared ephemeral server. The database is dropped after the
// test finishes, so each caller starts from empty tables. Reopen hands back a
// second Store over the same database within the same test.
func Store(t *testing.T) *pg.Store {
	t.Helper()
	base := shared(t)

	name := allocate(t, base)
	s := open(t, dsn(name))
	t.Cleanup(func() {
		for _, st := range openStores(t) {
			_ = st.Close()
		}
		// WITH (FORCE) terminates lingering connections instead of failing
		// the drop after a test leaks one.
		if _, err := base.Exec(`DROP DATABASE IF EXISTS ` + name + ` WITH (FORCE)`); err != nil {
			t.Errorf("pgtest: drop database %s: %v", name, err)
		}
		perTest.Delete(t)
	})
	return s
}

// DSN boots the shared ephemeral server and returns the DSN of a fresh,
// empty database; migrations run when the caller opens it (pg.Open). The
// database is dropped after the test finishes. Lets tests exercise the
// real open-with-migrations path instead of pgtest.Store's internal one.
func DSN(t *testing.T) string {
	t.Helper()
	base := shared(t)
	name := allocate(t, base)
	t.Cleanup(func() {
		if _, err := base.Exec(`DROP DATABASE IF EXISTS ` + name + ` WITH (FORCE)`); err != nil {
			t.Errorf("pgtest: drop database %s: %v", name, err)
		}
	})
	return dsn(name)
}

// allocate creates a uniquely named, empty database on the shared server.
func allocate(t *testing.T, base *sql.DB) string {
	t.Helper()
	name := fmt.Sprintf("castle_suite_%d_%d", os.Getpid(), dbCounter.Add(1))
	if !dbNameRE.MatchString(name) {
		t.Fatalf("pgtest: generated database name %q is not a valid identifier", name)
	}
	// DROP first: a crashed prior run of the same pid may have left the name.
	if _, err := base.Exec(`DROP DATABASE IF EXISTS ` + name); err != nil {
		t.Fatalf("pgtest: prune database %s: %v", name, err)
	}
	if _, err := base.Exec(`CREATE DATABASE ` + name); err != nil {
		t.Fatalf("pgtest: create database %s: %v", name, err)
	}
	return name
}

// Reopen returns a new Store over the same isolated database the test's
// Store call created, without dropping anything: the restart leg of
// persistence tests. Handles registered under t are closed by the test's
// cleanup.
func Reopen(t *testing.T) *pg.Store {
	t.Helper()
	d, ok := perTest.Load(t)
	if !ok {
		t.Fatalf("pgtest: Reopen called before Store for this test")
	}
	return open(t, d.(*suiteDB).dsn)
}

func openStores(t *testing.T) []*pg.Store {
	t.Helper()
	d, ok := perTest.Load(t)
	if !ok {
		return nil
	}
	return d.(*suiteDB).stores
}

func open(t *testing.T, d string) *pg.Store {
	t.Helper()
	s, err := pg.Open(d)
	if err != nil {
		t.Fatalf("pgtest: open store on %s: %v", d, err)
	}
	entry := &suiteDB{dsn: d, stores: []*pg.Store{s}}
	if prev, loaded := perTest.LoadOrStore(t, entry); loaded {
		prev.(*suiteDB).stores = append(prev.(*suiteDB).stores, s)
	}
	return s
}
