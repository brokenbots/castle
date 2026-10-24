// Package reap implements the reaper verdict machinery shared by every store
// engine: candidate scans + in-transaction re-validation + the human-gate
// verdict classification and stamping (KB-226). Engines keep only their own
// plumbing on top — pool topology (sqlite's dedicated reader, serialized
// writer), transient-fault retry, and interrupt recovery — while the SQL and
// the verdict semantics stay lockstep across engines.
//
// All builders take a Dialect so the same statement text renders for
// ?-placeholder (sqlite) and $n-placeholder (pg) engines with placeholders
// and args kept in lockstep (CRI-143: a stale placeholder list failed whole
// reap passes).
package reap

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	"github.com/brokenbots/castle/castle/internal/store"
)

// Dialect renders the per-engine SQL grammar bits the shared builders need.
type Dialect interface {
	// P renders the placeholder for 1-based parameter position i ("?" or "$i").
	P(i int) string
	// Placeholders renders n placeholders starting at 1-based position first.
	Placeholders(first, n int) string
	// TS converts a timestamp to the engine's comparison value for
	// timestamp columns (RFC3339Nano text for sqlite's TEXT columns, the
	// time itself for pg's TIMESTAMPTZ).
	TS(t time.Time) any
}

// SQLiteDialect is the sqlite grammar: ?-placeholders, timestamp TEXT.
type SQLiteDialect struct{}

func (SQLiteDialect) P(_ int) string { return "?" }

func (SQLiteDialect) Placeholders(_ int, n int) string {
	out := ""
	for i := 0; i < n; i++ {
		if i > 0 {
			out += ","
		}
		out += "?"
	}
	return out
}

func (SQLiteDialect) TS(t time.Time) any { return t.Format(store.TimestampLayout) }

// PostgresDialect is the pg grammar: $n placeholders, native timestamps.
type PostgresDialect struct{}

func (PostgresDialect) P(i int) string { return "$" + fmt.Sprint(i) }

func (PostgresDialect) Placeholders(first, n int) string {
	out := ""
	for i := 0; i < n; i++ {
		if i > 0 {
			out += ","
		}
		out += "$" + fmt.Sprint(first+i)
	}
	return out
}

func (PostgresDialect) TS(t time.Time) any { return t }

// SQLite and Postgres are the dialect instances the engines use.
var (
	SQLite   Dialect = SQLiteDialect{}
	Postgres Dialect = PostgresDialect{}
)

// Runner is the query surface both *sql.DB and *sql.Tx satisfy.
type Runner interface {
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
}

// StaleCandidates scans for stale-agent reaper candidates (CRI-142): still
// pending/running runs of online agents whose heartbeat is older than
// staleBefore. Stopped runs are deliberately absent (CRI-207): a run an
// operator parked as stopped has no live agent heartbeat by design and is
// never reaped regardless of how old its agent's last heartbeat is.
// With ids empty this is the fresh reader-scan; with ids set it is the
// re-validation inside the write transaction (scan-then-write split, CRI-143):
// runs whose agent heartbeated or reached a terminal state in between are
// never clobbered.
func StaleCandidates(ctx context.Context, runner Runner, d Dialect, staleBefore time.Time, ids []string) ([]string, error) {
	q := `
		SELECT r.id
		FROM runs r
		JOIN overseers o ON o.id = r.overseer_id
		WHERE r.status IN ('pending', 'running') AND o.last_seen_at < ` + d.P(1)
	args := []any{d.TS(staleBefore)}
	if len(ids) > 0 {
		q += ` AND r.id IN (` + d.Placeholders(2, len(ids)) + `)`
		for _, id := range ids {
			args = append(args, id)
		}
	}
	q += ` ORDER BY r.created_at ASC, r.id ASC`
	return scanIDs(ctx, runner, q, args)
}

// NeverStartedCandidates scans for KB-233 rule-2 candidates: still
// pending/running runs with started_at still NULL and created_at older than
// createdBefore — a run record that was minted but whose execution never
// began. Paused and stopped runs are deliberately absent: a run an operator
// or agent parked is reaper-exempt regardless of age, because park states
// have no live execution by design (CRI-207) and stay resumable.
// With ids empty this is the fresh reader-scan; with ids set it is the
// re-validation inside the write transaction: runs that reached RunStarted or
// a terminal state between the scan and the write are never clobbered.
func NeverStartedCandidates(ctx context.Context, runner Runner, d Dialect, createdBefore time.Time, ids []string) ([]string, error) {
	q := `
		SELECT id
		FROM runs
		WHERE status IN ('pending', 'running') AND started_at IS NULL AND created_at < ` + d.P(1)
	args := []any{d.TS(createdBefore)}
	if len(ids) > 0 {
		q += ` AND id IN (` + d.Placeholders(2, len(ids)) + `)`
		for _, id := range ids {
			args = append(args, id)
		}
	}
	q += ` ORDER BY created_at ASC, id ASC`
	return scanIDs(ctx, runner, q, args)
}

// ReasonCreatedNeverStarted names the KB-233 rule-2 verdict: the run was
// created but execution never began. It is both the run's failure_reason and
// the workflow assignment's terminal_reason.
const ReasonCreatedNeverStarted = "created_never_started"

// StampVerdicts is the final write stage shared by both reapers. It must run
// on the caller's write transaction AFTER StaleCandidates/NeverStartedCandidates
// re-validated the candidate ids there, so the ids are known-current. From its
// own record each candidate is classified and stamped:
//
//   - Candidates whose latest persisted event parks at a human gate (KB-226)
//     are reclassified to awaiting_human: the engine reached the gate, so
//     stamping "agent heartbeat lost" would fail completed work. The
//     reclassification touches nothing but status — no ended_at, no
//     failure_reason, no workflow-assignment change — so the parked run stays
//     resumable. Classification happens here, inside the write transaction,
//     so a human-gate completion appended between the candidate scan and this
//     write can never be reaped.
//   - The remaining candidates are stamped terminal-failed with the calling
//     reaper's verdict reason and their workflow assignments marked terminal
//     (CRI-142).
//
// Returns one RunReapOutcome per stamped run, in the ids order.
func StampVerdicts(ctx context.Context, tx Runner, d Dialect, now time.Time, reapReason string, ids []string) ([]store.RunReapOutcome, error) {
	if len(ids) == 0 {
		return nil, nil
	}

	parked, err := ParkedReasons(ctx, tx, d, ids)
	if err != nil {
		return nil, err
	}

	parkedIDs := make([]string, 0, len(parked))
	reapedIDs := make([]string, 0, len(ids))
	outcomes := make([]store.RunReapOutcome, 0, len(ids))
	for _, id := range ids {
		if reason, ok := parked[id]; ok {
			parkedIDs = append(parkedIDs, id)
			outcomes = append(outcomes, store.RunReapOutcome{
				RunID:  id,
				Parked: true,
				Status: store.RunStatusAwaitingHuman,
				Reason: reason,
			})
			continue
		}
		reapedIDs = append(reapedIDs, id)
		outcomes = append(outcomes, store.RunReapOutcome{
			RunID:  id,
			Status: "failed",
			Reason: reapReason,
		})
	}

	if err := stampParkedRunsAwaitingHuman(ctx, tx, d, parkedIDs); err != nil {
		return nil, err
	}
	if err := stampReapedRunsTerminal(ctx, tx, d, now, reapReason, reapedIDs); err != nil {
		return nil, err
	}
	return outcomes, nil
}

func scanIDs(ctx context.Context, runner Runner, q string, args []any) ([]string, error) {
	rows, err := runner.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return nil, err
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return nil, err
	}
	rows.Close()
	return ids, nil
}

// ParkedReasons classifies the re-validated candidates inside the write
// transaction: the latest persisted event per run decides whether the run
// parks at a human gate (KB-226). Runs without events are absent from the
// result and follow the legacy reap path.
func ParkedReasons(ctx context.Context, runner Runner, d Dialect, ids []string) (map[string]string, error) {
	q := `
		SELECT e.run_id, e.type, e.payload
		FROM events e
		JOIN (SELECT run_id, MAX(seq) AS seq FROM events WHERE run_id IN (` + d.Placeholders(1, len(ids)) + `) GROUP BY run_id) l
		  ON l.run_id = e.run_id AND l.seq = e.seq`
	args := make([]any, 0, len(ids))
	for _, id := range ids {
		args = append(args, id)
	}
	rows, err := runner.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	parked := make(map[string]string, len(ids))
	for rows.Next() {
		var runID, evType string
		var payload []byte
		if err := rows.Scan(&runID, &evType, &payload); err != nil {
			return nil, err
		}
		if isParked, reason := LastEventParksHumanGate(evType, payload); isParked {
			parked[runID] = reason
		}
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return parked, nil
}

// stampParkedRunsAwaitingHuman reclassifies human-gate-parked candidates to
// the parked status (KB-226). The status guard keeps the write from
// clobbering a competing transition on this transaction's snapshot: only
// still-active runs move to awaiting_human, and the stamp touches nothing
// else — no ended_at, no failure_reason, no workflow-assignment change — so
// the parked run stays resumable.
func stampParkedRunsAwaitingHuman(ctx context.Context, tx Runner, d Dialect, ids []string) error {
	if len(ids) == 0 {
		return nil
	}
	q := `
		UPDATE runs
		SET status=` + d.P(1) + `
		WHERE id IN (` + d.Placeholders(2, len(ids)) + `) AND status IN ('pending', 'running')`
	args := make([]any, 0, len(ids)+1)
	args = append(args, store.RunStatusAwaitingHuman)
	for _, id := range ids {
		args = append(args, id)
	}
	_, err := tx.ExecContext(ctx, q, args...)
	return err
}

// stampReapedRunsTerminal is the failed-stamp stage of the shared reaper
// verdict pass: the given ids were re-validated on this transaction and carry
// no human-gate ending, so they are stamped terminal-failed with
// (ended_at, failure_reason) and their still queued or leased workflow
// assignments are marked terminal so reaped work is never re-queued or
// redelivered. The placeholder lists are derived from the re-validated ids —
// never from earlier, larger candidate lists — keeping placeholders and args
// in lockstep (CRI-143 regression: a stale placeholder list failed the whole
// pass with "missing argument with index N" whenever re-validation dropped a
// candidate). The status guards are defense in depth: terminal runs are never
// rewritten, even if a concurrent writer committed between the tx snapshot
// and the UPDATE.
func stampReapedRunsTerminal(ctx context.Context, tx Runner, d Dialect, now time.Time, reapReason string, ids []string) error {
	if len(ids) == 0 {
		return nil
	}
	args := make([]any, 0, len(ids)+2)
	args = append(args, d.TS(now), reapReason)
	for _, id := range ids {
		args = append(args, id)
	}
	if _, err := tx.ExecContext(ctx, `
		UPDATE runs
		SET status='failed', ended_at=`+d.P(1)+`, failure_reason=`+d.P(2)+`
		WHERE id IN (`+d.Placeholders(3, len(ids))+`) AND status IN ('pending', 'running')`,
		args...); err != nil {
		return err
	}

	asgArgs := make([]any, 0, len(ids)+5)
	asgArgs = append(asgArgs,
		store.WorkflowAssignmentStateTerminal, reapReason, d.TS(now))
	for _, id := range ids {
		asgArgs = append(asgArgs, id)
	}
	asgArgs = append(asgArgs,
		store.WorkflowAssignmentStateQueued, store.WorkflowAssignmentStateLeased)
	firstGuard := 4 + len(ids)
	_, err := tx.ExecContext(ctx, `
		UPDATE workflow_assignments
		SET state=`+d.P(1)+`, terminal_reason=`+d.P(2)+`, updated_at=`+d.P(3)+`
		WHERE run_id IN (`+d.Placeholders(4, len(ids))+`) AND state IN (`+d.Placeholders(firstGuard, 2)+`)`,
		asgArgs...)
	return err
}
