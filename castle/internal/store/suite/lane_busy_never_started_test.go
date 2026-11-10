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

// newProjectWithMember seeds a project and an agent membership: the
// dispatch-eligibility key for project-scoped assignments.
func (f *neverStartedFixture) newProjectWithMember(projectID, memberID string, memberSeen time.Time) {
	f.t.Helper()
	p := &store.Project{
		ID: projectID, MembershipPolicy: store.ProjectMembershipClosed,
		CreatedAt: microUTC(memberSeen), UpdatedAt: microUTC(memberSeen),
	}
	if err := f.s.CreateProject(f.ctx, p); err != nil {
		f.t.Fatalf("create project %s: %v", projectID, err)
	}
	m := &store.ProjectMember{
		ProjectID: projectID, MemberID: memberID, Kind: store.ProjectMemberKindAI,
		CreatedAt: microUTC(memberSeen),
	}
	if err := f.s.AddProjectMember(f.ctx, m); err != nil {
		f.t.Fatalf("add member %s to %s: %v", memberID, projectID, err)
	}
}

// newBoardAssignment mints an assignment the way the Kanboard trigger fires
// them through SubmitProjectAssignment: the owner is the trigger identity
// (no overseer record — triggers cannot register as agents), the work is
// shape-validated input and target metadata, and project scoping carries the
// card's project. This is the KB-371 incident population.
func (f *neverStartedFixture) newBoardAssignment(owner, idem, projectID string, createdAt time.Time) *store.WorkflowAssignment {
	f.t.Helper()
	a, created, err := f.s.CreateWorkflowAssignment(f.ctx, &store.WorkflowAssignment{
		OwnerCriteriaID: owner,
		WorkflowName:    "wf",
		WorkflowSource:  "workflow main {}",
		IdempotencyKey:  idem,
		ProjectID:       projectID,
		InputValues:     map[string]string{"card": idem},
		TargetMetadata:  map[string]string{"lane": "operator-kanban-tickets"},
		CreatedAt:       createdAt,
		UpdatedAt:       createdAt,
	})
	if err != nil || !created {
		f.t.Fatalf("create board assignment %s: %v created=%v", idem, err, created)
	}
	return a
}

// newUnscopedAssignment mints a plain unscoped queued assignment.
func (f *neverStartedFixture) newUnscopedAssignment(owner, idem string, createdAt time.Time) *store.WorkflowAssignment {
	f.t.Helper()
	a, created, err := f.s.CreateWorkflowAssignment(f.ctx, &store.WorkflowAssignment{
		OwnerCriteriaID: owner,
		WorkflowName:    "wf",
		WorkflowSource:  "workflow main {}",
		IdempotencyKey:  idem,
		CreatedAt:       createdAt,
		UpdatedAt:       createdAt,
	})
	if err != nil || !created {
		f.t.Fatalf("create unscoped assignment %s: %v created=%v", idem, err, created)
	}
	return a
}

// TestReapNeverStartedRuns_DispatchEligibilityPause pins the KB-371 gate
// arms: a never-started run whose assignment is still dispatchable — a
// board-fired (trigger-owned) assignment a live project member could take,
// an unscoped one any live agent could take, or a leased one whose holder
// is alive — pauses. Every degraded variant reaps by timer as before: a
// lease holder whose heartbeat stopped, a project whose members are gone,
// a project with no members at all, an offline executor with a fresh
// last_seen, and terminal work. Under the pre-fix submitter-keyed gate every
// pause arm reaped, because trigger identities have no overseer row.
func TestReapNeverStartedRuns_DispatchEligibilityPause(t *testing.T) {
	runWithCtx(t, func(t *testing.T, ctx context.Context, s store.Store) {
		f := newNeverStartedFixture(t, ctx, s)
		old := f.createdBefore.Add(-time.Minute)

		// Arm A — the KB-371 incident shape: the board fires a project-scoped
		// assignment under a trigger identity while a run of the operator
		// agent holds the serial lane. The trigger has no overseer record; the
		// executing membership is what keeps the work alive.
		f.newProjectWithMember("proj-kb371", "agent-op", f.now)
		f.overseer("agent-op", f.now)
		first := f.newBoardAssignment("agent-op", "kb-371-busy-first", "proj-kb371", old)
		f.leaseAndStart("agent-op", f.now)
		pausedBoard := f.newBoardAssignment("trigger-board", "kb-371-busy-queued", "proj-kb371", f.createdBefore.Add(-30*time.Second))

		// Arm B — an unscoped assignment queues while any live agent exists:
		// dispatch to a live executor is still possible, so the work survives
		// the timer.
		pausedUnscoped := f.newUnscopedAssignment("trigger-unscoped", "kb-371-unscoped-queued", f.createdBefore.Add(-30*time.Second))

		// Arm C — a leased assignment whose holder is online with a fresh
		// heartbeat: claimed work waits for the executor to call RunStarted.
		f.newProjectWithMember("proj-holder", "agent-holder", f.now)
		f.overseer("agent-holder", f.now)
		claimed := f.newBoardAssignment("trigger-holder", "kb-371-holder", "proj-holder", old)
		if _, _, err := s.LeaseWorkflowAssignment(ctx, "agent-holder", map[string]string{}, microUTC(f.now.Add(-2*time.Hour)), 3*time.Hour); err != nil {
			t.Fatalf("lease claimed: %v", err)
		}

		// Arm D — leased but the holder's heartbeat stopped: the executor is
		// gone, so the claimed run dies by timer even though the lease has
		// not expired.
		f.overseer("agent-died", f.now.Add(-2*time.Minute))
		died := f.newUnscopedAssignment("trigger-died", "kb-371-died-holder", old)
		if _, _, err := s.LeaseWorkflowAssignment(ctx, "agent-died", map[string]string{}, microUTC(f.now.Add(-2*time.Hour)), 3*time.Hour); err != nil {
			t.Fatalf("lease died-holder: %v", err)
		}

		// Arm E — project-scoped, but the only member's heartbeat stopped:
		// nobody can dispatch the work, so it reaps.
		f.newProjectWithMember("proj-stale-member", "agent-e-stale", f.now.Add(-2*time.Minute))
		deadMembership := f.newBoardAssignment("trigger-stale-membership", "kb-371-dead-members", "proj-stale-member", old)

		// Arm F — project-scoped with no members at all: undispatchable, so
		// it reaps even though live agents exist elsewhere.
		p := &store.Project{
			ID: "proj-no-members", MembershipPolicy: store.ProjectMembershipClosed,
			CreatedAt: f.now, UpdatedAt: f.now,
		}
		if err := s.CreateProject(ctx, p); err != nil {
			t.Fatalf("create proj-no-members: %v", err)
		}
		undispatchable := f.newBoardAssignment("trigger-nomembers", "kb-371-nomembers", "proj-no-members", old)

		// Arm G — the only member is a live-heartbeat overseer whose status
		// is offline: an offline executor cannot dispatch, so it reaps.
		f.newProjectWithMember("proj-offline", "agent-offline", f.now)
		if err := s.UpdateOverseerStatus(ctx, "agent-offline", "offline"); err != nil {
			t.Fatalf("mark agent-offline offline: %v", err)
		}
		offlineExecutor := f.newBoardAssignment("trigger-offline", "kb-371-offline", "proj-offline", old)

		// The incident shape precondition: the board-fired owner really has
		// no overseer record.
		if _, err := s.GetOverseer(ctx, "trigger-board"); err == nil {
			t.Fatal("owner trigger-board unexpectedly has an overseer record")
		}

		reaped := f.reapNeverStarted()
		want := []string{died.RunID, undispatchable.RunID, offlineExecutor.RunID, deadMembership.RunID}
		got := reapedIDs(reaped)
		if len(got) != len(want) || !slices.Contains(got, died.RunID) ||
			!slices.Contains(got, undispatchable.RunID) || !slices.Contains(got, offlineExecutor.RunID) ||
			!slices.Contains(got, deadMembership.RunID) {
			t.Fatalf("reaped = %v, want %v (board %s, unscoped %s paused; claimed holder fresh)",
				got, want, pausedBoard.RunID, pausedUnscoped.RunID)
		}

		// The paused arms: runs stay pending and unstamped, assignments keep
		// cycling through dispatch instead of going terminal. The claimed
		// assignment holds its live lease; the two queued ones stay queued.
		if run := f.getRun(pausedBoard.RunID); run.Status != "pending" || run.FailureReason != "" || run.StartedAt != nil {
			t.Fatalf("board-queued run = %+v, want pending unstarted without failure_reason", run)
		}
		for _, pa := range []struct {
			a         *store.WorkflowAssignment
			wantState string
		}{
			{pausedBoard, store.WorkflowAssignmentStateQueued},
			{pausedUnscoped, store.WorkflowAssignmentStateQueued},
			{claimed, store.WorkflowAssignmentStateLeased},
		} {
			a, err := s.GetWorkflowAssignment(ctx, pa.a.ID)
			if err != nil {
				t.Fatalf("get paused assignment %s: %v", pa.a.ID, err)
			}
			if a.State != pa.wantState || a.TerminalReason != "" {
				t.Fatalf("paused assignment %s = state %q reason %q, want %s with no terminal_reason",
					pa.a.ID, a.State, a.TerminalReason, pa.wantState)
			}
		}

		// The reaped arms: terminal runs and assignments carry the rule-2
		// verdict.
		for _, qa := range []*store.WorkflowAssignment{died, undispatchable, offlineExecutor, deadMembership} {
			if run := f.getRun(qa.RunID); run.Status != "failed" || run.FailureReason != reap.ReasonCreatedNeverStarted || run.StartedAt != nil {
				t.Errorf("reaped run %s = %+v, want failed/created_never_started", qa.RunID, run)
			}
			a, err := s.GetWorkflowAssignment(ctx, qa.ID)
			if err != nil {
				t.Fatalf("get reaped assignment %s: %v", qa.ID, err)
			}
			if a.State != store.WorkflowAssignmentStateTerminal || a.TerminalReason != reap.ReasonCreatedNeverStarted {
				t.Errorf("reaped assignment %s = state %q reason %q, want terminal/created_never_started", qa.ID, a.State, a.TerminalReason)
			}
		}

		// Containment: the actively executing sibling was a started run, so
		// the pass never touched it.
		if run := f.getRun(first.RunID); run.Status != "running" || run.StartedAt == nil {
			t.Errorf("executing sibling %s rewritten: %q", first.RunID, run.Status)
		}
	})
}

// TestNeverStartedRunSurvivesBusyLaneThenLeasesAtSuccession is the KB-371
// regression: two assignments against one project lane, the first run busy
// across four lease rotations and past the never-started window many times
// over. The queued assignment must survive every reaping cadence — including
// the passes after the first run finishes, while Castle's lease-expiry
// dispatch has not yet re-granted the lane (re-dispatch is timer-driven:
// ExpireLeasesNow ticks at half the lease duration while the never-started
// reaper ticks every 15 s, leaving a genuine succession gap) — and then
// lease at the succession point and execute. Before the fix the first pass
// past the window stamped the run created_never_started mid-rotation, and
// the first pass after the sibling finished stamped it in the gap.
func TestNeverStartedRunSurvivesBusyLaneThenLeasesAtSuccession(t *testing.T) {
	runWithCtx(t, func(t *testing.T, ctx context.Context, s store.Store) {
		f := newNeverStartedFixture(t, ctx, s)
		agent := "kb-371-op"
		f.newProjectWithMember("proj-succession", agent, f.now)
		f.overseer(agent, f.now)

		// The first assignment leases and starts: it holds the serial lane
		// through the whole busy window.
		first := f.newBoardAssignment(agent, "kb-371-succession-first", "proj-succession", f.createdBefore.Add(-time.Minute))
		f.leaseAndStart(agent, f.now)

		// The second assignment queues behind it while the lane is busy.
		queued := f.newBoardAssignment("trigger-succession", "kb-371-succession-queued", "proj-succession", f.now)

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

		// The sibling finishes: the assignment goes terminal and the lane
		// frees at doneAt — but the queued assignment is NOT re-granted here.
		// Dispatch is timer-driven, so the queued run now sits in the
		// succession gap: past its never-started window, lane free, run not
		// started yet, lease-expiry dispatch still minutes away. The owner
		// agent stays alive and keeps heartbeating the whole time — that is
		// the healthy-succession shape.
		doneAt := microUTC(f.now.Add(9 * time.Minute))
		f.finishRun(first.RunID, "succeeded", "run completed", doneAt)
		if err := s.UpdateOverseerSeen(ctx, agent, doneAt); err != nil {
			t.Fatalf("gap: heartbeat agent: %v", err)
		}

		// Reaper passes at the production tick cadence (15 s) across the gap
		// assert zero reaps. First while the cycle-4 lease is still recorded:
		// even past the lease's wall-clock expiry the assignment is leased
		// state with a live holder, so the leased arm holds the pause until
		// the expiry sweep requeues it.
		for _, offset := range []time.Duration{15 * time.Second, 30 * time.Second} {
			pass := microUTC(doneAt.Add(offset))
			if err := s.UpdateOverseerSeen(ctx, agent, microUTC(pass.Add(-time.Second))); err != nil {
				t.Fatalf("gap pass %v: heartbeat agent: %v", offset, err)
			}
			reaped, err := s.ReapNeverStartedRuns(ctx, pass, pass.Add(-2*time.Minute), pass.Add(-60*time.Second))
			if err != nil {
				t.Fatalf("gap pass %v: reap: %v", offset, err)
			}
			if len(reaped) != 0 {
				t.Fatalf("gap pass %v reaped %v, want none inside the succession window", offset, reaped)
			}
			if run := f.getRun(queued.RunID); run.Status != "pending" || run.FailureReason != "" || run.StartedAt != nil {
				t.Fatalf("gap pass %v: queued run = %+v, want pending unstarted without failure reason", offset, run)
			}
		}

		// The lease-expiry sweep (part of the designed rotation): the
		// cycle-4 lease expired mid-gap, so the assignment returns to queued.
		swept, err := s.ExpireWorkflowAssignmentLeases(ctx, microUTC(doneAt.Add(35*time.Second)))
		if err != nil {
			t.Fatalf("gap: expire leases: %v", err)
		}
		if !slices.Contains(swept, queued.ID) {
			t.Fatalf("gap: sweep = %v, want %s requeued", swept, queued.ID)
		}
		a, err := s.GetWorkflowAssignment(ctx, queued.ID)
		if err != nil {
			t.Fatalf("gap: get assignment: %v", err)
		}
		if a.State != store.WorkflowAssignmentStateQueued || a.TerminalReason != "" {
			t.Fatalf("gap: assignment after sweep = state %q reason %q, want queued with no terminal story", a.State, a.TerminalReason)
		}

		// Continuation of the gap, now under the queued arm: the assignment
		// is dispatch-eligible (live project member) while the timer-driven
		// dispatch has not yet fired.
		for _, offset := range []time.Duration{45 * time.Second, 50 * time.Second} {
			pass := microUTC(doneAt.Add(offset))
			if err := s.UpdateOverseerSeen(ctx, agent, microUTC(pass.Add(-time.Second))); err != nil {
				t.Fatalf("gap pass %v: heartbeat agent: %v", offset, err)
			}
			reaped, err := s.ReapNeverStartedRuns(ctx, pass, pass.Add(-2*time.Minute), pass.Add(-60*time.Second))
			if err != nil {
				t.Fatalf("gap pass %v: reap: %v", offset, err)
			}
			if len(reaped) != 0 {
				t.Fatalf("gap pass %v reaped %v, want none inside the succession window", offset, reaped)
			}
			if run := f.getRun(queued.RunID); run.Status != "pending" || run.FailureReason != "" || run.StartedAt != nil {
				t.Fatalf("gap pass %v: queued run = %+v, want pending unstarted without failure reason", offset, run)
			}
		}

		// Succession: the lane is free, the expired lease has been swept, and
		// the dispatch grants the queued assignment to the same agent.
		successionAt := microUTC(doneAt.Add(time.Minute))
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
		finalReaped, err := s.ReapNeverStartedRuns(ctx, finalPass, finalPass.Add(-2*time.Minute), finalPass.Add(-60*time.Second))
		if err != nil {
			t.Fatalf("final reap pass: %v", err)
		}
		if len(finalReaped) != 0 {
			t.Fatalf("final pass reaped %v, want none", finalReaped)
		}
		a, err = s.GetWorkflowAssignment(ctx, queued.ID)
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
