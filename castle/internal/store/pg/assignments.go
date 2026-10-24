package pg

import (
	"context"
	"database/sql"
	"errors"
	"sort"
	"time"

	"github.com/google/uuid"

	"github.com/brokenbots/castle/castle/internal/store"
)

// assignmentColumns is the projection scanned by assignment row readers.
const assignmentColumns = `
	id, owner_criteria_id, run_id, workflow_name, workflow_source, lockfile_source,
	idempotency_key, state, terminal_reason, leased_criteria_id, lease_expires_at,
	created_at, updated_at`

func (s *Store) CreateWorkflowAssignment(ctx context.Context, a *store.WorkflowAssignment) (*store.WorkflowAssignment, bool, error) {
	ctx, cancel := s.opCtx(ctx)
	defer cancel()
	if a == nil {
		return nil, false, errors.New("nil assignment")
	}
	if a.OwnerCriteriaID == "" {
		return nil, false, errors.New("owner_criteria_id required")
	}
	if a.WorkflowName == "" {
		return nil, false, errors.New("workflow_name required")
	}
	if a.WorkflowSource == "" {
		return nil, false, errors.New("workflow_source required")
	}
	if a.IdempotencyKey == "" {
		return nil, false, errors.New("idempotency_key required")
	}
	if a.ID == "" {
		a.ID = uuid.NewString()
	}
	if a.RunID == "" {
		a.RunID = uuid.NewString()
	}
	if a.State == "" {
		a.State = store.WorkflowAssignmentStateQueued
	}
	now := time.Now().UTC()
	if a.CreatedAt.IsZero() {
		a.CreatedAt = now
	}
	if a.UpdatedAt.IsZero() {
		a.UpdatedAt = now
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, false, err
	}
	defer tx.Rollback() //nolint:errcheck

	existing, err := scanAssignmentTx(ctx, tx, `
		SELECT `+assignmentColumns+`
		FROM workflow_assignments
		WHERE owner_criteria_id=$1 AND idempotency_key=$2`, a.OwnerCriteriaID, a.IdempotencyKey)
	if err != nil && !errors.Is(err, store.ErrNotFound) {
		return nil, false, err
	}
	if existing != nil {
		_ = tx.Rollback()
		return existing, false, nil
	}

	if _, err := tx.ExecContext(ctx,
		`INSERT INTO runs(id, overseer_id, workflow_name, workflow_hcl, status, current_step, last_seq, created_at)
		 VALUES($1, NULL, $2, $3, 'pending', '', 0, $4)`,
		a.RunID, a.WorkflowName, a.WorkflowSource, us(a.CreatedAt)); err != nil {
		return nil, false, err
	}

	if _, err := tx.ExecContext(ctx,
		`INSERT INTO workflow_assignments(id, owner_criteria_id, run_id, workflow_name, workflow_source, lockfile_source,
		                                   idempotency_key, state, created_at, updated_at)
		 VALUES($1, $2, $3, $4, $5, $6, $7, $8, $9, $10)`,
		a.ID, a.OwnerCriteriaID, a.RunID, a.WorkflowName, a.WorkflowSource, a.LockfileSource,
		a.IdempotencyKey, a.State, us(a.CreatedAt), us(a.UpdatedAt)); err != nil {
		return nil, false, err
	}

	if err := s.insertAssignmentLabelsTx(ctx, tx, a.ID, a.Labels); err != nil {
		return nil, false, err
	}

	if err := tx.Commit(); err != nil {
		return nil, false, err
	}
	return a, true, nil
}

func (s *Store) insertAssignmentLabelsTx(ctx context.Context, tx *sql.Tx, assignmentID string, labels map[string]string) error {
	for k, v := range labels {
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO workflow_assignment_labels(assignment_id, key, value) VALUES($1, $2, $3)`,
			assignmentID, k, v); err != nil {
			return err
		}
	}
	return nil
}

func (s *Store) GetWorkflowAssignment(ctx context.Context, id string) (*store.WorkflowAssignment, error) {
	ctx, cancel := s.opCtx(ctx)
	defer cancel()
	if id == "" {
		return nil, errors.New("id required")
	}
	return s.scanAssignment(ctx, s.db.QueryRowContext(ctx, `
		SELECT `+assignmentColumns+`
		FROM workflow_assignments WHERE id=$1`, id))
}

func (s *Store) GetWorkflowAssignmentByRunID(ctx context.Context, runID string) (*store.WorkflowAssignment, error) {
	ctx, cancel := s.opCtx(ctx)
	defer cancel()
	if runID == "" {
		return nil, errors.New("run_id required")
	}
	return s.scanAssignment(ctx, s.db.QueryRowContext(ctx, `
		SELECT `+assignmentColumns+`
		FROM workflow_assignments WHERE run_id=$1`, runID))
}

func (s *Store) scanAssignment(ctx context.Context, row *sql.Row) (*store.WorkflowAssignment, error) {
	a, err := scanAssignmentRow(row.Scan)
	if err != nil {
		return nil, err
	}
	labels, err := s.loadAssignmentLabels(ctx, a.ID)
	if err != nil {
		return nil, err
	}
	a.Labels = labels
	return a, nil
}

func scanAssignmentRow(scan func(...any) error) (*store.WorkflowAssignment, error) {
	var a store.WorkflowAssignment
	var created, updated time.Time
	var lockfile, terminalReason, leasedID sql.NullString
	var expires sql.NullTime
	err := scan(
		&a.ID, &a.OwnerCriteriaID, &a.RunID, &a.WorkflowName, &a.WorkflowSource, &lockfile,
		&a.IdempotencyKey, &a.State, &terminalReason, &leasedID, &expires,
		&created, &updated)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, store.ErrNotFound
		}
		return nil, err
	}
	a.CreatedAt = created
	a.UpdatedAt = updated
	if lockfile.Valid {
		a.LockfileSource = lockfile.String
	}
	if terminalReason.Valid {
		a.TerminalReason = terminalReason.String
	}
	if leasedID.Valid {
		a.LeasedCriteriaID = leasedID.String
	}
	if expires.Valid {
		t := expires.Time
		a.LeaseExpiresAt = &t
	}
	return &a, nil
}

func scanAssignmentTx(ctx context.Context, tx *sql.Tx, query string, args ...any) (*store.WorkflowAssignment, error) {
	row := tx.QueryRowContext(ctx, query, args...)
	return scanAssignmentRow(row.Scan)
}

func (s *Store) loadAssignmentLabels(ctx context.Context, assignmentID string) (map[string]string, error) {
	rows, err := s.reader.QueryContext(ctx,
		`SELECT key, value FROM workflow_assignment_labels WHERE assignment_id=$1`, assignmentID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	m := make(map[string]string)
	for rows.Next() {
		var k, v string
		if err := rows.Scan(&k, &v); err != nil {
			return nil, err
		}
		m[k] = v
	}
	return m, rows.Err()
}

// LeaseWorkflowAssignment atomically expires stale leases, finds a queued
// assignment whose required labels are satisfied by agentLabels, and leases it
// to criteriaID. The lease is written to workflow_assignment_leases and the
// first attempt is recorded in workflow_assignment_attempts. The associated
// run's overseer_id is updated to criteriaID. Invariant commentary mirrors
// the sqlite profile — the guards, ordering and error mapping are identical.
func (s *Store) LeaseWorkflowAssignment(ctx context.Context, criteriaID string, agentLabels map[string]string, now time.Time, leaseDuration time.Duration) (*store.WorkflowAssignment, error) {
	ctx, cancel := s.opCtx(ctx)
	defer cancel()
	if criteriaID == "" {
		return nil, errors.New("criteria_id required")
	}
	expiresAt := us(now.Add(leaseDuration))

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback() //nolint:errcheck

	// Transactionally expire stale leases for unstarted runs back to a leasable
	// queued state. Runs that have already emitted RunStarted (status='running')
	// are intentionally left leased so they can reattach without being requeued.
	if _, err := tx.ExecContext(ctx,
		`UPDATE workflow_assignments
		 SET state=$1, leased_criteria_id=NULL, lease_expires_at=NULL, updated_at=$2
		 WHERE state=$3 AND lease_expires_at < $4 AND run_id IN (
		     SELECT id FROM runs WHERE status='pending'
		 )`,
		store.WorkflowAssignmentStateQueued, us(now),
		store.WorkflowAssignmentStateLeased, us(now)); err != nil {
		return nil, err
	}
	// Clear the stale overseer_id for any runs whose lease just expired.
	if _, err := tx.ExecContext(ctx,
		`UPDATE runs
		 SET overseer_id=NULL
		 WHERE id IN (
		     SELECT run_id FROM workflow_assignments
		     WHERE state=$1 AND lease_expires_at IS NULL AND run_id IN (
		         SELECT id FROM runs WHERE status='pending'
		     )
		 )`,
		store.WorkflowAssignmentStateQueued); err != nil {
		return nil, err
	}

	// Verify the agent is online.
	var status string
	if err := tx.QueryRowContext(ctx, `SELECT status FROM overseers WHERE id=$1`, criteriaID).Scan(&status); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, store.ErrNotFound
		}
		return nil, err
	}
	if status != "online" {
		return nil, store.ErrNotFound
	}

	// Ensure the agent does not already hold an unstarted lease. This enforces
	// sequential execution: an agent receives the next assignment only after it
	// has accepted (RunStarted) the current one.
	var hasPendingLease bool
	if err := tx.QueryRowContext(ctx, `
		SELECT EXISTS(
			SELECT 1 FROM workflow_assignments a
			JOIN runs r ON r.id = a.run_id
			WHERE a.leased_criteria_id=$1 AND a.state=$2 AND r.status='pending'
		)`, criteriaID, store.WorkflowAssignmentStateLeased).Scan(&hasPendingLease); err != nil {
		return nil, err
	}
	if hasPendingLease {
		return nil, store.ErrNotFound
	}

	// Find queued assignments and their required labels, oldest first. Only
	// work whose run is still executable is eligible (CRI-207): the dispatch
	// path must never lease an assignment whose run the operator parked as
	// stopped, or whose run already reached a terminal state — only an
	// explicit ResumeRun moves a stopped run back to running (and back into
	// this scan), so parked work is delivered only through resume. Runs
	// parked at a human gate (awaiting_human, KB-226) are not executable
	// work either: the engine already completed them for a human decision,
	// so re-dispatching would re-run finished work.
	rows, err := tx.QueryContext(ctx, `
		SELECT a.id, a.run_id, a.workflow_name, a.workflow_source, a.lockfile_source,
		       a.idempotency_key, a.created_at, a.updated_at, l.key, l.value
		FROM workflow_assignments a
		JOIN runs r ON r.id = a.run_id
		LEFT JOIN workflow_assignment_labels l ON l.assignment_id = a.id
		WHERE a.state = $1
		  AND r.status NOT IN ('succeeded', 'failed', 'cancelled', 'stopped', '`+store.RunStatusAwaitingHuman+`')
		ORDER BY a.created_at ASC, a.id ASC`,
		store.WorkflowAssignmentStateQueued)
	if err != nil {
		return nil, err
	}

	candidates := make(map[string]*assignmentCandidate)
	for rows.Next() {
		var id, runID, wfName, wfSource, idemp string
		var created, updated time.Time
		var lockfile sql.NullString
		var lkey, lvalue sql.NullString
		if err := rows.Scan(&id, &runID, &wfName, &wfSource, &lockfile, &idemp,
			&created, &updated, &lkey, &lvalue); err != nil {
			rows.Close()
			return nil, err
		}
		c, ok := candidates[id]
		if !ok {
			c = &assignmentCandidate{
				assignment: &store.WorkflowAssignment{
					ID:             id,
					RunID:          runID,
					WorkflowName:   wfName,
					WorkflowSource: wfSource,
					IdempotencyKey: idemp,
					State:          store.WorkflowAssignmentStateQueued,
					CreatedAt:      created,
					UpdatedAt:      updated,
					Labels:         make(map[string]string),
				},
				required: make(map[string]string),
			}
			if lockfile.Valid {
				c.assignment.LockfileSource = lockfile.String
			}
			candidates[id] = c
		}
		if lkey.Valid {
			c.required[lkey.String] = lvalue.String
		}
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return nil, err
	}
	rows.Close()

	ordered := make([]*assignmentCandidate, 0, len(candidates))
	for _, c := range candidates {
		ordered = append(ordered, c)
	}
	sort.Slice(ordered, func(i, j int) bool {
		if !ordered[i].assignment.CreatedAt.Equal(ordered[j].assignment.CreatedAt) {
			return ordered[i].assignment.CreatedAt.Before(ordered[j].assignment.CreatedAt)
		}
		return ordered[i].assignment.ID < ordered[j].assignment.ID
	})

	for _, c := range ordered {
		if !labelsSatisfy(agentLabels, c.required) {
			continue
		}
		res, err := tx.ExecContext(ctx,
			`UPDATE workflow_assignments
			 SET state=$1, leased_criteria_id=$2, lease_expires_at=$3, updated_at=$4
			 WHERE id=$5 AND state=$6`,
			store.WorkflowAssignmentStateLeased, criteriaID, expiresAt,
			us(now), c.assignment.ID, store.WorkflowAssignmentStateQueued)
		if err != nil {
			return nil, err
		}
		affected, _ := res.RowsAffected()
		if affected == 0 {
			continue
		}
		if _, err := tx.ExecContext(ctx,
			`UPDATE runs SET overseer_id=$1 WHERE id=$2`, criteriaID, c.assignment.RunID); err != nil {
			return nil, err
		}
		leaseID := uuid.NewString()
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO workflow_assignment_leases(id, assignment_id, criteria_id, created_at, expires_at)
			 VALUES($1, $2, $3, $4, $5)`,
			leaseID, c.assignment.ID, criteriaID, us(now), expiresAt); err != nil {
			return nil, err
		}
		var attempt int
		if err := tx.QueryRowContext(ctx,
			`SELECT COALESCE(MAX(attempt),0)+1 FROM workflow_assignment_attempts WHERE assignment_id=$1`,
			c.assignment.ID).Scan(&attempt); err != nil {
			return nil, err
		}
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO workflow_assignment_attempts(assignment_id, attempt, criteria_id, created_at)
			 VALUES($1, $2, $3, $4)`,
			c.assignment.ID, attempt, criteriaID, us(now)); err != nil {
			return nil, err
		}
		if err := tx.Commit(); err != nil {
			return nil, err
		}
		c.assignment.State = store.WorkflowAssignmentStateLeased
		c.assignment.LeasedCriteriaID = criteriaID
		c.assignment.LeaseExpiresAt = &expiresAt
		c.assignment.Labels = c.required
		return c.assignment, nil
	}

	return nil, store.ErrNotFound
}

type assignmentCandidate struct {
	assignment *store.WorkflowAssignment
	required   map[string]string
}

func labelsSatisfy(agentLabels, required map[string]string) bool {
	for k, v := range required {
		if agentLabels[k] != v {
			return false
		}
	}
	return true
}

// ExpireWorkflowAssignmentLeases transitions leased assignments whose lease
// has expired and whose run has not yet started back to queued. It clears the
// associated run's overseer_id so the assignment can be safely redispatched to
// a new agent. The returned IDs can be used by callers to redispatch the work.
func (s *Store) ExpireWorkflowAssignmentLeases(ctx context.Context, now time.Time) ([]string, error) {
	ctx, cancel := s.opCtx(ctx)
	defer cancel()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback() //nolint:errcheck

	rows, err := tx.QueryContext(ctx, `
		SELECT a.id
		FROM workflow_assignments a
		JOIN runs r ON r.id = a.run_id
		WHERE a.state = $1 AND a.lease_expires_at < $2 AND r.status = 'pending'
		ORDER BY a.created_at ASC, a.id ASC`,
		store.WorkflowAssignmentStateLeased, us(now))
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

	if len(ids) == 0 {
		_ = tx.Rollback()
		return nil, nil
	}

	p := &plf{}
	ph := ""
	for _, id := range ids {
		ph = orComma(ph, p.next(id))
	}
	if _, err := tx.ExecContext(ctx,
		`UPDATE workflow_assignments
		 SET state=$1, leased_criteria_id=NULL, lease_expires_at=NULL, updated_at=$2
		 WHERE id IN (`+ph+`)`,
		append([]any{store.WorkflowAssignmentStateQueued, us(now)}, p.args[0:len(ids)]...)...); err != nil {
		return nil, err
	}

	if _, err := tx.ExecContext(ctx,
		`UPDATE runs
		 SET overseer_id=NULL
		 WHERE id IN (
		     SELECT run_id FROM workflow_assignments WHERE id IN (`+ph+`)
		 )`,
		p.args[0:len(ids)]...); err != nil {
		return nil, err
	}

	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return ids, nil
}

func orComma(acc, next string) string {
	if acc == "" {
		return next
	}
	return acc + "," + next
}

// ListLeasedPendingAssignmentsByCriteriaID returns assignments currently
// leased to criteriaID whose run has not yet started. These are in-flight
// leases that should be redelivered to the agent after a Castle restart.
func (s *Store) ListLeasedPendingAssignmentsByCriteriaID(ctx context.Context, criteriaID string) ([]*store.WorkflowAssignment, error) {
	ctx, cancel := s.opCtx(ctx)
	defer cancel()
	if criteriaID == "" {
		return nil, errors.New("criteria_id required")
	}
	rows, err := s.reader.QueryContext(ctx, `
		SELECT a.id, a.run_id, a.workflow_name, a.workflow_source, a.lockfile_source,
		       a.idempotency_key, a.created_at, a.updated_at, l.key, l.value
		FROM workflow_assignments a
		JOIN runs r ON r.id = a.run_id
		LEFT JOIN workflow_assignment_labels l ON l.assignment_id = a.id
		WHERE a.leased_criteria_id = $1 AND a.state = $2 AND r.status = 'pending'
		ORDER BY a.created_at ASC, a.id ASC`,
		criteriaID, store.WorkflowAssignmentStateLeased)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	candidates := make(map[string]*assignmentCandidate)
	for rows.Next() {
		var id, runID, wfName, wfSource, idemp string
		var created, updated time.Time
		var lockfile sql.NullString
		var lkey, lvalue sql.NullString
		if err := rows.Scan(&id, &runID, &wfName, &wfSource, &lockfile, &idemp,
			&created, &updated, &lkey, &lvalue); err != nil {
			return nil, err
		}
		c, ok := candidates[id]
		if !ok {
			c = &assignmentCandidate{
				assignment: &store.WorkflowAssignment{
					ID:             id,
					RunID:          runID,
					WorkflowName:   wfName,
					WorkflowSource: wfSource,
					IdempotencyKey: idemp,
					State:          store.WorkflowAssignmentStateLeased,
					CreatedAt:      created,
					UpdatedAt:      updated,
					Labels:         make(map[string]string),
				},
				required: make(map[string]string),
			}
			if lockfile.Valid {
				c.assignment.LockfileSource = lockfile.String
			}
			candidates[id] = c
		}
		if lkey.Valid {
			c.required[lkey.String] = lvalue.String
		}
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	ordered := make([]*assignmentCandidate, 0, len(candidates))
	for _, c := range candidates {
		ordered = append(ordered, c)
	}
	sort.Slice(ordered, func(i, j int) bool {
		if !ordered[i].assignment.CreatedAt.Equal(ordered[j].assignment.CreatedAt) {
			return ordered[i].assignment.CreatedAt.Before(ordered[j].assignment.CreatedAt)
		}
		return ordered[i].assignment.ID < ordered[j].assignment.ID
	})

	out := make([]*store.WorkflowAssignment, 0, len(ordered))
	for _, c := range ordered {
		c.assignment.LeasedCriteriaID = criteriaID
		c.assignment.Labels = c.required
		out = append(out, c.assignment)
	}
	return out, nil
}

func (s *Store) RecordWorkflowAssignmentLease(ctx context.Context, lease *store.WorkflowAssignmentLease) error {
	ctx, cancel := s.opCtx(ctx)
	defer cancel()
	if lease == nil || lease.AssignmentID == "" || lease.CriteriaID == "" {
		return errors.New("assignment_id and criteria_id required")
	}
	if lease.ID == "" {
		lease.ID = uuid.NewString()
	}
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO workflow_assignment_leases(id, assignment_id, criteria_id, created_at, expires_at)
		 VALUES($1, $2, $3, $4, $5)`,
		lease.ID, lease.AssignmentID, lease.CriteriaID, us(lease.CreatedAt), us(lease.ExpiresAt))
	return err
}

func (s *Store) RecordWorkflowAssignmentAttempt(ctx context.Context, attempt *store.WorkflowAssignmentAttempt) error {
	ctx, cancel := s.opCtx(ctx)
	defer cancel()
	if attempt == nil || attempt.AssignmentID == "" || attempt.Attempt <= 0 {
		return errors.New("assignment_id and attempt > 0 required")
	}
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO workflow_assignment_attempts(assignment_id, attempt, criteria_id, created_at)
		 VALUES($1, $2, $3, $4)`,
		attempt.AssignmentID, attempt.Attempt, attempt.CriteriaID, us(attempt.CreatedAt))
	return err
}

func (s *Store) CompleteWorkflowAssignmentAttempt(ctx context.Context, assignmentID string, attempt int, outcome string) error {
	ctx, cancel := s.opCtx(ctx)
	defer cancel()
	if assignmentID == "" || attempt <= 0 {
		return errors.New("assignment_id and attempt > 0 required")
	}
	now := time.Now().UTC()
	_, err := s.db.ExecContext(ctx,
		`UPDATE workflow_assignment_attempts SET completed_at=$1, outcome=$2
		 WHERE assignment_id=$3 AND attempt=$4`,
		us(now), outcome, assignmentID, attempt)
	return err
}

func (s *Store) MarkWorkflowAssignmentTerminal(ctx context.Context, runID, reason string) error {
	ctx, cancel := s.opCtx(ctx)
	defer cancel()
	if runID == "" {
		return errors.New("run_id required")
	}
	now := time.Now().UTC()
	_, err := s.db.ExecContext(ctx,
		`UPDATE workflow_assignments
		 SET state=$1, terminal_reason=$2, updated_at=$3
		 WHERE run_id=$4 AND state IN ($5, $6)`,
		store.WorkflowAssignmentStateTerminal, reason, us(now),
		runID, store.WorkflowAssignmentStateQueued, store.WorkflowAssignmentStateLeased)
	return err
}
