package kanboardtrigger

import (
	"fmt"

	"testing"

	pb "github.com/brokenbots/criteria/sdk/pb/criteria/v1"
)

// envStarted / envCompleted / envFailed build run envelopes for the mirror
// (schema v1).
func envStarted(runID string) *pb.Envelope {
	return &pb.Envelope{SchemaVersion: 1, RunId: runID, Payload: &pb.Envelope_RunStarted{
		RunStarted: &pb.RunStarted{WorkflowName: "kb-develop"},
	}}
}

func envCompleted(runID, finalState string) *pb.Envelope {
	return &pb.Envelope{SchemaVersion: 1, RunId: runID, Payload: &pb.Envelope_RunCompleted{
		RunCompleted: &pb.RunCompleted{FinalState: finalState, Success: true},
	}}
}

func envFailed(runID, reason string) *pb.Envelope {
	return &pb.Envelope{SchemaVersion: 1, RunId: runID, Payload: &pb.Envelope_RunFailed{
		RunFailed: &pb.RunFailed{Reason: reason, Step: "handler"},
	}}
}

// runIDOf returns the fake submitter's deterministic run id for the n-th
// fire (run-1, run-2, ...).
func runIDOf(n int) string { return fmt.Sprintf("run-%d", n) }

// mirrorHarness fires from `column` with the standard dev+triage config.
func mirrorHarness(t *testing.T, ticketID int, column string, sub func(*fakeSubmitter)) (*harness, *fakeKanboard) {
	fk := newFakeKanboard(t, "KB board", boardColumns()...)
	fk.addCard(t, ticketID, column, fmt.Sprintf("KB-%d", ticketID), "k8s-run", "repo:own/name")
	h := newHarness(t, fk, testLaneConfig(), sub)
	return h, fk
}

// recordOf snapshots one run record.
func recordOf(t *testing.T, h *harness, runID string) RunRecord {
	t.Helper()
	h.a.mu.Lock()
	defer h.a.mu.Unlock()
	r := h.a.runs[runID]
	if r == nil {
		t.Fatalf("no run record %s", runID)
	}
	return *r
}

// pollUntil drives polls until cond holds (beyond the pump/reconcile split
// an envelope may take two cycles) or retries exhaust.
func pollUntil(t *testing.T, h *harness, cond func() bool) {
	t.Helper()
	const tries = 20
	for i := 0; i < tries; i++ {
		h.poll(t)
		if cond() {
			return
		}
	}
	t.Fatalf("condition not met after %d polls", tries)
}

func cardIn(h *harness, taskID int, column string) bool { return h.fk.columnOf(taskID) == column }

func TestMirrorRunEventsSettleRecord(t *testing.T) {
	h, _ := mirrorHarness(t, 30, "Work in progress", nil)
	h.watcher.setEvents(runIDOf(1), envStarted("run-1"), envCompleted("run-1", "handler_complete"))
	h.poll(t) // fires run-1; its watch pump applies the envelopes
	pollUntil(t, h, func() bool {
		rec := recordOf(t, h, "run-1")
		return rec.Settled && rec.SeenEvents && rec.FinalState == "handler_complete" && rec.Succeeded
	})
	rec := recordOf(t, h, "run-1")

	// Further polls are idempotent: a settled record is never re-applied.
	for i := 0; i < 3; i++ {
		h.poll(t)
	}
	after := recordOf(t, h, "run-1")
	if after != rec {
		t.Errorf("settled record changed across polls: %+v -> %+v", rec, after)
	}
}

func TestMirrorFailedRunParksWithOneComment(t *testing.T) {
	h, fk := mirrorHarness(t, 31, "Work in progress", nil)
	h.watcher.setEvents(runIDOf(1), envStarted("run-1"), envFailed("run-1", "compile error"))
	pollUntil(t, h, func() bool { return cardIn(h, 31, "Review") })

	// Exactly one failure comment, posted at the move; further polls (with
	// the reconcile running again over replays) must not repeat it.
	for i := 0; i < 3; i++ {
		h.poll(t)
	}
	comments := fk.commentsOn(31)
	if n := len(comments); n != 1 {
		t.Fatalf("comments = %d, want exactly one (KB-302)", n)
	}
	if want := fmt.Sprintf("kanboard-trigger-adapter: run %s failed: compile error", "run-1"); comments[0] != want {
		t.Errorf("comment = %q, want %q", comments[0], want)
	}
}

func TestMirrorTriageSuccessLeavesCard(t *testing.T) {
	h, fk := mirrorHarness(t, 32, "Backlog", nil)
	h.watcher.setEvents(runIDOf(1), envStarted("run-1"), envCompleted("run-1", "handler_complete"))
	pollUntil(t, h, func() bool { return recordOf(t, h, "run-1").Mirrored })
	rec := recordOf(t, h, "run-1")
	if rec.Class != ClassTriage || !rec.Succeeded {
		t.Fatalf("record = %+v, want succeeded triage run", rec)
	}
	if col := fk.columnOf(32); col != "Backlog" {
		t.Errorf("triage card column = %q, want Backlog untouched (KB-9)", col)
	}
	if fk.commentsOn(32) != nil {
		t.Errorf("triage card commented: %v", fk.commentsOn(32))
	}
}
func TestMirrorDevelopHandlerCompleteMovesDone(t *testing.T) {
	h, fk := mirrorHarness(t, 33, "Work in progress", nil)
	h.watcher.setEvents(runIDOf(1), envStarted("run-1"), envCompleted("run-1", "handler_complete"))
	pollUntil(t, h, func() bool { return cardIn(h, 33, "Done") })
	if fk.commentsOn(33) != nil {
		t.Errorf("develop completion commented: %v", fk.commentsOn(33))
	}
}

func TestMirrorHandoffRepairsOnlyFromDone(t *testing.T) {
	for _, finalState := range []string{"failed", "need_assistance", "awaiting_human"} {
		t.Run(finalState, func(t *testing.T) {
			h, fk := mirrorHarness(t, 34, "Work in progress", nil)
			h.watcher.setEvents(runIDOf(1), envStarted("run-1"), envCompleted("run-1", finalState))
			h.poll(t) // settle; card still in WIP (no repair outside Done)
			if col := fk.columnOf(34); col != "Work in progress" {
				t.Fatalf("pre-move card column = %q, want Work in progress (KB-269)", col)
			}
			fk.setCardColumn(34, "Done")
			pollUntil(t, h, func() bool { return cardIn(h, 34, "Review") })
		})
	}
}

func TestMirrorHandoffNonDoneStays(t *testing.T) {
	h, fk := mirrorHarness(t, 35, "Ready", nil)
	h.watcher.setEvents(runIDOf(1), envStarted("run-1"), envCompleted("run-1", "need_assistance"))
	pollUntil(t, h, func() bool { return recordOf(t, h, "run-1").Mirrored })
	if col := fk.columnOf(35); col != "Ready" {
		t.Errorf("card column = %q, want Ready (KB-269: no repair outside Done)", col)
	}
	if fk.commentsOn(35) != nil {
		t.Errorf("handoff repair commented: %v", fk.commentsOn(35))
	}
}

func TestMirrorUnknownFinalStateAbstains(t *testing.T) {
	h, fk := mirrorHarness(t, 36, "Work in progress", nil)
	h.watcher.setEvents(runIDOf(1), envStarted("run-1"), envCompleted("run-1", "screwy"))
	pollUntil(t, h, func() bool { return recordOf(t, h, "run-1").Mirrored })
	if col := fk.columnOf(36); col != "Work in progress" {
		t.Errorf("card column = %q, want untouched (abstain on unknown final state)", col)
	}
}

// TestMirrorRejectedDisposition: an assignment rejected before any run
// event keeps the card where it is and marks the record no-verdict.
func TestMirrorRejectedDisposition(t *testing.T) {
	h, fk := mirrorHarness(t, 37, "Work in progress", func(sub *fakeSubmitter) {
		sub.dispState = pb.WorkflowAssignmentState_WORKFLOW_ASSIGNMENT_STATE_REJECTED
		sub.rejectionReason = "route not deployed"
	})
	pollUntil(t, h, func() bool {
		rec := recordOf(t, h, "run-1")
		return rec.Settled && rec.NoVerdict
	})
	rec := recordOf(t, h, "run-1")
	if rec.FailureReason != "route not deployed" {
		t.Errorf("rejection reason lost: %q", rec.FailureReason)
	}
	if col := fk.columnOf(37); col != "Work in progress" {
		t.Errorf("card column = %q, want untouched", col)
	}
}

// TestSettleUnblocksSecondFire: re-entries blocked while a run is unsettled
// fire with the next sequence once the run settles by disposition.
func TestSettleUnblocksSecondFire(t *testing.T) {
	h, fk := mirrorHarness(t, 38, "Work in progress", nil)
	h.poll(t) // run-1 in flight
	fk.setCardColumn(38, "Ready")
	h.poll(t) // in-flight abstention: still one fire
	if n := len(h.sub.submitted()); n != 1 {
		t.Fatalf("in-flight poll fired %d times, want 1", n)
	}
	h.sub.dispState = pb.WorkflowAssignmentState_WORKFLOW_ASSIGNMENT_STATE_TERMINAL
	pollUntil(t, h, func() bool { return recordOf(t, h, "run-1").Settled })
	fk.setCardColumn(38, "Work in progress") // re-entry into a lane column
	pollUntil(t, h, func() bool { return len(h.sub.submitted()) == 2 })
	calls := h.sub.submitted()
	if calls[1].IdempotencyKey == calls[0].IdempotencyKey {
		t.Errorf("re-fire reused key %q", calls[0].IdempotencyKey)
	}
}

// TestMirrorAbstainDoesNotComment: an abstaining record produces no board
// comments at all (only the failure-park rule comments).
func TestMirrorAbstainDoesNotComment(t *testing.T) {
	h, fk := mirrorHarness(t, 39, "Work in progress", nil)
	h.watcher.setEvents(runIDOf(1), envStarted("run-1"), envCompleted("run-1", "awaiting_human"))
	pollUntil(t, h, func() bool { return recordOf(t, h, "run-1").Mirrored })
	if c := fk.commentsOn(39); len(c) != 0 {
		t.Errorf("abstaining card commented: %v", c)
	}
}
