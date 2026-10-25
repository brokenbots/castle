package pg

import (
	"context"
	"database/sql"
	"time"

	"github.com/brokenbots/castle/castle/internal/store"
)

// UpsertOrchestrator inserts or replaces an orchestrator identity (CRI-133).
// On conflict, name and token_hash are replaced — rotating the token
// invalidates previously issued accept tokens — and created_at is preserved.
// KB-330: the orchestrator row is an alias view of its operator row — keep
// both consistent, operator first (the alias FK points at it).
func (s *Store) UpsertOrchestrator(ctx context.Context, o *store.Orchestrator) error {
	ctx, cancel := s.opCtx(ctx)
	defer cancel()
	if _, err := s.db.ExecContext(ctx,
		`INSERT INTO operators(id,name,display_name,password_hash,attributes,created_at,updated_at) VALUES($1,$2,$3,NULL,'{"role":"orchestrator"}'::jsonb,$4,$4)
		 ON CONFLICT(id) DO UPDATE SET name=excluded.name, updated_at=excluded.updated_at`,
		o.ID, o.Name, o.Name, us(o.CreatedAt)); err != nil {
		return err
	}
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO orchestrators(id,name,token_hash,created_at,operator_id) VALUES($1,$2,$3,$4,$1)
		 ON CONFLICT(id) DO UPDATE SET name=excluded.name, token_hash=excluded.token_hash, operator_id=excluded.operator_id`,
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
// token (CRI-133). Deleting an unknown ID is a no-op. Since KB-330 the
// orchestrator's alias operator row goes too, so the identity disappears
// from the operators model as well.
func (s *Store) DeleteOrchestrator(ctx context.Context, id string) error {
	ctx, cancel := s.opCtx(ctx)
	defer cancel()
	if _, err := s.db.ExecContext(ctx, `DELETE FROM orchestrators WHERE id = $1`, id); err != nil {
		return err
	}
	_, err := s.db.ExecContext(ctx, `DELETE FROM operators WHERE id = $1`, id)
	return err
}

// UpsertConsoleUser inserts or replaces a console user (CRI-195). Since
// KB-330 the console user IS an operator: the write lands on the
// password-carrying operators row (admin role) keyed by the same id. On
// conflict, name and password_hash are replaced — rotating the seeded
// credentials — and created_at is preserved.
func (s *Store) UpsertConsoleUser(ctx context.Context, u *store.ConsoleUser) error {
	op := &store.Operator{
		ID:           u.ID,
		Name:         u.Username,
		DisplayName:  u.Username,
		PasswordHash: &u.PasswordHash,
		Attributes:   map[string]string{store.OperatorAttributeRole: store.OperatorRoleAdmin},
		CreatedAt:    u.CreatedAt,
		UpdatedAt:    u.UpdatedAt,
	}
	return s.UpsertOperator(ctx, op)
}

// GetConsoleUser returns the console user with the given username: the
// password-carrying operator whose name matches (KB-330). Token-only
// operators never resolve as console users.
func (s *Store) GetConsoleUser(ctx context.Context, username string) (*store.ConsoleUser, error) {
	ctx, cancel := s.opCtx(ctx)
	defer cancel()
	row := s.db.QueryRowContext(ctx,
		`SELECT id,name,password_hash,created_at,updated_at FROM operators WHERE name=$1 AND password_hash IS NOT NULL`, username)
	var u store.ConsoleUser
	if err := row.Scan(&u.ID, &u.Username, &u.PasswordHash, &u.CreatedAt, &u.UpdatedAt); err != nil {
		if err == sql.ErrNoRows {
			return nil, store.ErrNotFound
		}
		return nil, err
	}
	return &u, nil
}

// DeleteConsoleUsers removes the password-carrying operators (console
// users); the ON DELETE CASCADE on console_sessions revokes their sessions
// with them (CRI-195). Token-only operators are untouched.
func (s *Store) DeleteConsoleUsers(ctx context.Context) error {
	ctx, cancel := s.opCtx(ctx)
	defer cancel()
	_, err := s.db.ExecContext(ctx, `DELETE FROM operators WHERE password_hash IS NOT NULL`)
	return err
}

func (s *Store) CreateConsoleSession(ctx context.Context, sess *store.ConsoleSession) error {
	ctx, cancel := s.opCtx(ctx)
	defer cancel()
	membership, err := store.MarshalMembership(sess.Membership)
	if err != nil {
		return err
	}
	_, err = s.db.ExecContext(ctx,
		`INSERT INTO console_sessions(id,operator_id,token_hash,created_at,revoked_at,membership) VALUES($1,$2,$3,$4,$5,$6)`,
		sess.ID, sess.OperatorID, sess.TokenHash, us(sess.CreatedAt), timePtr(sess.RevokedAt), membership)
	return err
}

func (s *Store) ListConsoleSessions(ctx context.Context) ([]*store.ConsoleSession, error) {
	ctx, cancel := s.opCtx(ctx)
	defer cancel()
	rows, err := s.reader.QueryContext(ctx, `SELECT id,operator_id,token_hash,created_at,revoked_at,membership FROM console_sessions ORDER BY created_at ASC`)

	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*store.ConsoleSession
	for rows.Next() {
		var sess store.ConsoleSession
		var revoked *time.Time
		var membership []byte
		if err := rows.Scan(&sess.ID, &sess.OperatorID, &sess.TokenHash, &sess.CreatedAt, &revoked, &membership); err != nil {
			return nil, err
		}
		sess.RevokedAt = revoked
		sess.Membership = store.UnmarshalMembership(string(membership))
		out = append(out, &sess)
	}
	return out, rows.Err()
}

// RevokeConsoleSession stamps the server-side revocation (KB-325) on the
// ledger row. Idempotent: unknown ids and already-revoked sessions are
// touched again with the FIRST stamp winning (COALESCE keeps the original
// stamp on a re-logout, so the ledger records one revocation instant).
func (s *Store) RevokeConsoleSession(ctx context.Context, sessionID string, at time.Time) error {
	ctx, cancel := s.opCtx(ctx)
	defer cancel()
	_, err := s.db.ExecContext(ctx,
		`UPDATE console_sessions SET revoked_at = COALESCE(revoked_at, $1) WHERE id = $2`,
		us(at), sessionID)
	return err
}

func (s *Store) DeleteConsoleSessions(ctx context.Context) error {
	ctx, cancel := s.opCtx(ctx)
	defer cancel()
	_, err := s.db.ExecContext(ctx, `DELETE FROM console_sessions`)
	return err
}

// DeleteConsoleSessionsByUser revokes every login session of one console
// operator (keyed by operator id since KB-330), e.g. when the seeded
// password is rotated (CRI-195).
func (s *Store) DeleteConsoleSessionsByUser(ctx context.Context, userID string) error {
	ctx, cancel := s.opCtx(ctx)
	defer cancel()
	_, err := s.db.ExecContext(ctx, `DELETE FROM console_sessions WHERE operator_id = $1`, userID)
	return err
}
