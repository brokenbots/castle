// Package sqlite is the SQLite implementation of store.Store using the
// pure-Go modernc.org/sqlite driver (no CGO required).
package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"
	_ "modernc.org/sqlite"

	"github.com/brokenbots/overlord/castle/internal/store"
	"github.com/brokenbots/overlord/shared/events"
	pb "github.com/brokenbots/overlord/shared/pb/overlord/v1"
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
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO overseers(id,name,hostname,version,token_hash,status,created_at,last_seen_at) VALUES(?,?,?,?,?,?,?,?)`,
		o.ID, o.Name, o.Hostname, o.Version, o.TokenHash, o.Status, o.CreatedAt.Format(tsLayout), o.LastSeenAt.Format(tsLayout))
	return err
}

func (s *Store) GetOverseer(ctx context.Context, id string) (*store.Overseer, error) {
	row := s.db.QueryRowContext(ctx, `SELECT id,name,hostname,version,token_hash,status,created_at,last_seen_at FROM overseers WHERE id=?`, id)
	var o store.Overseer
	var created, seen string
	if err := row.Scan(&o.ID, &o.Name, &o.Hostname, &o.Version, &o.TokenHash, &o.Status, &created, &seen); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, store.ErrNotFound
		}
		return nil, err
	}
	o.CreatedAt, _ = time.Parse(tsLayout, created)
	o.LastSeenAt, _ = time.Parse(tsLayout, seen)
	return &o, nil
}

func (s *Store) ListOverseers(ctx context.Context) ([]*store.Overseer, error) {
	rows, err := s.db.QueryContext(ctx, `SELECT id,name,hostname,version,token_hash,status,created_at,last_seen_at FROM overseers ORDER BY created_at DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*store.Overseer
	for rows.Next() {
		var o store.Overseer
		var created, seen string
		if err := rows.Scan(&o.ID, &o.Name, &o.Hostname, &o.Version, &o.TokenHash, &o.Status, &created, &seen); err != nil {
			return nil, err
		}
		o.CreatedAt, _ = time.Parse(tsLayout, created)
		o.LastSeenAt, _ = time.Parse(tsLayout, seen)
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
	var ended sql.NullString
	var variableScope sql.NullString
	var pendingSignal sql.NullString
	var pausedAt sql.NullString
	err := scan(&r.ID, &r.OverseerID, &r.WorkflowName, &r.WorkflowHCL, &r.Status, &r.CurrentStep, &r.LastSeq, &created, &ended, &variableScope, &pendingSignal, &pausedAt)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, store.ErrNotFound
		}
		return nil, err
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

func (s *Store) AppendEvent(ctx context.Context, env *pb.Envelope) (uint64, bool, error) {
	if env == nil {
		return 0, false, errors.New("nil envelope")
	}
	if env.RunId == "" {
		return 0, false, errors.New("run_id required")
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, false, err
	}
	defer tx.Rollback() //nolint:errcheck

	// Idempotency on (run_id, correlation_id): if an event with this
	// correlation id already exists for this run, return its seq without
	// inserting again. Empty correlation ids skip this check.
	if env.CorrelationId != "" {
		var existing sql.NullInt64
		if err := tx.QueryRowContext(ctx,
			`SELECT seq FROM events WHERE run_id=? AND correlation_id=? LIMIT 1`,
			env.RunId, env.CorrelationId).Scan(&existing); err != nil && !errors.Is(err, sql.ErrNoRows) {
			return 0, false, err
		}
		if existing.Valid {
			// No commit necessary (read-only); release the tx.
			_ = tx.Rollback()
			seq := uint64(existing.Int64)
			env.Seq = seq
			return seq, false, nil
		}
	}

	var lastSeq sql.NullInt64
	if err := tx.QueryRowContext(ctx, `SELECT MAX(seq) FROM events WHERE run_id=?`, env.RunId).Scan(&lastSeq); err != nil {
		return 0, false, err
	}
	next := uint64(1)
	if lastSeq.Valid {
		next = uint64(lastSeq.Int64) + 1
	}
	payload, err := marshalPayload(env)
	if err != nil {
		return 0, false, err
	}
	ts := time.Now().UTC()
	if env.Ts != nil && !env.Ts.AsTime().IsZero() {
		ts = env.Ts.AsTime().UTC()
	}
	_, err = tx.ExecContext(ctx,
		`INSERT INTO events(run_id,seq,type,ts,correlation_id,payload) VALUES(?,?,?,?,?,?)`,
		env.RunId, next, events.TypeString(env), ts.Format(tsLayout), env.CorrelationId, string(payload))
	if err != nil {
		return 0, false, err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE runs SET last_seq=? WHERE id=? AND last_seq < ?`, next, env.RunId, next); err != nil {
		return 0, false, err
	}
	if err := tx.Commit(); err != nil {
		return 0, false, err
	}
	env.Seq = next
	env.Ts = timestamppb.New(ts)
	return next, true, nil
}

func (s *Store) ListEvents(ctx context.Context, runID string, since uint64, limit int) ([]*pb.Envelope, error) {
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

func (s *Store) ListStepLogs(ctx context.Context, runID, step string, since uint64, limit int) ([]*pb.Envelope, error) {
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

func scanEventRows(rows *sql.Rows, runID string) ([]*pb.Envelope, error) {
	var out []*pb.Envelope
	for rows.Next() {
		var seq int64
		var ts string
		var payload string
		var corr sql.NullString
		var typ string
		if err := rows.Scan(&seq, &typ, &ts, &corr, &payload); err != nil {
			return nil, err
		}
		env := &pb.Envelope{
			SchemaVersion: events.SchemaVersion,
			RunId:         runID,
			Seq:           uint64(seq),
		}
		if parsed, err := time.Parse(tsLayout, ts); err == nil {
			env.Ts = timestamppb.New(parsed)
		}
		if corr.Valid {
			env.CorrelationId = corr.String
		}
		if err := unmarshalPayload(env, typ, []byte(payload)); err != nil {
			return nil, fmt.Errorf("unmarshal event %d: %w", seq, err)
		}
		out = append(out, env)
	}
	return out, rows.Err()
}

// marshalPayload serialises the payload message (the concrete value inside
// env.Payload) as protojson. The envelope's discriminator string lives in the
// `type` column so the payload blob only needs the inner message.
func marshalPayload(env *pb.Envelope) ([]byte, error) {
	msg := payloadMessage(env)
	if msg == nil {
		return []byte(`{}`), nil
	}
	return protojson.Marshal(msg)
}

// unmarshalPayload hydrates env.Payload from payload bytes according to typ.
func unmarshalPayload(env *pb.Envelope, typ string, payload []byte) error {
	if len(payload) == 0 {
		payload = []byte(`{}`)
	}
	unmarshal := func(msg proto.Message, wrap func(proto.Message)) error {
		u := protojson.UnmarshalOptions{DiscardUnknown: true}
		if err := u.Unmarshal(payload, msg); err != nil {
			return err
		}
		wrap(msg)
		return nil
	}
	switch typ {
	case "run.started":
		return unmarshal(&pb.RunStarted{}, func(m proto.Message) {
			env.Payload = &pb.Envelope_RunStarted{RunStarted: m.(*pb.RunStarted)}
		})
	case "run.completed":
		return unmarshal(&pb.RunCompleted{}, func(m proto.Message) {
			env.Payload = &pb.Envelope_RunCompleted{RunCompleted: m.(*pb.RunCompleted)}
		})
	case "run.failed":
		return unmarshal(&pb.RunFailed{}, func(m proto.Message) {
			env.Payload = &pb.Envelope_RunFailed{RunFailed: m.(*pb.RunFailed)}
		})
	case "step.entered":
		return unmarshal(&pb.StepEntered{}, func(m proto.Message) {
			env.Payload = &pb.Envelope_StepEntered{StepEntered: m.(*pb.StepEntered)}
		})
	case "step.outcome":
		return unmarshal(&pb.StepOutcome{}, func(m proto.Message) {
			env.Payload = &pb.Envelope_StepOutcome{StepOutcome: m.(*pb.StepOutcome)}
		})
	case "step.transition":
		return unmarshal(&pb.StepTransition{}, func(m proto.Message) {
			env.Payload = &pb.Envelope_StepTransition{StepTransition: m.(*pb.StepTransition)}
		})
	case "step.log":
		return unmarshal(&pb.StepLog{}, func(m proto.Message) {
			env.Payload = &pb.Envelope_StepLog{StepLog: m.(*pb.StepLog)}
		})
	case "adapter.event":
		return unmarshal(&pb.AdapterEvent{}, func(m proto.Message) {
			env.Payload = &pb.Envelope_AdapterEvent{AdapterEvent: m.(*pb.AdapterEvent)}
		})
	case "overseer.heartbeat":
		return unmarshal(&pb.OverseerHeartbeat{}, func(m proto.Message) {
			env.Payload = &pb.Envelope_OverseerHeartbeat{OverseerHeartbeat: m.(*pb.OverseerHeartbeat)}
		})
	case "overseer.disconnected":
		return unmarshal(&pb.OverseerDisconnected{}, func(m proto.Message) {
			env.Payload = &pb.Envelope_OverseerDisconnected{OverseerDisconnected: m.(*pb.OverseerDisconnected)}
		})
	case "step.resumed":
		return unmarshal(&pb.StepResumed{}, func(m proto.Message) {
			env.Payload = &pb.Envelope_StepResumed{StepResumed: m.(*pb.StepResumed)}
		})
	default:
		return fmt.Errorf("unknown event type %q", typ)
	}
}

// payloadMessage returns the concrete payload message stored in env.Payload.
func payloadMessage(env *pb.Envelope) proto.Message {
	switch p := env.Payload.(type) {
	case *pb.Envelope_RunStarted:
		return p.RunStarted
	case *pb.Envelope_RunCompleted:
		return p.RunCompleted
	case *pb.Envelope_RunFailed:
		return p.RunFailed
	case *pb.Envelope_StepEntered:
		return p.StepEntered
	case *pb.Envelope_StepOutcome:
		return p.StepOutcome
	case *pb.Envelope_StepTransition:
		return p.StepTransition
	case *pb.Envelope_StepLog:
		return p.StepLog
	case *pb.Envelope_AdapterEvent:
		return p.AdapterEvent
	case *pb.Envelope_OverseerHeartbeat:
		return p.OverseerHeartbeat
	case *pb.Envelope_OverseerDisconnected:
		return p.OverseerDisconnected
	case *pb.Envelope_StepResumed:
		return p.StepResumed
	default:
		return nil
	}
}
