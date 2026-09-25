package sqlite

import (
	"bytes"
	"context"
	"database/sql/driver"
	"fmt"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/brokenbots/castle/castle/internal/auth"
	"github.com/brokenbots/castle/castle/internal/store"
)

// pinReaderSnapshot checks a reader connection out of the pool, starts a
// driver-level read transaction, reads from the overseers table to materialize
// the WAL snapshot, and returns the connection with that transaction still
// open — the shape of the KB-10 incident, where the single pooled reader
// connection carried a pinned WAL snapshot between reads. A deferred BEGIN
// pins nothing by itself; the first read inside the transaction fixes the
// snapshot. database/sql assumes released connections are clean, so the pool
// keeps handing this pinned connection to every subsequent read.
func pinReaderSnapshot(t *testing.T, s *Store) {
	t.Helper()
	ctx := context.Background()
	conn, err := s.reader.Conn(ctx)
	if err != nil {
		t.Fatalf("acquire reader conn: %v", err)
	}
	err = conn.Raw(func(dc any) error {
		b, ok := dc.(driver.ConnBeginTx)
		if !ok {
			return fmt.Errorf("reader conn does not implement driver.ConnBeginTx")
		}
		if _, err := b.BeginTx(ctx, driver.TxOptions{}); err != nil {
			return fmt.Errorf("begin pinned read transaction: %w", err)
		}
		// Materialize the snapshot: a deferred BEGIN pins nothing until the
		// transaction performs its first read.
		q, ok := dc.(driver.QueryerContext)
		if !ok {
			return fmt.Errorf("reader conn does not implement driver.QueryerContext")
		}
		rows, err := q.QueryContext(ctx, `SELECT count(*) FROM overseers`, nil)
		if err != nil {
			return fmt.Errorf("materialize pinned snapshot: %w", err)
		}
		rows.Close()
		return nil
	})
	if err != nil {
		conn.Close()
		t.Fatalf("pin reader snapshot: %v", err)
	}
	// Deliberately do not commit or roll back: the open read transaction is
	// the pin. Releasing the connection returns it to the pool in that state.
	if err := conn.Close(); err != nil {
		t.Fatalf("release pinned reader conn: %v", err)
	}
}

func registerOverseer(t *testing.T, s *Store, id string, createdAt time.Time) string {
	t.Helper()
	token := fmt.Sprintf("token-%s", id)
	o := &store.Overseer{
		ID:         id,
		Name:       id,
		TokenHash:  auth.HashToken(token),
		Status:     "online",
		CreatedAt:  createdAt,
		LastSeenAt: createdAt,
	}
	if err := s.CreateOverseer(context.Background(), o); err != nil {
		t.Fatalf("create overseer %s: %v", id, err)
	}
	return token
}

// TestListOverseers_FreshAfterReaderSnapshotPin is the KB-10 regression: a
// reader connection pinned to a pre-registration WAL snapshot must not make
// later registrations invisible to ListOverseers, the auth token resolution
// path. Before the fix the pinned pooled connection was reused and agent B's
// registration stayed invisible, so CreateRun with B's token was rejected
// with 401 "unauthenticated: invalid token" while Register kept succeeding.
func TestListOverseers_FreshAfterReaderSnapshotPin(t *testing.T) {
	s := tempStore(t)
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Second)

	tokenA := registerOverseer(t, s, "agent-a", now)

	// Warm a read so a pooled reader connection exists, then pin it.
	if got, err := s.ListOverseers(ctx); err != nil || len(got) != 1 {
		t.Fatalf("warm read: got %d overseers, err %v; want 1", len(got), err)
	}
	pinReaderSnapshot(t, s)

	// Register agent B after the pin, exactly like the incident: writes kept
	// succeeding while the reader served the stale snapshot.
	tokenB := registerOverseer(t, s, "agent-b", now.Add(time.Second))

	got, err := s.ListOverseers(ctx)
	if err != nil {
		t.Fatalf("list overseers: %v", err)
	}
	ids := map[string]bool{}
	for _, o := range got {
		ids[o.ID] = true
	}
	if !ids["agent-b"] {
		t.Fatalf("ListOverseers after pinned reader snapshot is missing agent-b; got %v (reader served a stale WAL snapshot)", ids)
	}
	if !ids["agent-a"] {
		t.Fatalf("ListOverseers missing previously visible agent-a; got %v", ids)
	}

	// The CreateRun authentication path resolves tokens through ListOverseers:
	// the fresh token must resolve after the pin.
	resolved, err := auth.ResolveToken(ctx, s, tokenB)
	if err != nil {
		t.Fatalf("resolve token: %v", err)
	}
	if resolved == nil || resolved.ID != "agent-b" {
		t.Fatalf("ResolveToken(agent-b token) = %v, want agent-b (fresh token must authenticate after registration)", resolved)
	}
	if _, err := auth.ResolveToken(ctx, s, tokenA); err != nil {
		t.Fatalf("resolve token a: %v", err)
	}
}

func newTestLogger(buf *bytes.Buffer) *slog.Logger {
	return slog.New(slog.NewTextHandler(buf, &slog.HandlerOptions{Level: slog.LevelDebug}))
}

// TestListOverseers_WarnsOnStaleReaderView covers the operator-visible guard:
// when the writer has committed an overseer newer than anything the reader
// can see, ListOverseers logs the staleness warning.
func TestListOverseers_WarnsOnStaleReaderView(t *testing.T) {
	s := tempStore(t)
	ctx := context.Background()
	now := time.Now().UTC()

	buf := &bytes.Buffer{}
	s.log = newTestLogger(buf)

	if err := s.CreateOverseer(ctx, &store.Overseer{ID: "old", TokenHash: "x", Status: "online", CreatedAt: now, LastSeenAt: now}); err != nil {
		t.Fatalf("create overseer: %v", err)
	}
	// Simulate the writer committing a registration the reader cannot see.
	s.freshness.recordWrite(now.Add(2 * time.Hour))

	if _, err := s.ListOverseers(ctx); err != nil {
		t.Fatalf("list overseers: %v", err)
	}
	if !strings.Contains(buf.String(), "stale WAL snapshot") {
		t.Fatalf("ListOverseers did not log the staleness warning; log = %q", buf.String())
	}
}

// TestReaderFreshness_Check covers the detector in isolation: grace window,
// rate limiting, and the no-writer baseline.
func TestReaderFreshness_Check(t *testing.T) {
	ctx := context.Background()
	now := time.Now().UTC()

	t.Run("no writer writes means never stale", func(t *testing.T) {
		var f readerFreshness
		buf := &bytes.Buffer{}
		if f.check(ctx, time.Time{}, newTestLogger(buf)) {
			t.Fatal("check reported stale with no recorded writes")
		}
		if buf.Len() != 0 {
			t.Fatalf("unexpected warning: %q", buf.String())
		}
	})

	t.Run("reader within grace is not stale", func(t *testing.T) {
		var f readerFreshness
		f.recordWrite(now)
		buf := &bytes.Buffer{}
		if f.check(ctx, now.Add(-staleReaderGrace), newTestLogger(buf)) {
			t.Fatal("check reported stale within the grace window")
		}
		if buf.Len() != 0 {
			t.Fatalf("unexpected warning: %q", buf.String())
		}
	})

	t.Run("reader older than grace warns and is rate limited", func(t *testing.T) {
		var f readerFreshness
		f.recordWrite(now.Add(2 * staleReaderGrace))
		buf := &bytes.Buffer{}
		log := newTestLogger(buf)
		if !f.check(ctx, now, log) {
			t.Fatal("check did not report stale beyond the grace window")
		}
		for i := 0; i < 5; i++ {
			if f.check(ctx, now, log) != true {
				t.Fatal("check stopped reporting stale while the gap persists")
			}
		}
		if got := strings.Count(buf.String(), "stale WAL snapshot"); got != 1 {
			t.Fatalf("warning emitted %d times, want 1 (rate limited)", got)
		}
	})

	t.Run("warning repeats after the interval", func(t *testing.T) {
		var f readerFreshness
		f.recordWrite(now.Add(2 * staleReaderGrace))
		buf := &bytes.Buffer{}
		log := newTestLogger(buf)
		if !f.check(ctx, now, log) {
			t.Fatal("check did not report stale")
		}
		f.warned = f.warned.Add(-2 * staleReaderWarnInterval)
		if !f.check(ctx, now, log) {
			t.Fatal("check did not report stale")
		}
		if got := strings.Count(buf.String(), "stale WAL snapshot"); got != 2 {
			t.Fatalf("warning emitted %d times, want 2 after the interval elapsed", got)
		}
	})
}