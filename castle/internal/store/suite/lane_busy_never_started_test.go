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

// newLabeledAssignment mints an unscoped queued assignment carrying required
// labels: the lease scan only grants it to executors whose labels satisfy
// the set, and the reaper's queued arm must mirror the same rule.
func (f *neverStartedFixture) newLabeledAssignment(owner, idem string, labels map[string]string, createdAt time.Time) *store.WorkflowAssignment {
	f.t.Helper()
	a, created, err := f.s.CreateWorkflowAssignment(f.ctx, &store.WorkflowAssignment{
		OwnerCriteriaID: owner,
		WorkflowName:    "wf",
		WorkflowSource:  "workflow main {}",
		IdempotencyKey:  idem,
		Labels:          labels,
		CreatedAt:       createdAt,
		UpdatedAt:       createdAt,
	})
	if err != nil || !created {
		f.t.Fatalf("create labeled assignment %s: %v created=%v", idem, err, created)
	}
	return a
}

// newProjectLabeledAssignment mints a project-scoped queued assignment
// carrying required labels: the lease scan can only grant it to the
// project's member executors whose labels satisfy the set.
func (f *neverStartedFixture) newProjectLabeledAssignment(projectID, owner, idem string, labels map[string]string, createdAt time.Time) *store.WorkflowAssignment {
	f.t.Helper()
	a, created, err := f.s.CreateWorkflowAssignment(f.ctx, &store.WorkflowAssignment{
		OwnerCriteriaID: owner,
		WorkflowName:    "wf",
		WorkflowSource:  "workflow main {}",
		IdempotencyKey:  idem,
		ProjectID:       projectID,
		Labels:          labels,
		CreatedAt:       createdAt,
		UpdatedAt:       createdAt,
	})
	if err != nil || !created {
		f.t.Fatalf("create project labeled assignment %s: %v created=%v", idem, err, created)
	}
	return a
}

// leaseOnly leases the agent's oldest eligible assignment WITHOUT starting
// its run: the claimed-but-not-started shape the serial-lane gate leaves
// holding the lane, and the held-unstarted-lease lane evidence for queued
// work behind the same agent.
func (f *neverStartedFixture) leaseOnly(agent string, at time.Time, ttl time.Duration) *store.WorkflowAssignment {
	f.t.Helper()
	a, _, err := f.s.LeaseWorkflowAssignment(f.ctx, agent, map[string]string{}, microUTC(at), ttl)
	if err != nil {
		f.t.Fatalf("lease (unstarted) for %s: %v", agent, err)
	}
	return a
}

// overseerLabels registers an online fresh overseer with an explicit label
// set — the executor side of the lease scan's label eligibility, mirrored in
// the reaper's queued arm.
func (f *neverStartedFixture) overseerLabels(id string, seen time.Time, labels map[string]string) {
	f.t.Helper()
	newOverseer(f.t, f.s, id, microUTC(seen), labels)
}

// TestReapNeverStartedRuns_DispatchEligibilityPause pins the KB-371 gate
// arms: the queue pause needs BOTH a live dispatch-eligible executor AND
// lane-occupancy evidence for that same executor. Pausing shapes — the
// incident board assignment behind a running sibling, unscoped work behind a
// busy unscoped agent, a queued assignment whose serial-gate rotation has
// the agent holding an unstarted lease, and a claimed lease whose holder is
// alive — survive the timer. Every arm without lane evidence reaps by
// timer now, including the idle-lane queued case: with no lease progress
// and no running sibling, rule 2 (KB-233 — a queued assignment nobody ever
// leased) must reach the created_never_started verdict.
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

		// Arm B — unscoped work behind a busy agent: the idle executor
		// population alone no longer holds the pause, so the arm carries
		// explicit lane evidence — an unscoped sibling the agent is running.
		f.overseer("agent-lane", f.now)
		f.newUnscopedAssignment("agent-lane", "kb-371-unscoped-first", old)
		f.leaseAndStart("agent-lane", f.now)
		pausedUnscoped := f.newUnscopedAssignment("trigger-unscoped", "kb-371-unscoped-queued", f.createdBefore.Add(-30*time.Second))

		// Arm C — serial-gate rotation: the member holds an unstarted lease on
		// another assignment while the candidate stays queued. The dispatch
		// gate hands the lane over on the designed succession, so rule 2 must
		// not kill the queued work mid-rotation.
		f.newProjectWithMember("proj-rotation", "agent-claimer", f.now)
		f.overseer("agent-claimer", f.now)
		f.newBoardAssignment("agent-claimer", "kb-371-rot-first", "proj-rotation", old)
		f.leaseOnly("agent-claimer", f.now, 3*time.Hour)
		rotNext := f.newBoardAssignment("trigger-rotation", "kb-371-rot-queued", "proj-rotation", f.createdBefore.Add(-30*time.Second))

		// Arm D — a leased assignment whose holder is online with a fresh
		// heartbeat: claimed work waits for the executor to call RunStarted.
		f.newProjectWithMember("proj-holder", "agent-holder", f.now)
		f.overseer("agent-holder", f.now)
		claimed := f.newBoardAssignment("trigger-holder", "kb-371-holder", "proj-holder", old.Add(-time.Minute))
		f.leaseOnly("agent-holder", f.now, 3*time.Hour)

		// Arm E — leased but the holder's heartbeat stopped: the executor is
		// gone, so the claimed run dies by timer even though the lease has
		// not expired.
		f.overseer("agent-died", f.now.Add(-2*time.Minute))
		died := f.newUnscopedAssignment("trigger-died", "kb-371-died-holder", old)
		f.leaseOnly("agent-died", f.now, 3*time.Hour)

		// Arm F — the idle-lane queued case: the only member is online with a
		// fresh heartbeat, but nobody ever leased the work and no sibling is
		// progressing. Rule 2 reaps it — this is the arm that kept the
		// original implementation from ever reaching a verdict.
		f.newProjectWithMember("proj-idle", "agent-idle", f.now)
		f.overseer("agent-idle", f.now)
		quiet := f.newBoardAssignment("trigger-idle", "kb-371-idle-queued", "proj-idle", old)

		// Arm G — project-scoped, but the only member's heartbeat stopped:
		// nobody can dispatch the work, so it reaps.
		f.newProjectWithMember("proj-stale-member", "agent-e-stale", f.now.Add(-2*time.Minute))
		deadMembership := f.newBoardAssignment("trigger-stale-membership", "kb-371-dead-members", "proj-stale-member", old)

		// Arm H — project-scoped with no members at all: undispatchable, so
		// it reaps even though live agents exist elsewhere.
		p := &store.Project{
			ID: "proj-no-members", MembershipPolicy: store.ProjectMembershipClosed,
			CreatedAt: f.now, UpdatedAt: f.now,
		}
		if err := s.CreateProject(ctx, p); err != nil {
			t.Fatalf("create proj-no-members: %v", err)
		}
		undispatchable := f.newBoardAssignment("trigger-nomembers", "kb-371-nomembers", "proj-no-members", old)

		// Arm I — the only member is a live-heartbeat overseer whose status
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
		want := []string{died.RunID, quiet.RunID, undispatchable.RunID, offlineExecutor.RunID, deadMembership.RunID}
		got := reapedIDs(reaped)
		if len(got) != len(want) || !slices.Contains(got, died.RunID) ||
			!slices.Contains(got, quiet.RunID) || !slices.Contains(got, offlineExecutor.RunID) ||
			!slices.Contains(got, deadMembership.RunID) || !slices.Contains(got, undispatchable.RunID) {
			t.Fatalf("reaped = %v, want %v (board %s, unscoped %s, rotation %s paused; claimed holder fresh)",
				got, want, pausedBoard.RunID, pausedUnscoped.RunID, rotNext.RunID)
		}

		// The paused arms: runs stay pending and unstamped, assignments keep
		// cycling through dispatch instead of going terminal. The claimed
		// assignment holds its live lease; the queued ones stay queued.
		if run := f.getRun(pausedBoard.RunID); run.Status != "pending" || run.FailureReason != "" || run.StartedAt != nil {
			t.Fatalf("board-queued run = %+v, want pending unstarted without failure_reason", run)
		}
		for _, pa := range []struct {
			a         *store.WorkflowAssignment
			wantState string
		}{
			{pausedBoard, store.WorkflowAssignmentStateQueued},
			{pausedUnscoped, store.WorkflowAssignmentStateQueued},
			{rotNext, store.WorkflowAssignmentStateQueued},
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
		for _, qa := range []*store.WorkflowAssignment{died, quiet, undispatchable, offlineExecutor, deadMembership} {
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

// TestReapNeverStartedRuns_LabelEligibilityMirror pins the lease-scan label
// mirror in the queued arm: required labels are matched key-by-key against
// the executor's labels exactly as labelsSatisfy does — a missing key reads
// as the empty string, so a required label with an empty value is satisfied
// by any executor that carries no value for that key — so required labels
// that no live executor carries make the work undispatchable and rule 2
// reaps it even with lanes busy, while work whose labels the busy executor
// does carry waits for the lane. JSON keys containing '.' and '"' must
// resolve exactly, not as path segments or pattern characters.
func TestReapNeverStartedRuns_LabelEligibilityMirror(t *testing.T) {
	runWithCtx(t, func(t *testing.T, ctx context.Context, s store.Store) {
		f := newNeverStartedFixture(t, ctx, s)
		old := f.createdBefore.Add(-time.Minute)

		f.overseerLabels("agent-gpu", f.now, map[string]string{"gpu": "true", `weird.key"q`: "v"})

		// The busy lane: the GPU agent runs an unscoped sibling.
		gpuFirst := f.newUnscopedAssignment("agent-gpu", "kb-371-gpu-first", old)
		f.leaseAndStart("agent-gpu", f.now)

		// The empty-value arms: labelsSatisfy reads a missing key as the
		// empty string, so a required {"k": ""} is satisfied by an executor
		// that carries no value for k at all (agent-nok) and is NOT
		// satisfied by an executor carrying a non-empty value (agent-kx).
		// Each project's membership isolates its candidate to exactly one
		// executor, and each executor holds a busy lane.
		f.newProjectWithMember("proj-mirror-empty", "agent-nok", f.now)
		f.overseerLabels("agent-nok", f.now, map[string]string{"tier": "free"})
		nokFirst := f.newUnscopedAssignment("agent-nok", "kb-371-nok-first", old)
		f.leaseAndStart("agent-nok", f.now)

		f.newProjectWithMember("proj-mirror-filled", "agent-kx", f.now)
		f.overseerLabels("agent-kx", f.now, map[string]string{"gpu": "true", "k": "x"})
		kxFirst := f.newUnscopedAssignment("agent-kx", "kb-371-kx-first", old)
		f.leaseAndStart("agent-kx", f.now)

		// Pauses: every required label is present on the busy executor with
		// the matching value, including the JSON-special key resolved
		// exactly.
		mirror := f.newLabeledAssignment("trigger-gpu-mirror", "kb-371-label-mirror", map[string]string{"gpu": "true"}, f.createdBefore.Add(-30*time.Second))
		weirdMirror := f.newLabeledAssignment("trigger-gpu-weird", "kb-371-label-weird", map[string]string{`weird.key"q`: "v"}, f.createdBefore.Add(-30*time.Second))

		// Reaps: the required label is absent on the busy executor (a
		// different accelerator nobody carries) or the key exists with a
		// mismatched value — undispatchable work must reach the verdict.
		tpuRequired := f.newLabeledAssignment("trigger-tpu", "kb-371-label-tpu", map[string]string{"tpu": "true"}, f.createdBefore.Add(-30*time.Second))
		valueMismatch := f.newLabeledAssignment("trigger-value", "kb-371-label-value", map[string]string{`weird.key"q`: "wrong-value"}, f.createdBefore.Add(-30*time.Second))

		// Empty-value pause: the busy executor (agent-nok) does not carry k
		// at all, which labelsSatisfy reads as satisfying {"k": ""} — the
		// work must wait for the lane, not be reaped.
		emptyRequired := f.newProjectLabeledAssignment("proj-mirror-empty", "trigger-empty", "kb-371-label-empty", map[string]string{"k": ""}, f.createdBefore.Add(-30*time.Second))
		// Empty-value reap: the only eligible executor for the project
		// carries k with a non-empty value, so {"k": ""} is unsatisfiable
		// there — undispatchable work must reach the verdict even though
		// the lane is busy.
		filledRequired := f.newProjectLabeledAssignment("proj-mirror-filled", "trigger-filled", "kb-371-label-filled", map[string]string{"k": ""}, f.createdBefore.Add(-30*time.Second))

		reaped := f.reapNeverStarted()
		got := reapedIDs(reaped)
		if len(got) != 3 || !slices.Contains(got, tpuRequired.RunID) || !slices.Contains(got, valueMismatch.RunID) || !slices.Contains(got, filledRequired.RunID) {
			t.Fatalf("reaped = %v, want [%s %s %s]", got, tpuRequired.RunID, valueMismatch.RunID, filledRequired.RunID)
		}
		if run := f.getRun(mirror.RunID); run.Status != "pending" || run.FailureReason != "" || run.StartedAt != nil {
			t.Fatalf("label-mirror run = %+v, want pending unstarted without failure reason", run)
		}
		if run := f.getRun(emptyRequired.RunID); run.Status != "pending" || run.FailureReason != "" || run.StartedAt != nil {
			t.Fatalf("empty-required run = %+v, want pending unstarted without failure reason", run)
		}
		for _, qa := range []*store.WorkflowAssignment{tpuRequired, valueMismatch, filledRequired} {
			if a, err := s.GetWorkflowAssignment(ctx, qa.ID); err != nil {
				t.Fatalf("get reaped assignment %s: %v", qa.ID, err)
			} else if a.State != store.WorkflowAssignmentStateTerminal || a.TerminalReason != reap.ReasonCreatedNeverStarted {
				t.Fatalf("reaped assignment %s = state %q reason %q, want terminal/created_never_started", qa.ID, a.State, a.TerminalReason)
			}
		}
		for _, pa := range []*store.WorkflowAssignment{mirror, weirdMirror, emptyRequired} {
			a, err := s.GetWorkflowAssignment(ctx, pa.ID)
			if err != nil {
				t.Fatalf("get mirror assignment %s: %v", pa.ID, err)
			}
			if a.State != store.WorkflowAssignmentStateQueued || a.TerminalReason != "" {
				t.Fatalf("mirror assignment %s = state %q reason %q, want queued with no terminal story", pa.ID, a.State, a.TerminalReason)
			}
		}
		for _, sb := range []*store.WorkflowAssignment{gpuFirst, nokFirst, kxFirst} {
			sbRun := f.getRun(sb.RunID)
			if sbRun.Status != "running" || sbRun.StartedAt == nil {
				t.Fatalf("busy sibling %s = %+v, want running untouched", sb.RunID, sbRun)
			}
		}
	})
}

// TestReapNeverStartedRuns_IdleLaneQueuedReaps pins the idle-lane queued
// case on both scoping arms: an online, fresh executor exists (even several)
// but holds no lease, runs no sibling, and carries no recently vacated lane
// — so the assignment nobody ever leased reaches the created_never_started
// verdict by timer, on a project-scoped assignment and an unscoped one.
func TestReapNeverStartedRuns_IdleLaneQueuedReaps(t *testing.T) {
	runWithCtx(t, func(t *testing.T, ctx context.Context, s store.Store) {
		f := newNeverStartedFixture(t, ctx, s)

		f.newProjectWithMember("proj-idle-standalone", "agent-idle-standalone", f.now)
		f.overseer("agent-idle-standalone", f.now)
		scopedIdle := f.newBoardAssignment("trigger-idle-scoped", "kb-371-idle-scoped", "proj-idle-standalone", f.createdBefore.Add(-time.Minute))
		unscopedIdle := f.newUnscopedAssignment("trigger-idle-unscoped", "kb-371-idle-unscoped", f.createdBefore.Add(-time.Minute))

		reaped := f.reapNeverStarted()
		got := reapedIDs(reaped)
		if len(got) != 2 || !slices.Contains(got, scopedIdle.RunID) || !slices.Contains(got, unscopedIdle.RunID) {
			t.Fatalf("reaped = %v, want [%s %s]", got, scopedIdle.RunID, unscopedIdle.RunID)
		}
		for _, qa := range []*store.WorkflowAssignment{scopedIdle, unscopedIdle} {
			if run := f.getRun(qa.RunID); run.Status != "failed" || run.FailureReason != reap.ReasonCreatedNeverStarted || run.StartedAt != nil {
				t.Errorf("idle reaped run %s = %+v, want failed/created_never_started", qa.RunID, run)
			}
			if a, err := s.GetWorkflowAssignment(f.ctx, qa.ID); err != nil {
				t.Fatalf("get idle reaped assignment %s: %v", qa.ID, err)
			} else if a.State != store.WorkflowAssignmentStateTerminal || a.TerminalReason != reap.ReasonCreatedNeverStarted {
				t.Errorf("idle reaped assignment %s = state %q reason %q, want terminal/created_never_started", qa.ID, a.State, a.TerminalReason)
			}
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
			reaped, err := s.ReapNeverStartedRuns(ctx, pass, pass.Add(-2*time.Minute), pass.Add(-60*time.Second), pass.Add(-5*time.Minute))
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
		// assert zero reaps. The first two passes run while the assignment is
		// still in leased state with a live holder, so the leased arm holds
		// the pause. At the +30 s pass the cycle-4 lease expiry (+9m30s)
		// equals the pass instant exactly and the expiry sweep uses a strict
		// `<`, so the assignment is still leased there too; the queued-arm
		// gap coverage only begins after the sweep at +35 s requeues the
		// assignment, at the +45 s passes below.
		for _, offset := range []time.Duration{15 * time.Second, 30 * time.Second} {
			pass := microUTC(doneAt.Add(offset))
			if err := s.UpdateOverseerSeen(ctx, agent, microUTC(pass.Add(-time.Second))); err != nil {
				t.Fatalf("gap pass %v: heartbeat agent: %v", offset, err)
			}
			reaped, err := s.ReapNeverStartedRuns(ctx, pass, pass.Add(-2*time.Minute), pass.Add(-60*time.Second), pass.Add(-5*time.Minute))
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
		// is dispatch-eligible (live project member) and the lane-anchor
		// evidence (the sibling's ended_at and the cycle-4 lease expiry) is
		// younger than the succession grace, while the timer-driven dispatch
		// has not yet fired.
		for _, offset := range []time.Duration{45 * time.Second, 50 * time.Second} {
			pass := microUTC(doneAt.Add(offset))
			if err := s.UpdateOverseerSeen(ctx, agent, microUTC(pass.Add(-time.Second))); err != nil {
				t.Fatalf("gap pass %v: heartbeat agent: %v", offset, err)
			}
			reaped, err := s.ReapNeverStartedRuns(ctx, pass, pass.Add(-2*time.Minute), pass.Add(-60*time.Second), pass.Add(-5*time.Minute))
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
		finalReaped, err := s.ReapNeverStartedRuns(ctx, finalPass, finalPass.Add(-2*time.Minute), finalPass.Add(-60*time.Second), finalPass.Add(-5*time.Minute))
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

// TestReapNeverStartedRuns_GraceReleaseStaleLane pins the RELEASE side of
// the lane-occupancy grace: the anchors that pause a queued assignment bind
// only while their evidence is fresh relative to the grace horizon. An
// anchor that predates graceBefore stops pausing — a terminal sibling that
// ended before the grace and a lease row that expired before the grace are
// dead lane traces, and work behind them reaps — while a single live anchor
// (a lease row that has not yet expired past the grace) keeps holding the
// work. Under the pure-timer semantics reapNeverStarted's default grace
// provides, every anchor here would count as lane evidence forever, so this
// test is what fails if the grace parameters are mis-wired, ignored, or
// treated as always-true.
func TestReapNeverStartedRuns_GraceReleaseStaleLane(t *testing.T) {
	runWithCtx(t, func(t *testing.T, ctx context.Context, s store.Store) {
		f := newNeverStartedFixture(t, ctx, s)
		graceBefore := f.now.Add(-5 * time.Minute)

		// Arm A — stale terminal sibling: the executor's only lane trace is
		// a sibling whose lease was granted at a past time with a short TTL
		// (so its lease row also expired before the grace) and whose run
		// finished terminal with ended_at before the grace. The successor
		// queued behind that dead trace must be reaped.
		f.newProjectWithMember("proj-grace-a", "agent-grace-a", f.now)
		f.overseer("agent-grace-a", f.now)
		siblingA := f.newBoardAssignment("trigger-grace-a", "kb-371-grace-a-first", "proj-grace-a", f.now.Add(-40*time.Minute))
		leasedA := f.leaseOnly("agent-grace-a", f.now.Add(-31*time.Minute), 15*time.Minute)
		if leasedA.ID != siblingA.ID || leasedA.RunID != siblingA.RunID {
			t.Fatalf("lease grabbed %s, want sibling %s", leasedA.ID, siblingA.ID)
		}
		f.finishRun(siblingA.RunID, "succeeded", "run completed", f.now.Add(-30*time.Minute))
		staleA := f.newBoardAssignment("trigger-grace-a", "kb-371-grace-a-queued", "proj-grace-a", f.createdBefore.Add(-30*time.Second))

		// Arm B — stale lease anchor only: no sibling runs at all; the
		// executor's only lane trace is a lease row that expired before the
		// grace, planted raw so no assignment sits in state leased. The
		// queued successor behind that expired trace must be reaped too.
		f.newProjectWithMember("proj-grace-b", "agent-grace-b", f.now)
		f.overseer("agent-grace-b", f.now)
		auxB := f.newBoardAssignment("trigger-grace-b-aux", "kb-371-grace-b-aux", "proj-grace-b", f.now)
		if err := s.RecordWorkflowAssignmentLease(ctx, &store.WorkflowAssignmentLease{
			AssignmentID: auxB.ID,
			CriteriaID:   "agent-grace-b",
			CreatedAt:    microUTC(f.now.Add(-31 * time.Minute)),
			ExpiresAt:    microUTC(f.now.Add(-16 * time.Minute)),
		}); err != nil {
			t.Fatalf("record stale lease: %v", err)
		}
		staleB := f.newBoardAssignment("trigger-grace-b", "kb-371-grace-b-queued", "proj-grace-b", f.createdBefore.Add(-30*time.Second))

		// Arm C — fresh-anchor control: the executor's only lane trace is a
		// lease row that has NOT expired past the grace. The pause must
		// hold: without this arm, an implementation whose every anchor went
		// stale would satisfy arms A and B.
		f.newProjectWithMember("proj-grace-c", "agent-grace-c", f.now)
		f.overseer("agent-grace-c", f.now)
		auxC := f.newBoardAssignment("trigger-grace-c-aux", "kb-371-grace-c-aux", "proj-grace-c", f.now)
		if err := s.RecordWorkflowAssignmentLease(ctx, &store.WorkflowAssignmentLease{
			AssignmentID: auxC.ID,
			CriteriaID:   "agent-grace-c",
			CreatedAt:    microUTC(f.now.Add(-31 * time.Minute)),
			ExpiresAt:    microUTC(f.now.Add(time.Hour)),
		}); err != nil {
			t.Fatalf("record fresh lease: %v", err)
		}
		heldC := f.newBoardAssignment("trigger-grace-c", "kb-371-grace-c-queued", "proj-grace-c", f.createdBefore.Add(-30*time.Second))

		reaped := f.reapNeverStartedGraced(graceBefore)
		got := reapedIDs(reaped)
		if len(got) != 2 || !slices.Contains(got, staleA.RunID) || !slices.Contains(got, staleB.RunID) {
			t.Fatalf("reaped = %v, want [%s %s]", got, staleA.RunID, staleB.RunID)
		}

		for _, qa := range []*store.WorkflowAssignment{staleA, staleB} {
			run := f.getRun(qa.RunID)
			if run.Status != "failed" || run.FailureReason != reap.ReasonCreatedNeverStarted || run.StartedAt != nil {
				t.Errorf("grace-released run %s = %+v, want failed/created_never_started unstarted", qa.RunID, run)
			}
			a, err := s.GetWorkflowAssignment(ctx, qa.ID)
			if err != nil {
				t.Fatalf("get grace-released assignment %s: %v", qa.ID, err)
			}
			if a.State != store.WorkflowAssignmentStateTerminal || a.TerminalReason != reap.ReasonCreatedNeverStarted {
				t.Errorf("grace-released assignment %s = state %q reason %q, want terminal/created_never_started", qa.ID, a.State, a.TerminalReason)
			}
		}

		if run := f.getRun(heldC.RunID); run.Status != "pending" || run.FailureReason != "" || run.StartedAt != nil {
			t.Errorf("fresh-anchor run %s = %+v, want pending unstarted without failure reason", heldC.RunID, run)
		}
		c, err := s.GetWorkflowAssignment(ctx, heldC.ID)
		if err != nil {
			t.Fatalf("get held assignment: %v", err)
		}
		if c.State != store.WorkflowAssignmentStateQueued || c.TerminalReason != "" {
			t.Errorf("held assignment %s = state %q reason %q, want queued with no terminal story", heldC.ID, c.State, c.TerminalReason)
		}

		// Containment: the terminal sibling stayed terminal, and both lease
		// auxiliaries (created inside the reap window) were never touched.
		sibRun := f.getRun(siblingA.RunID)
		if sibRun.Status != "succeeded" {
			t.Errorf("sibling A run = %q, want succeeded untouched", sibRun.Status)
		}
		for _, aux := range []*store.WorkflowAssignment{auxB, auxC} {
			if run := f.getRun(aux.RunID); run.Status != "pending" {
				t.Errorf("aux run %s = %q, want pending untouched", aux.RunID, run.Status)
			}
		}
	})
}
