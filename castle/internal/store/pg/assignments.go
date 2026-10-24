package pg

import (
	"context"
	"database/sql"
	"errors"
	"sort"
	"strings"
	"time"

	"github.com/google/uuid"

	"github.com/brokenbots/castle/castle/internal/store"
)

// assignmentColumns is the projection scanned by assignment row readers.
const assignmentColumns = `
	id, owner_criteria_id, run_id, workflow_name, workflow_source, lockfile_source,
	idempotency_key, state, terminal_reason, leased_criteria_id, lease_expires_at,
	created_at, updated_at, project_id, input_values, target_metadata,
	refire_root_run_id, refire_attempt`

// expireLeasesHook is test-only mutable state: a hook fired by
// ExpireWorkflowAssignmentLeases after the candidate scan and before the
// expiry UPDATE (same transaction) so pg-package tests can interleave an
// out-of-band write into the READ COMMITTED window the re-assertion
// predicates guard. Nil in production; see events.go's eventAppendHook for
// the same idiom.
var expireLeasesHook func()

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
	defer func() { _ = tx.Rollback() }()

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

	inputValues, err := store.MarshalLabels(a.InputValues)
	if err != nil {
		return nil, false, err
	}
	targetMetadata, err := store.MarshalLabels(a.TargetMetadata)
	if err != nil {
		return nil, false, err
	}
	var projectIDArg any
	if a.ProjectID != "" {
		projectIDArg = a.ProjectID
	}
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO workflow_assignments(id, owner_criteria_id, run_id, workflow_name, workflow_source, lockfile_source,
		                                   idempotency_key, state, created_at, updated_at, project_id, input_values, target_metadata)
		 VALUES($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13)`,
		a.ID, a.OwnerCriteriaID, a.RunID, a.WorkflowName, a.WorkflowSource, a.LockfileSource,
		a.IdempotencyKey, a.State, us(a.CreatedAt), us(a.UpdatedAt), projectIDArg, nonEmpty(inputValues), nonEmpty(targetMetadata)); err != nil {
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
	var projectID, inputValues, targetMetadata sql.NullString
	err := scan(
		&a.ID, &a.OwnerCriteriaID, &a.RunID, &a.WorkflowName, &a.WorkflowSource, &lockfile,
		&a.IdempotencyKey, &a.State, &terminalReason, &leasedID, &expires,
		&created, &updated, &projectID, &inputValues, &targetMetadata,
		&a.RefireRootRunID, &a.RefireAttempt)
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
	if projectID.Valid {
		a.ProjectID = projectID.String
	}
	a.InputValues = store.UnmarshalLabels(inputValues.String)
	a.TargetMetadata = store.UnmarshalLabels(targetMetadata.String)
	return &a, nil
}

func scanAssignmentTx(ctx context.Context, tx *sql.Tx, query string, args ...any) (*store.WorkflowAssignment, error) {
	row := tx.QueryRowContext(ctx, query, args...)
	return scanAssignmentRow(row.Scan)
}

// scanAssignmentMaps fills the nullable scoping/convention columns (project
// scope, input values, target metadata) into an assignment assembled from a
// projection that carries them; keeps the pending-list scan aligned with
// scanAssignmentRow.
func scanAssignmentMaps(projectID, inputValues, targetMetadata sql.NullString, a *store.WorkflowAssignment) {
	if projectID.Valid {
		a.ProjectID = projectID.String
	}
	a.InputValues = store.UnmarshalLabels(inputValues.String)
	a.TargetMetadata = store.UnmarshalLabels(targetMetadata.String)
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
	defer func() { _ = tx.Rollback() }()

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

	// Verify the agent is online and read its identity block for the
	// exact-build stamp on the leased run (KB-318).
	var status, identity string
	if err := tx.QueryRowContext(ctx, `SELECT status, COALESCE(identity, '') FROM overseers WHERE id=$1`, criteriaID).Scan(&status, &identity); err != nil {
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
	//
	// Project scoping (KB-318) is enforced here, at the store layer: the
	// scan only offers an assignment carrying a project_id to an agent in
	// that project's member set. Unscoped assignments (project_id IS NULL)
	// stay visible to every agent.
	rows, err := tx.QueryContext(ctx, `
		SELECT a.id, a.run_id, a.workflow_name, a.workflow_source, a.lockfile_source,
		       a.idempotency_key, a.created_at, a.updated_at, a.project_id, l.key, l.value
		FROM workflow_assignments a
		JOIN runs r ON r.id = a.run_id
		LEFT JOIN workflow_assignment_labels l ON l.assignment_id = a.id
		WHERE a.state = $1
		  AND r.status NOT IN ('succeeded', 'failed', 'cancelled', 'stopped', '`+store.RunStatusAwaitingHuman+`')
		  AND (
		          a.project_id IS NULL
		          OR EXISTS (
		              SELECT 1 FROM project_members pm
		              WHERE pm.project_id = a.project_id AND pm.member_id = $2
		          )
		      )
		ORDER BY a.created_at ASC, a.id ASC`,
		store.WorkflowAssignmentStateQueued, criteriaID)
	if err != nil {
		return nil, err
	}

	candidates := make(map[string]*assignmentCandidate)
	for rows.Next() {
		var id, runID, wfName, wfSource, idemp string
		var created, updated time.Time
		var lockfile, projectID, lkey, lvalue sql.NullString
		if err := rows.Scan(&id, &runID, &wfName, &wfSource, &lockfile, &idemp,
			&created, &updated, &projectID, &lkey, &lvalue); err != nil {
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
			if projectID.Valid {
				c.assignment.ProjectID = projectID.String
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
			`UPDATE runs SET overseer_id=$1, agent_identity=$2 WHERE id=$3`, criteriaID, identity, c.assignment.RunID); err != nil {
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
//
// Every statement re-asserts all three expiry predicates — assignment
// state='leased', lease_expires_at < now, and run status='pending' — because a
// run's status is written unguarded and READ COMMITTED means the expiry
// transaction's later statements see a newer snapshot than the candidate
// scan: a run that starts (pending → running) between the scan and the
// expiry UPDATE — the same window LeaseWorkflowAssignment's inline
// stale-lease sweep guards — would otherwise be requeued with a live run.
// The run's overseer_id is cleared only while the run is still pending.
func (s *Store) ExpireWorkflowAssignmentLeases(ctx context.Context, now time.Time) ([]string, error) {
	ctx, cancel := s.opCtx(ctx)
	defer cancel()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback() }()

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

	// Test-only seam: fired after the candidate scan and before the expiry
	// UPDATE, inside the same transaction, so tests can interleave writes on
	// a second connection into the READ COMMITTED window the re-assertion
	// predicates guard. Nil in production.
	if expireLeasesHook != nil {
		expireLeasesHook()
	}

	if len(ids) == 0 {
		_ = tx.Rollback()
		return nil, nil
	}

	// Number the IN-list placeholders after the leading state/updated_at
	// parameters by pre-seeding the factory with them. The nested
	// `WHERE id IN (...)` subquery cannot infer parameter types through
	// pgx's renumbering, so every placeholder gets an explicit ::text cast.
	//
	// The re-assertion predicates (state='leased', lease still expired, run
	// still pending) make the expiry safe under READ COMMITTED: the candidate
	// scan above ran against an earlier snapshot, and the UPDATE re-reads the
	// latest committed row, so without the predicates an assignment that was
	// re-queued and re-leased to another agent after the scan — or whose run
	// started in that window — would be clobbered here. The re-assertion
	// reuses $2 for the expiry bound; the leased state and the pending
	// status are inlined literals like r.status='pending' in the scan.
	p := &plf{args: []any{store.WorkflowAssignmentStateQueued, us(now)}}
	phs := make([]string, 0, len(ids))
	for _, id := range ids {
		phs = append(phs, p.next(id)+"::text")
	}
	casted := strings.Join(phs, ",")
	tRows, err := tx.QueryContext(ctx,
		`UPDATE workflow_assignments
		 SET state=$1, leased_criteria_id=NULL, lease_expires_at=NULL, updated_at=$2
		 WHERE id IN (`+casted+`)
		   AND state='leased' AND lease_expires_at < $2
		   AND run_id IN (SELECT id FROM runs WHERE status='pending')
		 RETURNING id, run_id`,
		p.args...)
	if err != nil {
		return nil, err
	}
	// Only the rows actually transitioned: both the returned ids and the
	// run overseer cleanup below derive from RETURNING, never from the
	// earlier candidate scan.
	var transitionedIDs, transitionedRunIDs []string
	for tRows.Next() {
		var id, runID string
		if err := tRows.Scan(&id, &runID); err != nil {
			tRows.Close()
			return nil, err
		}
		transitionedIDs = append(transitionedIDs, id)
		transitionedRunIDs = append(transitionedRunIDs, runID)
	}
	if err := tRows.Err(); err != nil {
		tRows.Close()
		return nil, err
	}
	tRows.Close()

	// The runs update is a separate statement and must number its parameters
	// from $1: the extended protocol parses placeholder references as a dense
	// sequence, and continuing the first statement's numbering ($3+) leaves a
	// $1/$2 gap that fails to parse (42P18). It clears overseer_id only for
	// runs of assignments actually transitioned above — a candidate that was
	// re-leased between scan and update keeps its run's overseer, and a run
	// that started in that window keeps its overseer too (the run-status
	// predicate re-reads the latest snapshot, so the stale-pending run row
	// never lands here).
	if len(transitionedRunIDs) > 0 {
		p2 := &plf{}
		phs2 := make([]string, 0, len(transitionedRunIDs))
		for _, runID := range transitionedRunIDs {
			phs2 = append(phs2, p2.next(runID)+"::text")
		}
		if _, err := tx.ExecContext(ctx,
			`UPDATE runs
			 SET overseer_id=NULL
			 WHERE id IN (`+strings.Join(phs2, ",")+`)
			   AND status='pending'`,
			p2.args...); err != nil {
			return nil, err
		}
	}

	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return transitionedIDs, nil
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
		       a.idempotency_key, a.created_at, a.updated_at, a.project_id,
		       a.input_values, a.target_metadata, l.key, l.value
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
		var lockfile, projectID, inputValues, targetMetadata, lkey, lvalue sql.NullString
		if err := rows.Scan(&id, &runID, &wfName, &wfSource, &lockfile, &idemp,
			&created, &updated, &projectID, &inputValues, &targetMetadata, &lkey, &lvalue); err != nil {
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
			scanAssignmentMaps(projectID, inputValues, targetMetadata, c.assignment)
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
