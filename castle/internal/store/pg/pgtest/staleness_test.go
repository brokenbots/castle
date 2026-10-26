package pgtest

import (
	"bytes"
	"context"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/brokenbots/castle/castle/internal/store"
	"github.com/brokenbots/castle/castle/internal/store/pg"
)

func stalenessTestLogger(buf *bytes.Buffer) *slog.Logger {
	return slog.New(slog.NewTextHandler(buf, &slog.HandlerOptions{Level: slog.LevelDebug}))
}

// nsNow returns a deliberately sub-microsecond UTC timestamp so a store that
// records write freshness at raw nanosecond precision cannot possibly agree
// with the microsecond granularity the Postgres table persists (the KB-343
// defect shape: recorded ns > persisted µs forever).
func nsNow() time.Time {
	sec := time.Now().Unix()
	return time.Unix(sec, 123456789).UTC()
}

// TestCreateOverseer_FreshnessRecordsPersistedGranularity pins the KB-343
// convergence contract: the write-recorded freshness stamp must be expressed
// at the granularity the reader view can return. Postgres persists
// TIMESTAMPTZ at microsecond precision, so recording the caller's nanosecond
// timestamp made the stored view structurally older than freshness.newest
// forever and every ListOverseers re-read warned "reader view healed".
func TestCreateOverseer_FreshnessRecordsPersistedGranularity(t *testing.T) {
	ctx := context.Background()
	s := Store(t)

	buf := &bytes.Buffer{}
	s.SetLogger(stalenessTestLogger(buf))

	for range 3 {
		registerStalenessOverseer(t, ctx, s, "agent-"+time.Now().Format("150405.000000000"), nsNow())
		if _, err := s.ListOverseers(ctx); err != nil {
			t.Fatalf("ListOverseers: %v", err)
		}
	}
	if lines := buf.String(); strings.Contains(lines, "healed") || strings.Contains(lines, "stale") {
		t.Fatalf("freshness audit diverged on sub-microsecond writes:\n%s", lines)
	}
}

// TestListOverseers_FreshnessConvergesSoak is the KB-343 acceptance soak: a
// 100-read steady state after real writes must produce zero freshness
// telemetry — no authoritative fallback re-reads, no heal warnings, no
// staleness warnings on the auth token resolution path.
func TestListOverseers_FreshnessConvergesSoak(t *testing.T) {
	ctx := context.Background()
	s := Store(t)

	buf := &bytes.Buffer{}
	s.SetLogger(stalenessTestLogger(buf))

	// Two agents at nanosecond precision, the shape of time.Now()-driven
	// registrations on the pre-fix code.
	sec := time.Now().Unix()
	registerStalenessOverseer(t, ctx, s, "agent-soak-a", time.Unix(sec-2, 321456789).UTC())
	registerStalenessOverseer(t, ctx, s, "agent-soak-b", time.Unix(sec, 123456789).UTC())

	for i := range 100 {
		if _, err := s.ListOverseers(ctx); err != nil {
			t.Fatalf("ListOverseers read %d: %v", i, err)
		}
	}
	if lines := buf.String(); strings.Contains(lines, "healed") || strings.Contains(lines, "stale") {
		t.Fatalf("freshness audit did not converge across 100 soak reads:\n%s", lines)
	}
}

func registerStalenessOverseer(t *testing.T, ctx context.Context, s *pg.Store, id string, created time.Time) {
	t.Helper()
	if err := s.CreateOverseer(ctx, &store.Overseer{
		ID:         id,
		Name:       id,
		TokenHash:  "hash-" + id,
		Status:     "online",
		CreatedAt:  created,
		LastSeenAt: created,
	}); err != nil {
		t.Fatalf("CreateOverseer(%s): %v", id, err)
	}
}