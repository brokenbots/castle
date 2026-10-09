package sqlite

import (
	"context"
	"errors"
	"slices"
	"testing"
	"time"

	"github.com/brokenbots/castle/castle/internal/store"
)

// neverStartedFixture drives the KB-233 rule-2 reaper tests. createdBefore is
// the boundary used for the reaper call: runs created before it whose
// started_at is still NULL qualify for the created_never_started verdict.
type neverStartedFixture struct {
	reapFixture
	createdBefore time.Time
}

func newNeverStartedFixture(t *testing.T) *neverStartedFixture {
	t.Helper()
	base := newReapFixture(t)
	return &neverStartedFixture{
		reapFixture:   *base,
		createdBefore: base.now.Add(-10 * time.Minute),
	}
}

// createRunAt seeds a run with an explicit created_at: the rule-2 predicate
// keys on created_at age, so most of these tests need records minted in the
// past (createRun always stamps f.now).
func (f *neverStartedFixture) createRunAt(t *testing.T, id, agentID, status string, createdAt time.Time) {
	t.Helper()
	if err := f.s.CreateRun(f.ctx, &store.Run{
		ID: id, OverseerID: agentID, WorkflowName: "wf", Status: status, CreatedAt: createdAt,
	}); err != nil {
		t.Fatalf("create run %s: %v", id, err)
	}
}

// seedStarted stamps a run as truly started: pending → running the same way
// the RPC layer does it, with started_at stamped at the running transition
// (it stays set forever after).
func seedNeverStartedRunStarted(t *testing.T, f *neverStartedFixture, id, agentID string, createdAt, startedAt time.Time) {
	t.Helper()
	f.createRunAt(t, id, agentID, "pending", createdAt)
	if err := f.s.UpdateRun(f.ctx, &store.Run{
		ID: id, Status: "running", StartedAt: &startedAt,
	}); err != nil {
		t.Fatalf("start run %s: %v", id, err)
	}
}

// TestReapNeverStartedRuns_Acceptance is the KB-233 rule-2 acceptance test: a
// CreateRun without a StartRun is transitioned terminal-failed with the named
// reason created_never_started, inside the operator-tunable window; the pass
// is idempotent and the reaped record keeps its nil StartedAt.
func TestReapNeverStartedRuns_Acceptance(t *testing.T) {
	f := newNeverStartedFixture(t)

	// The orphan-minted shape (KB-227's surviving-Job mints): a run record
	// was minted by a lingering worker, created_at long past, started_at
	// never set, and nobody will ever start it.
	f.createRunAt(t, "r-orphan", "agent-stale", "pending", f.createdBefore.Add(-time.Minute))

	reaped, err := f.s.ReapNeverStartedRuns(f.ctx, f.now, f.createdBefore)
	if err != nil {
		t.Fatalf("reap: %v", err)
	}
	if !slices.Equal(reaped, []string{"r-orphan"}) {
		t.Fatalf("reaped = %v, want [r-orphan]", reaped)
	}

	r := f.getRun(t, "r-orphan")
	if r.Status != "failed" {
		t.Fatalf("status = %q, want failed", r.Status)
	}
	if r.FailureReason != "created_never_started" {
		t.Fatalf("failure_reason = %q, want created_never_started", r.FailureReason)
	}
	if r.EndedAt == nil {
		t.Fatal("ended_at not stamped")
	}
	if r.StartedAt != nil {
		t.Fatalf("started_at rewritten to %v, must stay NULL", r.StartedAt)
	}

	// Idempotent: the terminal record is never rewritten, the next pass finds
	// nothing left.
	reaped, err = f.s.ReapNeverStartedRuns(f.ctx, f.now, f.createdBefore)
	if err != nil {
		t.Fatalf("second reap: %v", err)
	}
	if len(reaped) != 0 {
		t.Fatalf("second pass reaped %v, want none", reaped)
	}
	if r := f.getRun(t, "r-orphan"); r.Status != "failed" || r.FailureReason != "created_never_started" {
		t.Fatalf("terminal record rewritten: status=%q reason=%q", r.Status, r.FailureReason)
	}
}

// TestReapNeverStartedRuns_StatusMatrix pins the rule-2 predicates: only
// pending/running runs whose started_at is still NULL and whose created_at is
// older than createdBefore are reaped. Created-in-window, started, parked
// (paused/stopped, CRI-207/W05) and terminal runs are never rewritten.
func TestReapNeverStartedRuns_StatusMatrix(t *testing.T) {
	f := newNeverStartedFixture(t)
	old := f.createdBefore.Add(-time.Minute)

	// In-window created runs must survive even though started_at is NULL.
	f.createRun(t, "r-new-pending", "agent-stale", "pending")
	// Started runs survive regardless of age: started_at is set.
	seedNeverStartedRunStarted(t, f, "r-old-started", "agent-stale", f.createdBefore.Add(-time.Minute), f.createdBefore)
	// Parked runs are reaper-exempt regardless of age (CRI-207/W05).
	f.createRunAt(t, "r-old-paused", "agent-stale", "paused", old)
	f.createRunAt(t, "r-old-stopped", "agent-stale", "stopped", old)
	// Terminal runs are never rewritten.
	f.createRunAt(t, "r-old-succeeded", "agent-stale", "succeeded", old)
	f.createRunAt(t, "r-old-failed", "agent-stale", "failed", old)
	f.createRunAt(t, "r-old-cancelled", "agent-stale", "cancelled", old)
	// Reapable: past-window, never started.
	f.createRunAt(t, "r-old-pending", "agent-stale", "pending", old)
	f.createRunAt(t, "r-old-running-unstarted", "agent-stale", "running", old)

	reaped, err := f.s.ReapNeverStartedRuns(f.ctx, f.now, f.createdBefore)
	if err != nil {
		t.Fatalf("reap: %v", err)
	}
	if !slices.Equal(reaped, []string{"r-old-pending", "r-old-running-unstarted"}) {
		t.Fatalf("reaped = %v, want [r-old-pending r-old-running-unstarted]", reaped)
	}

	for _, id := range []string{"r-old-pending", "r-old-running-unstarted"} {
		if r := f.getRun(t, id); r.Status != "failed" || r.FailureReason != "created_never_started" || r.EndedAt == nil {
			t.Errorf("%s: status=%q reason=%q ended=%v, want failed/created_never_started/ended", id, r.Status, r.FailureReason, r.EndedAt)
		}
	}

	for _, id := range []string{
		"r-new-pending", "r-old-started", "r-old-paused", "r-old-stopped",
		"r-old-succeeded", "r-old-failed", "r-old-cancelled",
	} {
		if r := f.getRun(t, id); r.FailureReason != "" {
			t.Errorf("%s: reaper wrote failure_reason %q", id, r.FailureReason)
		}
	}
	if r := f.getRun(t, "r-new-pending"); r.Status != "pending" {
		t.Errorf("in-window pending run reaped: %q", r.Status)
	}
	if r := f.getRun(t, "r-old-started"); r.Status != "running" || r.StartedAt == nil {
		t.Errorf("started run rewritten: status=%q started_at=%v", r.Status, r.StartedAt)
	}
	if r := f.getRun(t, "r-old-paused"); r.Status != "paused" {
		t.Errorf("paused run reaped: %q", r.Status)
	}
	if r := f.getRun(t, "r-old-stopped"); r.Status != "stopped" {
		t.Errorf("stopped run reaped: %q", r.Status)
	}
	for _, c := range []struct{ id, want string }{
		{"r-old-succeeded", "succeeded"},
		{"r-old-failed", "failed"},
		{"r-old-cancelled", "cancelled"},
	} {
		if r := f.getRun(t, c.id); r.Status != c.want {
			t.Errorf("terminal run %s rewritten: %q", c.id, r.Status)
		}
	}
}

// TestReapNeverStartedRuns_MarksAssignmentTerminal pins that the rule-2 reap
// terminates the still-queued or leased workflow assignment so orphan-minted
// work (KB-227's surviving-Job mints) can never be re-dispatched after the
// run dies.
func TestReapNeverStartedRuns_MarksAssignmentTerminal(t *testing.T) {
	f := newNeverStartedFixture(t)

	// A queued assignment owned by an identity with no overseer record: the
	// orphan-minted shape the heartbeat reaper deliberately leaves alone (no
	// heartbeating agent exists) but rule 2 kills by timer.
	a, created, err := f.s.CreateWorkflowAssignment(f.ctx, &store.WorkflowAssignment{
		OwnerCriteriaID: "orphan-job",
		WorkflowName:    "wf",
		WorkflowSource:  "workflow main {}",
		IdempotencyKey:  "key-1",
		State:           store.WorkflowAssignmentStateQueued,
		CreatedAt:       f.createdBefore.Add(-time.Minute),
		UpdatedAt:       f.createdBefore.Add(-time.Minute),
	})
	if err != nil || !created {
		t.Fatalf("create assignment: %v created=%v", err, created)
	}

	reaped, err := f.s.ReapNeverStartedRuns(f.ctx, f.now, f.createdBefore)
	if err != nil {
		t.Fatalf("reap: %v", err)
	}
	if !slices.Equal(reaped, []string{a.RunID}) {
		t.Fatalf("reaped = %v, want [%s]", reaped, a.RunID)
	}

	got, err := f.s.GetWorkflowAssignment(f.ctx, a.ID)
	if err != nil {
		t.Fatalf("get assignment: %v", err)
	}
	if got.State != store.WorkflowAssignmentStateTerminal {
		t.Errorf("assignment state = %q, want terminal", got.State)
	}
	if got.TerminalReason != "created_never_started" {
		t.Errorf("assignment terminal_reason = %q, want created_never_started", got.TerminalReason)
	}
}

// TestReapNeverStartedRuns_RequeueResetsWindow pins the operator-resume
// interplay (CRI-207 × KB-233 rule 2): a never-started stopped run parked past
// the window is reaper-exempt, but the MarkRunUnstarted re-queue refreshes
// created_at, so the resumed incarnation gets exactly one full
// created_never_started window to land its redelivery.
func TestReapNeverStartedRuns_RequeueResetsWindow(t *testing.T) {
	f := newNeverStartedFixture(t)

	a, _, err := f.s.CreateWorkflowAssignment(f.ctx, &store.WorkflowAssignment{
		OwnerCriteriaID: "owner-1",
		WorkflowName:    "wf",
		WorkflowSource:  "source",
		IdempotencyKey:  "key-1",
		CreatedAt:       f.createdBefore.Add(-time.Hour),
		UpdatedAt:       f.createdBefore.Add(-time.Hour),
	})
	if err != nil {
		t.Fatalf("create assignment: %v", err)
	}
	if err := f.s.CreateOverseer(f.ctx, &store.Overseer{
		ID: "o1", Name: "agent-1", TokenHash: "t", Status: "online",
		CreatedAt: f.now, LastSeenAt: f.now,
	}); err != nil {
		t.Fatalf("create overseer: %v", err)
	}
	if _, err := f.s.LeaseWorkflowAssignment(f.ctx, "o1", map[string]string{}, f.now.Add(-2*time.Hour), time.Minute); err != nil {
		t.Fatalf("lease: %v", err)
	}
	if err := f.s.SetRunStopped(f.ctx, a.RunID); err != nil {
		t.Fatalf("stop: %v", err)
	}

	// Parked: exempt from the rule-2 reaper no matter how old the record is.
	if reaped, err := f.s.ReapNeverStartedRuns(f.ctx, f.now, f.createdBefore); err != nil || len(reaped) != 0 {
		t.Fatalf("parked run reaped: %v err=%v", reaped, err)
	}

	// The operator resumes: the run returns to the leasable bucket with a
	// fresh created_at, so the next window applies to the new incarnation.
	resumedAt := f.now.Add(time.Minute)
	if err := f.s.MarkRunUnstarted(f.ctx, a.RunID, resumedAt); err != nil {
		t.Fatalf("mark unstarted: %v", err)
	}
	if reaped, err := f.s.ReapNeverStartedRuns(f.ctx, f.now, f.createdBefore); err != nil || len(reaped) != 0 {
		t.Fatalf("just-resumed run reaped inside its fresh window: %v err=%v", reaped, err)
	}

	// Past the fresh window with the run still unstarted, it dies by timer.
	later := resumedAt.Add(11 * time.Minute)
	reaped, err := f.s.ReapNeverStartedRuns(f.ctx, later, later.Add(-10*time.Minute))
	if err != nil {
		t.Fatalf("reap later: %v", err)
	}
	if !slices.Equal(reaped, []string{a.RunID}) {
		t.Fatalf("reaped = %v, want [%s]", reaped, a.RunID)
	}
	if r := f.getRun(t, a.RunID); r.Status != "failed" || r.FailureReason != "created_never_started" {
		t.Fatalf("resumed-then-unstarted run: status=%q reason=%q", r.Status, r.FailureReason)
	}
}

// TestReapNeverStartedRunIDs_DropsResolvedCandidatesFromWrite is the rule-2
// twin of the CRI-143 re-validation regression: when the write-transaction
// re-validation drops a strict subset of the scanned candidates — a run that
// started or reached a terminal state between the reader scan and the writer
// transaction — the surviving set is smaller and the UPDATE placeholder lists
// must be derived from the surviving ids, never from the scanned candidates.
func TestReapNeverStartedRunIDs_DropsResolvedCandidatesFromWrite(t *testing.T) {
	f := newNeverStartedFixture(t)
	ctx := context.Background()

	old := f.createdBefore.Add(-time.Minute)
	f.createRunAt(t, "r-zombie-1", "agent-stale", "pending", old)
	f.createRunAt(t, "r-zombie-2", "agent-stale", "running", old)
	// Candidate that started between scan and write.
	f.createRunAt(t, "r-started", "agent-stale", "pending", old)
	if err := f.s.UpdateRun(ctx, &store.Run{
		ID: "r-started", Status: "running", StartedAt: &old,
	}); err != nil {
		t.Fatalf("start r-started: %v", err)
	}
	// Candidate that resolved between scan and write.
	f.createRunAt(t, "r-resolved", "agent-stale", "pending", old)
	if _, err := f.s.CancelRun(ctx, "r-resolved", "done", f.now); err != nil {
		t.Fatalf("cancel r-resolved: %v", err)
	}

	candidates := []string{"r-zombie-1", "r-zombie-2", "r-started", "r-resolved"}
	reaped, err := f.s.reapNeverStartedRunIDs(ctx, f.now, f.createdBefore, candidates)
	if err != nil {
		t.Fatalf("reap with partially resolved candidates: %v", err)
	}
	if !slices.Equal(reaped, []string{"r-zombie-1", "r-zombie-2"}) {
		t.Fatalf("reaped = %v, want [r-zombie-1 r-zombie-2]", reaped)
	}

	for _, id := range []string{"r-zombie-1", "r-zombie-2"} {
		if r := f.getRun(t, id); r.Status != "failed" || r.FailureReason != "created_never_started" || r.EndedAt == nil {
			t.Fatalf("run %s: status=%q reason=%q ended=%v", id, r.Status, r.FailureReason, r.EndedAt)
		}
	}
	// The started candidate keeps its running state with its stamped start.
	if r := f.getRun(t, "r-started"); r.Status != "running" || r.StartedAt == nil {
		t.Fatalf("started candidate clobbered: status=%q started=%v", r.Status, r.StartedAt)
	}
	// The resolved candidate keeps its own terminal state and reason.
	if r := f.getRun(t, "r-resolved"); r.Status != "cancelled" || r.FailureReason != "done" {
		t.Fatalf("resolved candidate clobbered: status=%q reason=%q", r.Status, r.FailureReason)
	}
}

// TestReapNeverStartedRuns_EmptyStore is a smoke check that reaping never
// errors on an empty database.
func TestReapNeverStartedRuns_EmptyStore(t *testing.T) {
	f := newNeverStartedFixture(t)
	reaped, err := f.s.ReapNeverStartedRuns(f.ctx, f.now, f.createdBefore)
	if err != nil {
		t.Fatalf("reap empty store: %v", err)
	}
	if len(reaped) != 0 {
		t.Fatalf("reaped runs from empty store: %v", reaped)
	}
}

// TestReapNeverStartedRuns_GetUnknownRunIsNotFound guards the reaper's error
// contract: a pass that reaps nothing still leaves lookups of unknown runs
// reporting store.ErrNotFound rather than a swallowed error.
func TestReapNeverStartedRuns_GetUnknownRunIsNotFound(t *testing.T) {
	f := newNeverStartedFixture(t)
	if _, err := f.s.ReapNeverStartedRuns(f.ctx, f.now, f.createdBefore); err != nil {
		t.Fatalf("reap: %v", err)
	}
	if _, err := f.s.GetRun(f.ctx, "does-not-exist"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("get unknown run: err = %v, want ErrNotFound", err)
	}
}