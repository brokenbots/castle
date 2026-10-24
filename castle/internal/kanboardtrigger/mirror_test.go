package kanboardtrigger

import (
	"context"
	"fmt"
	"testing"
	"time"

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

// snapAfterReconcile runs the poll cycle up to and including the mirror
// pass and returns the run record snapshot BEFORE the end-of-poll flush:
// once bounded-state pruning is on, a settled-and-mirrored record exists
// only in that window.
func snapAfterReconcile(t *testing.T, h *harness, runID string) (RunRecord, bool) {
	t.Helper()
	ctx := context.Background()
	tasks, err := h.a.kb.GetAllTasks(ctx, h.a.boardProjectID)
	if err != nil {
		t.Fatalf("fetch tasks: %v", err)
	}
	h.a.adoptWatches()
	h.a.checkDispositions(ctx)
	h.a.reconcileMirrors(ctx, tasks)
	h.a.mu.Lock()
	defer h.a.mu.Unlock()
	r, ok := h.a.runs[runID]
	if !ok {
		return RunRecord{}, false
	}
	return *r, true
}

// recordWhenSnap polls until the run record satisfies cond (snapshot taken
// after reconcile; completing each cycle so pumps settle normally).
func recordWhenSnap(t *testing.T, h *harness, runID string, cond func(RunRecord) bool) RunRecord {
	t.Helper()
	for i := 0; i < 20; i++ {
		rec, ok := snapAfterReconcile(t, h, runID)
		if ok && cond(rec) {
			return rec
		}
		h.poll(t)
	}
	t.Fatalf("run %s: condition not met after 20 cycles", runID)
	return RunRecord{}
}

// hasRun reports whether the adapter currently tracks the run.
func hasRun(h *harness, runID string) bool {
	h.a.mu.Lock()
	defer h.a.mu.Unlock()
	_, ok := h.a.runs[runID]
	return ok
}

// waitFor spins until a non-poll-driven condition (e.g. goroutine state)
// holds, bounded.
func waitFor(t *testing.T, d time.Duration, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(2 * time.Millisecond)
	}
	t.Fatalf("%s: condition not met within %s", what, d)
}

// isWatching reports whether a watch stream is live (or being attempted)
// for the run.
func isWatching(h *harness, runID string) bool {
	h.a.mu.Lock()
	defer h.a.mu.Unlock()
	return h.a.watching[runID]
}

// TestTerminalDispositionDefersToReplayedTerminal: a TERMINAL disposition
// read while the run's watch is live and its replay is pending must not
// settle the record ahead of the envelope. The ticket stays fire-blocked
// (no duplicate run) until the replayed RunCompleted applies, then the
// card mirrors the real verdict.
func TestTerminalDispositionDefersToReplayedTerminal(t *testing.T) {
	h, fk := mirrorHarness(t, 40, "Work in progress", nil)
	h.watcher.holdEvents(runIDOf(1), envStarted("run-1"), envCompleted("run-1", "handler_complete"))
	h.sub.dispState = pb.WorkflowAssignmentState_WORKFLOW_ASSIGNMENT_STATE_TERMINAL
	h.poll(t) // fires run-1
	h.poll(t) // disposition settle defers: watch live
	rec, ok := snapAfterReconcile(t, h, "run-1")
	if !ok {
		t.Fatal("run record missing")
	}
	if rec.Settled || rec.Mirrored || !rec.TerminalByDisposition {
		t.Errorf("deferred settle not recorded: %+v", rec)
	}

	// Fire-blocked while the verdict is unknown: a re-entry submits nothing.
	fk.setCardColumn(40, "Ready")
	h.poll(t)
	if n := len(h.sub.submitted()); n != 1 {
		t.Fatalf("fire-blocked ticket submitted %d assignments, want 1", n)
	}

	// The replayed terminal envelope applies even to the deferred record.
	h.watcher.release(runIDOf(1))
	pollUntil(t, h, func() bool { return cardIn(h, 40, "Done") })
	if c := fk.commentsOn(40); len(c) != 0 {
		t.Errorf("successful run commented: %v", c)
	}
	rec, ok = snapAfterReconcile(t, h, "run-1")
	if ok && (!rec.Succeeded || rec.NoVerdict) {
		t.Errorf("real verdict lost on replayed terminal: %+v", rec)
	}

	// With the verdict applied the ticket unblocks with a new sequence.
	fk.setCardColumn(40, "Work in progress")
	pollUntil(t, h, func() bool { return len(h.sub.submitted()) == 2 })
	calls := h.sub.submitted()
	if calls[1].IdempotencyKey == calls[0].IdempotencyKey {
		t.Errorf("duplicate fire reused key %q", calls[0].IdempotencyKey)
	}
}

// TestTerminalDispositionDefersToReplayedFailure: same deferral with a
// replayed RunFailed — the card parks in Review with exactly one failure
// comment, and the deferred record never pre-settles.
func TestTerminalDispositionDefersToReplayedFailure(t *testing.T) {
	h, fk := mirrorHarness(t, 41, "Work in progress", nil)
	h.watcher.holdEvents(runIDOf(1), envStarted("run-1"), envFailed("run-1", "flaky step"))
	h.sub.dispState = pb.WorkflowAssignmentState_WORKFLOW_ASSIGNMENT_STATE_TERMINAL
	h.poll(t) // fires run-1
	h.poll(t) // disposition settle defers: watch live
	rec, ok := snapAfterReconcile(t, h, "run-1")
	if !ok {
		t.Fatal("run record missing")
	}
	if rec.Settled || rec.Mirrored || !rec.TerminalByDisposition {
		t.Errorf("deferred settle not recorded: %+v", rec)
	}
	if n := len(h.sub.submitted()); n != 1 {
		t.Fatalf("submitted %d times, want 1", n)
	}

	h.watcher.release(runIDOf(1))
	pollUntil(t, h, func() bool {
		return cardIn(h, 41, "Review") && len(fk.commentsOn(41)) == 1
	})
	// Exactly one comment, never repeated, despite the deferred settle
	// having been "owed" a park move at disposition time.
	for i := 0; i < 2; i++ {
		h.poll(t)
	}
	if c := fk.commentsOn(41); len(c) != 1 {
		t.Fatalf("comments = %d, want exactly one (KB-302)", len(c))
	} else if want := "kanboard-trigger-adapter: run run-1 failed: flaky step"; c[0] != want {
		t.Errorf("comment = %q, want %q", c[0], want)
	}
}

// TestBoundedSettleAfterStreamEndsSilently: a deferred TERMINAL disposition
// whose stream ends WITHOUT a terminal envelope reaches the NoVerdict
// settle on the following poll — card untouched, record mirrored then
// pruned, ticket unblocked. The settle is bounded, never stuck.
func TestBoundedSettleAfterStreamEndsSilently(t *testing.T) {
	h, fk := mirrorHarness(t, 42, "Work in progress", nil)
	h.sub.dispState = pb.WorkflowAssignmentState_WORKFLOW_ASSIGNMENT_STATE_TERMINAL
	h.poll(t) // fires run-1; its (empty) watch stream ends without a verdict
	h.poll(t) // deferred record finalizes as NoVerdict and mirrors
	pollUntil(t, h, func() bool { return !hasRun(h, "run-1") })

	if col := fk.columnOf(42); col != "Work in progress" {
		t.Errorf("card column = %q, want untouched (NoVerdict moves nothing)", col)
	}
	if c := fk.commentsOn(42); len(c) != 0 {
		t.Errorf("NoVerdict settle commented: %v", c)
	}
	// The NoVerdict settle unblocks the ticket (settled runs are inert).
	fk.setCardColumn(42, "Ready")
	pollUntil(t, h, func() bool { return len(h.sub.submitted()) == 2 })
}

// TestPruneSettledMirroredRunsBoundedState: after a run settles and
// mirrors, the record disappears from the adapter map and from the
// persisted state file, and the run count stays bounded across N
// completed runs.
func TestPruneSettledMirroredRunsBoundedState(t *testing.T) {
	h, fk := mirrorHarness(t, 43, "Work in progress", nil)
	for n := 1; n <= 3; n++ {
		id := runIDOf(n)
		h.poll(t) // fires run-n
		h.watcher.setEvents(id, envStarted(id), envCompleted(id, "handler_complete"))
		pollUntil(t, h, func() bool {
			return cardIn(h, 43, "Done") && !hasRun(h, id)
		})
		if n < 3 {
			fk.setCardColumn(43, "Work in progress") // re-enter: next fire
		}
	}
	h.a.mu.Lock()
	inMap := len(h.a.runs)
	h.a.mu.Unlock()
	if inMap != 0 {
		t.Errorf("adapter run map holds %d records after settle+prune, want 0", inMap)
	}
	s, err := loadState(h.statePath)
	if err != nil {
		t.Fatalf("load state: %v", err)
	}
	if got := len(s.Runs); got != 0 {
		t.Errorf("state file kept %d run records, want 0", got)
	}
}

// TestWatchPumpsStopOnShutdown: the watch goroutines run on the adapter-
// scoped stream context and exit when Run returns.
func TestWatchPumpsStopOnShutdown(t *testing.T) {
	h, _ := mirrorHarness(t, 45, "Work in progress", nil)
	h.watcher.holdEvents(runIDOf(1)) // keep the pump parked and observable
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() { done <- h.a.Run(ctx) }()

	waitFor(t, 5*time.Second, "run-1 watch spawning", func() bool { return isWatching(h, "run-1") })
	if n := len(h.sub.submitted()); n != 1 {
		t.Fatalf("Run fired %d assignments, want 1", n)
	}
	sctx := h.watcher.streamCtxOf(runIDOf(1))
	if sctx == nil {
		t.Fatal("watch stream context was never captured")
	}

	cancel()
	select {
	case err := <-done:
		if err != context.Canceled {
			t.Errorf("Run err = %v, want context.Canceled", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Adapter.Run did not return after cancel")
	}
	waitFor(t, 5*time.Second, "pump exit", func() bool {
		return sctx.Err() == context.Canceled && !isWatching(h, "run-1")
	})
	quiescePumps(t, h.watcher, h.a)
}

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

// TestMirrorRunEventsSettleRecord: stream envelopes settle the record with
// a real verdict; the mirror applies the verdict exactly once (card moves
// to Done) and the settled-and-mirrored record is pruned — the persisted
// state file holds no run records, and further polls re-derive nothing
// (no extra moves, no resurrected records).
func TestMirrorRunEventsSettleRecord(t *testing.T) {
	h, fk := mirrorHarness(t, 30, "Work in progress", nil)
	h.watcher.setEvents(runIDOf(1), envStarted("run-1"), envCompleted("run-1", "handler_complete"))
	// The pump delivers at spawn; pruning requires exactly Settled &&
	// Mirrored, so "card Done and record gone" proves the settle and the
	// mirror both applied.
	pollUntil(t, h, func() bool {
		return cardIn(h, 30, "Done") && !hasRun(h, "run-1")
	})
	s, err := loadState(h.statePath)
	if err != nil {
		t.Fatalf("load state: %v", err)
	}
	if got := len(s.Runs); got != 0 {
		t.Errorf("state file kept %d run records, want 0", got)
	}
	for i := 0; i < 2; i++ {
		h.poll(t)
	}
	if moves := fk.moveTargets(); len(moves) != 1 {
		t.Errorf("moves = %v, want exactly one after further polls", moves)
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
	recordWhenSnap(t, h, "run-1", func(r RunRecord) bool {
		return r.Settled && r.Mirrored && r.Class == ClassTriage && r.Succeeded
	})
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
	recordWhenSnap(t, h, "run-1", func(r RunRecord) bool {
		return r.Settled && r.Mirrored
	})
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
	pollUntil(t, h, func() bool { return !hasRun(h, "run-1") })
	if col := fk.columnOf(36); col != "Work in progress" {
		t.Errorf("card column = %q, want untouched (abstain on unknown final state)", col)
	}
}

// TestMirrorRejectedDisposition: an assignment rejected before any run
// event keeps the card where it is and settles no-verdict — immediately,
// in the same poll that reads the disposition (no replayable verdict can
// exist for a run that never started).
func TestMirrorRejectedDisposition(t *testing.T) {
	h, fk := mirrorHarness(t, 37, "Work in progress", func(sub *fakeSubmitter) {
		sub.dispState = pb.WorkflowAssignmentState_WORKFLOW_ASSIGNMENT_STATE_REJECTED
		sub.rejectionReason = "route not deployed"
	})
	rec := recordWhenSnap(t, h, "run-1", func(r RunRecord) bool {
		return r.Settled && r.NoVerdict && r.Mirrored
	})
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
	pollUntil(t, h, func() bool { return !hasRun(h, "run-1") })
	if col := fk.columnOf(38); col != "Ready" {
		t.Errorf("card column = %q, want untouched (NoVerdict moves nothing)", col)
	}
	if c := fk.commentsOn(38); len(c) != 0 {
		t.Errorf("abstaining card commented: %v", c)
	}
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
	recordWhenSnap(t, h, "run-1", func(r RunRecord) bool {
		return r.Settled && r.Mirrored
	})
	if c := fk.commentsOn(39); len(c) != 0 {
		t.Errorf("abstaining card commented: %v", c)
	}
}
