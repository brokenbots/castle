package main

import (
	"context"
	"log/slog"
	"testing"
	"time"

	"github.com/brokenbots/castle/castle/internal/store"
	"github.com/brokenbots/castle/castle/internal/store/sqlite"
)

// TestReapStaleRunsOnce pins the main.go reaper wiring (CRI-142): the
// staleness passed by the ticker must be subtracted from now (not added), and
// a reaped pass stamps exactly the stale agent's active runs.
func TestReapStaleRunsOnce(t *testing.T) {
	s, err := sqlite.Open(t.TempDir() + "/castle.db")
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer s.Close()
	ctx := context.Background()
	log := slog.New(slog.NewTextHandler(testWriter{}, nil))

	now := time.Now().UTC()
	staleSeen := now.Add(-10 * time.Minute)
	freshSeen := now.Add(-5 * time.Second)
	for _, o := range []struct {
		id   string
		seen time.Time
	}{
		{"agent-dead", staleSeen},
		{"agent-alive", freshSeen},
	} {
		if err := s.CreateOverseer(ctx, &store.Overseer{
			ID: o.id, Name: o.id, TokenHash: "x", Status: "online",
			CreatedAt: o.seen, LastSeenAt: o.seen,
		}); err != nil {
			t.Fatalf("create overseer %s: %v", o.id, err)
		}
	}
	for _, r := range []struct{ id, agent string }{
		{"r-zombie", "agent-dead"},
		{"r-live", "agent-alive"},
	} {
		if err := s.CreateRun(ctx, &store.Run{
			ID: r.id, OverseerID: r.agent, WorkflowName: "wf", Status: "running", CreatedAt: now,
		}); err != nil {
			t.Fatalf("create run %s: %v", r.id, err)
		}
	}

	reapStaleRunsOnce(ctx, s, log, 60*time.Second)

	zombie, err := s.GetRun(ctx, "r-zombie")
	if err != nil {
		t.Fatalf("get zombie: %v", err)
	}
	if zombie.Status != "failed" || zombie.FailureReason != "agent heartbeat lost" || zombie.EndedAt == nil {
		t.Fatalf("zombie not reaped: status=%q reason=%q ended=%v", zombie.Status, zombie.FailureReason, zombie.EndedAt)
	}
	live, err := s.GetRun(ctx, "r-live")
	if err != nil {
		t.Fatalf("get live: %v", err)
	}
	if live.Status != "running" || live.FailureReason != "" {
		t.Fatalf("live run reaped: status=%q reason=%q", live.Status, live.FailureReason)
	}
}

type testWriter struct{}

func (testWriter) Write(p []byte) (int, error) { return len(p), nil }
