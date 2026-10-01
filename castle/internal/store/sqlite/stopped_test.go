package sqlite

import (
	"testing"
	"time"

	"github.com/brokenbots/castle/castle/internal/store"
)

// stoppedFixture mirrors the reaper tests: one agent whose heartbeat went
// stale long before the expiry window and one fresh agent.
func newStoppedFixture(t *testing.T) *reapFixture {
	t.Helper()
	f := newReapFixture(t)
	return f
}

// TestSetRunStoppedParksRun (CRI-207) pins the park semantics: STOPPED
// replaces the active status on the SAME run id, is not terminal (ended_at is
// untouched), and drops any pause state — a parked run is neither paused nor
// waiting on a signal. Terminal runs are never rewritten.
func TestSetRunStoppedParksRun(t *testing.T) {
	f := newStoppedFixture(t)
	ctx, s := f.ctx, f.s

	f.createRun(t, "r-stop-running", "agent-stale", "running")
	f.createRun(t, "r-stop-paused", "agent-stale", "paused")
	f.createRun(t, "r-stop-succeeded", "agent-stale", "succeeded")
	f.createRun(t, "r-stop-failed", "agent-stale", "failed")
	f.createRun(t, "r-stop-cancelled", "agent-stale", "cancelled")

	// Seed pause state on the stopped-from-paused run.
	before := time.Now().UTC().Add(-time.Hour)
	if err := s.SetRunPaused(ctx, "r-stop-paused", "deploy-signal", before); err != nil {
		t.Fatalf("set paused: %v", err)
	}
	// Give the succeeded run a terminal stamp that must survive.
	if err := s.UpdateRun(ctx, &store.Run{ID: "r-stop-succeeded", Status: "succeeded", EndedAt: &before}); err != nil {
		t.Fatalf("stamp succeeded: %v", err)
	}

	for _, id := range []string{"r-stop-running", "r-stop-paused"} {
		if err := s.SetRunStopped(ctx, id); err != nil {
			t.Fatalf("stop %s: %v", id, err)
		}
	}
	if err := s.SetRunStopped(ctx, "r-stop-succeeded"); err != nil {
		t.Fatalf("stop terminal succeeded: %v", err)
	}
	if err := s.SetRunStopped(ctx, "r-stop-failed"); err != nil {
		t.Fatalf(" stop terminal failed: %v", err)
	}
	if err := s.SetRunStopped(ctx, "r-stop-cancelled"); err != nil {
		t.Fatalf("stop terminal cancelled: %v", err)
	}

	r := f.getRun(t, "r-stop-running")
	if r.Status != "stopped" {
		t.Fatalf("r-stop-running: status = %q, want stopped", r.Status)
	}
	if r.EndedAt != nil {
		t.Fatalf("r-stop-running: ended_at stamped on a resumable park: %v", r.EndedAt)
	}
	if r.PendingSignal != "" || r.PausedAt != nil {
		t.Fatal("r-stop-running: pause state left behind")
	}

	r = f.getRun(t, "r-stop-paused")
	if r.Status != "stopped" {
		t.Fatalf("r-stop-paused: status = %q, want stopped", r.Status)
	}
	if r.PendingSignal != "" || r.PausedAt != nil {
		t.Fatalf("r-stop-paused: pause state not dropped: signal=%q paused_at=%v", r.PendingSignal, r.PausedAt)
	}

	for _, id := range []string{"r-stop-succeeded", "r-stop-failed", "r-stop-cancelled"} {
		if got := f.getRun(t, id).Status; got != map[string]string{
			"r-stop-succeeded": "succeeded", "r-stop-failed": "failed", "r-stop-cancelled": "cancelled",
		}[id] {
			t.Errorf("%s: terminal run rewritten, status = %q", id, got)
		}
	}
}

// TestClearRunStoppedResumes (CRI-207) pins the resume semantics: only a
// stopped run moves back to running on the same run id, and a paused or
// terminal run is never touched.
func TestClearRunStoppedResumes(t *testing.T) {
	f := newStoppedFixture(t)
	ctx, s := f.ctx, f.s

	f.createRun(t, "r-resume-stopped", "agent-stale", "running")
	if err := s.SetRunStopped(ctx, "r-resume-stopped"); err != nil {
		t.Fatalf("stop: %v", err)
	}
	if err := s.ClearRunStopped(ctx, "r-resume-stopped"); err != nil {
		t.Fatalf("resume: %v", err)
	}
	r := f.getRun(t, "r-resume-stopped")
	if r.Status != "running" {
		t.Fatalf("resumed run status = %q, want running (same run id %s)", r.Status, r.ID)
	}

	// A late resume must not touch a run that is not stopped anymore.
	if err := s.ClearRunStopped(ctx, "r-resume-stopped"); err != nil {
		t.Fatalf("resume twice: %v", err)
	}
	if r := f.getRun(t, "r-resume-stopped"); r.Status != "running" {
		t.Fatalf("non-stopped run rewritten on resume: %q", r.Status)
	}

	// Terminal runs are never resurrected through the stop/resume path.
	f.createRun(t, "r-resume-terminal", "agent-stale", "succeeded")
	if err := s.SetRunStopped(ctx, "r-resume-terminal"); err != nil {
		t.Fatalf("stop terminal: %v", err)
	}
	if err := s.ClearRunStopped(ctx, "r-resume-terminal"); err != nil {
		t.Fatalf("resume terminal: %v", err)
	}
	if r := f.getRun(t, "r-resume-terminal"); r.Status != "succeeded" {
		t.Fatalf("terminal run rewritten: %q", r.Status)
	}
}

// TestReapStaleAgentRuns_IgnoresStopped (CRI-207) is the explicit reaper
// exemption regression: a run parked as stopped is NOT reaped even though its
// agent has not heartbeated since long before the staleness window — a parked
// run has no live heartbeat by design. After the operator resumes it
// (ClearRunStopped, status back to running), the SAME reaper pass must reap
// it, proving no residual exemption survives a resume.
func TestReapStaleAgentRuns_IgnoresStopped(t *testing.T) {
	f := newStoppedFixture(t)
	ctx, s := f.ctx, f.s

	f.createRun(t, "r-stale-stopped", "agent-stale", "running")
	if err := s.SetRunStopped(ctx, "r-stale-stopped"); err != nil {
		t.Fatalf("stop: %v", err)
	}

	// The agent's heartbeat is minutes older than the expiry window.
	if _, err := s.ReapStaleAgentRuns(ctx, f.now, f.staleBefore); err != nil {
		t.Fatalf("reap: %v", err)
	}
	if r := f.getRun(t, "r-stale-stopped"); r.Status != "stopped" {
		t.Fatalf("stopped run reaped: status = %q", r.Status)
	}

	// Resume path: stopped -> running on the same run id.
	if err := s.ClearRunStopped(ctx, "r-stale-stopped"); err != nil {
		t.Fatalf("resume: %v", err)
	}

	// Resumed run is an ordinary run again: the unchanged reaper applies.
	reaped, err := s.ReapStaleAgentRuns(ctx, f.now, f.staleBefore)
	if err != nil {
		t.Fatalf("reap after resume: %v", err)
	}
	if len(reaped) != 1 || reaped[0] != "r-stale-stopped" {
		t.Fatalf("resumed run not reaped with unchanged semantics: %v", reaped)
	}
	r := f.getRun(t, "r-stale-stopped")
	if r.Status != "failed" || r.FailureReason != "agent heartbeat lost" {
		t.Fatalf("after resume+reap: status=%q reason=%q", r.Status, r.FailureReason)
	}
}