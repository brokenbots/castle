package sqlite

import (
	"context"
	"errors"
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

// TestLeaseWorkflowAssignment_SkipsStoppedRun pins the dispatch-side half of
// the CRI-207 invariant: the dispatch path must never hand a parked run's
// queued work to an agent. A silent re-dispatch would restart the run out of
// band of the operator — violating the rule that only an explicit ResumeRun
// moves a stopped run back to running (and back into this scan). The second
// half proves a resumed run that never started is actually executable again:
// its assignment is leasable, so resume leads to real execution instead of a
// running record with no lease.
func TestLeaseWorkflowAssignment_SkipsStoppedRun(t *testing.T) {
	s := tempStore(t)
	ctx := context.Background()
	now := time.Now().UTC()

	a := &store.WorkflowAssignment{
		OwnerCriteriaID: "owner-1",
		WorkflowName:    "wf",
		WorkflowSource:  "source",
		IdempotencyKey:  "key-1",
		CreatedAt:       now,
		UpdatedAt:       now,
	}
	if _, _, err := s.CreateWorkflowAssignment(ctx, a); err != nil {
		t.Fatalf("create assignment: %v", err)
	}
	if err := s.CreateOverseer(ctx, &store.Overseer{
		ID: "o1", Name: "agent-1", TokenHash: "t", Status: "online", CreatedAt: now, LastSeenAt: now,
	}); err != nil {
		t.Fatalf("create overseer: %v", err)
	}

	if err := s.SetRunStopped(ctx, a.RunID); err != nil {
		t.Fatalf("stop: %v", err)
	}
	if _, err := s.LeaseWorkflowAssignment(ctx, "o1", map[string]string{}, now, time.Minute); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("expected ErrNotFound leasing parked run's queued work, got %v", err)
	}

	if err := s.ClearRunStopped(ctx, a.RunID); err != nil {
		t.Fatalf("resume: %v", err)
	}
	leased, err := s.LeaseWorkflowAssignment(ctx, "o1", map[string]string{}, now, time.Minute)
	if err != nil {
		t.Fatalf("expected lease after resume, got %v", err)
	}
	if leased.RunID != a.RunID || leased.State != store.WorkflowAssignmentStateLeased {
		t.Fatalf("unexpected lease after resume: run=%s state=%s", leased.RunID, leased.State)
	}
}

// TestLeaseWorkflowAssignment_SkipsTerminalRun covers the same guard for the
// terminal cases: queued work whose run already finished is never dispatched.
func TestLeaseWorkflowAssignment_SkipsTerminalRun(t *testing.T) {
	s := tempStore(t)
	ctx := context.Background()
	now := time.Now().UTC()

	a := &store.WorkflowAssignment{
		OwnerCriteriaID: "owner-1",
		WorkflowName:    "wf",
		WorkflowSource:  "source",
		IdempotencyKey:  "key-1",
		CreatedAt:       now,
		UpdatedAt:       now,
	}
	if _, _, err := s.CreateWorkflowAssignment(ctx, a); err != nil {
		t.Fatalf("create assignment: %v", err)
	}
	if err := s.CreateOverseer(ctx, &store.Overseer{
		ID: "o1", Name: "agent-1", TokenHash: "t", Status: "online", CreatedAt: now, LastSeenAt: now,
	}); err != nil {
		t.Fatalf("create overseer: %v", err)
	}

	r, err := s.GetRun(ctx, a.RunID)
	if err != nil {
		t.Fatalf("get run: %v", err)
	}
	r.Status = "cancelled"
	r.EndedAt = &now
	if err := s.UpdateRun(ctx, r); err != nil {
		t.Fatalf("cancel run: %v", err)
	}

	if _, err := s.LeaseWorkflowAssignment(ctx, "o1", map[string]string{}, now, time.Minute); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("expected ErrNotFound leasing terminal run's queued work, got %v", err)
	}
}

// TestMarkRunUnstartedReturnsToLeasableBucket (CRI-207 review R1) pins the
// resume re-queue contract for a never-started parked run: the run returns to
// pending with started_at NULL and its held lease binding intact, the
// redelivery scan serves the held lease to the leasing agent again, a second
// lease attempt is refused while the agent holds the lease, and the stopped
// guard keeps a terminal run untouched.
func TestMarkRunUnstartedReturnsToLeasableBucket(t *testing.T) {
	s := tempStore(t)
	ctx := context.Background()
	now := time.Now().UTC()

	a := &store.WorkflowAssignment{
		OwnerCriteriaID: "owner-1",
		WorkflowName:    "wf",
		WorkflowSource:  "source",
		IdempotencyKey:  "key-1",
		CreatedAt:       now,
		UpdatedAt:       now,
	}
	if _, _, err := s.CreateWorkflowAssignment(ctx, a); err != nil {
		t.Fatalf("create assignment: %v", err)
	}
	if err := s.CreateOverseer(ctx, &store.Overseer{
		ID: "o1", Name: "agent-1", TokenHash: "t", Status: "online", CreatedAt: now, LastSeenAt: now,
	}); err != nil {
		t.Fatalf("create overseer: %v", err)
	}

	// Lease the run to the agent without any acceptance event: the
	// assignment is leased and the run has started_at NULL.
	leased, err := s.LeaseWorkflowAssignment(ctx, "o1", map[string]string{}, now, time.Minute)
	if err != nil {
		t.Fatalf("lease assignment: %v", err)
	}
	if leased.RunID != a.RunID || leased.State != store.WorkflowAssignmentStateLeased {
		t.Fatalf("unexpected lease: run=%s state=%s", leased.RunID, leased.State)
	}

	// Park it with the operator stop while the agent holds the lease.
	if err := s.SetRunStopped(ctx, a.RunID); err != nil {
		t.Fatalf("stop: %v", err)
	}

	// While parked, the redelivery scan stays quiet: a stopped run is not
	// deliverable — only resume returns it to the deliverable bucket.
	active, err := s.ListLeasedPendingAssignmentsByCriteriaID(ctx, "o1")
	if err != nil {
		t.Fatalf("redelivery scan while stopped: %v", err)
	}
	if len(active) != 0 {
		t.Fatalf("parked run redelivered while stopped: %d assignments", len(active))
	}

	if err := s.MarkRunUnstarted(ctx, a.RunID, now.Add(time.Second)); err != nil {
		t.Fatalf("mark unstarted: %v", err)
	}
	r, err := s.GetRun(ctx, a.RunID)
	if err != nil {
		t.Fatal(err)
	}
	if r.Status != "pending" || r.StartedAt != nil || r.EndedAt != nil {
		t.Fatalf("run after mark: status=%q started_at=%v ended_at=%v, want pending with started_at NULL", r.Status, r.StartedAt, r.EndedAt)
	}
	// The re-queue refreshes created_at (KB-233 rule 2): the created_never_started
	// reaper derives its window from created_at, so the resumed run must get a
	// fresh window before redelivery is counted as failing.
	if !r.CreatedAt.Equal(now.Add(time.Second)) {
		t.Fatalf("run after mark: created_at = %v, want requeuedAt %v", r.CreatedAt, now.Add(time.Second))
	}

	// The redelivery scan now serves the held lease to the leasing agent —
	// the path a resumed run relies on to get its work delivered again.
	active, err = s.ListLeasedPendingAssignmentsByCriteriaID(ctx, "o1")
	if err != nil {
		t.Fatalf("redelivery scan after mark: %v", err)
	}
	if len(active) != 1 || active[0].RunID != a.RunID {
		t.Fatalf("resumed unstarted run not redeliverable: %d assignments", len(active))
	}

	// The agent still holds the lease, so a competing lease attempt is
	// refused: redelivery goes through the held lease, not a re-lease.
	if _, err := s.LeaseWorkflowAssignment(ctx, "o1", map[string]string{}, now.Add(time.Second), time.Minute); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("expected ErrNotFound while the agent holds the redeliverable lease, got %v", err)
	}

	// The stopped guard keeps the transition out of a terminal run's state.
	r.Status = "succeeded"
	r.EndedAt = &now
	if err := s.UpdateRun(ctx, r); err != nil {
		t.Fatalf("stamp terminal: %v", err)
	}
	if err := s.MarkRunUnstarted(ctx, a.RunID, now.Add(2*time.Second)); err != nil {
		t.Fatalf("mark unstarted on terminal run: %v", err)
	}
	got, err := s.GetRun(ctx, a.RunID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != "succeeded" {
		t.Fatalf("terminal run moved to %q, want succeeded (stopped guard)", got.Status)
	}
}
