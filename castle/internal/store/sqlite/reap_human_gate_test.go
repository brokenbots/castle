package sqlite

import (
	"errors"
	"fmt"
	"slices"
	"testing"
	"time"

	"github.com/brokenbots/castle/castle/internal/store"
)

// The KB-226 regression suite: a run whose engine reached a human gate keeps
// no live heartbeat by design, so the CRI-142 heartbeat reaper must classify
// from the run's own record and park it instead of stamping the historic
// "agent heartbeat lost" failure onto finished work. The classification
// source is the run's latest persisted event — the engine stamps the terminal
// state there even when the run-status stamp was lost (the exact shape of the
// corrupted cohort: run.completed with final_state awaiting_human sitting in
// the event log of a run whose status row stayed 'running').

var eventCorrCounter int

func (f *reapFixture) appendRunEvent(t *testing.T, runID, evType, payload string) {
	t.Helper()
	eventCorrCounter++
	ev := &store.Event{
		SchemaVersion: store.EventSchemaVersion,
		RunID:         runID,
		Type:          evType,
		Ts:            f.now.Add(-time.Second),
		CorrelationID: fmt.Sprintf("corr-%s-%d", runID, eventCorrCounter),
		Payload:       []byte(payload),
	}
	if _, inserted, err := f.s.AppendEvent(f.ctx, ev); err != nil {
		t.Fatalf("append %s event to %s: %v", evType, runID, err)
	} else if !inserted {
		t.Fatalf("append %s event to %s: idempotency collided", evType, runID)
	}
}

func (f *reapFixture) reapAt(t *testing.T, at time.Time, staleness time.Duration) []store.RunReapOutcome {
	t.Helper()
	outcomes, err := f.s.ReapStaleAgentRuns(f.ctx, at, at.Add(-staleness))
	if err != nil {
		t.Fatalf("reap at %s: %v", at.Format(time.RFC3339), err)
	}
	return outcomes
}

// lastEventOfType returns the run's most recent event, the record the reaper
// classifies from.
func (f *reapFixture) lastEvent(t *testing.T, runID string) *store.Event {
	t.Helper()
	ev, err := f.s.GetLatestEvent(f.ctx, runID)
	if err != nil {
		t.Fatalf("get latest event of %s: %v", runID, err)
	}
	return ev
}

// TestReapStaleAgentRuns_AwaitingHumanParksNotFails is the KB-226 regression
// scenario from the live capture (kb-215/cri-321): the engine finalized
// step=review_qa_output with outcome needs_human, logged the workflow
// completion with terminal state awaiting_human and exited; the run's status
// stamp was never updated and its heartbeat went quiet. The reaper must not
// stamp that run failed — it reclassifies it to awaiting_human and stops,
// past any reap threshold.
func TestReapStaleAgentRuns_AwaitingHumanParksNotFails(t *testing.T) {
	f := newReapFixture(t)

	f.createRun(t, "r-parked", "agent-stale", "running")
	f.appendRunEvent(t, "r-parked", "step.entered",
		`{"step":"review_qa_output"}`)
	f.appendRunEvent(t, "r-parked", "adapter.outcome",
		`{"step":"review_qa_output","outcome":"needs_human"}`)
	f.appendRunEvent(t, "r-parked", "run.completed",
		`{"finalState":"awaiting_human","success":false}`)

	// Just past the configured reap threshold (60s at current flags).
	outcomes := f.reapAt(t, f.now.Add(101*time.Second), 60*time.Second)
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
	if o.Reason != awaitingHumanGateVerdictReason {
		t.Fatalf("reclassification reason = %q, want %q", o.Reason, awaitingHumanGateVerdictReason)
	}

	r := f.getRun(t, "r-parked")
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
	later := f.now.Add(time.Hour)
	outcomes = f.reapAt(t, later, 60*time.Second)
	if len(outcomes) != 0 {
		t.Fatalf("second pass stamped anything: %v", outcomes)
	}
	if got := f.getRun(t, "r-parked"); got.Status != store.RunStatusAwaitingHuman {
		t.Fatalf("parked run mutated by a later pass: %q", got.Status)
	}
}

// TestReapStaleAgentRuns_ParkedRunKeepsResumableShape pins that the parked
// reclassification leaves the run parkable/resumable: no run-ends, no reason,
// and — the historic failure-flood driver — the workflow assignment is NOT
// terminated, because the parked run's work is done, not dead.
func TestReapStaleAgentRuns_ParkedRunKeepsResumableShape(t *testing.T) {
	f := newReapFixture(t)

	a, created, err := f.s.CreateWorkflowAssignment(f.ctx, &store.WorkflowAssignment{
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
	// The stale agent leases the work so the minted run has an owning agent
	// and its heartbeat is the one tracked by the reaper.
	if _, err := f.s.LeaseWorkflowAssignment(f.ctx, "agent-stale", nil, f.now, time.Minute); err != nil {
		t.Fatalf("lease: %v", err)
	}
	// Seed the parked shape: the leased run's engine reached the gate.
	f.appendRunEvent(t, a.RunID, "run.completed",
		`{"finalState":"awaiting_human","success":false}`)
	outcomes := f.reapAt(t, f.now.Add(2*time.Minute), 60*time.Second)
	if len(outcomes) != 1 || outcomes[0].RunID != a.RunID || !outcomes[0].Parked {
		t.Fatalf("parked run verdict: %v", outcomes)
	}

	got, err := f.s.GetWorkflowAssignment(f.ctx, a.ID)
	if err != nil {
		t.Fatalf("get assignment: %v", err)
	}
	if got.State == store.WorkflowAssignmentStateTerminal {
		t.Fatalf("assignment state terminal (parked run's assignment must NOT be terminated): %+v", got)
	}
	if got.TerminalReason != "" {
		t.Fatalf("assignment terminal_reason = %q, want empty", got.TerminalReason)
	}

	r := f.getRun(t, a.RunID)
	if r.Status != store.RunStatusAwaitingHuman || r.FailureReason != "" || r.EndedAt != nil {
		t.Fatalf("parked run shape: status=%q reason=%q ended=%v", r.Status, r.FailureReason, r.EndedAt)
	}

	// Dispatch eligibility: the parked run's assignment must never be leased
	// again — re-dispatching would re-run completed work (the spurious refire).
	if _, err := f.s.LeaseWorkflowAssignment(f.ctx, "agent-fresh", nil, f.now.Add(3*time.Minute), time.Minute); !errIsNotFound(err) {
		t.Fatalf("lease for parked run: want ErrNotFound, got %v", err)
	}
	if _, err := f.s.LeaseWorkflowAssignment(f.ctx, "agent-stale", nil, f.now.Add(3*time.Minute), time.Minute); !errIsNotFound(err) {
		t.Fatalf("lease for parked run by its own agent: want ErrNotFound, got %v", err)
	}
}

func errIsNotFound(err error) bool {
	return errors.Is(err, store.ErrNotFound)
}

// TestReapStaleAgentRuns_MidFlightStaleStillReaped is the second KB-226
// regression case: a run whose agent died mid-flight — no human-gate in its
// record, only step progress — still reaps failed with "agent heartbeat lost"
// under the unchanged legacy semantics.
func TestReapStaleAgentRuns_MidFlightStaleStillReaped(t *testing.T) {
	f := newReapFixture(t)

	f.createRun(t, "r-midflight", "agent-stale", "running")
	f.appendRunEvent(t, "r-midflight", "step.entered", `{"step":"s1"}`)
	f.appendRunEvent(t, "r-midflight", "step.outcome", `{"step":"s1","outcome":"ok"}`)
	// No terminal or human-gate event follows; the agent just vanished.

	outcomes := f.reapAt(t, f.now.Add(61*time.Second), 60*time.Second)
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

	r := f.getRun(t, "r-midflight")
	if r.Status != "failed" || r.FailureReason != "agent heartbeat lost" || r.EndedAt == nil {
		t.Fatalf("mid-flight stale run: status=%q reason=%q ended=%v", r.Status, r.FailureReason, r.EndedAt)
	}
}

// TestReapStaleAgentRuns_NoEventsMidFlightStillReaped pins the conservative
// corner: a heartbeat-stale run with NO events at all has nothing to classify
// from and follows the legacy verdict.
func TestReapStaleAgentRuns_NoEventsMidFlightStillReaped(t *testing.T) {
	f := newReapFixture(t)
	f.createRun(t, "r-silent", "agent-stale", "running")

	outcomes := f.reapAt(t, f.now.Add(61*time.Second), 60*time.Second)
	if got := reapedIDs(outcomes); !slices.Equal(got, []string{"r-silent"}) {
		t.Fatalf("reaped = %v, want [r-silent]", reapedIDs(outcomes))
	}
}

// TestLastEventParksHumanGate is the classifier's contract table: exactly
// these last-event shapes park a run at a human gate, and everything else —
// including terminal completions that are not human gates and the duration
// variants — stays reapable with the legacy verdict.
func TestLastEventParksHumanGate(t *testing.T) {
	cases := []struct {
		name       string
		evType     string
		payload    string
		wantParked bool
		wantReason string
	}{
		{
			name:       "run.completed awaiting_human camelCase",
			evType:     "run.completed",
			payload:    `{"finalState":"awaiting_human","success":false}`,
			wantParked: true,
			wantReason: awaitingHumanGateVerdictReason,
		},
		{
			name:       "run.completed awaiting_human snake_case",
			evType:     "run.completed",
			payload:    `{"final_state":"awaiting_human","success":false}`,
			wantParked: true,
			wantReason: awaitingHumanGateVerdictReason,
		},
		{
			name:       "run.completed awaiting_human only success true",
			evType:     "run.completed",
			payload:    `{"finalState":"awaiting_human","success":true}`,
			wantParked: true,
			wantReason: awaitingHumanGateVerdictReason,
		},
		{
			name:    "run.completed succeeded does not park",
			evType:  "run.completed",
			payload: `{"finalState":"succeeded","success":true}`,
		},
		{
			name:    "run.completed failed does not park",
			evType:  "run.completed",
			payload: `{"finalState":"failed","success":false}`,
		},
		{
			name:    "run.completed undecodable payload does not park",
			evType:  "run.completed",
			payload: `not json`,
		},
		{
			name:       "approval.requested parks on type",
			evType:     "approval.requested",
			payload:    `{"node":"ship","approvers":["dave"],"reason":"go/no-go"}`,
			wantParked: true,
			wantReason: approvalGateVerdictReason,
		},
		{
			name:       "approval.requested parks with empty payload",
			evType:     "approval.requested",
			payload:    `{}`,
			wantParked: true,
			wantReason: approvalGateVerdictReason,
		},
		{
			name:       "run.paused signal parks",
			evType:     "run.paused",
			payload:    `{"mode":"signal","signal":"resume_signal"}`,
			wantParked: true,
			wantReason: humanPauseGateVerdictReason,
		},
		{
			name:       "run.paused external (actor) parks",
			evType:     "run.paused",
			payload:    `{"mode":"external","actor":"console"}`,
			wantParked: true,
			wantReason: humanPauseGateVerdictReason,
		},
		{
			name:    "run.paused duration does not park",
			evType:  "run.paused",
			payload: `{"mode":"duration"}`,
		},
		{
			name:    "run.paused undecodable payload does not park",
			evType:  "run.paused",
			payload: `not json`,
		},
		{
			name:       "wait.entered signal parks via mode",
			evType:     "wait.entered",
			payload:    `{"node":"watch_deploy","mode":"signal","signal":"deploy_done"}`,
			wantParked: true,
			wantReason: signalWaitGateVerdictReason,
		},
		{
			name:       "wait.entered parks via signal without mode",
			evType:     "wait.entered",
			payload:    `{"node":"watch_deploy","signal":"deploy_done"}`,
			wantParked: true,
			wantReason: signalWaitGateVerdictReason,
		},
		{
			name:    "wait.entered duration does not park",
			evType:  "wait.entered",
			payload: `{"node":"sleepy","mode":"duration","duration":30}`,
		},
		{
			name:    "wait.entered undecodable payload does not park",
			evType:  "wait.entered",
			payload: `not json`,
		},
		{
			name:    "mid-flight step event does not park",
			evType:  "step.outcome",
			payload: `{"step":"s1","outcome":"ok"}`,
		},
		{
			name:    "run.failed does not park",
			evType:  "run.failed",
			payload: `{"reason":"boom","step":"s1"}`,
		},
		{
			name:    "unknown type does not park",
			evType:  "run.metadata",
			payload: `{"ticket":"CRI-104"}`,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			parked, reason := lastEventParksHumanGate(tc.evType, []byte(tc.payload))
			if parked != tc.wantParked {
				t.Fatalf("parked = %v (reason %q), want %v", parked, reason, tc.wantParked)
			}
			if tc.wantParked && reason != tc.wantReason {
				t.Fatalf("reason = %q, want %q", reason, tc.wantReason)
			}
		})
	}
}

// TestReapRunIDs_ParksCandidatesInsideWriteTx pins the classification point:
// the verdict is derived from the run's own record inside the write
// transaction, so a candidate whose awaiting_human completion landed between
// the reader scan and the writer transaction — the scan-then-write race the
// CRI-142 re-validation already handles for terminal stamps — is parked, not
// reaped.
func TestReapRunIDs_ParksCandidatesInsideWriteTx(t *testing.T) {
	f := newReapFixture(t)

	f.createRun(t, "r-gated", "agent-stale", "running")
	f.appendRunEvent(t, "r-gated", "run.completed",
		`{"finalState":"awaiting_human","success":false}`)
	// A genuinely dead neighbor sharing the same candidate snapshot.
	f.createRun(t, "r-dead", "agent-stale", "running")

	outcomes, err := f.s.reapRunIDs(f.ctx, f.now, f.staleBefore,
		[]string{"r-gated", "r-dead"}, store.ReapReasonAgentHeartbeatLost)
	if err != nil {
		t.Fatalf("reapRunIDs: %v", err)
	}
	parked := reapRunIDsParked(t, outcomes)
	reaped := reapedIDs(outcomes)
	if !slices.Equal(parked, []string{"r-gated"}) {
		t.Fatalf("parked = %v, want [r-gated]", parked)
	}
	if !slices.Equal(reaped, []string{"r-dead"}) {
		t.Fatalf("reaped = %v, want [r-dead]", reaped)
	}
	if got := f.getRun(t, "r-gated").Status; got != store.RunStatusAwaitingHuman {
		t.Fatalf("r-gated status = %q, want awaiting_human", got)
	}
}

// reapRunIDsParked reduces verdicts to the parked run ids. Sibling of
// reapedIDs for assertions over mixed verdict lists.
func reapRunIDsParked(t *testing.T, outcomes []store.RunReapOutcome) []string {
	t.Helper()
	ids := make([]string, 0, len(outcomes))
	for _, o := range outcomes {
		if o.Parked {
			ids = append(ids, o.RunID)
		}
	}
	return ids
}
