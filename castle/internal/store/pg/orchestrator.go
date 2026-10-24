package pg

import (
	"context"
	"database/sql"

	"github.com/brokenbots/castle/castle/internal/store"
)

// UpsertOrchestrator inserts or replaces an orchestrator identity (CRI-133).
// On conflict, name and token_hash are replaced — rotating the token
// invalidates previously issued accept tokens — and created_at is preserved.
func (s *Store) UpsertOrchestrator(ctx context.Context, o *store.Orchestrator) error {
	ctx, cancel := s.opCtx(ctx)
	defer cancel()
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO orchestrators(id,name,token_hash,created_at) VALUES($1,$2,$3,$4)
		 ON CONFLICT(id) DO UPDATE SET name=excluded.name, token_hash=excluded.token_hash`,
		o.ID, o.Name, o.TokenHash, us(o.CreatedAt))
	return err
}

func (s *Store) ListOrchestrators(ctx context.Context) ([]*store.Orchestrator, error) {
	ctx, cancel := s.opCtx(ctx)
	defer cancel()
	rows, err := s.reader.QueryContext(ctx, `SELECT id,name,token_hash,created_at FROM orchestrators ORDER BY created_at ASC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*store.Orchestrator
	for rows.Next() {
		var o store.Orchestrator
		if err := rows.Scan(&o.ID, &o.Name, &o.TokenHash, &o.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, &o)
	}
	return out, rows.Err()
}

// DeleteOrchestrator removes the orchestrator identity, revoking its accept
// token (CRI-133). Deleting an unknown ID is a no-op.
func (s *Store) DeleteOrchestrator(ctx context.Context, id string) error {
	ctx, cancel := s.opCtx(ctx)
	defer cancel()
	_, err := s.db.ExecContext(ctx, `DELETE FROM orchestrators WHERE id = $1`, id)
	return err
}

// UpsertConsoleUser inserts or replaces a console user (CRI-195). On
// conflict, username and password_hash are replaced — rotating the seeded
// credentials — and created_at is preserved.
func (s *Store) UpsertConsoleUser(ctx context.Context, u *store.ConsoleUser) error {
	ctx, cancel := s.opCtx(ctx)
	defer cancel()
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO console_users(id,username,password_hash,created_at,updated_at) VALUES($1,$2,$3,$4,$5)
		 ON CONFLICT(id) DO UPDATE SET username=excluded.username, password_hash=excluded.password_hash, updated_at=excluded.updated_at`,
		u.ID, u.Username, u.PasswordHash, us(u.CreatedAt), us(u.UpdatedAt))
	return err
}

func (s *Store) GetConsoleUser(ctx context.Context, username string) (*store.ConsoleUser, error) {
	ctx, cancel := s.opCtx(ctx)
	defer cancel()
	row := s.db.QueryRowContext(ctx,
		`SELECT id,username,password_hash,created_at,updated_at FROM console_users WHERE username=$1`, username)
	var u store.ConsoleUser
	if err := row.Scan(&u.ID, &u.Username, &u.PasswordHash, &u.CreatedAt, &u.UpdatedAt); err != nil {
		if err == sql.ErrNoRows {
			return nil, store.ErrNotFound
		}
		return nil, err
	}
	return &u, nil
}

// DeleteConsoleUsers removes every console user; the ON DELETE CASCADE on
// console_sessions revokes their sessions with them (CRI-195).
func (s *Store) DeleteConsoleUsers(ctx context.Context) error {
	ctx, cancel := s.opCtx(ctx)
	defer cancel()
	_, err := s.db.ExecContext(ctx, `DELETE FROM console_users`)
	return err
}

func (s *Store) CreateConsoleSession(ctx context.Context, sess *store.ConsoleSession) error {
	ctx, cancel := s.opCtx(ctx)
	defer cancel()
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO console_sessions(id,user_id,token_hash,created_at) VALUES($1,$2,$3,$4)`,
		sess.ID, sess.UserID, sess.TokenHash, us(sess.CreatedAt))
	return err
}

func (s *Store) ListConsoleSessions(ctx context.Context) ([]*store.ConsoleSession, error) {
	ctx, cancel := s.opCtx(ctx)
	defer cancel()
	rows, err := s.reader.QueryContext(ctx, `SELECT id,user_id,token_hash,created_at FROM console_sessions ORDER BY created_at ASC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*store.ConsoleSession
	for rows.Next() {
		var sess store.ConsoleSession
		if err := rows.Scan(&sess.ID, &sess.UserID, &sess.TokenHash, &sess.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, &sess)
	}
	return out, rows.Err()
}

func (s *Store) DeleteConsoleSessions(ctx context.Context) error {
	ctx, cancel := s.opCtx(ctx)
	defer cancel()
	_, err := s.db.ExecContext(ctx, `DELETE FROM console_sessions`)
	return err
}

// DeleteConsoleSessionsByUser revokes every session of one console user,
// e.g. when the seeded password is rotated (CRI-195).
func (s *Store) DeleteConsoleSessionsByUser(ctx context.Context, userID string) error {
	ctx, cancel := s.opCtx(ctx)
	defer cancel()
	_, err := s.db.ExecContext(ctx, `DELETE FROM console_sessions WHERE user_id = $1`, userID)
	return err
}
