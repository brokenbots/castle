package suite

import (
	"context"
	"slices"
	"testing"
	"time"

	"github.com/brokenbots/castle/castle/internal/store"
	"github.com/brokenbots/castle/castle/internal/store/reap"
)

// leaseAndStart leases the agent's oldest eligible assignment and starts its
// run: the busy-lane sibling shape (a healthy run holding the serial lane).
func (f *neverStartedFixture) leaseAndStart(agent string, at time.Time) *store.WorkflowAssignment {
	f.t.Helper()
	a, _, err := f.s.LeaseWorkflowAssignment(f.ctx, agent, map[string]string{}, microUTC(at), 90*time.Second)
	if err != nil {
		f.t.Fatalf("lease assignment for %s: %v", agent, err)
	}
	run, err := f.s.GetRun(f.ctx, a.RunID)
	if err != nil {
		f.t.Fatalf("get leased run %s: %v", a.RunID, err)
	}
	startedAt := microUTC(at)
	run.Status = "running"
	run.StartedAt = &startedAt
	if err := f.s.UpdateRun(f.ctx, run); err != nil {
		f.t.Fatalf("start leased run %s: %v", a.RunID, err)
	}
	return a
}

// finishRun stamps a run terminal the way the agent's terminal event leg
// does, and takes the owning assignment off the lane with the matching
// terminal story.
func (f *neverStartedFixture) finishRun(runID, status, assignmentReason string, endedAt time.Time) {
	f.t.Helper()
	run, err := f.s.GetRun(f.ctx, runID)
	if err != nil {
		f.t.Fatalf("get run %s: %v", runID, err)
	}
	run.Status = status
	run.EndedAt = &endedAt
	if err := f.s.UpdateRun(f.ctx, run); err != nil {
		f.t.Fatalf("finish run %s: %v", runID, err)
	}
	if err := f.s.MarkWorkflowAssignmentTerminal(f.ctx, runID, assignmentReason); err != nil {
		f.t.Fatalf("mark assignment terminal for %s: %v", runID, err)
	}
}

// TestReapNeverStartedRuns_LaneBusyPause pins the KB-371 gate arms: a
// never-started run whose assignment owner is online with a fresh heartbeat
// and an actively executing sibling run pauses (no reap, no terminal
// assignment) — the queued-behind-a-busy-lane shape. Every degraded variant
// reaps by timer as before: a finished sibling (idle lane), or a stale
// heartbeat with the lane nominally busy (the CRI-142 heartbeat reaper frees
// it; the queued run must not outlive its dead agent).
func TestReapNeverStartedRuns_LaneBusyPause(t *testing.T) {
	runWithCtx(t, func(t *testing.T, ctx context.Context, s store.Store) {
		f := newNeverStartedFixture(t, ctx, s)

		// Arm A — the KB-371 incident shape: online owner, fresh heartbeat,
		// sibling run holding the serial lane; a second assignment queues
		// behind it.
		baFirst := newAssignment(t, ctx, s, "lane-busy-owner", "kb-371-busy-first", nil, f.createdBefore.Add(-time.Minute))
		f.reapFixture.overseer("lane-busy-owner", f.now)
		f.leaseAndStart("lane-busy-owner", f.now)
		baQueued := newAssignment(t, ctx, s, "lane-busy-owner", "kb-371-busy-queued", nil, f.createdBefore.Add(-30*time.Second))

		// Arm B — same shape, but the sibling finished: the lane idles and
		// timer semantics apply.
		iaFirst := newAssignment(t, ctx, s, "lane-idle-owner", "kb-371-idle-first", nil, f.createdBefore.Add(-time.Minute))
		f.reapFixture.overseer("lane-idle-owner", f.now)
		f.leaseAndStart("lane-idle-owner", f.now)
		iaQueued := newAssignment(t, ctx, s, "lane-idle-owner", "kb-371-idle-queued", nil, f.createdBefore.Add(-30*time.Second))
		f.finishRun(iaFirst.RunID, "succeeded", "run completed", microUTC(f.now))

		// Arm C — nominally busy lane, but the heartbeat stopped: the agent
		// is gone, so the pause must not hold.
		saFirst := newAssignment(t, ctx, s, "lane-stale-owner", "kb-371-stale-first", nil, f.createdBefore.Add(-time.Minute))
		f.reapFixture.overseer("lane-stale-owner", f.now.Add(-2*time.Minute))
		f.leaseAndStart("lane-stale-owner", f.now)
		saQueued := newAssignment(t, ctx, s, "lane-stale-owner", "kb-371-stale-queued", nil, f.createdBefore.Add(-30*time.Second))

		reaped := f.reapNeverStarted()
		if got := reapedIDs(reaped); len(got) != 2 || !slices.Contains(got, iaQueued.RunID) || !slices.Contains(got, saQueued.RunID) {
			t.Fatalf("reaped = %v, want %s and %s only (busy-lane %s paused)", got, iaQueued.RunID, saQueued.RunID, baQueued.RunID)
		}

		// The paused arm: the queued run stays pending and unstamped, and its
		// assignment keeps cycling through the lease rotation instead of
		// going terminal.
		if run := f.getRun(baQueued.RunID); run.Status != "pending" || run.FailureReason != "" || run.StartedAt != nil {
			t.Fatalf("busy-lane queued run = %+v, want pending unstarted without failure_reason", run)
		}
		a, err := s.GetWorkflowAssignment(ctx, baQueued.ID)
		if err != nil {
			t.Fatalf("get busy-lane assignment: %v", err)
		}
		if a.State != store.WorkflowAssignmentStateQueued || a.TerminalReason != "" {
			t.Fatalf("busy-lane assignment = state %q reason %q, want queued with no terminal_reason", a.State, a.TerminalReason)
		}

		// The reaped arms: terminal runs and assignments carry the rule-2
		// verdict.
		for _, qa := range []*store.WorkflowAssignment{iaQueued, saQueued} {
			if run := f.getRun(qa.RunID); run.Status != "failed" || run.FailureReason != reap.ReasonCreatedNeverStarted || run.StartedAt != nil {
				t.Errorf("reaped run %s = %+v, want failed/created_never_started", qa.RunID, run)
			}
			a, err := s.GetWorkflowAssignment(ctx, qa.ID)
			if err != nil {
				t.Fatalf("get assignment %s: %v", qa.ID, err)
			}
			if a.State != store.WorkflowAssignmentStateTerminal || a.TerminalReason != reap.ReasonCreatedNeverStarted {
				t.Errorf("reaped assignment %s = state %q reason %q, want terminal/created_never_started", qa.ID, a.State, a.TerminalReason)
			}
		}

		// The actively executing siblings were never candidates; the pass
		// left them untouched.
		for _, first := range []*store.WorkflowAssignment{baFirst, saFirst} {
			if run := f.getRun(first.RunID); run.Status != "running" {
				t.Errorf("executing sibling %s rewritten: %q", first.RunID, run.Status)
			}
		}
	})
}

// TestNeverStartedRunSurvivesBusyLaneThenLeasesAtSuccession is the KB-371
// regression: two assignments against one agent lane, the first run busy
// across four lease rotations (the incident saw three grants) and past the
// never-started window many times over. The queued assignment must survive
// every reaping cadence — no run stamp, no terminal assignment, no consumed
// budget — and then lease at the succession point, when the lane frees, and
// execute. Before the fix the first pass past the window stamped the run
// created_never_started mid-rotation.
func TestNeverStartedRunSurvivesBusyLaneThenLeasesAtSuccession(t *testing.T) {
	runWithCtx(t, func(t *testing.T, ctx context.Context, s store.Store) {
		f := newNeverStartedFixture(t, ctx, s)
		agent := "kb-371-lane"
		f.reapFixture.overseer(agent, f.now)

		// The first assignment leases and starts: it holds the serial lane
		// through the whole busy window.
		first := newAssignment(t, ctx, s, agent, "kb-371-lane-first", nil, f.createdBefore.Add(-time.Minute))
		f.leaseAndStart(agent, f.now)

		// The second assignment queues behind it while the lane is busy.
		queued := newAssignment(t, ctx, s, agent, "kb-371-lane-queued", nil, f.now)

		// Four lease rotations with the sibling running. Each cycle: the
		// agent heartbeats, the expired lease requeues the queued assignment
		// and the scan re-grants it (the designed rotation), then a reaper
		// pass runs mid-lease with the queued run already past its window.
		ttl := 90 * time.Second
		cadence := 2 * time.Minute
		for cycle := 1; cycle <= 4; cycle++ {
			at := microUTC(f.now.Add(time.Duration(cycle) * cadence))
			if err := s.UpdateOverseerSeen(ctx, agent, microUTC(at.Add(-time.Second))); err != nil {
				t.Fatalf("cycle %d: heartbeat agent: %v", cycle, err)
			}
			leased, _, err := s.LeaseWorkflowAssignment(ctx, agent, map[string]string{}, at, ttl)
			if err != nil {
				t.Fatalf("cycle %d: rotation lease: %v", cycle, err)
			}
			if leased.ID != queued.ID {
				t.Fatalf("cycle %d: rotation leased %s, want %s", cycle, leased.ID, queued.ID)
			}
			pass := microUTC(at.Add(45 * time.Second))
			reaped, err := s.ReapNeverStartedRuns(ctx, pass, pass.Add(-2*time.Minute), pass.Add(-60*time.Second))
			if err != nil {
				t.Fatalf("cycle %d: reap pass: %v", cycle, err)
			}
			if len(reaped) != 0 {
				t.Fatalf("cycle %d: reaped %v, want none while the lane is busy", cycle, reaped)
			}
			if run := f.getRun(queued.RunID); run.Status != "pending" || run.FailureReason != "" || run.StartedAt != nil {
				t.Fatalf("cycle %d: queued run = %+v, want pending unstarted without failure reason", cycle, run)
			}
			a, err := s.GetWorkflowAssignment(ctx, queued.ID)
			if err != nil {
				t.Fatalf("cycle %d: get assignment: %v", cycle, err)
			}
			if a.State != store.WorkflowAssignmentStateLeased || a.TerminalReason != "" {
				t.Fatalf("cycle %d: assignment state %q reason %q, want leased with no terminal story", cycle, a.State, a.TerminalReason)
			}
		}

		// The sibling finishes: the lane frees at doneAt.
		doneAt := microUTC(f.now.Add(9 * time.Minute))
		f.finishRun(first.RunID, "succeeded", "run completed", doneAt)

		// Succession: once the cycle-4 lease has rotated off (90s ttl, so at
		// now+10m the sweep requeues the assignment and the scan re-grants it
		// against the freed lane), the queued run finally executes.
		successionAt := microUTC(f.now.Add(10 * time.Minute))
		if err := s.UpdateOverseerSeen(ctx, agent, microUTC(successionAt.Add(-time.Second))); err != nil {
			t.Fatalf("succession: heartbeat agent: %v", err)
		}
		leased, _, err := s.LeaseWorkflowAssignment(ctx, agent, map[string]string{}, successionAt, ttl)
		if err != nil {
			t.Fatalf("succession lease: %v", err)
		}
		if leased.ID != queued.ID {
			t.Fatalf("succession leased %s, want %s", leased.ID, queued.ID)
		}
		if run := f.getRun(queued.RunID); run.OverseerID != agent {
			t.Fatalf("succession run owned by %q, want %q", run.OverseerID, agent)
		}

		// The executor picks it up: RunStarted lands, and the final pass
		// leaves the executing run alone. The assignment never carried a
		// terminal story and never burned a refire attempt.
		run := f.getRun(queued.RunID)
		run.Status = "running"
		run.StartedAt = &successionAt
		if err := s.UpdateRun(ctx, run); err != nil {
			t.Fatalf("start succession run %s: %v", queued.RunID, err)
		}
		finalPass := microUTC(successionAt.Add(time.Minute))
		reaped, err := s.ReapNeverStartedRuns(ctx, finalPass, finalPass.Add(-2*time.Minute), finalPass.Add(-60*time.Second))
		if err != nil {
			t.Fatalf("final reap pass: %v", err)
		}
		if len(reaped) != 0 {
			t.Fatalf("final pass reaped %v, want none", reaped)
		}
		a, err := s.GetWorkflowAssignment(ctx, queued.ID)
		if err != nil {
			t.Fatalf("final get assignment: %v", err)
		}
		if a.State != store.WorkflowAssignmentStateLeased || a.TerminalReason != "" {
			t.Fatalf("assignment after execution = state %q reason %q, want leased with no terminal story", a.State, a.TerminalReason)
		}
		if a.RefireAttempt != 1 {
			t.Fatalf("assignment refire attempt = %d, want 1 (budget never consumed)", a.RefireAttempt)
		}
		if run := f.getRun(queued.RunID); run.Status != "running" || run.StartedAt == nil {
			t.Fatalf("succession run = %+v, want running and started", run)
		}
	})
}
