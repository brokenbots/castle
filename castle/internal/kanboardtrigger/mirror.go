package kanboardtrigger

import (
	"context"
	"errors"
	"fmt"
	"io"
	"sort"
	"strings"
	"time"

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
// persisted subscriber cursor so replays after a restart are safe. Streams
// run on the adapter-scoped stream context: they outlive individual poll
// cycles but are cancelled with the adapter.
func (a *Adapter) watchRun(runID string) {
	if a.isWatching(runID) {
		return
	}
	a.mu.Lock()
	a.watchingLocked(runID, true)
	a.mu.Unlock()

	a.pumps.Add(1)
	go func() {
		defer a.pumps.Done()
		sawTerminal, err := a.pumpRun(a.streamsCtx, runID)
		a.mu.Lock()
		a.watchingLocked(runID, false)
		a.mu.Unlock()
		// Shutdown cancels the stream; that is not a watch failure.
		if err != nil && a.streamsCtx.Err() == nil {
			a.log.Error("watching run events", "run", runID, "err", err)
		}
		// Bounded disposition settle (item: terminal-by-disposition may not
		// preempt a replayable verdict): if the watch deferred a TERMINAL
		// disposition and the stream ended WITHOUT delivering a terminal
		// envelope, finalize the NoVerdict settle now — the record is never
		// left unsettled forever. Shutdown skips finalization.
		if !sawTerminal && a.streamsCtx.Err() == nil {
			a.finalizeNoVerdict(runID)
		}
	}()
}

// pumpRun reads one WatchRun stream until the terminal envelope or a
// transport error, applying envelopes to the run record as they arrive.
// Reports whether a terminal envelope was delivered.
func (a *Adapter) pumpRun(ctx context.Context, runID string) (bool, error) {
	stream, err := a.watch.WatchRun(ctx, connect.NewRequest(&pb.WatchRunRequest{
		RunId:        runID,
		SubscriberId: a.subscriberID,
	}))
	if err != nil {
		return false, fmt.Errorf("opening watch stream for run %s: %w", runID, err)
	}
	sawTerminal := false
	for stream.Receive() {
		env := stream.Msg()
		a.applyEnvelope(runID, env)
		if criteria.IsTerminal(env) {
			sawTerminal = true
			return sawTerminal, nil
		}
	}
	if err := stream.Err(); err != nil {
		if errors.Is(err, io.EOF) || ctx.Err() != nil {
			return sawTerminal, nil
		}
		return sawTerminal, fmt.Errorf("reading watch stream for run %s: %w", runID, err)
	}
	return sawTerminal, nil
}

// applyEnvelope applies one run envelope to the run's record (idempotent:
// a replay of the same envelope rewrites the same verdict). A terminal
// envelope is ALWAYS applied, including to a record already settled as
// NoVerdict (terminal-by-disposition may not preempt a replayable verdict):
// it clears NoVerdict and re-arms the mirror so the real verdict moves the
// card on the next poll. Only a record holding a real (non-NoVerdict)
// verdict may ignore a replayed envelope. The column move happens on the
// next poll's reconcileMirrors.
func (a *Adapter) applyEnvelope(runID string, env *criteria.Envelope) {
	a.mu.Lock()
	defer a.mu.Unlock()
	r := a.runs[runID]
	if r == nil {
		return // unknown run (state loss or pruned while stream lives)
	}
	if r.Settled && !r.NoVerdict {
		return // already judged with a real verdict: replays are no-ops
	}
	r.SeenEvents = true
	switch {
	case env.GetRunFailed() != nil:
		r.Succeeded = false
		r.FailureReason = env.GetRunFailed().GetReason()
		r.FinalState = "failed"
		r.Settled = true
		r.NoVerdict = false
		r.TerminalByDisposition = false
		r.Mirrored = false
	case env.GetRunCompleted() != nil:
		r.Succeeded = true
		r.FinalState = env.GetRunCompleted().GetFinalState()
		r.Settled = true
		r.NoVerdict = false
		r.TerminalByDisposition = false
		r.Mirrored = false
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
// carries RejectionReason) settles immediately as NoVerdict — no replayable
// verdict can exist for a run that never started — and needs no later board
// pass (marked mirrored at settle). A TERMINAL disposition may NOT preempt
// a replayable terminal verdict: while that run's watch is live its replay
// (gaps are closed in the live phase) can still deliver the actual terminal
// envelope, so the settle is DEFERRED — the record stays unsettled (the
// ticket stays fire-blocked) and the disposition is re-read on later polls
// until the watch either delivers the envelope (which wins) or ends
// without one (the pump finalizes the NoVerdict settle). When no watch is
// live no replay can arrive anymore; the run settles as NoVerdict. A
// terminal WITH a real envelope settles by envelope above.
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
		r.TerminalByDisposition = false
		// A rejected assignment never streamed events: the record's mirror
		// cycle ends here (bounded; card untouched).
		r.Mirrored = true
	case pb.WorkflowAssignmentState_WORKFLOW_ASSIGNMENT_STATE_TERMINAL:
		// Deferral budget: bound how long an unsettled TERMINAL record may
		// block the ticket while its watch is live. WatchRun tails live
		// indefinitely (no idle timeout) and the reaper publishes no
		// envelope, so "wait for the stream" is not a bound on its own —
		// DeferSinceMs (persisted, hence restart-safe) starts at the first
		// deferral and its expiry settles the record NoVerdict even while
		// the stream is still open. The marker stays set through a budget
		// settle so the mirror cycle still completes on a live watch; a
		// terminal envelope arriving before the budget expires still wins
		// via applyEnvelope (as does a late one while the record survives
		// the next prune — the bounded contract).
		if r.TerminalByDisposition && a.deferralExpired(r) {
			r.Settled = true
			r.NoVerdict = true
			// FailureReason/FinalState stay empty and SeenEvents is not
			// overridden: no envelope was observed, mirror leaves the card
			// untouched. TerminalByDisposition is kept (see above).
			a.log.Info("deferral budget exhausted; settling run without a verdict", "run", runID)
			break
		}
		if a.watching[runID] { // read the map directly: helpers lock
			// The watch replay is still pending: stay unsettled (ticket
			// fire-blocked) until the envelope applies or the deferral
			// budget expires — a later poll applies whichever lands first.
			if !r.TerminalByDisposition {
				r.TerminalByDisposition = true
				r.DeferSinceMs = time.Now().UnixMilli()
				a.markDirtyLocked()
				a.log.Info("deferring disposition settle; watch replay still pending", "run", runID)
			}
			break
		}
		r.Settled = true
		r.TerminalByDisposition = false
		if !r.SeenEvents {
			r.NoVerdict = true
		}
	}
	if r.Settled {
		a.log.Info("settling run by assignment disposition", "run", runID, "state", st.String())
		a.persistLocked()
	}
}

// deferralExpired reports whether the persisted deferral budget is
// exhausted (deadline counted from the first TERMINAL observation). A
// zero DeferSinceMs — records written before the field existed — counts
// as expired, which keeps the settle bounded for every persisted shape.
func (a *Adapter) deferralExpired(r *RunRecord) bool {
	return time.Now().UnixMilli()-r.DeferSinceMs >= a.deferralBudget.Milliseconds()
}

// finalizeNoVerdict settles a deferred terminal-disposition record whose
// watch ended WITHOUT delivering a terminal envelope: the run reached the
// disposition's terminal state but its events were never observed — no
// column move is derived (NoVerdict). Called by the watch goroutine after
// the stream closes; bounded (one settle, never re-run).
func (a *Adapter) finalizeNoVerdict(runID string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	r := a.runs[runID]
	if r == nil || r.Settled || !r.TerminalByDisposition {
		return
	}
	r.Settled = true
	r.NoVerdict = true
	a.log.Info("watch ended without a terminal envelope; settling run by disposition", "run", runID)
	a.persistLocked()
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
func (a *Adapter) adoptWatches() {
	for _, runID := range a.activeRunIDs() {
		if !a.isWatching(runID) {
			a.watchRun(runID)
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
			// While the watch is live a terminal envelope may still arrive,
			// unless the record was settled by the deferral budget
			// (marked TerminalByDisposition, kept through the settle): its
			// mirror cycle is bounded and completes now.
			if a.isWatching(rec.RunID) && !rec.TerminalByDisposition {
				continue
			}
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
// card already in Review gets no move and no comment (the obligation is
// waived — it was either discharged by an earlier move or the card was
// pre-parked), and a comment whose earlier post failed is retried here.
// The record is marked mirrored only when the comment obligation stands;
// a failed comment leaves it unmarked (retried next poll; the park must
// not be undone).
func (a *Adapter) parkFailureReview(ctx context.Context, task *kanboard.Task, rec RunRecord) {
	if task.ColumnName == a.cfg.ReviewColumn {
		// Already parked: either a failed comment from an earlier poll (its
		// post still owed — exactly-one-comment), or the obligation is
		// discharged/waived.
		owesRetry := false
		a.mu.Lock()
		if r := a.runs[rec.RunID]; r != nil {
			switch {
			case r.ParkCommented:
				// comment already posted; nothing owed
			case r.ParkFailed:
				owesRetry = true // earlier post failed; repost per poll
			default:
				r.CommentWaived = true // pre-parked card: never a comment debt
				a.markDirtyLocked()
			}
		}
		a.mu.Unlock()
		if owesRetry && !a.postFailureComment(ctx, task.ID, rec) {
			return // comment still owed: unmarked, retried next poll
		}
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
	a.adoptMirrorMove(ctx, task, rec.TicketID)
	if !a.postFailureComment(ctx, task.ID, rec) {
		return // comment retry owed: the parked card is re-observed next poll
	}
	a.markMirrored(rec.RunID)
}

// postFailureComment posts the single failure explanation. Returns whether
// the comment obligation now stands (posted now or previously); a failed
// post marks the record ParkFailed so the exactly-one-comment debt is
// retried on a later poll. Only the run's failure reason is echoed into
// the comment; card content never is.
func (a *Adapter) postFailureComment(ctx context.Context, taskID int, rec RunRecord) bool {
	reason := rec.FailureReason
	if strings.TrimSpace(reason) == "" {
		reason = "no reason recorded"
	}
	body := fmt.Sprintf("kanboard-trigger-adapter: run %s failed: %s", rec.RunID, reason)
	if err := a.kb.AddComment(ctx, taskID, body); err != nil {
		if ctx.Err() == nil {
			a.log.ErrorContext(ctx, "posting failure-park comment",
				"ticket", rec.TicketID, "task", taskID, "run", rec.RunID, "err", err)
		}
		a.mu.Lock()
		if r := a.runs[rec.RunID]; r != nil {
			r.ParkFailed = true
			a.markDirtyLocked()
		}
		a.mu.Unlock()
		return false
	}
	a.mu.Lock()
	if r := a.runs[rec.RunID]; r != nil {
		r.ParkCommented = true
		r.ParkFailed = false
	}
	a.markDirtyLocked()
	a.mu.Unlock()
	a.log.InfoContext(ctx, "posted failure-park comment", "ticket", rec.TicketID, "run", rec.RunID)
	return true
}

// adoptMirrorMove records the mirror's own move in the ticket's entry memo
// (KB-302): the memo's prev-column is "the column the adapter observed the
// card in last", and a mirrored move is an observation — without it an
// operator reopening a card from Done within one poll interval of the
// mirror move would be invisible (memo still holding the pre-run column)
// and the re-entry would never fire. It also updates the poll's task
// snapshot, which fire compares the memo against: the board was moved
// after the snapshot was fetched, so leaving the snapshot stale would
// read the move as a fresh entry and fire a duplicate run. Best effort: a
// failed refresh leaves the memo and the snapshot untouched; the next
// poll's fire re-observes the card anyway.
func (a *Adapter) adoptMirrorMove(ctx context.Context, task *kanboard.Task, ticket string) {
	t, err := a.kb.GetTask(ctx, task.ID)
	if err != nil {
		if ctx.Err() == nil {
			a.log.ErrorContext(ctx, "observing mirrored card", "task", task.ID, "err", err)
		}
		return
	}
	a.setMemo(ticket, t.ColumnID)
	task.ColumnID = t.ColumnID
	if name := a.kb.TaskColumnName(ctx, t); name != "" {
		task.ColumnName = name
	}
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
	a.adoptMirrorMove(ctx, task, rec.TicketID)
	a.markMirrored(rec.RunID)
}
