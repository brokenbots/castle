package kanboardtrigger

import (
	"context"
	"slices"
	"time"

	"github.com/brokenbots/castle/castle/internal/kanboard"
)

// poll runs one board cycle: read tasks, apply settled verdicts to columns
// (mirror), re-adopt watches and settle dispositions, then evaluate the
// firing gates (trigger loop). Entry memos are recorded for every polled
// task before any gate, so a card only ever fires when observed entering a
// lane column.
func (a *Adapter) poll(ctx context.Context) error {
	tasks, err := a.kb.GetAllTasks(ctx, a.boardProjectID)
	if err != nil {
		return err
	}
	a.adoptWatches(ctx)
	a.checkDispositions(ctx)
	a.reconcileMirrors(ctx, tasks)
	a.fire(ctx, tasks)
	a.flush()
	return nil
}

// fire evaluates every task against the firing gates and submits
// assignments for entries. Gates in order (each skip leaves the memo
// recorded, so a next entry still fires):
//
//  1. trigger tag: the card must carry the trigger tag (skip otherwise).
//  2. lane: the card's column must belong to a declared lane (skip if no
//     route covers the column).
//  3. in-flight: the ticket must have no unsettled run (one run at a time;
//     re-fires after settle, like the outgoing watcher's reconcile).
//  4. repo: the card must resolve a repo (repo:<owner>/<name> tag, else the
//     lane's default if it has one, else the config's repo-default; never
//     fire without a repo — resolved fail-closed, the card stays where it
//     is and the poll log carries the explanation).
//  5. entry: the card must be newly seen in the lane (first sighting or a
//     column change since the previous poll; KB-302 anti-refire discipline,
//     durable via the state file).
//
// The submitted payload merges the lane's static inputs with ticket_id
// (the one required run-level input of the develop workflow); repo binding
// stays a fire GATE, the payload stays lane-driven.
func (a *Adapter) fire(ctx context.Context, tasks []kanboard.Task) {
	for i := range tasks {
		task := &tasks[i]
		ticket := task.Identifier()

		prevID, known := 0, false
		a.mu.Lock()
		if memo := a.memos[ticket]; memo != nil {
			prevID, known = memo.PrevColumnID, true
		}
		a.mu.Unlock()
		entry := !known || prevID != task.ColumnID
		a.setMemo(ticket, task.ColumnID)

		if a.cfg.TriggerTag != "" && !slices.Contains(task.Tags, a.cfg.TriggerTag) {
			continue
		}
		lane := a.laneFor(task.ColumnName)
		if lane == nil {
			continue
		}
		if a.hasActiveRun(ticket) {
			continue
		}
		repoURL := a.cfg.RepoURL(lane, task.Tags)
		if repoURL == "" {
			a.log.WarnContext(ctx, "no repo resolvable for ticket (repo: tag or --repo-default)",
				"ticket", ticket, "tags", task.Tags, "project", a.cfg.BoardProject)
			continue
		}
		if !entry {
			continue
		}

		fireSeq := a.nextFireSeq(ticket)
		runID, err := a.submitAssignment(ctx, ticket, task.ID, lane, fireSeq)
		if err != nil {
			a.log.ErrorContext(ctx, "submitting assignment for ticket",
				"ticket", ticket, "lane", lane.RouteName,
				"fire_key", fireKey(a.cfg.BoardProject, ticket, fireSeq), "err", err)
			// Reset only the entry marker: the card re-enters next poll and
			// the fire retries with the SAME key — the deterministic fire
			// sequence survives the failure. Reverting the whole memo would
			// alias an older fire's key (silent dispatch loss) or orphan a
			// run the server already accepted.
			a.resetEntryMemo(ticket)
			continue
		}
		a.log.InfoContext(ctx, "fired assignment from board event",
			"ticket", ticket, "task", task.ID, "lane", lane.RouteName,
			"run", runID, "fire_key", fireKey(a.cfg.BoardProject, ticket, fireSeq))
		a.commitFire(ticket, fireSeq)
		a.recordRun(ctx, runID, ticket, task.ID, lane.RouteName, lane.Class)
	}
}

// recordRun stores a fired/adopted run and starts its mirror watch.
func (a *Adapter) recordRun(ctx context.Context, runID, ticket string, taskID int, route, class string) {
	a.mu.Lock()
	if _, exists := a.runs[runID]; exists {
		a.mu.Unlock()
		return // already tracked (adopted fire colliding with prior record)
	}
	a.runs[runID] = &RunRecord{
		RunID:            runID,
		TicketID:         ticket,
		TicketKanboardID: taskID,
		LaneRoute:        route,
		Class:            class,
		SubmittedAtMs:    time.Now().UnixMilli(),
	}
	a.markDirtyLocked()
	a.mu.Unlock()
	a.watchRun(ctx, runID)
}

// markWatched flags a run's stream attempt / drop (watching map) so poll
// boot adoption does not spawn duplicates.
func (a *Adapter) watchingLocked(runID string, live bool) {
	if live {
		a.watching[runID] = true
		return
	}
	delete(a.watching, runID)
}

func (a *Adapter) isWatching(runID string) bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.watching[runID]
}
