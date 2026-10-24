package pg

import (
	"context"
	"database/sql"
	"errors"
	"time"

	"github.com/brokenbots/castle/castle/internal/store"
)

func (s *Store) AppendEvent(ctx context.Context, ev *store.Event) (uint64, bool, error) {
	ctx, cancel := s.opCtx(ctx)
	defer cancel()
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
	defer func() { _ = tx.Rollback() }()

	// Idempotency on (run_id, correlation_id): if an event with this
	// correlation id already exists for this run, return its seq without
	// inserting again. Empty correlation ids skip this check. The partial
	// unique index (0002) enforces the same at-most-once semantics.
	if ev.CorrelationID != "" {
		var existing sql.NullInt64
		if err := tx.QueryRowContext(ctx,
			`SELECT seq FROM events WHERE run_id=$1 AND correlation_id=$2 LIMIT 1`,
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

	// Serialize per-run append ordering. sqlite gets this for free from its
	// single-writer pool; on Postgres concurrent appends to one run must not
	// race the MAX(seq)+1 assignment, and the advisory transaction lock holds
	// until commit. The partition bookkeeping below takes its own lock, so
	// the two locks cannot deadlock ordering-wise.
	if _, err := tx.ExecContext(ctx, `SELECT pg_advisory_xact_lock(hashtext('castle_event_append_' || $1))`, ev.RunID); err != nil {
		return 0, false, err
	}

	var lastSeq sql.NullInt64
	if err := tx.QueryRowContext(ctx, `SELECT MAX(seq) FROM events WHERE run_id=$1`, ev.RunID).Scan(&lastSeq); err != nil {
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
	// The events table is a single logical log partitioned monthly by ts;
	// make sure the target month exists before the first insert of the
	// window. The default partition is the safety net.
	if _, err := tx.ExecContext(ctx, `SELECT castle_ensure_event_partition($1::timestamptz)`, ts); err != nil {
		return 0, false, err
	}
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO events(run_id,seq,type,ts,correlation_id,payload) VALUES($1,$2,$3,$4,$5,$6)`,
		ev.RunID, next, ev.Type, us(ts), ev.CorrelationID, string(ev.Payload)); err != nil {
		return 0, false, err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE runs SET last_seq=$1 WHERE id=$2 AND last_seq < $1`, next, ev.RunID); err != nil {
		return 0, false, err
	}
	if err := tx.Commit(); err != nil {
		return 0, false, err
	}
	ev.Seq = next
	ev.Ts = us(ts)
	return next, true, nil
}

func (s *Store) ListEvents(ctx context.Context, runID string, since uint64, limit int) ([]*store.Event, error) {
	ctx, cancel := s.opCtx(ctx)
	defer cancel()
	normalized, err := store.NormalizeListLimit(limit)
	if err != nil {
		return nil, err
	}
	rows, err := s.reader.QueryContext(ctx,
		`SELECT seq,type,ts,correlation_id,payload FROM events WHERE run_id=$1 AND seq>$2 ORDER BY seq ASC LIMIT $3`,
		runID, since, normalized)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanEventRows(rows, runID)
}

func (s *Store) ListStepLogs(ctx context.Context, runID, step string, since uint64, limit int) ([]*store.Event, error) {
	ctx, cancel := s.opCtx(ctx)
	defer cancel()
	normalized, err := store.NormalizeListLimit(limit)
	if err != nil {
		return nil, err
	}
	rows, err := s.reader.QueryContext(ctx,
		`SELECT seq,type,ts,correlation_id,payload
		 FROM events
		 WHERE run_id=$1 AND seq>$2 AND type='step.log' AND payload->>'step'=$3
		 ORDER BY seq ASC LIMIT $4`,
		runID, since, step, normalized)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanEventRows(rows, runID)
}

func (s *Store) UpsertSubscriberCursor(ctx context.Context, subscriberID, runID string, lastSeq uint64) error {
	ctx, cancel := s.opCtx(ctx)
	defer cancel()
	if subscriberID == "" {
		return errors.New("subscriber_id required")
	}
	if runID == "" {
		return errors.New("run_id required")
	}

	_, err := s.db.ExecContext(ctx, `
		INSERT INTO run_subscriptions(subscriber_id, run_id, last_seq, updated_at)
		VALUES ($1, $2, $3, now())
		ON CONFLICT(subscriber_id, run_id) DO UPDATE SET
			last_seq = CASE
				WHEN excluded.last_seq > run_subscriptions.last_seq THEN excluded.last_seq
				ELSE run_subscriptions.last_seq
			END,
			updated_at = CASE
				WHEN excluded.last_seq > run_subscriptions.last_seq THEN now()
				ELSE run_subscriptions.updated_at
			END
	`, subscriberID, runID, lastSeq)
	return err
}

func (s *Store) GetSubscriberCursor(ctx context.Context, subscriberID, runID string) (uint64, bool, error) {
	ctx, cancel := s.opCtx(ctx)
	defer cancel()
	if subscriberID == "" {
		return 0, false, errors.New("subscriber_id required")
	}
	if runID == "" {
		return 0, false, errors.New("run_id required")
	}

	var seq uint64
	err := s.db.QueryRowContext(ctx, `SELECT last_seq FROM run_subscriptions WHERE subscriber_id=$1 AND run_id=$2`, subscriberID, runID).Scan(&seq)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return 0, false, nil
		}
		return 0, false, err
	}
	return seq, true, nil
}

func (s *Store) RecordAttemptStart(ctx context.Context, ra *store.RunAttempt) error {
	ctx, cancel := s.opCtx(ctx)
	defer cancel()
	if ra == nil || ra.RunID == "" || ra.Step == "" || ra.Attempt <= 0 {
		return errors.New("run_id, step, and attempt > 0 required")
	}
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO run_attempts(run_id, step, attempt, started_at)
		VALUES ($1, $2, $3, $4)
		ON CONFLICT(run_id, step, attempt) DO NOTHING`,
		ra.RunID, ra.Step, ra.Attempt, us(ra.StartedAt))
	return err
}

func (s *Store) RecordAttemptComplete(ctx context.Context, runID, step string, attempt int, outcome string) error {
	ctx, cancel := s.opCtx(ctx)
	defer cancel()
	if runID == "" || step == "" || attempt <= 0 {
		return errors.New("run_id, step, and attempt > 0 required")
	}
	now := time.Now().UTC()
	_, err := s.db.ExecContext(ctx, `
		UPDATE run_attempts SET completed_at=$1, outcome=$2
		WHERE run_id=$3 AND step=$4 AND attempt=$5`,
		us(now), outcome, runID, step, attempt)
	return err
}

func (s *Store) GetLatestAttempt(ctx context.Context, runID, step string) (*store.RunAttempt, error) {
	ctx, cancel := s.opCtx(ctx)
	defer cancel()
	if runID == "" || step == "" {
		return nil, errors.New("run_id and step required")
	}
	row := s.db.QueryRowContext(ctx,
		`SELECT run_id, step, attempt, started_at, completed_at, outcome
		 FROM run_attempts WHERE run_id=$1 AND step=$2
		 ORDER BY attempt DESC LIMIT 1`,
		runID, step)
	var ra store.RunAttempt
	var started time.Time
	var completed sql.NullTime
	var outcome sql.NullString
	err := row.Scan(&ra.RunID, &ra.Step, &ra.Attempt, &started, &completed, &outcome)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, store.ErrNotFound
		}
		return nil, err
	}
	ra.StartedAt = started
	if completed.Valid {
		t := completed.Time
		ra.CompletedAt = &t
	}
	if outcome.Valid {
		ra.Outcome = outcome.String
	}
	return &ra, nil
}

func scanEvent(seq int64, typ string, ts time.Time, corr sql.NullString, payload []byte, runID string) *store.Event {
	ev := &store.Event{
		SchemaVersion: store.EventSchemaVersion,
		RunID:         runID,
		Seq:           uint64(seq),
		Type:          typ,
		Payload:       payload,
		Ts:            ts,
	}
	if corr.Valid {
		ev.CorrelationID = corr.String
	}
	return ev
}

func scanEventRows(rows *sql.Rows, runID string) ([]*store.Event, error) {
	var out []*store.Event
	for rows.Next() {
		var seq int64
		var ts time.Time
		var payload []byte
		var corr sql.NullString
		var typ string
		if err := rows.Scan(&seq, &typ, &ts, &corr, &payload); err != nil {
			return nil, err
		}
		out = append(out, scanEvent(seq, typ, ts, corr, payload, runID))
	}
	return out, rows.Err()
}

func scanEventRow(row *sql.Row, runID string) (*store.Event, error) {
	var seq int64
	var ts time.Time
	var payload []byte
	var corr sql.NullString
	var typ string
	if err := row.Scan(&seq, &typ, &ts, &corr, &payload); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, store.ErrNotFound
		}
		return nil, err
	}
	return scanEvent(seq, typ, ts, corr, payload, runID), nil
}

func (s *Store) GetLatestEvent(ctx context.Context, runID string) (*store.Event, error) {
	ctx, cancel := s.opCtx(ctx)
	defer cancel()
	row := s.db.QueryRowContext(ctx,
		`SELECT seq,type,ts,correlation_id,payload FROM events WHERE run_id=$1 ORDER BY seq DESC LIMIT 1`,
		runID)
	return scanEventRow(row, runID)
}

func (s *Store) GetLatestStepEnteredEvent(ctx context.Context, runID string) (*store.Event, error) {
	ctx, cancel := s.opCtx(ctx)
	defer cancel()
	row := s.db.QueryRowContext(ctx,
		`SELECT seq,type,ts,correlation_id,payload FROM events WHERE run_id=$1 AND type='step.entered' ORDER BY seq DESC LIMIT 1`,
		runID)
	return scanEventRow(row, runID)
}
