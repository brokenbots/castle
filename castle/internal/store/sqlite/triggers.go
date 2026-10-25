package sqlite

import (
	"context"
	"time"

	"github.com/brokenbots/castle/castle/internal/store"
)

// UpsertTrigger inserts or replaces a trigger identity (KB-320). On
// conflict, token_hash is replaced — rotating the token invalidates
// previously issued accept tokens — and created_at is preserved.
func (s *Store) UpsertTrigger(ctx context.Context, t *store.Trigger) error {
	ctx, cancel := s.opCtx(ctx)
	defer cancel()
	// KB-330: the trigger row is an alias view of its operator row — keep
	// both consistent, operator first (the alias FK points at it). Triggers
	// have no name column, so the id names the alias operator too.
	if _, err := s.db.ExecContext(ctx,
		`INSERT INTO operators(id,name,display_name,password_hash,attributes,created_at,updated_at) VALUES(?,?,?,NULL,'{"role":"trigger"}',?,?)
		 ON CONFLICT(id) DO UPDATE SET updated_at=excluded.updated_at`,
		t.ID, t.ID, t.ID, t.CreatedAt.Format(store.TimestampLayout), t.CreatedAt.Format(store.TimestampLayout)); err != nil {
		return err
	}
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO triggers(id,token_hash,created_at,operator_id) VALUES(?,?,?,?)
		 ON CONFLICT(id) DO UPDATE SET token_hash=excluded.token_hash, operator_id=excluded.operator_id`,
		t.ID, t.TokenHash, t.CreatedAt.Format(store.TimestampLayout), t.ID)
	return err
}

func (s *Store) ListTriggers(ctx context.Context) ([]*store.Trigger, error) {
	ctx, cancel := s.opCtx(ctx)
	defer cancel()
	rows, err := s.reader.QueryContext(ctx, `SELECT id,token_hash,created_at FROM triggers ORDER BY created_at ASC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*store.Trigger
	for rows.Next() {
		var t store.Trigger
		var created string
		if err := rows.Scan(&t.ID, &t.TokenHash, &created); err != nil {
			return nil, err
		}
		t.CreatedAt, _ = time.Parse(store.TimestampLayout, created)
		out = append(out, &t)
	}
	return out, rows.Err()
}

// DeleteTrigger removes the trigger identity, revoking its accept token
// (KB-320). Deleting an unknown ID is a no-op. Since KB-330 the trigger's
// alias operator row goes too, so the identity disappears from the
// operators model as well.
func (s *Store) DeleteTrigger(ctx context.Context, id string) error {
	ctx, cancel := s.opCtx(ctx)
	defer cancel()
	if _, err := s.db.ExecContext(ctx, `DELETE FROM triggers WHERE id = ?`, id); err != nil {
		return err
	}
	_, err := s.db.ExecContext(ctx, `DELETE FROM operators WHERE id = ?`, id)
	return err
}
