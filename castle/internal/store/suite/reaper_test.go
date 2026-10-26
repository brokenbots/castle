package suite

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"testing"
	"time"

	"github.com/brokenbots/castle/castle/internal/store"
	"github.com/brokenbots/castle/castle/internal/store/reap"
)

// The CRI-142/KB-226/KB-233/KB-270/KB-308 reaper contract, enforced on both
// engines: heartbeat-stale runs reap failed with "agent heartbeat lost",
// human-gate records park instead of failing, never-started records die by
// timer inside their window, and parked runs keep their resumable shape.

// reapFixture seeds two agents — one with a fresh heartbeat, one whose
// heartbeat stopped before staleBefore — and runs across the status space so
// reaping is only ever exercised where the reaper allows it.
type reapFixture struct {
	t   *testing.T
	s   store.Store
	ctx context.Context
	now time.Time
	// staleBefore is the boundary used for the heartbeat reaper call.
	staleBefore time.Time

	eventCorr int
}

func newReapFixture(t *testing.T, ctx context.Context, s store.Store) *reapFixture {
	t.Helper()
	now := microUTC(time.Now())
	f := &reapFixture{t: t, s: s, ctx: ctx, now: now, staleBefore: now.Add(-60 * time.Second)}
	// fresh: heartbeat 5s ago; stale: heartbeat stopped 10 minutes ago.
	f.overseer("agent-fresh", now.Add(-5*time.Second))
	f.overseer("agent-stale", now.Add(-10*time.Minute))
	return f
}

func (f *reapFixture) overseer(id string, seen time.Time) {
	f.t.Helper()
	newOverseer(f.t, f.s, id, microUTC(seen), nil)
}

func (f *reapFixture) createRun(id, agentID, status string) {
	f.t.Helper()
	f.createRunAt(id, agentID, status, f.now)
}

func (f *reapFixture) createRunAt(id, agentID, status string, createdAt time.Time) {
	f.t.Helper()
	if err := f.s.CreateRun(f.ctx, &store.Run{
		ID: id, OverseerID: agentID, WorkflowName: "wf", Status: status, CreatedAt: microUTC(createdAt),
	}); err != nil {
		f.t.Fatalf("create run %s: %v", id, err)
	}
}

func (f *reapFixture) getRun(id string) *store.Run {
	f.t.Helper()
	r, err := f.s.GetRun(f.ctx, id)
	if err != nil {
		f.t.Fatalf("get run %s: %v", id, err)
	}
	return r
}

// appendRunEvent seeds a run event without an agent connection: the reaper
// classifies candidates from the run's own event record.
func (f *reapFixture) appendRunEvent(runID, evType, payload string) {
	f.t.Helper()
	f.eventCorr++
	ev := &store.Event{
		SchemaVersion: int32(store.EventSchemaVersion),
		RunID:         runID,
		Type:          evType,
		Ts:            microUTC(f.now.Add(-time.Second)),
		CorrelationID: fmt.Sprintf("corr-%s-%d", runID, f.eventCorr),
		Payload:       []byte(payload),
	}
	if _, inserted, err := f.s.AppendEvent(f.ctx, ev); err != nil {
		f.t.Fatalf("append %s event to %s: %v", evType, runID, err)
	} else if !inserted {
		f.t.Fatalf("append %s event to %s: idempotency collided", evType, runID)
	}
}

func (f *reapFixture) reapAt(at time.Time, staleness time.Duration) []store.RunReapOutcome {
	f.t.Helper()
	outcomes, err := f.s.ReapStaleAgentRuns(f.ctx, at, at.Add(-staleness))
	if err != nil {
		f.t.Fatalf("reap at %s: %v", at.Format(time.RFC3339), err)
	}
	return outcomes
}

// reapedIDs reduces reaper verdicts to the reaped (non-parked) run ids in
// stamped order.
func reapedIDs(outcomes []store.RunReapOutcome) []string {
	ids := make([]string, 0, len(outcomes))
	for _, o := range outcomes {
		if !o.Parked {
			ids = append(ids, o.RunID)
		}
	}
	return ids
}

// reapedParked reduces reaper verdicts to the parked run ids.
func reapedParked(t *testing.T, outcomes []store.RunReapOutcome) []string {
	t.Helper()
	ids := make([]string, 0, len(outcomes))
	for _, o := range outcomes {
		if o.Parked {
			ids = append(ids, o.RunID)
		}
	}
	return ids
}

// TestReapStaleAgentRuns covers the CRI-142 heartbeat-staleness reaper matrix:
// only pending/running runs of agents whose heartbeat is stale are stamped
// failed with "agent heartbeat lost"; fresh agents, paused runs, and already
// terminal runs are untouched.
func TestReapStaleAgentRuns(t *testing.T) {
	runWithCtx(t, func(t *testing.T, ctx context.Context, s store.Store) {
		f := newReapFixture(t, ctx, s)

		// Stale agent: reapable states.
		f.createRun("r-stale-pending", "agent-stale", "pending")
		f.createRun("r-stale-running", "agent-stale", "running")
		// Stale agent: protected states.
		f.createRun("r-stale-paused", "agent-stale", "paused")
		f.createRun("r-stale-succeeded", "agent-stale", "succeeded")
		f.createRun("r-stale-failed", "agent-stale", "failed")
		f.createRun("r-stale-cancelled", "agent-stale", "cancelled")
		// Fresh agent: identical statuses must survive.
		f.createRun("r-fresh-pending", "agent-fresh", "pending")
		f.createRun("r-fresh-running", "agent-fresh", "running")

		reaped := f.reapAt(f.now, 60*time.Second)
		if got := reapedIDs(reaped); !slices.Equal(got, []string{"r-stale-pending", "r-stale-running"}) {
			t.Fatalf("want [r-stale-pending r-stale-running], got %v", reaped)
		}

		for _, id := range []string{"r-stale-pending", "r-stale-running"} {
			r := f.getRun(id)
			if r.Status != "failed" {
				t.Errorf("%s: want failed, got %q", id, r.Status)
			}
			if r.FailureReason != store.ReapReasonAgentHeartbeatLost {
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
			if r := f.getRun(id); r.FailureReason != "" {
				t.Errorf("%s: failure_reason must stay empty, got %q", id, r.FailureReason)
			}
		}
		if r := f.getRun("r-fresh-pending"); r.Status != "pending" {
			t.Errorf("fresh pending run reaped: %q", r.Status)
		}
		if r := f.getRun("r-stale-paused"); r.Status != "paused" {
			t.Errorf("paused run reaped: %q", r.Status)
		}
		if r := f.getRun("r-stale-succeeded"); r.Status != "succeeded" {
			t.Errorf("terminal run rewritten: %q", r.Status)
		}
	})
}

// TestReapStaleAgentRuns_MarksAssignmentTerminal verifies that reaping a
// leased run also terminates its queued or leased workflow assignment so a
// dead agent's work is never re-dispatched (CRI-142). Queued assignments
// belong to no agent and survive the pass.
func TestReapStaleAgentRuns_MarksAssignmentTerminal(t *testing.T) {
	runWithCtx(t, func(t *testing.T, ctx context.Context, s store.Store) {
		f := newReapFixture(t, ctx, s)

		a, created, err := s.CreateWorkflowAssignment(ctx, &store.WorkflowAssignment{
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
		if reaped := f.reapAt(f.now, 60*time.Second); len(reaped) != 0 {
			t.Fatalf("queued run must not be reaped, got %v", reaped)
		}
		if r := f.getRun(a.RunID); r.Status != "pending" {
			t.Fatalf("queued run mutated: %q", r.Status)
		}

		// The stale agent leases the work; the run now belongs to it.
		leased, _, err := s.LeaseWorkflowAssignment(ctx, "agent-stale", nil, f.now, time.Minute)
		if err != nil {
			t.Fatalf("lease: %v", err)
		}
		if leased.State != store.WorkflowAssignmentStateLeased {
			t.Fatalf("lease state: %q", leased.State)
		}

		reaped := f.reapAt(f.now, 60*time.Second)
		if got := reapedIDs(reaped); !slices.Equal(got, []string{a.RunID}) {
			t.Fatalf("want [%s], got %v", a.RunID, reaped)
		}
		if r := f.getRun(a.RunID); r.Status != "failed" || r.FailureReason != store.ReapReasonAgentHeartbeatLost {
			t.Fatalf("reaped run: status=%q reason=%q", r.Status, r.FailureReason)
		}

		got, err := s.GetWorkflowAssignment(ctx, a.ID)
		if err != nil {
			t.Fatalf("get assignment: %v", err)
		}
		if got.State != store.WorkflowAssignmentStateTerminal {
			t.Errorf("assignment state = %q, want terminal", got.State)
		}
		if got.TerminalReason != store.ReapReasonAgentHeartbeatLost {
			t.Errorf("assignment terminal_reason = %q", got.TerminalReason)
		}
	})
}

// TestReapStaleAgentRuns_AgentHeartbeatLostMidRun is the CRI-142 regression
// scenario: an agent registers, heartbeats, starts a run, then dies. Once
// heartbeat staleness exceeds the window the run must be terminal without any
// action from the dead agent — and still alive just before it.
func TestReapStaleAgentRuns_AgentHeartbeatLostMidRun(t *testing.T) {
	runWithCtx(t, func(t *testing.T, ctx context.Context, s store.Store) {
		t0 := microUTC(time.Now().UTC().Add(-2 * time.Minute))
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
		run, err := s.GetRun(ctx, "r-zombie")
		if err != nil {
			t.Fatalf("get run: %v", err)
		}
		run.Status = "running"
		run.CurrentStep = "s1"
		if err := s.UpdateRun(ctx, run); err != nil {
			t.Fatalf("run started: %v", err)
		}

		// Last heartbeat at t0+30s. The agent dies here; no SubmitEvents
		// terminal stamp ever arrives.
		lastBeat := t0.Add(30 * time.Second)
		if err := s.UpdateOverseerSeen(ctx, "agent-dies", lastBeat); err != nil {
			t.Fatalf("last heartbeat: %v", err)
		}

		// Before expiry the run survives.
		if _, err := s.ReapStaleAgentRuns(ctx, lastBeat.Add(50*time.Second), lastBeat.Add(50*time.Second).Add(-60*time.Second)); err != nil {
			t.Fatalf("reap before expiry: %v", err)
		}
		if r, err := s.GetRun(ctx, "r-zombie"); err != nil || r.Status != "running" {
			t.Fatalf("run reaped before heartbeat expiry: %v %q", err, r.Status)
		}

		// Past expiry: the run goes terminal.
		reapNow := lastBeat.Add(61 * time.Second)
		reaped, err := s.ReapStaleAgentRuns(ctx, reapNow, reapNow.Add(-60*time.Second))
		if err != nil {
			t.Fatalf("reap after expiry: %v", err)
		}
		if got := reapedIDs(reaped); !slices.Equal(got, []string{"r-zombie"}) {
			t.Fatalf("want [r-zombie], got %v", reaped)
		}
		r, err := s.GetRun(ctx, "r-zombie")
		if err != nil {
			t.Fatalf("get reaped run: %v", err)
		}
		if r.Status != "failed" || r.FailureReason != store.ReapReasonAgentHeartbeatLost || r.EndedAt == nil {
			t.Fatalf("zombie not stamped: status=%q reason=%q ended=%v", r.Status, r.FailureReason, r.EndedAt)
		}
	})
}

// TestReapStaleAgentRuns_AwaitingHumanParksNotFails is the KB-226 regression
// scenario: the engine finalized a step with needs_human, logged the workflow
// completion with terminal state awaiting_human and exited; the run's status
// stamp was never updated and its heartbeat went quiet. The reaper must not
// stamp that run failed — it reclassifies it to awaiting_human and stops,
// past any reap threshold.
func TestReapStaleAgentRuns_AwaitingHumanParksNotFails(t *testing.T) {
	runWithCtx(t, func(t *testing.T, ctx context.Context, s store.Store) {
		f := newReapFixture(t, ctx, s)

		f.createRun("r-parked", "agent-stale", "running")
		f.appendRunEvent("r-parked", "step.entered", `{"step":"review_qa_output"}`)
		f.appendRunEvent("r-parked", "adapter.outcome", `{"step":"review_qa_output","outcome":"needs_human"}`)
		f.appendRunEvent("r-parked", "run.completed", `{"finalState":"awaiting_human","success":false}`)

		// Just past the configured reap threshold (60s).
		outcomes := f.reapAt(f.now.Add(101*time.Second), 60*time.Second)
		if len(outcomes) != 1 {
			t.Fatalf("want exactly one verdict, got %v", outcomes)
		}
		o := outcomes[0]
		if o.RunID != "r-parked" || !o.Parked {
			t.Fatalf("parked run verdict: %+v", o)
		}
		if o.Status != store.RunStatusAwaitingHuman {
			t.Fatalf("parked status = %q, want awaiting_human", o.Status)
		}
		if o.Reason != reap.AwaitingHumanGateVerdictReason {
			t.Fatalf("reclassification reason = %q, want %q", o.Reason, reap.AwaitingHumanGateVerdictReason)
		}

		r := f.getRun("r-parked")
		if r.Status != store.RunStatusAwaitingHuman {
			t.Fatalf("status = %q, want awaiting_human (run stamped failed by the reaper)", r.Status)
		}
		if r.FailureReason != "" {
			t.Fatalf("failure_reason = %q, want empty", r.FailureReason)
		}
		if r.EndedAt != nil {
			t.Fatalf("ended_at stamped on a parked run: %v", r.EndedAt)
		}

		// Past ANY threshold a parked run stays parked: a pass an hour later
		// changes nothing (the stale heartbeat must not eventually win).
		if got := f.reapAt(f.now.Add(time.Hour), 60*time.Second); len(got) != 0 {
			t.Fatalf("second pass stamped anything: %v", got)
		}
		if got := f.getRun("r-parked"); got.Status != store.RunStatusAwaitingHuman {
			t.Fatalf("parked run mutated by a later pass: %q", got.Status)
		}
	})
}

// TestReapStaleAgentRuns_ParkedRunKeepsResumableShape pins that the parked
// reclassification leaves the run resumable: no run-ends, no reason, and —
// the historic failure-flood driver — the workflow assignment is NOT
// terminated, but also never leased again by anyone (re-dispatching would
// re-run completed work).
func TestReapStaleAgentRuns_ParkedRunKeepsResumableShape(t *testing.T) {
	runWithCtx(t, func(t *testing.T, ctx context.Context, s store.Store) {
		f := newReapFixture(t, ctx, s)

		a, created, err := s.CreateWorkflowAssignment(ctx, &store.WorkflowAssignment{
			OwnerCriteriaID: "agent-stale",
			WorkflowName:    "wf",
			WorkflowSource:  "workflow main {}",
			IdempotencyKey:  "key-parked",
			State:           store.WorkflowAssignmentStateQueued,
			CreatedAt:       f.now,
			UpdatedAt:       f.now,
		})
		if err != nil || !created {
			t.Fatalf("create assignment: %v created=%v", err, created)
		}
		// The stale agent leases the work so the minted run has an owning
		// agent and its heartbeat is the one tracked by the reaper.
		if _, _, err := s.LeaseWorkflowAssignment(ctx, "agent-stale", nil, f.now, time.Minute); err != nil {
			t.Fatalf("lease: %v", err)
		}
		// Seed the parked shape: the leased run's engine reached the gate.
		f.appendRunEvent(a.RunID, "run.completed", `{"finalState":"awaiting_human","success":false}`)

		outcomes := f.reapAt(f.now.Add(2*time.Minute), 60*time.Second)
		if len(outcomes) != 1 || outcomes[0].RunID != a.RunID || !outcomes[0].Parked {
			t.Fatalf("parked run verdict: %v", outcomes)
		}

		got, err := s.GetWorkflowAssignment(ctx, a.ID)
		if err != nil {
			t.Fatalf("get assignment: %v", err)
		}
		if got.State == store.WorkflowAssignmentStateTerminal {
			t.Fatalf("assignment state terminal (parked run's assignment must NOT be terminated): %+v", got)
		}
		if got.TerminalReason != "" {
			t.Fatalf("assignment terminal_reason = %q, want empty", got.TerminalReason)
		}

		r := f.getRun(a.RunID)
		if r.Status != store.RunStatusAwaitingHuman || r.FailureReason != "" || r.EndedAt != nil {
			t.Fatalf("parked run shape: status=%q reason=%q ended=%v", r.Status, r.FailureReason, r.EndedAt)
		}

		// Dispatch eligibility: the parked run's assignment must never be
		// leased again — neither by another agent nor by its own.
		if _, _, err := s.LeaseWorkflowAssignment(ctx, "agent-fresh", nil, f.now.Add(3*time.Minute), time.Minute); !errors.Is(err, store.ErrNotFound) {
			t.Fatalf("lease for parked run: want ErrNotFound, got %v", err)
		}
		if _, _, err := s.LeaseWorkflowAssignment(ctx, "agent-stale", nil, f.now.Add(3*time.Minute), time.Minute); !errors.Is(err, store.ErrNotFound) {
			t.Fatalf("lease for parked run by its own agent: want ErrNotFound, got %v", err)
		}
	})
}

// TestReapStaleAgentRuns_NeedAssistanceFinalStateParks is the KB-270
// transition-window regression: the engine renamed its human-gate final_state
// label from awaiting_human to need_assistance, so an event log can carry
// either spelling. Both spellings must classify identically — parked to
// awaiting_human, not failed — within one reaper pass.
func TestReapStaleAgentRuns_NeedAssistanceFinalStateParks(t *testing.T) {
	runWithCtx(t, func(t *testing.T, ctx context.Context, s store.Store) {
		f := newReapFixture(t, ctx, s)

		f.createRun("r-legacy", "agent-stale", "running")
		f.appendRunEvent("r-legacy", "run.completed", `{"finalState":"awaiting_human","success":false}`)
		f.createRun("r-renamed", "agent-stale", "running")
		f.appendRunEvent("r-renamed", "run.completed", `{"finalState":"need_assistance","success":false}`)

		outcomes := f.reapAt(f.now.Add(101*time.Second), 60*time.Second)
		if got := reapedParked(t, outcomes); !slices.Equal(got, []string{"r-legacy", "r-renamed"}) {
			t.Fatalf("parked = %v, want [r-legacy r-renamed]", got)
		}
		if got := reapedIDs(outcomes); len(got) != 0 {
			t.Fatalf("reaped = %v, want none", got)
		}
		for _, o := range outcomes {
			if o.Status != store.RunStatusAwaitingHuman {
				t.Fatalf("%s classified as %q, want awaiting_human", o.RunID, o.Status)
			}
			if o.Reason != reap.AwaitingHumanGateVerdictReason {
				t.Fatalf("%s reason = %q, want %q", o.RunID, o.Reason, reap.AwaitingHumanGateVerdictReason)
			}
		}

		// Either spelling leaves the run resumable: no failed stamp, no
		// ended_at, no failure_reason.
		for _, id := range []string{"r-legacy", "r-renamed"} {
			r := f.getRun(id)
			if r.Status != store.RunStatusAwaitingHuman || r.FailureReason != "" || r.EndedAt != nil {
				t.Fatalf("%s parked shape: status=%q reason=%q ended=%v", id, r.Status, r.FailureReason, r.EndedAt)
			}
		}

		// Past ANY threshold a parked run stays parked.
		if got := f.reapAt(f.now.Add(time.Hour), 60*time.Second); len(got) != 0 {
			t.Fatalf("second pass stamped anything: %v", got)
		}
	})
}

// TestReapStaleAgentRuns_MidFlightStaleStillReaped is the second KB-226
// regression case: a run whose agent died mid-flight — no human-gate in its
// record, only step progress — still reaps failed with "agent heartbeat lost"
// under the unchanged legacy semantics.
func TestReapStaleAgentRuns_MidFlightStaleStillReaped(t *testing.T) {
	runWithCtx(t, func(t *testing.T, ctx context.Context, s store.Store) {
		f := newReapFixture(t, ctx, s)

		f.createRun("r-midflight", "agent-stale", "running")
		f.appendRunEvent("r-midflight", "step.entered", `{"step":"s1"}`)
		f.appendRunEvent("r-midflight", "step.outcome", `{"step":"s1","outcome":"ok"}`)

		outcomes := f.reapAt(f.now.Add(61*time.Second), 60*time.Second)
		if got := reapedIDs(outcomes); !slices.Equal(got, []string{"r-midflight"}) {
			t.Fatalf("reaped = %v, want [r-midflight]", reapedIDs(outcomes))
		}
		o := outcomes[0]
		if o.Parked {
			t.Fatalf("mid-flight run must be reaped, not parked: %+v", o)
		}
		if o.Status != "failed" || o.Reason != store.ReapReasonAgentHeartbeatLost {
			t.Fatalf("mid-flight verdict: %+v", o)
		}

		r := f.getRun("r-midflight")
		if r.Status != "failed" || r.FailureReason != store.ReapReasonAgentHeartbeatLost || r.EndedAt == nil {
			t.Fatalf("mid-flight stale run: status=%q reason=%q ended=%v", r.Status, r.FailureReason, r.EndedAt)
		}
	})
}

// TestReapStaleAgentRuns_NoEventsStillReaped pins the conservative corner: a
// heartbeat-stale run with NO events at all has nothing to classify from and
// follows the legacy verdict.
func TestReapStaleAgentRuns_NoEventsStillReaped(t *testing.T) {
	runWithCtx(t, func(t *testing.T, ctx context.Context, s store.Store) {
		f := newReapFixture(t, ctx, s)
		f.createRun("r-silent", "agent-stale", "running")

		outcomes := f.reapAt(f.now.Add(61*time.Second), 60*time.Second)
		if got := reapedIDs(outcomes); !slices.Equal(got, []string{"r-silent"}) {
			t.Fatalf("reaped = %v, want [r-silent]", got)
		}
	})
}

// TestCancelRun covers the operator cancel path at the store layer: active
// runs go terminal with the operator reason, terminal runs are never
// rewritten, and unknown ids are ErrNotFound.
func TestCancelRun(t *testing.T) {
	runWithCtx(t, func(t *testing.T, ctx context.Context, s store.Store) {
		f := newReapFixture(t, ctx, s)

		f.createRun("r-cancel-pending", "agent-fresh", "pending")
		f.createRun("r-cancel-running", "agent-fresh", "running")
		f.createRun("r-done", "agent-fresh", "succeeded")

		cancelledAt := f.now.Add(time.Minute)
		r, err := s.CancelRun(ctx, "r-cancel-pending", "criteriarun deleted", cancelledAt)
		if err != nil {
			t.Fatalf("cancel pending: %v", err)
		}
		if r.Status != "cancelled" || r.FailureReason != "criteriarun deleted" || r.EndedAt == nil {
			t.Fatalf("cancelled run: status=%q reason=%q ended=%v", r.Status, r.FailureReason, r.EndedAt)
		}
		if got := f.getRun("r-cancel-pending"); got.Status != "cancelled" || got.FailureReason != "criteriarun deleted" {
			t.Fatalf("persisted cancel: status=%q reason=%q", got.Status, got.FailureReason)
		}

		// An empty reason stays empty: the record is simply cancelled with no
		// operator-supplied cause.
		if _, err := s.CancelRun(ctx, "r-cancel-running", "", cancelledAt); err != nil {
			t.Fatalf("cancel running: %v", err)
		}
		if got := f.getRun("r-cancel-running"); got.Status != "cancelled" || got.FailureReason != "" {
			t.Fatalf("cancelled run: status=%q reason=%q", got.Status, got.FailureReason)
		}

		// Terminal states are never overwritten.
		if _, err := s.CancelRun(ctx, "r-done", "late", cancelledAt); !errors.Is(err, store.ErrRunTerminal) {
			t.Fatalf("cancel succeeded run: want ErrRunTerminal, got %v", err)
		}
		if got := f.getRun("r-done"); got.Status != "succeeded" || got.EndedAt != nil {
			t.Fatalf("terminal run mutated: status=%q ended=%v", got.Status, got.EndedAt)
		}

		if _, err := s.CancelRun(ctx, "missing", "", cancelledAt); !errors.Is(err, store.ErrNotFound) {
			t.Fatalf("cancel unknown run: want ErrNotFound, got %v", err)
		}
	})
}

// TestUpdateRun_PreservesFailureReason verifies the CRI-131-style partial
// update contract extends to failure_reason: a status update that carries no
// reason must not clear an existing one.
func TestUpdateRun_PreservesFailureReason(t *testing.T) {
	runWithCtx(t, func(t *testing.T, ctx context.Context, s store.Store) {
		f := newReapFixture(t, ctx, s)
		f.createRun("r-reason", "agent-stale", "pending")

		if _, err := s.CancelRun(ctx, "r-reason", "criteriarun deleted", f.now); err != nil {
			t.Fatalf("cancel: %v", err)
		}

		run := f.getRun("r-reason")
		run.FailureReason = "" // callers holding a stale record must not clobber
		if err := s.UpdateRun(ctx, run); err != nil {
			t.Fatalf("update: %v", err)
		}
		if got := f.getRun("r-reason"); got.FailureReason != "criteriarun deleted" {
			t.Fatalf("failure_reason clobbered: %q", got.FailureReason)
		}
	})
}

// neverStartedFixture drives the KB-233 rule-2 reaper tests. createdBefore is
// the boundary used for the reaper call: runs created before it whose
// started_at is still NULL qualify for the created_never_started verdict.
type neverStartedFixture struct {
	*reapFixture
	createdBefore time.Time
}

func newNeverStartedFixture(t *testing.T, ctx context.Context, s store.Store) *neverStartedFixture {
	t.Helper()
	base := newReapFixture(t, ctx, s)
	return &neverStartedFixture{
		reapFixture:   base,
		createdBefore: base.now.Add(-10 * time.Minute),
	}
}

func (f *neverStartedFixture) reapNeverStarted() []store.RunReapOutcome {
	f.t.Helper()
	reaped, err := f.s.ReapNeverStartedRuns(f.ctx, f.now, f.createdBefore)
	if err != nil {
		f.t.Fatalf("reap never-started: %v", err)
	}
	return reaped
}

// TestReapNeverStartedRuns_Acceptance is the KB-233 rule-2 acceptance test: a
// CreateRun without a StartRun is transitioned terminal-failed with the named
// reason created_never_started, inside the operator-tunable window; the pass
// is idempotent and the reaped record keeps its nil StartedAt.
func TestReapNeverStartedRuns_Acceptance(t *testing.T) {
	runWithCtx(t, func(t *testing.T, ctx context.Context, s store.Store) {
		f := newNeverStartedFixture(t, ctx, s)

		// The orphan-minted shape: a run record was minted by a lingering
		// worker, created_at long past, started_at never set, and nobody will
		// ever start it.
		f.createRunAt("r-orphan", "agent-stale", "pending", f.createdBefore.Add(-time.Minute))

		reaped := f.reapNeverStarted()
		if got := reapedIDs(reaped); !slices.Equal(got, []string{"r-orphan"}) {
			t.Fatalf("reaped = %v, want [r-orphan]", reaped)
		}

		r := f.getRun("r-orphan")
		if r.Status != "failed" || r.FailureReason != reap.ReasonCreatedNeverStarted ||
			r.EndedAt == nil || r.StartedAt != nil {
			t.Fatalf("reaped record: status=%q reason=%q ended=%v started=%v, want failed/created_never_started/ended/nil",
				r.Status, r.FailureReason, r.EndedAt, r.StartedAt)
		}

		// Idempotent: the terminal record is never rewritten; the next pass
		// finds nothing left.
		if reaped := f.reapNeverStarted(); len(reaped) != 0 {
			t.Fatalf("second pass reaped %v, want none", reaped)
		}
		if r := f.getRun("r-orphan"); r.Status != "failed" || r.FailureReason != reap.ReasonCreatedNeverStarted {
			t.Fatalf("terminal record rewritten: status=%q reason=%q", r.Status, r.FailureReason)
		}
	})
}

// TestReapNeverStartedRuns_StatusMatrix pins the rule-2 predicates: only
// pending/running runs whose started_at is still NULL and whose created_at is
// older than createdBefore are reaped. Created-in-window, started, parked
// (paused/stopped, CRI-207) and terminal runs are never rewritten.
func TestReapNeverStartedRuns_StatusMatrix(t *testing.T) {
	runWithCtx(t, func(t *testing.T, ctx context.Context, s store.Store) {
		f := newNeverStartedFixture(t, ctx, s)
		old := f.createdBefore.Add(-time.Minute)

		// In-window created runs must survive even though started_at is NULL.
		f.createRun("r-new-pending", "agent-stale", "pending")
		// Started runs survive regardless of age: started_at is set.
		f.createRunAt("r-old-started", "agent-stale", "pending", old)
		if err := s.UpdateRun(ctx, &store.Run{
			ID: "r-old-started", Status: "running", StartedAt: &f.createdBefore,
		}); err != nil {
			t.Fatalf("start run: %v", err)
		}
		// Parked runs are reaper-exempt regardless of age (CRI-207).
		f.createRunAt("r-old-paused", "agent-stale", "paused", old)
		f.createRunAt("r-old-stopped", "agent-stale", "stopped", old)
		// Terminal runs are never rewritten.
		f.createRunAt("r-old-succeeded", "agent-stale", "succeeded", old)
		f.createRunAt("r-old-failed", "agent-stale", "failed", old)
		f.createRunAt("r-old-cancelled", "agent-stale", "cancelled", old)
		// Reapable: past-window, never started.
		f.createRunAt("r-old-pending", "agent-stale", "pending", old)
		f.createRunAt("r-old-running-unstarted", "agent-stale", "running", old)

		reaped := f.reapNeverStarted()
		if got := reapedIDs(reaped); !slices.Equal(got, []string{"r-old-pending", "r-old-running-unstarted"}) {
			t.Fatalf("reaped = %v, want [r-old-pending r-old-running-unstarted]", reaped)
		}

		for _, id := range []string{"r-old-pending", "r-old-running-unstarted"} {
			if r := f.getRun(id); r.Status != "failed" || r.FailureReason != reap.ReasonCreatedNeverStarted || r.EndedAt == nil {
				t.Errorf("%s: status=%q reason=%q ended=%v", id, r.Status, r.FailureReason, r.EndedAt)
			}
		}
		for _, id := range []string{
			"r-new-pending", "r-old-started", "r-old-paused", "r-old-stopped",
			"r-old-succeeded", "r-old-failed", "r-old-cancelled",
		} {
			if r := f.getRun(id); r.FailureReason != "" {
				t.Errorf("%s: reaper wrote failure_reason %q", id, r.FailureReason)
			}
		}
		if r := f.getRun("r-new-pending"); r.Status != "pending" {
			t.Errorf("in-window pending run reaped: %q", r.Status)
		}
		if r := f.getRun("r-old-started"); r.Status != "running" || r.StartedAt == nil {
			t.Errorf("started run rewritten: status=%q started_at=%v", r.Status, r.StartedAt)
		}
		if r := f.getRun("r-old-paused"); r.Status != "paused" {
			t.Errorf("paused run reaped: %q", r.Status)
		}
		if r := f.getRun("r-old-stopped"); r.Status != "stopped" {
			t.Errorf("stopped run reaped: %q", r.Status)
		}
		if r := f.getRun("r-old-succeeded"); r.Status != "succeeded" {
			t.Errorf("terminal run rewritten: %q", r.Status)
		}
		if r := f.getRun("r-old-failed"); r.Status != "failed" {
			t.Errorf("terminal run rewritten: %q", r.Status)
		}
		if r := f.getRun("r-old-cancelled"); r.Status != "cancelled" {
			t.Errorf("terminal run rewritten: %q", r.Status)
		}
	})
}

// TestReapNeverStartedRuns_MarksAssignmentTerminal pins that the rule-2 reap
// terminates the still-queued or leased workflow assignment so orphan-minted
// work can never be re-dispatched after the run dies.
func TestReapNeverStartedRuns_MarksAssignmentTerminal(t *testing.T) {
	runWithCtx(t, func(t *testing.T, ctx context.Context, s store.Store) {
		f := newNeverStartedFixture(t, ctx, s)

		// A queued assignment owned by an identity with no overseer record:
		// the orphan-minted shape the heartbeat reaper deliberately leaves
		// alone (no heartbeating agent exists) but rule 2 kills by timer.
		a, created, err := s.CreateWorkflowAssignment(ctx, &store.WorkflowAssignment{
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

		reaped := f.reapNeverStarted()
		if got := reapedIDs(reaped); !slices.Equal(got, []string{a.RunID}) {
			t.Fatalf("reaped = %v, want [%s]", reaped, a.RunID)
		}

		got, err := s.GetWorkflowAssignment(ctx, a.ID)
		if err != nil {
			t.Fatalf("get assignment: %v", err)
		}
		if got.State != store.WorkflowAssignmentStateTerminal {
			t.Errorf("assignment state = %q, want terminal", got.State)
		}
		if got.TerminalReason != reap.ReasonCreatedNeverStarted {
			t.Errorf("assignment terminal_reason = %q, want created_never_started", got.TerminalReason)
		}
	})
}

// TestReapNeverStartedRuns_RequeueResetsWindow pins the operator-resume
// interplay (CRI-207 × KB-233 rule 2): a never-started stopped run parked
// past the window is reaper-exempt, but the MarkRunUnstarted re-queue
// refreshes created_at, so the resumed incarnation gets exactly one full
// created_never_started window to land its redelivery.
func TestReapNeverStartedRuns_RequeueResetsWindow(t *testing.T) {
	runWithCtx(t, func(t *testing.T, ctx context.Context, s store.Store) {
		f := newNeverStartedFixture(t, ctx, s)

		a, _, err := s.CreateWorkflowAssignment(ctx, &store.WorkflowAssignment{
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
		f.overseer("o1", f.now)
		if _, _, err := s.LeaseWorkflowAssignment(ctx, "o1", map[string]string{}, f.now.Add(-2*time.Hour), time.Minute); err != nil {
			t.Fatalf("lease: %v", err)
		}
		if err := s.SetRunStopped(ctx, a.RunID); err != nil {
			t.Fatalf("stop: %v", err)
		}

		// Parked: exempt from the rule-2 reaper no matter how old the record.
		if reaped := f.reapNeverStarted(); len(reaped) != 0 {
			t.Fatalf("parked run reaped: %v", reaped)
		}

		// The operator resumes: the run returns to the leasable bucket with a
		// fresh created_at, so the next window applies to the new incarnation.
		resumedAt := microUTC(f.now.Add(time.Minute))
		if err := s.MarkRunUnstarted(ctx, a.RunID, resumedAt); err != nil {
			t.Fatalf("mark unstarted: %v", err)
		}
		if reaped := f.reapNeverStarted(); len(reaped) != 0 {
			t.Fatalf("just-resumed run reaped inside its fresh window: %v", reaped)
		}

		// Past the fresh window with the run still unstarted, it dies by timer.
		later := resumedAt.Add(11 * time.Minute)
		reaped, err := s.ReapNeverStartedRuns(ctx, later, later.Add(-10*time.Minute))
		if err != nil {
			t.Fatalf("reap later: %v", err)
		}
		if got := reapedIDs(reaped); !slices.Equal(got, []string{a.RunID}) {
			t.Fatalf("reaped = %v, want [%s]", reaped, a.RunID)
		}
		if r := f.getRun(a.RunID); r.Status != "failed" || r.FailureReason != reap.ReasonCreatedNeverStarted {
			t.Fatalf("resumed-then-unstarted run: status=%q reason=%q", r.Status, r.FailureReason)
		}
	})
}

// TestClearRunPausedTerminalSafe (CRI-197): clearing the pause state flips a
// paused run back to running with the pending signal cleared, but never
// resurrects a terminal run whose pause state is stale history.
func TestClearRunPausedTerminalSafe(t *testing.T) {
	runWithCtx(t, func(t *testing.T, ctx context.Context, s store.Store) {
		f := newReapFixture(t, ctx, s)
		// A paused run: clear restores running and drops the pending signal.
		f.createRun("r-pause", "agent-fresh", "running")
		if err := s.SetRunPaused(ctx, "r-pause", "deploy-signal", f.now); err != nil {
			t.Fatalf("set paused: %v", err)
		}
		if err := s.ClearRunPaused(ctx, "r-pause"); err != nil {
			t.Fatalf("clear paused: %v", err)
		}
		got := f.getRun("r-pause")
		if got.Status != "running" || got.PendingSignal != "" || got.PausedAt != nil {
			t.Fatalf("after clear: status=%q pending_signal=%q paused_at=%v", got.Status, got.PendingSignal, got.PausedAt)
		}

		// Terminal run: a late ClearRunPaused must not rewrite it back to
		// running.
		if err := s.SetRunPaused(ctx, "r-pause", "deploy-signal", f.now); err != nil {
			t.Fatalf("set paused again: %v", err)
		}
		if _, err := s.CancelRun(ctx, "r-pause", "operator stop", f.now); err != nil {
			t.Fatalf("cancel run: %v", err)
		}
		if err := s.ClearRunPaused(ctx, "r-pause"); err != nil {
			t.Fatalf("clear terminal: %v", err)
		}
		if got := f.getRun("r-pause"); got.Status != "cancelled" {
			t.Fatalf("terminal run resurrected: status=%q", got.Status)
		}
	})
}

// TestSetRunStoppedParksRun (CRI-207): the operator stop parks active runs
// without ending them, drops any pause state alongside, and never rewrites a
// terminal run.
func TestSetRunStoppedParksRun(t *testing.T) {
	runWithCtx(t, func(t *testing.T, ctx context.Context, s store.Store) {
		f := newReapFixture(t, ctx, s)

		f.createRun("r-stop-running", "agent-stale", "running")
		f.createRun("r-stop-paused", "agent-stale", "paused")
		f.createRun("r-stop-succeeded", "agent-stale", "succeeded")
		f.createRun("r-stop-failed", "agent-stale", "failed")
		f.createRun("r-stop-cancelled", "agent-stale", "cancelled")

		// Seed pause state on the stopped-from-paused run.
		if err := s.SetRunPaused(ctx, "r-stop-paused", "deploy-signal", f.now.Add(-time.Hour)); err != nil {
			t.Fatalf("set paused: %v", err)
		}
		// Give the succeeded run a terminal stamp that must survive.
		if err := s.UpdateRun(ctx, &store.Run{ID: "r-stop-succeeded", Status: "succeeded", EndedAt: &f.now}); err != nil {
			t.Fatalf("stamp succeeded: %v", err)
		}

		for _, id := range []string{"r-stop-running", "r-stop-paused", "r-stop-succeeded", "r-stop-failed", "r-stop-cancelled"} {
			if err := s.SetRunStopped(ctx, id); err != nil {
				t.Fatalf("stop %s: %v", id, err)
			}
		}

		r := f.getRun("r-stop-running")
		if r.Status != "stopped" {
			t.Fatalf("r-stop-running: status = %q, want stopped", r.Status)
		}
		if r.EndedAt != nil {
			t.Fatalf("r-stop-running: ended_at stamped on a resumable park: %v", r.EndedAt)
		}
		if r.PendingSignal != "" || r.PausedAt != nil {
			t.Fatal("r-stop-running: pause state left behind")
		}

		r = f.getRun("r-stop-paused")
		if r.Status != "stopped" {
			t.Fatalf("r-stop-paused: status = %q, want stopped", r.Status)
		}
		if r.PendingSignal != "" || r.PausedAt != nil {
			t.Fatalf("r-stop-paused: pause state not dropped: signal=%q paused_at=%v", r.PendingSignal, r.PausedAt)
		}

		for id, want := range map[string]string{
			"r-stop-succeeded": "succeeded", "r-stop-failed": "failed", "r-stop-cancelled": "cancelled",
		} {
			if got := f.getRun(id).Status; got != want {
				t.Errorf("%s: terminal run rewritten, status = %q (want %q)", id, got, want)
			}
		}
	})
}

// TestClearRunStoppedResumes (CRI-207) pins the resume semantics: only a
// stopped run moves back to running on the same run id, and a paused or
// terminal run is never touched.
func TestClearRunStoppedResumes(t *testing.T) {
	runWithCtx(t, func(t *testing.T, ctx context.Context, s store.Store) {
		f := newReapFixture(t, ctx, s)

		f.createRun("r-resume-stopped", "agent-stale", "running")
		if err := s.SetRunStopped(ctx, "r-resume-stopped"); err != nil {
			t.Fatalf("stop: %v", err)
		}
		if err := s.ClearRunStopped(ctx, "r-resume-stopped"); err != nil {
			t.Fatalf("resume: %v", err)
		}
		if r := f.getRun("r-resume-stopped"); r.Status != "running" {
			t.Fatalf("resumed run status = %q, want running (same run id)", r.Status)
		}

		// A late resume must not touch a run that is not stopped anymore.
		if err := s.ClearRunStopped(ctx, "r-resume-stopped"); err != nil {
			t.Fatalf("resume twice: %v", err)
		}
		if r := f.getRun("r-resume-stopped"); r.Status != "running" {
			t.Fatalf("non-stopped run rewritten on resume: %q", r.Status)
		}

		// Terminal runs are never resurrected through the stop/resume path.
		f.createRun("r-resume-terminal", "agent-stale", "succeeded")
		if err := s.SetRunStopped(ctx, "r-resume-terminal"); err != nil {
			t.Fatalf("stop terminal: %v", err)
		}
		if err := s.ClearRunStopped(ctx, "r-resume-terminal"); err != nil {
			t.Fatalf("resume terminal: %v", err)
		}
		if r := f.getRun("r-resume-terminal"); r.Status != "succeeded" {
			t.Fatalf("terminal run rewritten: %q", r.Status)
		}
	})
}

// TestReapStaleAgentRuns_IgnoresStopped (CRI-207) is the explicit reaper
// exemption regression: a run parked as stopped is NOT reaped even though its
// agent has not heartbeated since long before the staleness window. After the
// operator resumes it, the SAME reaper pass must reap it, proving no residual
// exemption survives a resume.
func TestReapStaleAgentRuns_IgnoresStopped(t *testing.T) {
	runWithCtx(t, func(t *testing.T, ctx context.Context, s store.Store) {
		f := newReapFixture(t, ctx, s)

		f.createRun("r-stale-stopped", "agent-stale", "running")
		if err := s.SetRunStopped(ctx, "r-stale-stopped"); err != nil {
			t.Fatalf("stop: %v", err)
		}

		if _, err := s.ReapStaleAgentRuns(ctx, f.now, f.staleBefore); err != nil {
			t.Fatalf("reap: %v", err)
		}
		if r := f.getRun("r-stale-stopped"); r.Status != "stopped" {
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
		if got := reapedIDs(reaped); len(got) != 1 || got[0] != "r-stale-stopped" {
			t.Fatalf("resumed run not reaped with unchanged semantics: %v", reaped)
		}
		r := f.getRun("r-stale-stopped")
		if r.Status != "failed" || r.FailureReason != store.ReapReasonAgentHeartbeatLost {
			t.Fatalf("after resume+reap: status=%q reason=%q", r.Status, r.FailureReason)
		}
	})
}

// TestLeaseWorkflowAssignment_SkipsParkedRun pins the dispatch-side half of
// the CRI-207 invariant: the dispatch path must never hand a parked run's
// queued work to an agent — only an explicit resume returns it to the
// deliverable bucket, after which its assignment is actually leasable again.
func TestLeaseWorkflowAssignment_SkipsParkedRun(t *testing.T) {
	runWithCtx(t, func(t *testing.T, ctx context.Context, s store.Store) {
		now := microUTC(time.Now())
		f := newReapFixture(t, ctx, s)
		f.overseer("o1", now)

		a, _, err := s.CreateWorkflowAssignment(ctx, &store.WorkflowAssignment{
			OwnerCriteriaID: "owner-1",
			WorkflowName:    "wf",
			WorkflowSource:  "source",
			IdempotencyKey:  "key-1",
			CreatedAt:       now,
			UpdatedAt:       now,
		})
		if err != nil {
			t.Fatalf("create assignment: %v", err)
		}

		if err := s.SetRunStopped(ctx, a.RunID); err != nil {
			t.Fatalf("stop: %v", err)
		}
		if _, _, err := s.LeaseWorkflowAssignment(ctx, "o1", map[string]string{}, now, time.Minute); !errors.Is(err, store.ErrNotFound) {
			t.Fatalf("expected ErrNotFound leasing parked run's queued work, got %v", err)
		}

		if err := s.ClearRunStopped(ctx, a.RunID); err != nil {
			t.Fatalf("resume: %v", err)
		}
		leased, _, err := s.LeaseWorkflowAssignment(ctx, "o1", map[string]string{}, now, time.Minute)
		if err != nil {
			t.Fatalf("expected lease after resume, got %v", err)
		}
		if leased.RunID != a.RunID || leased.State != store.WorkflowAssignmentStateLeased {
			t.Fatalf("unexpected lease after resume: run=%s state=%s", leased.RunID, leased.State)
		}
	})
}

// TestLeaseWorkflowAssignment_SkipsTerminalRun covers the same guard for the
// terminal cases: queued work whose run already finished is never dispatched.
func TestLeaseWorkflowAssignment_SkipsTerminalRun(t *testing.T) {
	runWithCtx(t, func(t *testing.T, ctx context.Context, s store.Store) {
		now := microUTC(time.Now())
		f := newReapFixture(t, ctx, s)
		f.overseer("o1", now)

		a, _, err := s.CreateWorkflowAssignment(ctx, &store.WorkflowAssignment{
			OwnerCriteriaID: "owner-1",
			WorkflowName:    "wf",
			WorkflowSource:  "source",
			IdempotencyKey:  "key-1",
			CreatedAt:       now,
			UpdatedAt:       now,
		})
		if err != nil {
			t.Fatalf("create assignment: %v", err)
		}

		r := f.getRun(a.RunID)
		r.Status = "cancelled"
		r.EndedAt = &now
		if err := s.UpdateRun(ctx, r); err != nil {
			t.Fatalf("cancel run: %v", err)
		}

		if _, _, err := s.LeaseWorkflowAssignment(ctx, "o1", map[string]string{}, now, time.Minute); !errors.Is(err, store.ErrNotFound) {
			t.Fatalf("expected ErrNotFound leasing terminal run's queued work, got %v", err)
		}
	})
}

// TestMarkRunUnstartedReturnsToLeasableBucket (CRI-207 review R1) pins the
// resume re-queue contract for a never-started parked run: the run returns to
// pending with started_at NULL and a refreshed created_at, the redelivery
// scan serves the held lease to the leasing agent again, a second lease
// attempt is refused while the agent holds the lease, and the stopped guard
// keeps a terminal run untouched.
func TestMarkRunUnstartedReturnsToLeasableBucket(t *testing.T) {
	runWithCtx(t, func(t *testing.T, ctx context.Context, s store.Store) {
		now := microUTC(time.Now())
		f := newReapFixture(t, ctx, s)
		f.overseer("o1", now)

		a, _, err := s.CreateWorkflowAssignment(ctx, &store.WorkflowAssignment{
			OwnerCriteriaID: "owner-1",
			WorkflowName:    "wf",
			WorkflowSource:  "source",
			IdempotencyKey:  "key-1",
			CreatedAt:       now,
			UpdatedAt:       now,
		})
		if err != nil {
			t.Fatalf("create assignment: %v", err)
		}

		// Lease the run to the agent without any acceptance event: the
		// assignment is leased and the run has started_at NULL.
		leased, _, err := s.LeaseWorkflowAssignment(ctx, "o1", map[string]string{}, now, time.Minute)
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

		requeuedAt := microUTC(now.Add(time.Second))
		if err := s.MarkRunUnstarted(ctx, a.RunID, requeuedAt); err != nil {
			t.Fatalf("mark unstarted: %v", err)
		}
		r := f.getRun(a.RunID)
		if r.Status != "pending" || r.StartedAt != nil || r.EndedAt != nil {
			t.Fatalf("run after mark: status=%q started_at=%v ended_at=%v, want pending with started_at NULL", r.Status, r.StartedAt, r.EndedAt)
		}
		// The re-queue refreshes created_at (KB-233 rule 2): the
		// created_never_started reaper derives its window from created_at, so
		// the resumed run must get a fresh window before redelivery counts as
		// failing.
		if !r.CreatedAt.Equal(requeuedAt) {
			t.Fatalf("run after mark: created_at = %v, want requeuedAt %v", r.CreatedAt, requeuedAt)
		}

		// The redelivery scan now serves the held lease to the leasing agent.
		active, err = s.ListLeasedPendingAssignmentsByCriteriaID(ctx, "o1")
		if err != nil {
			t.Fatalf("redelivery scan after mark: %v", err)
		}
		if len(active) != 1 || active[0].RunID != a.RunID {
			t.Fatalf("resumed unstarted run not redeliverable: %d assignments", len(active))
		}

		// The agent still holds the lease, so a competing lease attempt is
		// refused: redelivery goes through the held lease, not a re-lease.
		if _, _, err := s.LeaseWorkflowAssignment(ctx, "o1", map[string]string{}, requeuedAt, time.Minute); !errors.Is(err, store.ErrNotFound) {
			t.Fatalf("expected ErrNotFound while the agent holds the redeliverable lease, got %v", err)
		}

		// The stopped guard keeps the transition out of a terminal run's state.
		r.Status = "succeeded"
		r.EndedAt = &now
		if err := s.UpdateRun(ctx, r); err != nil {
			t.Fatalf("stamp terminal: %v", err)
		}
		if err := s.MarkRunUnstarted(ctx, a.RunID, microUTC(now.Add(2*time.Second))); err != nil {
			t.Fatalf("mark unstarted on terminal run: %v", err)
		}
		if got := f.getRun(a.RunID); got.Status != "succeeded" {
			t.Fatalf("terminal run moved to %q, want succeeded (stopped guard)", got.Status)
		}
	})
}
