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
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO triggers(id,token_hash,created_at) VALUES(?,?,?)
		 ON CONFLICT(id) DO UPDATE SET token_hash=excluded.token_hash`,
		t.ID, t.TokenHash, t.CreatedAt.Format(store.TimestampLayout))
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
// (KB-320). Deleting an unknown ID is a no-op.
func (s *Store) DeleteTrigger(ctx context.Context, id string) error {
	ctx, cancel := s.opCtx(ctx)
	defer cancel()
	_, err := s.db.ExecContext(ctx, `DELETE FROM triggers WHERE id = ?`, id)
	return err
}
