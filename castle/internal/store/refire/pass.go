package refire

import (
	"context"
	"fmt"
	"time"

	"github.com/google/uuid"

	"github.com/brokenbots/castle/castle/internal/store"
	"github.com/brokenbots/castle/castle/internal/store/reap"
)

// Candidate eligibility (KB-321): the reaper failure stamps, the agent
// failure stamps and the designed-park completions (KB-270) are the endings
// policy evaluates. Cancelled, stopped, parked, succeeded runs and rejected
// assignments are never candidates; the r.status='failed' guard keeps the
// run status authoritative for every case.
const candidateEligibleReasons = "'run failed', 'run completed', '" +
	store.ReapReasonAgentHeartbeatLost + "', '" + reap.ReasonCreatedNeverStarted + "'"

// refireFamilyKey is the per-row family-key expression (KB-321): an original
// attempt carries an empty refire_root_run_id and keys its family by its own
// run id; every refire successor carries the family root's run id.
const refireFamilyKey = "COALESCE(NULLIF(w.refire_root_run_id, ''), w.run_id)"

// refireFamilyKeyOther is the same expression over the family-scan alias.
const refireFamilyKeyOther = "COALESCE(NULLIF(w2.refire_root_run_id, ''), w2.run_id)"

// ScanLimit is the candidate-scan working-set bound used by the refire
// dispatcher (KB-321). Rows evaluated to a final disposition carry a
// non-NULL runs.refire_disposition stamp and age out of the set, so the
// bound is a batch size, not a starvation risk.
const ScanLimit = 200

// candidateProjection is the refire-candidate projection (both engines share
// column names): the full assignment row (aliased w.*) plus the run facts the
// policy evaluation needs.
const candidateProjection = `
	w.id AS a_id, w.owner_criteria_id AS a_owner, w.run_id AS a_run_id,
	w.workflow_name AS a_wf_name, w.workflow_source AS a_wf_source,
	w.lockfile_source AS a_lockfile, w.idempotency_key AS a_key,
	w.state AS a_state, w.terminal_reason AS a_term_reason,
	w.leased_criteria_id AS a_leased, w.lease_expires_at AS a_lease_exp,
	w.created_at AS a_created, w.updated_at AS a_updated,
	w.project_id AS a_project, w.input_values AS a_inputs,
	w.target_metadata AS a_target,
	w.refire_root_run_id AS a_refire_root, w.refire_attempt AS a_refire_attempt,
	r.failure_reason AS r_failure_reason, r.ended_at AS r_ended_at`

// CandidatesQuery renders the refire-candidate scan (both engines): the
// newest failed attempt of every eligible family, freshest failure first.
// A run evaluated to a final disposition (refire / park / cosmetic / none)
// carries a non-NULL runs.refire_disposition stamp and ages out of the scan,
// so permanent no-action endings can never displace a fresh failure from the
// working set; deferred and budget-exhausted rows stay unstamped and
// revisitable (policy retunes must reach them). The family-max guard means
// each pass evaluates only the newest attempt a family ever reached, so a
// successor already minted elsewhere can never double-refire a lineage.
// limit <= 0 means no limit.
func CandidatesQuery(d reap.Dialect, limit int) (string, []any) {
	q := `
		SELECT ` + candidateProjection + `
		FROM workflow_assignments w
		JOIN runs r ON r.id = w.run_id
		WHERE w.state = '` + store.WorkflowAssignmentStateTerminal + `'
		  AND w.terminal_reason IN (` + candidateEligibleReasons + `)
		  AND r.status = 'failed'
		  AND r.refire_disposition IS NULL
		  AND w.refire_attempt = (
		      SELECT MAX(w2.refire_attempt)
		      FROM workflow_assignments w2
		      WHERE ` + refireFamilyKeyOther + ` = ` + refireFamilyKey + `
		  )
		ORDER BY r.ended_at DESC, w.id DESC`
	args := []any{}
	if limit > 0 {
		q += ` LIMIT ` + d.P(1)
		args = append(args, limit)
	}
	return q, args
}

// LoadPolicies reads the (project, route, policy) material for resolution
// inside the caller's transaction, keeping resolution and evaluation on one
// consistent snapshot. Stamps are not read: resolution needs only the
// documents.
func LoadPolicies(ctx context.Context, runner reap.Runner) ([]store.RefirePolicyRow, error) {
	rows, err := runner.QueryContext(ctx,
		`SELECT project_id, route, policy FROM refire_policies ORDER BY project_id ASC, route ASC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []store.RefirePolicyRow
	for rows.Next() {
		var r store.RefirePolicyRow
		if err := rows.Scan(&r.ProjectID, &r.Route, &r.Policy); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// lastEventsOf is the last-event-per-run lookup (the same MAX(seq) join shape
// as the reaper's ParkedReasons): the refire verdict reads a candidate's own
// latest persisted event inside its transaction.
func lastEventsOf(ctx context.Context, runner reap.Runner, d reap.Dialect, runIDs []string) (map[string]LastEvent, error) {
	if len(runIDs) == 0 {
		return nil, nil
	}
	q := `
		SELECT e.run_id, e.type, e.payload
		FROM events e
		JOIN (SELECT run_id, MAX(seq) AS seq FROM events WHERE run_id IN (` +
		d.Placeholders(1, len(runIDs)) + `) GROUP BY run_id) l
		  ON l.run_id = e.run_id AND l.seq = e.seq`
	args := make([]any, 0, len(runIDs))
	for _, id := range runIDs {
		args = append(args, id)
	}
	rows, err := runner.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make(map[string]LastEvent, len(runIDs))
	for rows.Next() {
		var runID, evType string
		var payload []byte
		if err := rows.Scan(&runID, &evType, &payload); err != nil {
			return nil, err
		}
		out[runID] = LastEvent{Type: evType, Payload: payload}
	}
	return out, rows.Err()
}

// AttachLabels loads every candidate's eligibility-label rows in one round
// trip on the caller's snapshot and fills c.Assignment.Labels (KB-321):
// minted refire successors inherit the assignment's labels so eligibility
// matching survives a refire.
func AttachLabels(ctx context.Context, runner reap.Runner, d reap.Dialect, candidates []store.RefireCandidate) error {
	ids := make([]string, 0, len(candidates))
	for _, c := range candidates {
		if c.Assignment != nil && c.Assignment.ID != "" {
			ids = append(ids, c.Assignment.ID)
		}
	}
	if len(ids) == 0 {
		return nil
	}
	args := make([]any, 0, len(ids))
	for _, id := range ids {
		args = append(args, id)
	}
	rows, err := runner.QueryContext(ctx,
		`SELECT assignment_id, key, value FROM workflow_assignment_labels
		 WHERE assignment_id IN (`+d.Placeholders(1, len(ids))+`)`, args...)
	if err != nil {
		return err
	}
	defer rows.Close()
	var byID map[string]map[string]string
	for rows.Next() {
		var assignmentID, k, v string
		if err := rows.Scan(&assignmentID, &k, &v); err != nil {
			return err
		}
		if byID == nil {
			byID = make(map[string]map[string]string)
		}
		m, ok := byID[assignmentID]
		if !ok {
			m = make(map[string]string)
			byID[assignmentID] = m
		}
		m[k] = v
	}
	if err := rows.Err(); err != nil {
		return err
	}
	for _, c := range candidates {
		if c.Assignment == nil {
			continue
		}
		if labels, ok := byID[c.Assignment.ID]; ok {
			c.Assignment.Labels = labels
		}
	}
	return nil
}

// EvaluateCandidates classifies each candidate against its resolved policy
// and takes the verdicts on the caller's write transaction (KB-321):
//
//   - park: the run is reclassified to the KB-226 parked shape — status
//     awaiting_human, no ended_at, no failure_reason, no assignment change —
//     and never refires without a human ack.
//   - cosmetic / none: the verdict is stamped on the run as data
//     (runs.refire_disposition) and the run is left terminal-failed for an
//     operator decision; the stamp ages the row out of the candidate scan.
//   - refire: under the policy's attempt budget (family attempts counting
//     the original) and backoff deadline, a successor run plus queued
//     assignment is minted with an attempt-suffixed idempotency key and the
//     family lineage carried forward; the minted attempt's original is
//     stamped refired.
//
// Deferred (backoff pending, no ended_at), budget-exhausted, and skipped
// outcomes write nothing: those rows stay in the candidate set so policy
// retunes reach them on the next pass. A single candidate error — a racing
// writer colliding on the idempotency key, say — aborts the whole pass
// transaction; the tick retries the remaining candidates (single-replica
// constraint, KB-321).
//
// Returns one store.RefireOutcome per candidate, in candidates order.
func EvaluateCandidates(
	ctx context.Context,
	tx reap.Runner,
	d reap.Dialect,
	now time.Time,
	policies []store.RefirePolicyRow,
	candidates []store.RefireCandidate,
) ([]store.RefireOutcome, error) {
	if len(candidates) == 0 {
		return nil, nil
	}
	runIDs := make([]string, 0, len(candidates))
	for _, c := range candidates {
		runIDs = append(runIDs, c.Assignment.RunID)
	}
	events, err := lastEventsOf(ctx, tx, d, runIDs)
	if err != nil {
		return nil, fmt.Errorf("refire last events: %w", err)
	}

	outcomes := make([]store.RefireOutcome, 0, len(candidates))
	for _, c := range candidates {
		outcome, err := evaluateCandidate(ctx, tx, d, now, policies, events, c)
		if err != nil {
			return nil, err
		}
		outcomes = append(outcomes, outcome)
	}
	return outcomes, nil
}

func evaluateCandidate(
	ctx context.Context,
	tx reap.Runner,
	d reap.Dialect,
	now time.Time,
	policies []store.RefirePolicyRow,
	events map[string]LastEvent,
	c store.RefireCandidate,
) (store.RefireOutcome, error) {
	a := c.Assignment
	outcome := store.RefireOutcome{
		Disposition:   store.RefireDispositionSkipped,
		RunID:         a.RunID,
		RootRunID:     c.RootRunID,
		RefireAttempt: c.Attempt,
	}
	policy, source := ResolvePolicy(policies, a.ProjectID, a.WorkflowName)

	var ev *LastEvent
	if e, ok := events[a.RunID]; ok {
		ev = &e
	}
	disposition, classReason := Classify(policy, c.FailureReason, ev)

	switch disposition {
	case DispositionPark:
		changed, err := stampParkedRun(ctx, tx, d, a.RunID)
		if err != nil {
			return store.RefireOutcome{}, fmt.Errorf("refire park %s: %w", a.RunID, err)
		}
		if changed {
			outcome.Disposition = store.RefireDispositionPark
			outcome.Reason = classReason
		} else {
			outcome.Reason = "skipped: run no longer failed"
		}
		return outcome, nil
	case DispositionCosmetic, DispositionNone:
		// The verdict is stamped on the run as data: known-benign endings
		// (cosmetic) and unclassified endings (none) stay terminal-failed
		// for an operator decision, but their story is closed in the
		// automatic pass — the stamp ages them out of the candidate scan so
		// permanent no-action rows can never starve a fresh failure
		// (KB-321). Reopening a stamped row (a policy retune reclassifying
		// the reason) is a deliberate data edit: the stamp is data, not
		// code state.
		if err := stampRefireDisposition(ctx, tx, d, a.RunID, string(disposition)); err != nil {
			return store.RefireOutcome{}, fmt.Errorf("refire stamp %s: %w", a.RunID, err)
		}
		outcome.Disposition = string(disposition)
		outcome.Reason = classReason + " under refire policy " + source
		return outcome, nil
	}

	// Refire guard chain: nothing beyond this point mints a successor unless
	// every guard passes.
	if policy.MaxAttempts <= 0 {
		outcome.Reason = "refiring disabled by refire policy " + source
		return outcome, nil
	}
	if c.Attempt >= policy.MaxAttempts {
		outcome.Disposition = store.RefireDispositionExhausted
		outcome.Reason = fmt.Sprintf(
			"attempt budget exhausted (%d of max_attempts %d) under refire policy %s",
			c.Attempt, policy.MaxAttempts, source)
		return outcome, nil
	}
	if c.EndedAt == nil {
		outcome.Disposition = store.RefireDispositionDeferred
		outcome.Reason = "deferred: failed run has no ended_at"
		return outcome, nil
	}
	if readyAt := c.EndedAt.Add(policy.Backoff); now.Before(readyAt) {
		outcome.Disposition = store.RefireDispositionDeferred
		outcome.Reason = fmt.Sprintf(
			"deferred: backoff runs until %s (+%s) under refire policy %s",
			readyAt.Format(store.TimestampLayout), policy.Backoff, source)
		return outcome, nil
	}
	key := NextAttemptKey(a.IdempotencyKey, c.Attempt)
	exists, err := refireKeyExists(ctx, tx, d, a.OwnerCriteriaID, key)
	if err != nil {
		return store.RefireOutcome{}, fmt.Errorf("refire key check %s: %w", a.RunID, err)
	}
	if exists {
		outcome.Reason = "skipped: refire idempotency key already exists (" + key + ")"
		return outcome, nil
	}
	return mintSuccessor(ctx, tx, d, now, c, a, policy, source, key, outcome)
}

// stampRefireDisposition closes an evaluated run's story in the automatic
// pass (KB-321): the disposition is data on the run, so a terminally
// verdict row (successor minted, reclassified parked, cosmetic / none
// verdict) ages out of the candidate scan instead of permanently occupying
// it under the scan limit. Deferred and budget-exhausted rows are
// deliberately NOT stamped: policy retunes (re-enabling refiring, raising
// max_attempts, retuning the vocabulary) must reach them on the next pass.
// The status guard keeps a run that left the failed status between the scan
// and this write unstamped (CRI-142 discipline).
func stampRefireDisposition(ctx context.Context, tx reap.Runner, d reap.Dialect, runID, disposition string) error {
	_, err := tx.ExecContext(ctx,
		`UPDATE runs
		 SET refire_disposition = `+d.P(1)+`
		 WHERE id = `+d.P(2)+` AND status = 'failed' AND refire_disposition IS NULL`,
		disposition, runID)
	return err
}

// stampParkedRun reclassifies a failed run to the KB-226 parked shape under a
// status guard, so a run that left the failed status between the scan and
// this write is never clobbered (CRI-142 discipline). Returns whether the
// write landed.
func stampParkedRun(ctx context.Context, tx reap.Runner, d reap.Dialect, runID string) (bool, error) {
	res, err := tx.ExecContext(ctx,
		`UPDATE runs
		 SET status = `+d.P(1)+`, ended_at = NULL, failure_reason = NULL, refire_disposition = `+d.P(2)+`
		 WHERE id = `+d.P(3)+` AND status = 'failed'`,
		store.RunStatusAwaitingHuman, store.RefireDispositionPark, runID)
	if err != nil {
		return false, err
	}
	changed, err := res.RowsAffected()
	if err != nil {
		return false, err
	}
	return changed > 0, nil
}

// refireKeyExists reports whether an assignment with the (owner, key) pair
// already exists — a successor minted by an earlier pass, or an operator
// submission colliding with the attempt-suffixed key.
func refireKeyExists(ctx context.Context, tx reap.Runner, d reap.Dialect, owner, key string) (bool, error) {
	rows, err := tx.QueryContext(ctx,
		`SELECT 1 FROM workflow_assignments WHERE owner_criteria_id = `+d.P(1)+
			` AND idempotency_key = `+d.P(2)+` LIMIT 1`, owner, key)
	if err != nil {
		return false, err
	}
	defer rows.Close()
	exists := rows.Next()
	return exists, rows.Err()
}

// mintSuccessor inserts the successor run plus queued assignment (the same
// INSERT shape assignment submission uses, plus the family lineage and the
// attempt-suffixed idempotency key), so delivery reuses the normal dispatch
// path.
func mintSuccessor(
	ctx context.Context,
	tx reap.Runner,
	d reap.Dialect,
	now time.Time,
	c store.RefireCandidate,
	a *store.WorkflowAssignment,
	policy Policy,
	source,
	key string,
	outcome store.RefireOutcome,
) (store.RefireOutcome, error) {
	id := uuid.NewString()
	runID := uuid.NewString()

	inputValues, err := store.MarshalLabels(a.InputValues)
	if err != nil {
		return store.RefireOutcome{}, fmt.Errorf("refire marshal input values %s: %w", a.RunID, err)
	}
	targetMetadata, err := store.MarshalLabels(a.TargetMetadata)
	if err != nil {
		return store.RefireOutcome{}, fmt.Errorf("refire marshal target metadata %s: %w", a.RunID, err)
	}

	if _, err := tx.ExecContext(ctx,
		`INSERT INTO runs(id, overseer_id, workflow_name, workflow_hcl, status, current_step, last_seq, created_at)
		 VALUES(`+d.P(1)+`, NULL, `+d.P(2)+`, `+d.P(3)+`, 'pending', '', 0, `+d.P(4)+`)`,
		runID, a.WorkflowName, a.WorkflowSource, d.TS(now)); err != nil {
		return store.RefireOutcome{}, fmt.Errorf("refire insert run %s: %w", a.RunID, err)
	}
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO workflow_assignments(
			id, owner_criteria_id, run_id, workflow_name, workflow_source, lockfile_source,
			idempotency_key, state, created_at, updated_at, project_id, input_values, target_metadata,
			refire_root_run_id, refire_attempt)
		 VALUES(`+d.P(1)+`, `+d.P(2)+`, `+d.P(3)+`, `+d.P(4)+`, `+d.P(5)+`, `+d.P(6)+`,
		        `+d.P(7)+`, '`+store.WorkflowAssignmentStateQueued+`', `+d.P(8)+`, `+d.P(9)+`,
		        `+d.P(10)+`, `+d.P(11)+`, `+d.P(12)+`, `+d.P(13)+`, `+d.P(14)+`)`,
		id, a.OwnerCriteriaID, runID, a.WorkflowName, a.WorkflowSource, a.LockfileSource,
		key, d.TS(now), d.TS(now), emptyToNil(a.ProjectID),
		emptyToNil(inputValues), emptyToNil(targetMetadata),
		c.RootRunID, c.Attempt+1); err != nil {
		return store.RefireOutcome{}, fmt.Errorf("refire insert assignment %s: %w", a.RunID, err)
	}
	for k, v := range a.Labels {
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO workflow_assignment_labels(assignment_id, key, value)
			 VALUES(`+d.P(1)+`, `+d.P(2)+`, `+d.P(3)+`)`,
			id, k, v); err != nil {
			return store.RefireOutcome{}, fmt.Errorf("refire insert label %s: %w", a.RunID, err)
		}
	}
	// The minted attempt's original leaves the candidate set: the family's
	// fate moves to the successor (stamped refired; see
	// stampRefireDisposition — deferred/exhausted stay unstamped).
	if err := stampRefireDisposition(ctx, tx, d, a.RunID, store.RefireDispositionRefire); err != nil {
		return store.RefireOutcome{}, fmt.Errorf("refire stamp %s: %w", a.RunID, err)
	}

	outcome.Disposition = store.RefireDispositionRefire
	outcome.Reason = fmt.Sprintf(
		"refiring attempt %d of %d (%s) under refire policy %s",
		c.Attempt+1, policy.MaxAttempts, c.FailureReason, source)
	outcome.RefireAttempt = c.Attempt + 1
	outcome.RefireRunID = runID
	outcome.RefireAssignmentID = id
	return outcome, nil
}

// emptyToNil keeps NULL semantics engine-neutral for nullable TEXT columns
// (the engine helpers nullText/nonEmpty do the same on their own inserts).
func emptyToNil(s string) any {
	if s == "" {
		return nil
	}
	return s
}
