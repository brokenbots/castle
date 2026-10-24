// Package pg is the Postgres implementation of store.Store using the pgx v5
// stdlib driver (no ORM). Postgres is the server profile (KB-317); SQLite
// remains the local/dev profile.
package pg

import (
	"context"
	"database/sql"
	"fmt"
	"log/slog"
	"time"

	"github.com/brokenbots/castle/castle/internal/store"
	_ "github.com/jackc/pgx/v5/stdlib"
)

// Compile-time pin: the pg profile must satisfy the whole store contract.
var _ store.Store = (*Store)(nil)

type Store struct {
	db *sql.DB
	// reader aliases the writer pool (KB-317). Postgres needs no read/write
	// handle split — MVCC readers never block writers — but the field is kept
	// so reader/writer call sites stay symmetric with the sqlite profile and
	// the shared freshness telemetry keeps auditing the read path.
	reader *sql.DB
	// freshness lets ListOverseers audit its reader view against the newest
	// write the writer pool has committed (KB-10 telemetry, KB-317 parity).
	freshness readerFreshness
	log       *slog.Logger
}

// pgDriver is the database/sql driver name registered by the pgx stdlib
// package.
const pgDriver = "pgx"

// Open opens the Postgres-backed store at dsn, running the engine's embedded
// migrations before the first caller touches the store. The DSN must be a
// complete libpq/PQ connection string (or URL).
func Open(dsn string) (*Store, error) {
	if dsn == "" {
		return nil, fmt.Errorf("pg: dsn required")
	}
	db, err := sql.Open(pgDriver, dsn)
	if err != nil {
		return nil, err
	}
	pingCtx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if err := db.PingContext(pingCtx); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("pg: ping %w", err)
	}
	if err := Migrate(context.Background(), db); err != nil {
		_ = db.Close()
		return nil, err
	}
	return &Store{db: db, reader: db, log: slog.Default()}, nil
}

func (s *Store) Close() error {
	return s.db.Close()
}

// SetMaxOpenConns configures the connection pool ceiling. Unlike sqlite,
// Postgres tolerates concurrent writers, so the default is a generous pool
// rather than one serialized connection.
func (s *Store) SetMaxOpenConns(n int) { s.db.SetMaxOpenConns(n) }

// storeOpTimeout bounds each store operation executed on a detached context.
// Postgres has no driver-level interrupt analogue to sqlite3_interrupt, but
// opCtx is kept for parity with the sqlite profile (KB-222): a cancelled gRPC
// stream still detaches so statement execution never races a request teardown,
// and detached work still terminates.
const storeOpTimeout = 30 * time.Second

func (s *Store) opCtx(ctx context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.WithoutCancel(ctx), storeOpTimeout)
}

// us collapses a timestamp to microsecond precision. Postgres TIMESTAMPTZ
// stores microseconds (SQLite TEXT stored RFC3339Nano), so every persisted
// stamp is truncated on the way in and the suite's timestamp comparisons
// hold on both engines.
func us(t time.Time) time.Time {
	return t.UTC().Truncate(time.Microsecond)
}

// timePtr converts a nilable *time.Time into a database/sql value using
// microsecond-precision stamps.
func timePtr(t *time.Time) any {
	if t == nil {
		return nil
	}
	return us(*t)
}

// nonEmpty maps an empty string to SQL NULL so absent run metadata stays NULL
// rather than an empty-text value (CRI-131).
func nonEmpty(s string) any {
	if s == "" {
		return nil
	}
	return s
}
