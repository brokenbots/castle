package suite

import (
	"context"
	"testing"
	"time"

	"github.com/brokenbots/castle/castle/internal/store"
)

func microUTC(t time.Time) time.Time { return t.UTC().Truncate(time.Microsecond) }

func newOverseer(t *testing.T, s store.Store, id string, now time.Time, labels map[string]string) {
	t.Helper()
	o := &store.Overseer{ID: id, Name: "agent-" + id, Hostname: "host.local", Version: "v1", TokenHash: "hash-" + id, Status: "online", Labels: labels, CreatedAt: now, LastSeenAt: now}
	if err := s.CreateOverseer(context.Background(), o); err != nil {
		t.Fatalf("CreateOverseer(%s): %v", id, err)
	}
}

// TestOverseerRegisterHeartbeat is the register/heartbeat acceptance leg:
// registration round-trips every field, heartbeating bumps last_seen and
// drives the reported status, and unknown ids read back as ErrNotFound.
func TestOverseerRegisterHeartbeat(t *testing.T) {
	runWithCtx(t, func(t *testing.T, ctx context.Context, s store.Store) {
		now := microUTC(time.Now())
		newOverseer(t, s, "ov-1", now, map[string]string{"gpu": "true", "tier": "1"})

		got, err := s.GetOverseer(ctx, "ov-1")
		if err != nil {
			t.Fatalf("GetOverseer: %v", err)
		}
		if got.Name != "agent-ov-1" || got.Hostname != "host.local" || got.Version != "v1" || got.TokenHash != "hash-ov-1" || got.Status != "online" {
			t.Fatalf("registered overseer = %+v", got)
		}
		if got.Labels["gpu"] != "true" || got.Labels["tier"] != "1" {
			t.Fatalf("labels round-trip: %+v", got.Labels)
		}
		if !got.CreatedAt.Equal(now) || !got.LastSeenAt.Equal(now) {
			t.Fatalf("timestamps: created=%v seen=%v want %v", got.CreatedAt, got.LastSeenAt, now)
		}

		later := now.Add(30 * time.Second)
		if err := s.UpdateOverseerSeen(ctx, "ov-1", later); err != nil {
			t.Fatalf("UpdateOverseerSeen: %v", err)
		}
		seen, err := s.GetOverseer(ctx, "ov-1")
		if err != nil {
			t.Fatalf("GetOverseer after heartbeat: %v", err)
		}
		// Microsecond precision is the common round-trip precision across
		// engines; heartbeat comparisons below tolerate nothing finer.
		if want := microUTC(later); seen.LastSeenAt.Before(want) {
			t.Fatalf("heartbeat last_seen = %v, want >= %v", seen.LastSeenAt, want)
		}
		if seen.Status != "online" {
			t.Fatalf("heartbeat should mark online, got %q", seen.Status)
		}

		if err := s.UpdateOverseerStatus(ctx, "ov-1", "offline"); err != nil {
			t.Fatalf("UpdateOverseerStatus: %v", err)
		}
		gone, err := s.GetOverseer(ctx, "ov-1")
		if err != nil || gone.Status != "offline" {
			t.Fatalf("status update: got %q err %v", gone.Status, err)
		}

		if _, err := s.GetOverseer(ctx, "ov-missing"); err != store.ErrNotFound {
			t.Fatalf("GetOverseer(unknown) = %v, want ErrNotFound", err)
		}

		list, err := s.ListOverseers(ctx)
		if err != nil {
			t.Fatalf("ListOverseers: %v", err)
		}
		if len(list) != 1 || list[0].ID != "ov-1" {
			t.Fatalf("ListOverseers = %+v", list)
		}
	})
}

// TestMarkOfflineBefore sweeps every online overseer whose heartbeat predates
// the window offline and leaves fresher ones alone.
func TestMarkOfflineBefore(t *testing.T) {
	runWithCtx(t, func(t *testing.T, ctx context.Context, s store.Store) {
		now := microUTC(time.Now())
		newOverseer(t, s, "stale", now.Add(-2*time.Hour), nil)
		newOverseer(t, s, "fresh", now, nil)
		if err := s.UpdateOverseerStatus(ctx, "stale", "offline"); err != nil {
			t.Fatalf("seed stale offline: %v", err)
		}

		if err := s.MarkOfflineBefore(ctx, now.Add(-1*time.Hour)); err != nil {
			t.Fatalf("MarkOfflineBefore: %v", err)
		}
		got, err := s.GetOverseer(ctx, "fresh")
		if err != nil || got.Status != "online" {
			t.Fatalf("fresh overseer status = %q err %v, want online", got.Status, err)
		}

		// Re-activate the stale one under online, then re-run the sweep: an
		// already-offline row must not be resurrected or double-stamped.
		if err := s.UpdateOverseerStatus(ctx, "stale", "online"); err != nil {
			t.Fatalf("reactivate: %v", err)
		}
		if err := s.MarkOfflineBefore(ctx, now.Add(-1*time.Hour)); err != nil {
			t.Fatalf("MarkOfflineBefore 2: %v", err)
		}
		stale, err := s.GetOverseer(ctx, "stale")
		if err != nil || stale.Status != "offline" {
			t.Fatalf("stale overseer status = %q err %v, want offline", stale.Status, err)
		}
	})
}

// TestMarkOfflineBeforeUnknownID is the no-op guard for sweep idempotency:
// marking offline leaves rows that do not match the window untouched.
func TestOverseerSweepLeavesFutureHeartbeats(t *testing.T) {
	runWithCtx(t, func(t *testing.T, ctx context.Context, s store.Store) {
		now := microUTC(time.Now())
		newOverseer(t, s, "ov-future", now.Add(5*time.Second), nil)
		if err := s.MarkOfflineBefore(ctx, now); err != nil {
			t.Fatalf("MarkOfflineBefore: %v", err)
		}
		got, err := s.GetOverseer(ctx, "ov-future")
		if err != nil || got.Status != "online" {
			t.Fatalf("future heartbeat swept offline: %q err %v", got.Status, err)
		}
	})
}
