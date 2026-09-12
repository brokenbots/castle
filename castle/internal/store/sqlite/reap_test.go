package sqlite

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/brokenbots/castle/castle/internal/store"
)

// reapFixture seeds two agents — one with a fresh heartbeat, one whose
// heartbeat stopped before staleBefore — and runs across the status space so
// reaping is only ever exercised where CRI-142 allows it.
type reapFixture struct {
	s   *Store
	ctx context.Context
	now time.Time
	// staleBefore is the boundary used for the reaper call.
	staleBefore time.Time
}

func mustRun(t *testing.T, s *Store, ctx context.Context, id string) *store.Run {
	t.Helper()
	r, err := s.GetRun(ctx, id)
	if err != nil {
		t.Fatalf("get run %s: %v", id, err)
	}
	return r
}

func newReapFixture(t *testing.T) *reapFixture {
	t.Helper()
	s := tempStore(t)
	now := time.Now().UTC()
	f := &reapFixture{s: s, ctx: context.Background(), now: now, staleBefore: now.Add(-60 * time.Second)}

	freshSeen := now.Add(-5 * time.Second) // heartbeat 5s ago: fresh
	staleSeen := now.Add(-10 * time.Minute)
	for _, o := range []struct {
		id   string
		seen time.Time
	}{
		{"agent-fresh", freshSeen},
		{"agent-stale", staleSeen},
	} {
		if err := s.CreateOverseer(f.ctx, &store.Overseer{
			ID: o.id, Name: o.id, TokenHash: "x", Status: "online",
			CreatedAt: o.seen, LastSeenAt: o.seen,
		}); err != nil {
			t.Fatalf("create overseer %s: %v", o.id, err)
		}
	}
	return f
}

func (f *reapFixture) createRun(t *testing.T, id, agentID, status string) {
	t.Helper()
	if err := f.s.CreateRun(f.ctx, &store.Run{
		ID: id, OverseerID: agentID, WorkflowName: "wf", Status: status, CreatedAt: f.now,
	}); err != nil {
		t.Fatalf("create run %s: %v", id, err)
	}
}

func (f *reapFixture) getRun(t *testing.T, id string) *store.Run {
	t.Helper()
	return mustRun(t, f.s, f.ctx, id)
}

// TestReapStaleAgentRuns covers the CRI-142 heartbeat-staleness reaper: only
// pending/running runs of agents whose heartbeat is stale are stamped failed
// with "agent heartbeat lost"; fresh agents, queued work, paused runs and
// already terminal runs are untouched.
func TestReapStaleAgentRuns(t *testing.T) {
	f := newReapFixture(t)

	// Stale agent: reapable states.
	f.createRun(t, "r-stale-pending", "agent-stale", "pending")
	f.createRun(t, "r-stale-running", "agent-stale", "running")
	// Stale agent: protected states.
	f.createRun(t, "r-stale-paused", "agent-stale", "paused")
	f.createRun(t, "r-stale-succeeded", "agent-stale", "succeeded")
	f.createRun(t, "r-stale-failed", "agent-stale", "failed")
	f.createRun(t, "r-stale-cancelled", "agent-stale", "cancelled")
	// Fresh agent: identical statuses must survive.
	f.createRun(t, "r-fresh-pending", "agent-fresh", "pending")
	f.createRun(t, "r-fresh-running", "agent-fresh", "running")

	reaped, err := f.s.ReapStaleAgentRuns(f.ctx, f.now, f.staleBefore)
	if err != nil {
		t.Fatalf("reap: %v", err)
	}
	if len(reaped) != 2 {
		t.Fatalf("want 2 reaped runs, got %d: %v", len(reaped), reaped)
	}

	for _, id := range []string{"r-stale-pending", "r-stale-running"} {
		r := f.getRun(t, id)
		if r.Status != "failed" {
			t.Errorf("%s: want failed, got %q", id, r.Status)
		}
		if r.FailureReason != "agent heartbeat lost" {
			t.Errorf("%s: failure_reason = %q", id, r.FailureReason)
		}
		if r.EndedAt == nil {
			t.Errorf("%s: ended_at not stamped", id)
		}
	}

	for _, id := range []string{
		"r-stale-paused", "r-stale-succeeded", "r-stale-failed", "r-stale-cancelled",
		"r-fresh-pending", "r-fresh-running",
	} {
		r := f.getRun(t, id)
		if r.FailureReason != "" {
			t.Errorf("%s: failure_reason must stay empty, got %q", id, r.FailureReason)
		}
	}
	if r := f.getRun(t, "r-fresh-pending"); r.Status != "pending" {
		t.Errorf("fresh pending run reaped: %q", r.Status)
	}
	if r := f.getRun(t, "r-stale-paused"); r.Status != "paused" {
		t.Errorf("paused run reaped: %q", r.Status)
	}
	if r := f.getRun(t, "r-stale-succeeded"); r.Status != "succeeded" {
		t.Errorf("terminal run rewritten: %q", r.Status)
	}
}

// TestReapStaleAgentRuns_MarksAssignmentTerminal verifies that reaping a
// leased run also terminates its queued or leased workflow assignment so a
// dead agent's work is never re-dispatched (CRI-142). Queued assignments are
// not touched: their runs have no owning agent yet.
func TestReapStaleAgentRuns_MarksAssignmentTerminal(t *testing.T) {
	f := newReapFixture(t)

	a, created, err := f.s.CreateWorkflowAssignment(f.ctx, &store.WorkflowAssignment{
		OwnerCriteriaID: "agent-stale",
		WorkflowName:    "wf",
		WorkflowSource:  "workflow main {}",
		IdempotencyKey:  "key-1",
		State:           store.WorkflowAssignmentStateQueued,
		CreatedAt:       f.now,
		UpdatedAt:       f.now,
	})
	if err != nil || !created {
		t.Fatalf("create assignment: %v created=%v", err, created)
	}

	// Queued: no owning agent, nothing to reap yet.
	reaped, err := f.s.ReapStaleAgentRuns(f.ctx, f.now, f.staleBefore)
	if err != nil {
		t.Fatalf("reap queued: %v", err)
	}
	if len(reaped) != 0 {
		t.Fatalf("queued run must not be reaped, got %v", reaped)
	}
	if r := f.getRun(t, a.RunID); r.Status != "pending" {
		t.Fatalf("queued run mutated: %q", r.Status)
	}

	// The stale agent leases the work; the run now belongs to it.
	leased, err := f.s.LeaseWorkflowAssignment(f.ctx, "agent-stale", nil, f.now, time.Minute)
	if err != nil {
		t.Fatalf("lease: %v", err)
	}
	if leased.State != store.WorkflowAssignmentStateLeased {
		t.Fatalf("lease state: %q", leased.State)
	}

	reaped, err = f.s.ReapStaleAgentRuns(f.ctx, f.now, f.staleBefore)
	if err != nil {
		t.Fatalf("reap leased: %v", err)
	}
	if len(reaped) != 1 || reaped[0] != a.RunID {
		t.Fatalf("want [%s], got %v", a.RunID, reaped)
	}
	if r := f.getRun(t, a.RunID); r.Status != "failed" || r.FailureReason != "agent heartbeat lost" {
		t.Fatalf("reaped run: status=%q reason=%q", r.Status, r.FailureReason)
	}

	got, err := f.s.GetWorkflowAssignment(f.ctx, a.ID)
	if err != nil {
		t.Fatalf("get assignment: %v", err)
	}
	if got.State != store.WorkflowAssignmentStateTerminal {
		t.Errorf("assignment state = %q, want terminal", got.State)
	}
	if got.TerminalReason != "agent heartbeat lost" {
		t.Errorf("assignment terminal_reason = %q", got.TerminalReason)
	}
}

// TestReapStaleAgentRuns_AgentHeartbeatLostMidRun is the CRI-142 regression
// scenario: an agent registers, heartbeats, starts a run, then dies (pod
// deleted). Once heartbeat staleness exceeds the configured window the run
// must be terminal without any action from the dead agent.
func TestReapStaleAgentRuns_AgentHeartbeatLostMidRun(t *testing.T) {
	s := tempStore(t)
	ctx := context.Background()

	t0 := time.Now().UTC().Add(-2 * time.Minute) // wall-clock start
	if err := s.CreateOverseer(ctx, &store.Overseer{
		ID: "agent-dies", Name: "runner", TokenHash: "x", Status: "online",
		CreatedAt: t0, LastSeenAt: t0,
	}); err != nil {
		t.Fatalf("register: %v", err)
	}
	if err := s.CreateRun(ctx, &store.Run{
		ID: "r-zombie", OverseerID: "agent-dies", WorkflowName: "wf", Status: "pending", CreatedAt: t0,
	}); err != nil {
		t.Fatalf("create run: %v", err)
	}

	// Agent leases the work and emits RunStarted; heartbeats continue briefly.
	if err := s.UpdateOverseerSeen(ctx, "agent-dies", t0.Add(10*time.Second)); err != nil {
		t.Fatalf("heartbeat: %v", err)
	}
	run := mustRun(t, s, ctx, "r-zombie")
	run.Status = "running"
	run.CurrentStep = "s1"
	if err := s.UpdateRun(ctx, run); err != nil {
		t.Fatalf("run started: %v", err)
	}

	// Last heartbeat at t0+30s. The agent dies here; no SubmitEvents terminal
	// stamp ever arrives.
	lastBeat := t0.Add(30 * time.Second)
	if err := s.UpdateOverseerSeen(ctx, "agent-dies", lastBeat); err != nil {
		t.Fatalf("last heartbeat: %v", err)
	}

	// Before expiry the run survives.
	if _, err := s.ReapStaleAgentRuns(ctx, lastBeat.Add(50*time.Second), lastBeat.Add(50*time.Second).Add(-60*time.Second)); err != nil {
		t.Fatalf("reap before expiry: %v", err)
	}
	if r := mustRun(t, s, ctx, "r-zombie"); r.Status != "running" {
		t.Fatalf("run reaped before heartbeat expiry: %q", r.Status)
	}

	// Past expiry (last beat + 61s > 60s staleness): the run goes terminal.
	reapNow := lastBeat.Add(61 * time.Second)
	reaped, err := s.ReapStaleAgentRuns(ctx, reapNow, reapNow.Add(-60*time.Second))
	if err != nil {
		t.Fatalf("reap after expiry: %v", err)
	}
	if len(reaped) != 1 || reaped[0] != "r-zombie" {
		t.Fatalf("want [r-zombie], got %v", reaped)
	}
	r := mustRun(t, s, ctx, "r-zombie")
	if r.Status != "failed" || r.FailureReason != "agent heartbeat lost" || r.EndedAt == nil {
		t.Fatalf("zombie not stamped: status=%q reason=%q ended=%v", r.Status, r.FailureReason, r.EndedAt)
	}
}

// TestCancelRun covers the CRI-142 operator cancel path at the store layer:
// active runs go terminal with the operator reason, terminal runs are never
// rewritten, and unknown ids are ErrNotFound.
func TestCancelRun(t *testing.T) {
	f := newReapFixture(t)

	f.createRun(t, "r-cancel-pending", "agent-fresh", "pending")
	f.createRun(t, "r-cancel-running", "agent-fresh", "running")
	f.createRun(t, "r-done", "agent-fresh", "succeeded")

	cancelledAt := f.now.Add(time.Minute)
	r, err := f.s.CancelRun(f.ctx, "r-cancel-pending", "criteriarun deleted", cancelledAt)
	if err != nil {
		t.Fatalf("cancel pending: %v", err)
	}
	if r.Status != "cancelled" || r.FailureReason != "criteriarun deleted" || r.EndedAt == nil {
		t.Fatalf("cancelled run: status=%q reason=%q ended=%v", r.Status, r.FailureReason, r.EndedAt)
	}
	if got := f.getRun(t, "r-cancel-pending"); got.Status != "cancelled" || got.FailureReason != "criteriarun deleted" {
		t.Fatalf("persisted cancel: status=%q reason=%q", got.Status, got.FailureReason)
	}

	// A default reason is recorded when the operator supplies none.
	if _, err := f.s.CancelRun(f.ctx, "r-cancel-running", "", cancelledAt); err != nil {
		t.Fatalf("cancel running: %v", err)
	}
	if got := f.getRun(t, "r-cancel-running"); got.Status != "cancelled" || got.FailureReason != "" {
		t.Fatalf("cancelled run: status=%q reason=%q", got.Status, got.FailureReason)
	}

	// Terminal states are never overwritten.
	if _, err := f.s.CancelRun(f.ctx, "r-done", "late", cancelledAt); !errors.Is(err, store.ErrRunTerminal) {
		t.Fatalf("cancel succeeded run: want ErrRunTerminal, got %v", err)
	}
	if got := f.getRun(t, "r-done"); got.Status != "succeeded" || got.EndedAt != nil {
		t.Fatalf("terminal run mutated: status=%q ended=%v", got.Status, got.EndedAt)
	}

	if _, err := f.s.CancelRun(f.ctx, "missing", "", cancelledAt); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("cancel unknown run: want ErrNotFound, got %v", err)
	}
}

// TestUpdateRun_PreservesFailureReason verifies the CRI-131-style partial
// update contract extends to failure_reason: a status update that carries no
// reason must not clear an existing one.
func TestUpdateRun_PreservesFailureReason(t *testing.T) {
	f := newReapFixture(t)
	f.createRun(t, "r-reason", "agent-stale", "pending")

	if _, err := f.s.CancelRun(f.ctx, "r-reason", "criteriarun deleted", f.now); err != nil {
		t.Fatalf("cancel: %v", err)
	}

	run := f.getRun(t, "r-reason")
	run.FailureReason = "" // callers holding a stale record must not clobber
	if err := f.s.UpdateRun(f.ctx, run); err != nil {
		t.Fatalf("update: %v", err)
	}
	if got := f.getRun(t, "r-reason"); got.FailureReason != "criteriarun deleted" {
		t.Fatalf("failure_reason clobbered: %q", got.FailureReason)
	}
}
