// Package sqlite is the SQLite implementation of store.Store using the
// pure-Go modernc.org/sqlite driver (no CGO required).
package sqlite

import (
	"context"
	"database/sql"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	_ "modernc.org/sqlite"

	"github.com/brokenbots/overlord/castle/internal/store"
	"github.com/brokenbots/overlord/shared/events"
)

//go:embed migrations/0001_init.sql
var initSQL string

type Store struct {
	db *sql.DB
}

func Open(path string) (*Store, error) {
	dsn := fmt.Sprintf("file:%s?_pragma=journal_mode(WAL)&_pragma=foreign_keys(1)&_pragma=busy_timeout(5000)", path)
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, err
	}
	if _, err := db.Exec(initSQL); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("init schema: %w", err)
	}
	return &Store{db: db}, nil
}

func (s *Store) Close() error { return s.db.Close() }

const tsLayout = time.RFC3339Nano

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
	row := s.db.QueryRowContext(ctx, `SELECT id,overseer_id,workflow_name,workflow_hcl,status,current_step,last_seq,created_at,ended_at FROM runs WHERE id=?`, id)
	return scanRun(row.Scan)
}

func (s *Store) ListRuns(ctx context.Context, overseerID, status string) ([]*store.Run, error) {
	q := `SELECT id,overseer_id,workflow_name,workflow_hcl,status,current_step,last_seq,created_at,ended_at FROM runs WHERE 1=1`
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
	err := scan(&r.ID, &r.OverseerID, &r.WorkflowName, &r.WorkflowHCL, &r.Status, &r.CurrentStep, &r.LastSeq, &created, &ended)
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

func (s *Store) AppendEvent(ctx context.Context, env events.Envelope) (uint64, error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback() //nolint:errcheck

	var lastSeq sql.NullInt64
	if err := tx.QueryRowContext(ctx, `SELECT MAX(seq) FROM events WHERE run_id=?`, env.RunID).Scan(&lastSeq); err != nil {
		return 0, err
	}
	next := uint64(1)
	if lastSeq.Valid {
		next = uint64(lastSeq.Int64) + 1
	}
	payload := env.Payload
	if payload == nil {
		payload = json.RawMessage(`{}`)
	}
	_, err = tx.ExecContext(ctx,
		`INSERT INTO events(run_id,seq,type,ts,correlation_id,payload) VALUES(?,?,?,?,?,?)`,
		env.RunID, next, string(env.Type), env.Timestamp.Format(tsLayout), env.CorrelationID, string(payload))
	if err != nil {
		return 0, err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE runs SET last_seq=? WHERE id=? AND last_seq < ?`, next, env.RunID, next); err != nil {
		return 0, err
	}
	if err := tx.Commit(); err != nil {
		return 0, err
	}
	return next, nil
}

func (s *Store) ListEvents(ctx context.Context, runID string, since uint64, limit int) ([]events.Envelope, error) {
	if limit <= 0 || limit > 1000 {
		limit = 500
	}
	rows, err := s.db.QueryContext(ctx,
		`SELECT seq,type,ts,correlation_id,payload FROM events WHERE run_id=? AND seq>? ORDER BY seq ASC LIMIT ?`,
		runID, since, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []events.Envelope
	for rows.Next() {
		var e events.Envelope
		var seq int64
		var ts string
		var payload string
		var corr sql.NullString
		var typ string
		if err := rows.Scan(&seq, &typ, &ts, &corr, &payload); err != nil {
			return nil, err
		}
		e.SchemaVersion = events.SchemaVersion
		e.RunID = runID
		e.Seq = uint64(seq)
		e.Type = events.Type(typ)
		e.Timestamp, _ = time.Parse(tsLayout, ts)
		if corr.Valid {
			e.CorrelationID = corr.String
		}
		e.Payload = json.RawMessage(payload)
		out = append(out, e)
	}
	return out, rows.Err()
}
