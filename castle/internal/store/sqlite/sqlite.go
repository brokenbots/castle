// Package sqlite is the SQLite implementation of store.Store using the
// pure-Go modernc.org/sqlite driver (no CGO required).
package sqlite

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"time"

	"github.com/google/uuid"
	_ "modernc.org/sqlite"

	"github.com/brokenbots/castle/castle/internal/store"
)

type Store struct {
	db *sql.DB
}

func Open(path string) (*Store, error) {
	dsn := fmt.Sprintf("file:%s?_pragma=journal_mode(WAL)&_pragma=foreign_keys(1)&_pragma=busy_timeout(5000)", path)
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, err
	}
	if err := Migrate(context.Background(), db); err != nil {
		_ = db.Close()
		return nil, err
	}
	return &Store{db: db}, nil
}

func (s *Store) Close() error { return s.db.Close() }

const (
	tsLayout               = time.RFC3339Nano
	ListEventsDefaultLimit = 500
	ListEventsMaxLimit     = 2048
)

func (s *Store) CreateOverseer(ctx context.Context, o *store.Overseer) error {
	labels, err := marshalLabels(o.Labels)
	if err != nil {
		return err
	}
	_, err = s.db.ExecContext(ctx,
		`INSERT INTO overseers(id,name,hostname,version,token_hash,status,labels,created_at,last_seen_at) VALUES(?,?,?,?,?,?,?,?,?)`,
		o.ID, o.Name, o.Hostname, o.Version, o.TokenHash, o.Status, labels, o.CreatedAt.Format(tsLayout), o.LastSeenAt.Format(tsLayout))
	return err
}

func (s *Store) GetOverseer(ctx context.Context, id string) (*store.Overseer, error) {
	row := s.db.QueryRowContext(ctx, `SELECT id,name,hostname,version,token_hash,status,labels,created_at,last_seen_at FROM overseers WHERE id=?`, id)
	var o store.Overseer
	var created, seen string
	var labels sql.NullString
	if err := row.Scan(&o.ID, &o.Name, &o.Hostname, &o.Version, &o.TokenHash, &o.Status, &labels, &created, &seen); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, store.ErrNotFound
		}
		return nil, err
	}
	o.CreatedAt, _ = time.Parse(tsLayout, created)
	o.LastSeenAt, _ = time.Parse(tsLayout, seen)
	o.Labels = unmarshalLabels(labels.String)
	return &o, nil
}

func (s *Store) ListOverseers(ctx context.Context) ([]*store.Overseer, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id,name,hostname,version,token_hash,status,labels,created_at,last_seen_at FROM overseers ORDER BY created_at DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*store.Overseer
	for rows.Next() {
		var o store.Overseer
		var created, seen string
		var labels sql.NullString
		if err := rows.Scan(&o.ID, &o.Name, &o.Hostname, &o.Version, &o.TokenHash, &o.Status, &labels, &created, &seen); err != nil {
			return nil, err
		}
		o.CreatedAt, _ = time.Parse(tsLayout, created)
		o.LastSeenAt, _ = time.Parse(tsLayout, seen)
		o.Labels = unmarshalLabels(labels.String)
		out = append(out, &o)
	}
	return out, rows.Err()
}

func (s *Store) UpdateOverseerSeen(ctx context.Context, id string, ts time.Time) error {
	_, err := s.db.ExecContext(ctx, `UPDATE overseers SET last_seen_at=?, status='online' WHERE id=?`, ts.Format(tsLayout), id)
	return err
}

func (s *Store) UpdateOverseerStatus(ctx context.Context, id, status string) error {
	_, err := s.db.ExecContext(ctx, `UPDATE overseers SET status=? WHERE id=?`, status, id)
	return err
}

func (s *Store) MarkOfflineBefore(ctx context.Context, before time.Time) error {
	_, err := s.db.ExecContext(ctx, `UPDATE overseers SET status='offline' WHERE last_seen_at < ? AND status='online'`, before.Format(tsLayout))
	return err
}

func (s *Store) CreateRun(ctx context.Context, r *store.Run) error {
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO runs(id,overseer_id,workflow_name,workflow_hcl,status,current_step,last_seq,created_at) VALUES(?,?,?,?,?,?,?,?)`,
		r.ID, r.OverseerID, r.WorkflowName, r.WorkflowHCL, r.Status, r.CurrentStep, r.LastSeq, r.CreatedAt.Format(tsLayout))
	return err
}

func (s *Store) GetRun(ctx context.Context, id string) (*store.Run, error) {
	row := s.db.QueryRowContext(ctx, `SELECT id,overseer_id,workflow_name,workflow_hcl,status,current_step,last_seq,created_at,ended_at,variable_scope,pending_signal,paused_at FROM runs WHERE id=?`, id)
	return scanRun(row.Scan)
}

func (s *Store) ListRuns(ctx context.Context, overseerID, status string) ([]*store.Run, error) {
	q := `SELECT id,overseer_id,workflow_name,workflow_hcl,status,current_step,last_seq,created_at,ended_at,variable_scope,pending_signal,paused_at FROM runs WHERE 1=1`
	args := []any{}
	if overseerID != "" {
		q += ` AND overseer_id=?`
		args = append(args, overseerID)
	}
	if status != "" {
		q += ` AND status=?`
		args = append(args, status)
	}
	q += ` ORDER BY created_at DESC`
	rows, err := s.db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*store.Run
	for rows.Next() {
		r, err := scanRun(rows.Scan)
		if err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

func scanRun(scan func(...any) error) (*store.Run, error) {
	var r store.Run
	var created string
	var overseerID sql.NullString
	var ended sql.NullString
	var variableScope sql.NullString
	var pendingSignal sql.NullString
	var pausedAt sql.NullString
	err := scan(&r.ID, &overseerID, &r.WorkflowName, &r.WorkflowHCL, &r.Status, &r.CurrentStep, &r.LastSeq, &created, &ended, &variableScope, &pendingSignal, &pausedAt)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, store.ErrNotFound
		}
		return nil, err
	}
	if overseerID.Valid {
		r.OverseerID = overseerID.String
	}
	r.CreatedAt, _ = time.Parse(tsLayout, created)
	if ended.Valid {
		t, _ := time.Parse(tsLayout, ended.String)
		r.EndedAt = &t
	}
	if variableScope.Valid {
		r.VariableScope = variableScope.String
	}
	if pendingSignal.Valid {
		r.PendingSignal = pendingSignal.String
	}
	if pausedAt.Valid {
		t, _ := time.Parse(tsLayout, pausedAt.String)
		r.PausedAt = &t
	}
	return &r, nil
}

func (s *Store) UpdateRun(ctx context.Context, r *store.Run) error {
	var ended any
	if r.EndedAt != nil {
		ended = r.EndedAt.Format(tsLayout)
	}
	_, err := s.db.ExecContext(ctx,
		`UPDATE runs SET status=?, current_step=?, last_seq=?, ended_at=? WHERE id=?`,
		r.Status, r.CurrentStep, r.LastSeq, ended, r.ID)
	return err
}

// SetRunVariableScope persists a JSON-encoded variable scope snapshot (W04).
func (s *Store) SetRunVariableScope(ctx context.Context, runID, scope string) error {
	_, err := s.db.ExecContext(ctx,
		`UPDATE runs SET variable_scope=? WHERE id=?`, scope, runID)
	return err
}

// GetRunVariableScope returns the stored variable scope JSON for runID.
// Returns ("", nil) when the run exists but has no scope yet (NULL column).
func (s *Store) GetRunVariableScope(ctx context.Context, runID string) (string, error) {
	var scope sql.NullString
	err := s.db.QueryRowContext(ctx, `SELECT variable_scope FROM runs WHERE id=?`, runID).Scan(&scope)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return "", store.ErrNotFound
		}
		return "", err
	}
	if !scope.Valid {
		return "", nil
	}
	return scope.String, nil
}

// SetRunPaused marks a run as paused with the given pending signal (W05).
func (s *Store) SetRunPaused(ctx context.Context, runID, pendingSignal string, pausedAt time.Time) error {
	_, err := s.db.ExecContext(ctx,
		`UPDATE runs SET status='paused', pending_signal=?, paused_at=? WHERE id=?`,
		pendingSignal, pausedAt.Format(tsLayout), runID)
	return err
}

// ClearRunPaused clears the pause state and sets status back to running (W05).
func (s *Store) ClearRunPaused(ctx context.Context, runID string) error {
	_, err := s.db.ExecContext(ctx,
		`UPDATE runs SET status='running', pending_signal=NULL, paused_at=NULL WHERE id=?`, runID)
	return err
}

func (s *Store) AppendEvent(ctx context.Context, ev *store.Event) (uint64, bool, error) {
	if ev == nil {
		return 0, false, errors.New("nil event")
	}
	if ev.RunID == "" {
		return 0, false, errors.New("run_id required")
	}
	if ev.Type == "" {
		return 0, false, errors.New("event type required")
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, false, err
	}
	defer tx.Rollback() //nolint:errcheck

	// Idempotency on (run_id, correlation_id): if an event with this
	// correlation id already exists for this run, return its seq without
	// inserting again. Empty correlation ids skip this check.
	if ev.CorrelationID != "" {
		var existing sql.NullInt64
		if err := tx.QueryRowContext(ctx,
			`SELECT seq FROM events WHERE run_id=? AND correlation_id=? LIMIT 1`,
			ev.RunID, ev.CorrelationID).Scan(&existing); err != nil && !errors.Is(err, sql.ErrNoRows) {
			return 0, false, err
		}
		if existing.Valid {
			// No commit necessary (read-only); release the tx.
			_ = tx.Rollback()
			seq := uint64(existing.Int64)
			ev.Seq = seq
			return seq, false, nil
		}
	}

	var lastSeq sql.NullInt64
	if err := tx.QueryRowContext(ctx, `SELECT MAX(seq) FROM events WHERE run_id=?`, ev.RunID).Scan(&lastSeq); err != nil {
		return 0, false, err
	}
	next := uint64(1)
	if lastSeq.Valid {
		next = uint64(lastSeq.Int64) + 1
	}
	ts := ev.Ts
	if ts.IsZero() {
		ts = time.Now().UTC()
	}
	_, err = tx.ExecContext(ctx,
		`INSERT INTO events(run_id,seq,type,ts,correlation_id,payload) VALUES(?,?,?,?,?,?)`,
		ev.RunID, next, ev.Type, ts.Format(tsLayout), ev.CorrelationID, string(ev.Payload))
	if err != nil {
		return 0, false, err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE runs SET last_seq=? WHERE id=? AND last_seq < ?`, next, ev.RunID, next); err != nil {
		return 0, false, err
	}
	if err := tx.Commit(); err != nil {
		return 0, false, err
	}
	ev.Seq = next
	ev.Ts = ts
	return next, true, nil
}

func (s *Store) ListEvents(ctx context.Context, runID string, since uint64, limit int) ([]*store.Event, error) {
	normalized, err := normalizeListLimit(limit)
	if err != nil {
		return nil, err
	}
	rows, err := s.db.QueryContext(ctx,
		`SELECT seq,type,ts,correlation_id,payload FROM events WHERE run_id=? AND seq>? ORDER BY seq ASC LIMIT ?`,
		runID, since, normalized)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanEventRows(rows, runID)
}

func (s *Store) ListStepLogs(ctx context.Context, runID, step string, since uint64, limit int) ([]*store.Event, error) {
	normalized, err := normalizeListLimit(limit)
	if err != nil {
		return nil, err
	}
	rows, err := s.db.QueryContext(ctx,
		`SELECT seq,type,ts,correlation_id,payload
		 FROM events
		 WHERE run_id=? AND seq>? AND type='step.log' AND json_extract(payload, '$.step')=?
		 ORDER BY seq ASC LIMIT ?`,
		runID, since, step, normalized)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanEventRows(rows, runID)
}

func (s *Store) UpsertSubscriberCursor(ctx context.Context, subscriberID, runID string, lastSeq uint64) error {
	if subscriberID == "" {
		return errors.New("subscriber_id required")
	}
	if runID == "" {
		return errors.New("run_id required")
	}

	_, err := s.db.ExecContext(ctx, `
		INSERT INTO run_subscriptions(subscriber_id, run_id, last_seq, updated_at)
		VALUES (?, ?, ?, CURRENT_TIMESTAMP)
		ON CONFLICT(subscriber_id, run_id) DO UPDATE SET
			last_seq = CASE
				WHEN excluded.last_seq > run_subscriptions.last_seq THEN excluded.last_seq
				ELSE run_subscriptions.last_seq
			END,
			updated_at = CASE
				WHEN excluded.last_seq > run_subscriptions.last_seq THEN CURRENT_TIMESTAMP
				ELSE run_subscriptions.updated_at
			END
	`, subscriberID, runID, lastSeq)
	return err
}

func (s *Store) GetSubscriberCursor(ctx context.Context, subscriberID, runID string) (uint64, bool, error) {
	if subscriberID == "" {
		return 0, false, errors.New("subscriber_id required")
	}
	if runID == "" {
		return 0, false, errors.New("run_id required")
	}

	var seq uint64
	err := s.db.QueryRowContext(ctx, `SELECT last_seq FROM run_subscriptions WHERE subscriber_id=? AND run_id=?`, subscriberID, runID).Scan(&seq)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return 0, false, nil
		}
		return 0, false, err
	}
	return seq, true, nil
}

func (s *Store) RecordAttemptStart(ctx context.Context, ra *store.RunAttempt) error {
	if ra.RunID == "" || ra.Step == "" || ra.Attempt <= 0 {
		return errors.New("run_id, step, and attempt > 0 required")
	}
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO run_attempts(run_id, step, attempt, started_at)
		VALUES (?, ?, ?, ?)
		ON CONFLICT(run_id, step, attempt) DO NOTHING`,
		ra.RunID, ra.Step, ra.Attempt, ra.StartedAt.UTC().Format(tsLayout))
	return err
}

func (s *Store) RecordAttemptComplete(ctx context.Context, runID, step string, attempt int, outcome string) error {
	if runID == "" || step == "" || attempt <= 0 {
		return errors.New("run_id, step, and attempt > 0 required")
	}
	now := time.Now().UTC().Format(tsLayout)
	_, err := s.db.ExecContext(ctx, `
		UPDATE run_attempts SET completed_at=?, outcome=?
		WHERE run_id=? AND step=? AND attempt=?`,
		now, outcome, runID, step, attempt)
	return err
}

func (s *Store) GetLatestAttempt(ctx context.Context, runID, step string) (*store.RunAttempt, error) {
	if runID == "" || step == "" {
		return nil, errors.New("run_id and step required")
	}
	row := s.db.QueryRowContext(ctx,
		`SELECT run_id, step, attempt, started_at, completed_at, outcome
		 FROM run_attempts WHERE run_id=? AND step=?
		 ORDER BY attempt DESC LIMIT 1`,
		runID, step)
	var ra store.RunAttempt
	var started string
	var completed sql.NullString
	var outcome sql.NullString
	err := row.Scan(&ra.RunID, &ra.Step, &ra.Attempt, &started, &completed, &outcome)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, store.ErrNotFound
		}
		return nil, err
	}
	ra.StartedAt, _ = time.Parse(tsLayout, started)
	if completed.Valid {
		t, _ := time.Parse(tsLayout, completed.String)
		ra.CompletedAt = &t
	}
	if outcome.Valid {
		ra.Outcome = outcome.String
	}
	return &ra, nil
}

func normalizeListLimit(limit int) (int, error) {
	if limit <= 0 {
		return ListEventsDefaultLimit, nil
	}
	if limit > ListEventsMaxLimit {
		return 0, fmt.Errorf("%w: limit %d exceeds maximum %d", store.ErrInvalidLimit, limit, ListEventsMaxLimit)
	}
	return limit, nil
}

func scanEventRows(rows *sql.Rows, runID string) ([]*store.Event, error) {
	var out []*store.Event
	for rows.Next() {
		var seq int64
		var ts string
		var payload string
		var corr sql.NullString
		var typ string
		if err := rows.Scan(&seq, &typ, &ts, &corr, &payload); err != nil {
			return nil, err
		}
		ev := &store.Event{
			SchemaVersion: store.EventSchemaVersion,
			RunID:         runID,
			Seq:           uint64(seq),
			Type:          typ,
			Payload:       []byte(payload),
		}
		if parsed, err := time.Parse(tsLayout, ts); err == nil {
			ev.Ts = parsed
		}
		if corr.Valid {
			ev.CorrelationID = corr.String
		}
		out = append(out, ev)
	}
	return out, rows.Err()
}

func marshalLabels(m map[string]string) (string, error) {
	if len(m) == 0 {
		return "", nil
	}
	b, err := json.Marshal(m)
	if err != nil {
		return "", fmt.Errorf("marshal labels: %w", err)
	}
	return string(b), nil
}

func unmarshalLabels(s string) map[string]string {
	if s == "" {
		return nil
	}
	var m map[string]string
	if err := json.Unmarshal([]byte(s), &m); err != nil {
		return nil
	}
	return m
}

func (s *Store) CreateWorkflowAssignment(ctx context.Context, a *store.WorkflowAssignment) (*store.WorkflowAssignment, bool, error) {
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
		SELECT id, owner_criteria_id, run_id, workflow_name, workflow_source, lockfile_source,
		       idempotency_key, state, terminal_reason, leased_criteria_id, lease_expires_at,
		       created_at, updated_at
		FROM workflow_assignments
		WHERE owner_criteria_id=? AND idempotency_key=?`, a.OwnerCriteriaID, a.IdempotencyKey)
	if err != nil && !errors.Is(err, store.ErrNotFound) {
		return nil, false, err
	}
	if existing != nil {
		_ = tx.Rollback()
		return existing, false, nil
	}

	if _, err := tx.ExecContext(ctx,
		`INSERT INTO runs(id, overseer_id, workflow_name, workflow_hcl, status, current_step, last_seq, created_at)
		 VALUES(?, NULL, ?, ?, 'pending', '', 0, ?)`,
		a.RunID, a.WorkflowName, a.WorkflowSource, a.CreatedAt.Format(tsLayout)); err != nil {
		return nil, false, err
	}

	if _, err := tx.ExecContext(ctx,
		`INSERT INTO workflow_assignments(id, owner_criteria_id, run_id, workflow_name, workflow_source, lockfile_source,
		                                   idempotency_key, state, created_at, updated_at)
		 VALUES(?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		a.ID, a.OwnerCriteriaID, a.RunID, a.WorkflowName, a.WorkflowSource, a.LockfileSource,
		a.IdempotencyKey, a.State, a.CreatedAt.Format(tsLayout), a.UpdatedAt.Format(tsLayout)); err != nil {
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
			`INSERT INTO workflow_assignment_labels(assignment_id, key, value) VALUES(?, ?, ?)`,
			assignmentID, k, v); err != nil {
			return err
		}
	}
	return nil
}

func (s *Store) GetWorkflowAssignment(ctx context.Context, id string) (*store.WorkflowAssignment, error) {
	if id == "" {
		return nil, errors.New("id required")
	}
	return s.scanAssignment(ctx, s.db.QueryRowContext(ctx, `
		SELECT id, owner_criteria_id, run_id, workflow_name, workflow_source, lockfile_source,
		       idempotency_key, state, terminal_reason, leased_criteria_id, lease_expires_at,
		       created_at, updated_at
		FROM workflow_assignments WHERE id=?`, id))
}

func (s *Store) GetWorkflowAssignmentByRunID(ctx context.Context, runID string) (*store.WorkflowAssignment, error) {
	if runID == "" {
		return nil, errors.New("run_id required")
	}
	return s.scanAssignment(ctx, s.db.QueryRowContext(ctx, `
		SELECT id, owner_criteria_id, run_id, workflow_name, workflow_source, lockfile_source,
		       idempotency_key, state, terminal_reason, leased_criteria_id, lease_expires_at,
		       created_at, updated_at
		FROM workflow_assignments WHERE run_id=?`, runID))
}

func (s *Store) scanAssignment(ctx context.Context, row *sql.Row) (*store.WorkflowAssignment, error) {
	var a store.WorkflowAssignment
	var created, updated string
	var lockfile, terminalReason, leasedID, expires sql.NullString
	err := row.Scan(
		&a.ID, &a.OwnerCriteriaID, &a.RunID, &a.WorkflowName, &a.WorkflowSource, &lockfile,
		&a.IdempotencyKey, &a.State, &terminalReason, &leasedID, &expires,
		&created, &updated)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, store.ErrNotFound
		}
		return nil, err
	}
	a.CreatedAt, _ = time.Parse(tsLayout, created)
	a.UpdatedAt, _ = time.Parse(tsLayout, updated)
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
		t, _ := time.Parse(tsLayout, expires.String)
		a.LeaseExpiresAt = &t
	}
	labels, err := s.loadAssignmentLabels(ctx, a.ID)
	if err != nil {
		return nil, err
	}
	a.Labels = labels
	return &a, nil
}

func scanAssignmentTx(ctx context.Context, tx *sql.Tx, query string, args ...any) (*store.WorkflowAssignment, error) {
	var a store.WorkflowAssignment
	var created, updated string
	var lockfile, terminalReason, leasedID, expires sql.NullString
	row := tx.QueryRowContext(ctx, query, args...)
	err := row.Scan(
		&a.ID, &a.OwnerCriteriaID, &a.RunID, &a.WorkflowName, &a.WorkflowSource, &lockfile,
		&a.IdempotencyKey, &a.State, &terminalReason, &leasedID, &expires,
		&created, &updated)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, store.ErrNotFound
		}
		return nil, err
	}
	a.CreatedAt, _ = time.Parse(tsLayout, created)
	a.UpdatedAt, _ = time.Parse(tsLayout, updated)
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
		t, _ := time.Parse(tsLayout, expires.String)
		a.LeaseExpiresAt = &t
	}
	return &a, nil
}

func (s *Store) loadAssignmentLabels(ctx context.Context, assignmentID string) (map[string]string, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT key, value FROM workflow_assignment_labels WHERE assignment_id=?`, assignmentID)
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
// run's overseer_id is updated to criteriaID.
func (s *Store) LeaseWorkflowAssignment(ctx context.Context, criteriaID string, agentLabels map[string]string, now time.Time, leaseDuration time.Duration) (*store.WorkflowAssignment, error) {
	if criteriaID == "" {
		return nil, errors.New("criteria_id required")
	}
	expiresAt := now.Add(leaseDuration)

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback() //nolint:errcheck

	// Transactionally expire stale leases back to a leasable queued state.
	if _, err := tx.ExecContext(ctx,
		`UPDATE workflow_assignments
		 SET state=?, leased_criteria_id=NULL, lease_expires_at=NULL, updated_at=?
		 WHERE state=? AND lease_expires_at < ?`,
		store.WorkflowAssignmentStateQueued, now.Format(tsLayout),
		store.WorkflowAssignmentStateLeased, now.Format(tsLayout)); err != nil {
		return nil, err
	}

	// Verify the agent is online.
	var status string
	if err := tx.QueryRowContext(ctx, `SELECT status FROM overseers WHERE id=?`, criteriaID).Scan(&status); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, store.ErrNotFound
		}
		return nil, err
	}
	if status != "online" {
		return nil, store.ErrNotFound
	}

	// Find queued assignments and their required labels, oldest first.
	rows, err := tx.QueryContext(ctx, `
		SELECT a.id, a.run_id, a.workflow_name, a.workflow_source, a.lockfile_source,
		       a.idempotency_key, a.created_at, a.updated_at, l.key, l.value
		FROM workflow_assignments a
		LEFT JOIN workflow_assignment_labels l ON l.assignment_id = a.id
		WHERE a.state = ?
		ORDER BY a.created_at ASC, a.id ASC`,
		store.WorkflowAssignmentStateQueued)
	if err != nil {
		return nil, err
	}

	candidates := make(map[string]*assignmentCandidate)
	for rows.Next() {
		var id, runID, wfName, wfSource, idemp, created, updated string
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
					CreatedAt:      mustParseTime(created),
					UpdatedAt:      mustParseTime(updated),
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
			 SET state=?, leased_criteria_id=?, lease_expires_at=?, updated_at=?
			 WHERE id=? AND state=?`,
			store.WorkflowAssignmentStateLeased, criteriaID, expiresAt.Format(tsLayout),
			now.Format(tsLayout), c.assignment.ID, store.WorkflowAssignmentStateQueued)
		if err != nil {
			return nil, err
		}
		affected, _ := res.RowsAffected()
		if affected == 0 {
			continue
		}
		if _, err := tx.ExecContext(ctx,
			`UPDATE runs SET overseer_id=? WHERE id=?`, criteriaID, c.assignment.RunID); err != nil {
			return nil, err
		}
		leaseID := uuid.NewString()
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO workflow_assignment_leases(id, assignment_id, criteria_id, created_at, expires_at)
			 VALUES(?, ?, ?, ?, ?)`,
			leaseID, c.assignment.ID, criteriaID, now.Format(tsLayout), expiresAt.Format(tsLayout)); err != nil {
			return nil, err
		}
		var attempt int
		if err := tx.QueryRowContext(ctx,
			`SELECT COALESCE(MAX(attempt),0)+1 FROM workflow_assignment_attempts WHERE assignment_id=?`,
			c.assignment.ID).Scan(&attempt); err != nil {
			return nil, err
		}
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO workflow_assignment_attempts(assignment_id, attempt, criteria_id, created_at)
			 VALUES(?, ?, ?, ?)`,
			c.assignment.ID, attempt, criteriaID, now.Format(tsLayout)); err != nil {
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

func mustParseTime(s string) time.Time {
	t, _ := time.Parse(tsLayout, s)
	return t
}

func (s *Store) RecordWorkflowAssignmentLease(ctx context.Context, lease *store.WorkflowAssignmentLease) error {
	if lease == nil || lease.AssignmentID == "" || lease.CriteriaID == "" {
		return errors.New("assignment_id and criteria_id required")
	}
	if lease.ID == "" {
		lease.ID = uuid.NewString()
	}
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO workflow_assignment_leases(id, assignment_id, criteria_id, created_at, expires_at)
		 VALUES(?, ?, ?, ?, ?)`,
		lease.ID, lease.AssignmentID, lease.CriteriaID, lease.CreatedAt.Format(tsLayout), lease.ExpiresAt.Format(tsLayout))
	return err
}

func (s *Store) RecordWorkflowAssignmentAttempt(ctx context.Context, attempt *store.WorkflowAssignmentAttempt) error {
	if attempt == nil || attempt.AssignmentID == "" || attempt.Attempt <= 0 {
		return errors.New("assignment_id and attempt > 0 required")
	}
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO workflow_assignment_attempts(assignment_id, attempt, criteria_id, created_at)
		 VALUES(?, ?, ?, ?)`,
		attempt.AssignmentID, attempt.Attempt, attempt.CriteriaID, attempt.CreatedAt.Format(tsLayout))
	return err
}

func (s *Store) CompleteWorkflowAssignmentAttempt(ctx context.Context, assignmentID string, attempt int, outcome string) error {
	if assignmentID == "" || attempt <= 0 {
		return errors.New("assignment_id and attempt > 0 required")
	}
	now := time.Now().UTC().Format(tsLayout)
	_, err := s.db.ExecContext(ctx,
		`UPDATE workflow_assignment_attempts SET completed_at=?, outcome=?
		 WHERE assignment_id=? AND attempt=?`,
		now, outcome, assignmentID, attempt)
	return err
}

func (s *Store) MarkWorkflowAssignmentTerminal(ctx context.Context, runID, reason string) error {
	if runID == "" {
		return errors.New("run_id required")
	}
	now := time.Now().UTC()
	_, err := s.db.ExecContext(ctx,
		`UPDATE workflow_assignments
		 SET state=?, terminal_reason=?, updated_at=?
		 WHERE run_id=? AND state IN (?, ?)`,
		store.WorkflowAssignmentStateTerminal, reason, now.Format(tsLayout),
		runID, store.WorkflowAssignmentStateQueued, store.WorkflowAssignmentStateLeased)
	return err
}
