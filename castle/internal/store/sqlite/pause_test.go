package sqlite

import (
	"context"
	"testing"
	"time"

	"github.com/brokenbots/castle/castle/internal/store"
)

// TestClearRunPausedTerminalSafe (CRI-197): clearing the pause state flips a
// paused run back to running with the pending signal cleared, but never
// resurrects a terminal run whose pause state is stale history — e.g. a
// console resume accepted around the same moment the run went terminal.
func TestClearRunPausedTerminalSafe(t *testing.T) {
	s := tempStore(t)
	ctx := context.Background()

	now := time.Now().UTC()
	if err := s.CreateOverseer(ctx, &store.Overseer{ID: "ov-1", Name: "agent-a", TokenHash: "x", Status: "online", CreatedAt: now, LastSeenAt: now}); err != nil {
		t.Fatalf("create overseer: %v", err)
	}
	if err := s.CreateRun(ctx, &store.Run{ID: "r-pause", OverseerID: "ov-1", WorkflowName: "wf", Status: "running", CreatedAt: now}); err != nil {
		t.Fatalf("create run: %v", err)
	}

	// Paused run: clear restores running and drops the pending signal.
	if err := s.SetRunPaused(ctx, "r-pause", "deploy-signal", now); err != nil {
		t.Fatalf("set paused: %v", err)
	}
	if err := s.ClearRunPaused(ctx, "r-pause"); err != nil {
		t.Fatalf("clear paused: %v", err)
	}
	got, err := s.GetRun(ctx, "r-pause")
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if got.Status != "running" || got.PendingSignal != "" || got.PausedAt != nil {
		t.Fatalf("after clear: status=%q pending_signal=%q paused_at=%v", got.Status, got.PendingSignal, got.PausedAt)
	}

	// Terminal run: a late ClearRunPaused must not rewrite it back to running.
	if err := s.SetRunPaused(ctx, "r-pause", "deploy-signal", now); err != nil {
		t.Fatalf("set paused again: %v", err)
	}
	if _, err := s.CancelRun(ctx, "r-pause", "operator stop", now); err != nil {
		t.Fatalf("cancel run: %v", err)
	}
	if err := s.ClearRunPaused(ctx, "r-pause"); err != nil {
		t.Fatalf("clear terminal: %v", err)
	}
	got, err = s.GetRun(ctx, "r-pause")
	if err != nil {
		t.Fatalf("get terminal: %v", err)
	}
	if got.Status != "cancelled" {
		t.Fatalf("terminal run resurrected: status=%q", got.Status)
	}
}
