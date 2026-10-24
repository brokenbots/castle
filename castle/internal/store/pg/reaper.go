package pg

import (
	"context"
	"time"

	"github.com/brokenbots/castle/castle/internal/store"
	"github.com/brokenbots/castle/castle/internal/store/reap"
)

func (s *Store) ReapStaleAgentRuns(ctx context.Context, now time.Time, staleBefore time.Time) ([]store.RunReapOutcome, error) {
	ctx, cancel := s.opCtx(ctx)
	defer cancel()
	outcomes, err := retryOnTransient(ctx, reapRetryPolicy, func() ([]store.RunReapOutcome, error) {
		return s.reapStaleAgentRunsAttempt(ctx, now, staleBefore)
	})
	if isInterrupt(err) {
		s.recoverWriter(ctx)
	}
	return outcomes, err
}

// recoverWriter forces a health round-trip on the connection pool. On
// Postgres it never fires (isInterrupt is always false — no driver-level
// interrupt analogue), but the hook is kept so both engines share the reaper
// shape (KB-222 parity).
func (s *Store) recoverWriter(ctx context.Context) {
	pingCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	_ = s.db.PingContext(pingCtx)
}

// reapStaleAgentRunsAttempt performs one reaping pass. The candidate scan is a
// plain read on the pool: with nothing to reap — the common case — no write
// happens at all.
func (s *Store) reapStaleAgentRunsAttempt(ctx context.Context, now time.Time, staleBefore time.Time) ([]store.RunReapOutcome, error) {
	// Bound the pass so contention fails the tick instead of pinning the
	// reaper goroutine. Each retry attempt gets a fresh budget.
	ctx, cancel := context.WithTimeout(ctx, reapAttemptTimeout)
	defer cancel()

	// Stopped runs are deliberately absent from the scan (CRI-207): a run an
	// operator parked as stopped has no live agent heartbeat by design and is
	// never reaped regardless of how old its agent's last heartbeat is.
	ids, err := reap.StaleCandidates(ctx, s.reader, reap.Postgres, staleBefore, nil)
	if err != nil {
		return nil, err
	}
	if len(ids) == 0 {
		return nil, nil
	}

	return s.reapRunIDs(ctx, now, staleBefore, ids, store.ReapReasonAgentHeartbeatLost)
}

// reapRunIDs stamps the given candidates on the pool. The candidates were
// picked from a snapshot that may already be stale, so the transaction
// re-validates staleness and reapability against current data (the same
// shared candidate predicate, scoped to an IN-list): runs whose agent
// heartbeated or reached a terminal state in between are never clobbered
// (CRI-142 terminal invariants hold even under the scan-then-write split).
// Classification and stamping are delegated to the shared verdict stage
// (KB-226), which reads the candidate's own record inside this transaction.
func (s *Store) reapRunIDs(ctx context.Context, now time.Time, staleBefore time.Time, candidates []string, reapReason string) ([]store.RunReapOutcome, error) {
	// Nothing to reap: return before opening the transaction. The candidate
	// placeholder list cannot even be derived from zero ids, so an empty
	// candidate list must not reach the query builders below.
	if len(candidates) == 0 {
		return nil, nil
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback() }()

	// Re-validate inside the write transaction against the same staleness
	// criteria as the scan (CRI-207: stopped runs are reaper-exempt).
	ids, err := reap.StaleCandidates(ctx, tx, reap.Postgres, staleBefore, candidates)
	if err != nil {
		return nil, err
	}
	if len(ids) == 0 {
		_ = tx.Rollback()
		return nil, nil
	}

	outcomes, err := reap.StampVerdicts(ctx, tx, reap.Postgres, us(now), reapReason, ids)
	if err != nil {
		return nil, err
	}
	return outcomes, tx.Commit()
}

// ReapNeverStartedRuns stamps runs in status pending or running as failed with
// reason "created_never_started" (KB-233 rule 2) when started_at is still NULL
// and created_at is older than createdBefore. See the sqlite profile for the
// full invariant commentary — the semantics are shared with the sqlite engine
// through the reaper stage.
func (s *Store) ReapNeverStartedRuns(ctx context.Context, now time.Time, createdBefore time.Time) ([]store.RunReapOutcome, error) {
	ctx, cancel := s.opCtx(ctx)
	defer cancel()
	outcomes, err := retryOnTransient(ctx, reapRetryPolicy, func() ([]store.RunReapOutcome, error) {
		return s.reapNeverStartedRunsAttempt(ctx, now, createdBefore)
	})
	if isInterrupt(err) {
		s.recoverWriter(ctx)
	}
	return outcomes, err
}

func (s *Store) reapNeverStartedRunsAttempt(ctx context.Context, now time.Time, createdBefore time.Time) ([]store.RunReapOutcome, error) {
	// Bound the pass so contention fails the tick instead of pinning the
	// reaper goroutine. Each retry attempt gets a fresh budget.
	ctx, cancel := context.WithTimeout(ctx, reapAttemptTimeout)
	defer cancel()

	ids, err := reap.NeverStartedCandidates(ctx, s.reader, reap.Postgres, createdBefore, nil)
	if err != nil {
		return nil, err
	}
	if len(ids) == 0 {
		return nil, nil
	}

	return s.reapNeverStartedRunIDs(ctx, now, createdBefore, ids)
}

func (s *Store) reapNeverStartedRunIDs(ctx context.Context, now time.Time, createdBefore time.Time, candidates []string) ([]store.RunReapOutcome, error) {
	// Nothing to reap: return before opening the transaction. An empty
	// candidate list must not reach the query builders below.
	if len(candidates) == 0 {
		return nil, nil
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback() }()

	// Re-validate inside the write transaction against the same criteria as
	// the scan: only still-unstarted, still-active runs older than
	// createdBefore survive into the UPDATE.
	ids, err := reap.NeverStartedCandidates(ctx, tx, reap.Postgres, createdBefore, candidates)
	if err != nil {
		return nil, err
	}
	if len(ids) == 0 {
		_ = tx.Rollback()
		return nil, nil
	}

	outcomes, err := reap.StampVerdicts(ctx, tx, reap.Postgres, us(now), reap.ReasonCreatedNeverStarted, ids)
	if err != nil {
		return nil, err
	}
	return outcomes, tx.Commit()
}

// CancelRun stamps runID terminal as "cancelled" with the given reason
// (CRI-142). The conditional update never rewrites an already terminal run,
// so it cannot race a concurrent agent SubmitEvents terminal stamp into a
// regression: whoever wins, the run stays terminal. Unknown run ids return
// ErrNotFound; a run already terminal returns ErrRunTerminal. The updated run
// record is returned on success.
func (s *Store) CancelRun(ctx context.Context, runID, reason string, now time.Time) (*store.Run, error) {
	ctx, cancel := s.opCtx(ctx)
	defer cancel()
	res, err := s.db.ExecContext(ctx,
		`UPDATE runs
		 SET status='cancelled', ended_at=$1, failure_reason=COALESCE(NULLIF($2, ''), failure_reason)
		 WHERE id=$3 AND status NOT IN ('succeeded', 'failed', 'cancelled')`,
		us(now), nonEmpty(reason), runID)
	if err != nil {
		return nil, err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return nil, err
	}
	if n == 0 {
		if _, getErr := s.GetRun(ctx, runID); getErr != nil {
			return nil, getErr
		}
		return nil, store.ErrRunTerminal
	}
	return s.GetRun(ctx, runID)
}
