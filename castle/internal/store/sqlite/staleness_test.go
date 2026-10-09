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

// --- KB-223: reader-lag self-heal, freshness gating, and presence probe ---

// TestReaderFreshness_NewerThan covers the heal trigger: the writer view is
// authoritative whenever it has committed an overseer creation the read view
// does not show yet.
func TestReaderFreshness_NewerThan(t *testing.T) {
	now := time.Now().UTC()

	t.Run("no recorded writes never heals", func(t *testing.T) {
		var f readerFreshness
		if f.newerThan(time.Time{}) {
			t.Fatal("newerThan true with no recorded writes")
		}
	})

	t.Run("empty reader view counts as behind any write", func(t *testing.T) {
		var f readerFreshness
		f.recordWrite(now)
		if !f.newerThan(time.Time{}) {
			t.Fatal("newerThan false against a reader view with no overseers")
		}
	})

	t.Run("agreement is not newer", func(t *testing.T) {
		var f readerFreshness
		f.recordWrite(now)
		if f.newerThan(now) {
			t.Fatal("newerThan true when reader and writer agree on the newest row")
		}
	})

	t.Run("a newer committed write is newer", func(t *testing.T) {
		var f readerFreshness
		f.recordWrite(now.Add(time.Minute))
		if !f.newerThan(now) {
			t.Fatal("newerThan false while a newer committed write exists")
		}
	})
}

// TestReaderFreshness_BehindFlagTracksAudit covers the behind flag that gates
// the OverseerTokenHashPresent writer probe off the hot path.
func TestReaderFreshness_BehindFlagTracksAudit(t *testing.T) {
	ctx := context.Background()
	now := time.Now().UTC()

	t.Run("audit recording a gap marks behind", func(t *testing.T) {
		var f readerFreshness
		f.recordWrite(now.Add(2 * staleReaderGrace))
		buf := &bytes.Buffer{}
		if !f.check(ctx, now, newTestLogger(buf)) {
			t.Fatal("check did not report the gap")
		}
		if !f.readerBehind() {
			t.Fatal("behind flag not set while the reader view is behind the writer")
		}
	})

	t.Run("fresh audit clears the flag", func(t *testing.T) {
		var f readerFreshness
		f.noteBehind(true)
		buf := &bytes.Buffer{}
		if f.check(ctx, now, newTestLogger(buf)) {
			t.Fatal("check reported stale with no recorded writes")
		}
		if f.readerBehind() {
			t.Fatal("behind flag not cleared by a fresh audit")
		}
	})
}

// TestListOverseers_HealsLaggingReaderView is the KB-223 self-heal: when the
// store knows about a committed overseer write that the reader view cannot
// show, ListOverseers re-reads through the authoritative writer handle so
// token resolution cannot miss a registered token, and the gap is recorded so
// a subsequent unresolvable token is reported as "not yet visible" by the
// auth interceptor instead of a misleading "invalid token".
func TestListOverseers_HealsLaggingReaderView(t *testing.T) {
	s := tempStore(t)
	ctx := context.Background()
	now := time.Now().UTC()
	token := registerOverseer(t, s, "veteran", now)

	// The read view lacks a committed write: freshness has it, the rows the
	// reader can return do not.
	s.freshness.recordWrite(now.Add(time.Hour))

	buf := &bytes.Buffer{}
	s.log = newTestLogger(buf)

	got, err := s.ListOverseers(ctx)
	if err != nil {
		t.Fatalf("list overseers: %v", err)
	}
	if len(got) != 1 || got[0].ID != "veteran" {
		t.Fatalf("ListOverseers served %d rows, want exactly veteran: %+v", len(got), got)
	}
	if !strings.Contains(buf.String(), "served the authoritative writer view") {
		t.Fatalf("heal of the lagging reader view was not logged; log = %q", buf.String())
	}
	if !s.freshness.readerBehind() {
		t.Fatal("audited gap must mark the reader behind the writer")
	}

	// The presence probe consumed by the auth interceptor sees the registered
	// hash through the writer while the gap is suspected.
	present, err := s.OverseerTokenHashPresent(ctx, auth.HashToken(token))
	if err != nil {
		t.Fatalf("presence probe: %v", err)
	}
	if !present {
		t.Fatal("presence probe did not find the registered token hash in the writer view")
	}
}

// TestListOverseers_NoSelfHealWhenReaderFresh pins the steady state: no heal
// logging, no behind flag, and a gated-off presence probe that never touches
// the writer.
func TestListOverseers_NoSelfHealWhenReaderFresh(t *testing.T) {
	s := tempStore(t)
	ctx := context.Background()
	now := time.Now().UTC()
	token := registerOverseer(t, s, "veteran", now)

	buf := &bytes.Buffer{}
	s.log = newTestLogger(buf)

	got, err := s.ListOverseers(ctx)
	if err != nil {
		t.Fatalf("list overseers: %v", err)
	}
	if len(got) != 1 || got[0].ID != "veteran" {
		t.Fatalf("ListOverseers served %d rows, want exactly veteran: %+v", len(got), got)
	}
	if strings.Contains(buf.String(), "served the authoritative writer view") {
		t.Fatalf("heal fired with a fresh reader view; log = %q", buf.String())
	}
	if s.freshness.readerBehind() {
		t.Fatal("fresh audit marked the reader behind the writer")
	}

	// Gated off: the probe reports not-present without consulting the writer,
	// even though the hash is actually there — failed-token traffic must stay
	// off the write path in the steady state.
	present, err := s.OverseerTokenHashPresent(ctx, auth.HashToken(token))
	if err != nil {
		t.Fatalf("presence probe: %v", err)
	}
	if present {
		t.Fatal("presence probe fired while the freshness gate was closed (steady-state traffic must not touch the writer)")
	}
}

// TestOverseerTokenHashPresent_WriterBackedWhenSuspected covers the writer
// query itself while the staleness gate is open.
func TestOverseerTokenHashPresent_WriterBackedWhenSuspected(t *testing.T) {
	s := tempStore(t)
	ctx := context.Background()
	now := time.Now().UTC()
	token := registerOverseer(t, s, "veteran", now)

	// Simulate a suspected visibility gap so the probe is allowed.
	s.freshness.noteBehind(true)

	present, err := s.OverseerTokenHashPresent(ctx, auth.HashToken(token))
	if err != nil {
		t.Fatalf("presence probe: %v", err)
	}
	if !present {
		t.Fatal("probe did not find the registered token hash")
	}
	present, err = s.OverseerTokenHashPresent(ctx, auth.HashToken("never-issued-token"))
	if err != nil {
		t.Fatalf("presence probe: %v", err)
	}
	if present {
		t.Fatal("probe reported a token hash that was never registered")
	}
}
