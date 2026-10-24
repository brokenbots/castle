package kanboardtrigger

import (
	"context"
	"errors"
	"fmt"
	"io"
	"sort"
	"strings"

	"connectrpc.com/connect"

	criteria "github.com/brokenbots/criteria/sdk"
	pb "github.com/brokenbots/criteria/sdk/pb/criteria/v1"

	"github.com/brokenbots/castle/castle/internal/kanboard"
)

// mirror.go turns castle run events into board column transitions: every
// unsettled run gets a WatchRun stream (server-persisted cursors keyed by
// this adapter's subscriber id; adoptWatches re-spawns dead streams every
// poll), settled verdicts settle the record, and reconcileMirrors applies
// verdicts to the card column (the outgoing watcher's reconcile, sourced
// from assignment runs instead of a run scrape).
//
// Stream goroutines touch run records only through the mutex-guarded
// helpers below; the moves run on the poll goroutine.

// watchRun spawns one event stream for a run (best effort: a failed watch
// is retried on the next poll). The request resumes from the server-
// persisted subscriber cursor so replays after a restart are safe.
func (a *Adapter) watchRun(ctx context.Context, runID string) {
	if a.isWatching(runID) {
		return
	}
	a.mu.Lock()
	a.watchingLocked(runID, true)
	a.mu.Unlock()

	streamCtx := context.WithoutCancel(ctx) // stream outlives the poll cycle
	go func() {
		err := a.pumpRun(streamCtx, runID)
		a.mu.Lock()
		a.watchingLocked(runID, false)
		a.mu.Unlock()
		if err != nil && streamCtx.Err() == nil {
			a.log.Error("watching run events", "run", runID, "err", err)
		}
	}()
}

// pumpRun reads one WatchRun stream until the terminal envelope or a
// transport error, applying envelopes to the run record as they arrive.
func (a *Adapter) pumpRun(ctx context.Context, runID string) error {
	stream, err := a.watch.WatchRun(ctx, connect.NewRequest(&pb.WatchRunRequest{
		RunId:        runID,
		SubscriberId: a.subscriberID,
	}))
	if err != nil {
		return fmt.Errorf("opening watch stream for run %s: %w", runID, err)
	}
	for stream.Receive() {
		env := stream.Msg()
		a.applyEnvelope(runID, env)
		if criteria.IsTerminal(env) {
			return nil
		}
	}
	if err := stream.Err(); err != nil {
		if errors.Is(err, io.EOF) || ctx.Err() != nil {
			return nil
		}
		return fmt.Errorf("reading watch stream for run %s: %w", runID, err)
	}
	return nil
}

// applyEnvelope applies one run envelope to the run's record (idempotent:
// a replay of the same envelope rewrites the same verdict). A terminal
// envelope settles the record; the column move happens on the next poll's
// reconcileMirrors.
func (a *Adapter) applyEnvelope(runID string, env *criteria.Envelope) {
	a.mu.Lock()
	defer a.mu.Unlock()
	r := a.runs[runID]
	if r == nil || r.Settled {
		return // unknown run (state loss while stream lives) or already judged
	}
	r.SeenEvents = true
	switch {
	case env.GetRunFailed() != nil:
		r.Succeeded = false
		r.FailureReason = env.GetRunFailed().GetReason()
		r.FinalState = "failed"
		r.Settled = true
	case env.GetRunCompleted() != nil:
		r.Succeeded = true
		r.FinalState = env.GetRunCompleted().GetFinalState()
		r.Settled = true
	}
	a.persistLocked()
}

// persistLocked snapshots state (callers hold a.mu).
func (a *Adapter) persistLocked() {
	if err := (fileStore{a.statePath}).save(&state{Tickets: a.memos, Runs: a.runs}); err != nil {
		a.log.Error("persisting run state", "err", err)
	}
}

// checkDispositions asks castle for every active run's assignment
// disposition, settling runs whose assignments ended without observable
// envelopes. Called each poll; one query per active run.
func (a *Adapter) checkDispositions(ctx context.Context) {
	for _, runID := range a.activeRunIDs() {
		resp, err := a.dispose.GetAssignmentDisposition(ctx,
			connect.NewRequest(&pb.GetAssignmentDispositionRequest{RunId: runID}))
		if err != nil {
			if ctx.Err() == nil {
				a.log.ErrorContext(ctx, "reading assignment disposition", "run", runID, "err", err)
			}
			continue // settled on a later poll
		}
		a.settleByDisposition(runID, resp.Msg.GetState(), resp.Msg.GetRejectionReason())
	}
}

// settleByDisposition marks a run settled when its assignment ended without
// a run ever streaming observable events: REJECTED (never ran; disposition
// carries RejectionReason), or TERMINAL with no envelopes seen (already
// ended before this adapter ever watched). Both settle as NoVerdict — no
// column move is derived from them, cards stay untouched. A terminal WITH
// envelopes settles by envelope above.
func (a *Adapter) settleByDisposition(runID string, st pb.WorkflowAssignmentState, reason string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	r := a.runs[runID]
	if r == nil || r.Settled {
		return
	}
	switch st {
	case pb.WorkflowAssignmentState_WORKFLOW_ASSIGNMENT_STATE_REJECTED:
		r.Settled = true
		r.FailureReason = reason
		r.FinalState = ""
		r.Succeeded = false
		r.NoVerdict = true
	case pb.WorkflowAssignmentState_WORKFLOW_ASSIGNMENT_STATE_TERMINAL:
		r.Settled = true
		if !r.SeenEvents {
			r.NoVerdict = true
		}
	}
	if r.Settled {
		a.log.Info("settling run by assignment disposition", "run", runID, "state", st.String())
		a.persistLocked()
	}
}

// activeRunIDs returns the run ids without a settle verdict.
func (a *Adapter) activeRunIDs() []string {
	a.mu.Lock()
	defer a.mu.Unlock()
	ids := make([]string, 0, len(a.runs))
	for id, r := range a.runs {
		if !r.Settled {
			ids = append(ids, id)
		}
	}
	sort.Strings(ids)
	return ids
}

// adoptWatches re-spawns event streams for every unsettled run without a
// live one: boot adoption of persisted state and recovery of streams lost
// to transport errors. Idempotent via the watching map.
func (a *Adapter) adoptWatches(ctx context.Context) {
	for _, runID := range a.activeRunIDs() {
		if !a.isWatching(runID) {
			a.watchRun(ctx, runID)
		}
	}
}

// unmirroredRecords returns settled records whose verdict still needs a
// board move, ordered oldest-first (deterministic across restarts). Copies:
// the poll goroutine works on them; marking goes back through markMirrored.
func (a *Adapter) unmirroredRecords() []RunRecord {
	a.mu.Lock()
	defer a.mu.Unlock()
	out := make([]RunRecord, 0, len(a.runs))
	for _, r := range a.runs {
		if r.Settled && !r.Mirrored {
			out = append(out, *r)
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].SubmittedAtMs != out[j].SubmittedAtMs {
			return out[i].SubmittedAtMs < out[j].SubmittedAtMs
		}
		return out[i].RunID < out[j].RunID
	})
	return out
}

// markMirrored closes a record's mirror cycle (verdict applied or
// deliberately abstained).
func (a *Adapter) markMirrored(runID string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if r := a.runs[runID]; r != nil {
		r.Mirrored = true
		a.markDirtyLocked()
	}
}

// findTask resolves a record's card from the board listing, falling back to
// a direct task fetch for cards outside the listing (e.g. closed): moves
// and comments key by task id and remain applicable.
func (a *Adapter) findTask(ctx context.Context, tasks []kanboard.Task, taskID int) *kanboard.Task {
	for i := range tasks {
		if tasks[i].ID == taskID {
			return &tasks[i]
		}
	}
	t, err := a.kb.GetTask(ctx, taskID)
	if err != nil {
		if ctx.Err() == nil {
			a.log.ErrorContext(ctx, "fetching task for mirror", "task", taskID, "err", err)
		}
		return nil
	}
	t.ColumnName = a.kb.TaskColumnName(ctx, t)
	return t
}

// reconcileMirrors applies settled verdicts to card columns. Port of the
// outgoing watcher's reconcileTaskColumn verdict table:
//
//   - no observable verdict (rejected / unobserved terminal) → no move
//     (the card stays where a human left it).
//   - failed run (RunFailed) → park the card in Review once, with exactly
//     one failure comment posted AT the move (KB-302: repeated moves skip
//     the comment; a comment failure is best-effort).
//   - succeeded + triage lane → the card stays where the run left it (KB-9).
//   - succeeded + develop lane, final state handler_complete → move Done
//     (KB-50).
//   - succeeded + develop lane, final state failed/need_assistance/
//     awaiting_human → repair only when the card sits in Done: back to
//     Review (KB-269); no comment.
//   - any other final state → abstain, card stays (log for the scrape).
func (a *Adapter) reconcileMirrors(ctx context.Context, tasks []kanboard.Task) {
	for _, rec := range a.unmirroredRecords() {
		task := a.findTask(ctx, tasks, rec.TicketKanboardID)
		if task == nil {
			continue // fetch failed; retried next poll
		}
		switch {
		case rec.NoVerdict:
			a.log.InfoContext(ctx, "assignment settled without observable verdict; card untouched",
				"ticket", rec.TicketID, "run", rec.RunID)
			a.markMirrored(rec.RunID)
		case !rec.Succeeded:
			a.parkFailureReview(ctx, task, rec)
		case rec.Class == ClassTriage:
			a.log.InfoContext(ctx, "triage run succeeded; card stays (KB-9)",
				"ticket", rec.TicketID, "run", rec.RunID, "column", task.ColumnName)
			a.markMirrored(rec.RunID)
		case rec.Class == ClassDevelop && rec.FinalState == "handler_complete":
			a.moveCard(ctx, task, a.cfg.DoneColumn, rec, "develop run completed; moving to Done (KB-50)")
		case rec.Class == ClassDevelop && handoffStates[rec.FinalState]:
			if task.ColumnName == a.cfg.DoneColumn {
				a.moveCard(ctx, task, a.cfg.ReviewColumn, rec, "handoff run reopens a Done card (KB-269); moving to Review")
			} else {
				a.log.InfoContext(ctx, "handoff final state on non-Done card; card stays",
					"ticket", rec.TicketID, "run", rec.RunID, "final_state", rec.FinalState, "column", task.ColumnName)
				a.markMirrored(rec.RunID)
			}
		default:
			a.log.InfoContext(ctx, "unknown final state; card stays",
				"ticket", rec.TicketID, "run", rec.RunID, "final_state", rec.FinalState)
			a.markMirrored(rec.RunID)
		}
	}
}

// handoffStates are the develop final states that only matter when the card
// already reached Done.
var handoffStates = map[string]bool{
	"failed":          true,
	"need_assistance": true,
	"awaiting_human":  true,
}

// parkFailureReview moves a failed run's card to Review and posts the one
// failure comment at the move (KB-302). The move is the comment gate: a
// card already in Review gets no move and no comment; a failed comment is
// logged, not propagated (the park must not be undone).
func (a *Adapter) parkFailureReview(ctx context.Context, task *kanboard.Task, rec RunRecord) {
	if task.ColumnName == a.cfg.ReviewColumn {
		a.log.InfoContext(ctx, "failed run's card already parked; no move, no comment",
			"ticket", rec.TicketID, "run", rec.RunID)
		a.markMirrored(rec.RunID)
		return
	}
	if _, err := a.kb.MoveTaskToColumn(ctx, task.ID, a.cfg.ReviewColumn); err != nil {
		if ctx.Err() == nil {
			a.log.ErrorContext(ctx, "parking failed run's card in Review",
				"ticket", rec.TicketID, "task", task.ID, "run", rec.RunID, "err", err)
		}
		return // unmarked: retried next poll
	}
	a.log.InfoContext(ctx, "parked failed run's card in Review",
		"ticket", rec.TicketID, "task", task.ID, "run", rec.RunID)
	if !rec.ParkCommented {
		a.postFailureComment(ctx, task.ID, rec)
	}
	a.markMirrored(rec.RunID)
}

// postFailureComment posts the single failure explanation. Best-effort:
// a failure stays in the log; the park already happened. Only the run's
// failure reason is echoed into the comment; card content never is.
func (a *Adapter) postFailureComment(ctx context.Context, taskID int, rec RunRecord) {
	reason := rec.FailureReason
	if strings.TrimSpace(reason) == "" {
		reason = "no reason recorded"
	}
	body := fmt.Sprintf("kanboard-trigger-adapter: run %s failed: %s", rec.RunID, reason)
	if err := a.kb.AddComment(ctx, taskID, body); err != nil {
		a.log.ErrorContext(ctx, "posting failure-park comment",
			"ticket", rec.TicketID, "task", taskID, "run", rec.RunID, "err", err)
		return
	}
	a.mu.Lock()
	if r := a.runs[rec.RunID]; r != nil {
		r.ParkCommented = true
	}
	a.markDirtyLocked()
	a.mu.Unlock()
	a.log.InfoContext(ctx, "posted failure-park comment", "ticket", rec.TicketID, "run", rec.RunID)
}

// moveCard moves a card to a column and marks the record mirrored (a failed
// move leaves the record unmarked: retried next poll).
func (a *Adapter) moveCard(ctx context.Context, task *kanboard.Task, columnName string, rec RunRecord, why string) {
	if task.ColumnName == columnName {
		a.log.InfoContext(ctx, "card already in target column",
			"ticket", rec.TicketID, "run", rec.RunID, "column", columnName)
		a.markMirrored(rec.RunID)
		return
	}
	if _, err := a.kb.MoveTaskToColumn(ctx, task.ID, columnName); err != nil {
		if ctx.Err() == nil {
			a.log.ErrorContext(ctx, "moving card", "ticket", rec.TicketID,
				"task", task.ID, "run", rec.RunID, "column", columnName, "err", err)
		}
		return
	}
	a.log.InfoContext(ctx, why, "ticket", rec.TicketID, "run", rec.RunID, "column", columnName)
	a.markMirrored(rec.RunID)
}
