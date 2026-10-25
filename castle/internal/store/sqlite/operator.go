// Operator persistence (KB-330): the ONE identity model behind every
// credential class. The operators table carries the identity + attributes;
// operator_tokens is the credential space for operator-issued tokens. The
// legacy class registries (overseers, orchestrators, triggers) alias onto
// operator rows via their operator_id columns, which the upsert helpers in
// this file keep consistent for rows created after migration.
package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"time"

	"github.com/brokenbots/castle/castle/internal/store"
)

func (s *Store) UpsertOperator(ctx context.Context, o *store.Operator) error {
	ctx, cancel := s.opCtx(ctx)
	defer cancel()
	attrs, err := store.MarshalAttributes(o.Attributes)
	if err != nil {
		return err
	}
	_, err = s.db.ExecContext(ctx,
		`INSERT INTO operators(id,name,display_name,password_hash,attributes,created_at,updated_at) VALUES(?,?,?,?,?,?,?)
		 ON CONFLICT(id) DO UPDATE SET name=excluded.name, display_name=excluded.display_name,
		   password_hash=excluded.password_hash, attributes=excluded.attributes, updated_at=excluded.updated_at`,
		o.ID, o.Name, o.DisplayName, passwordHashPtr(o.PasswordHash), attrs,
		o.CreatedAt.Format(store.TimestampLayout), o.UpdatedAt.Format(store.TimestampLayout))
	return err
}

func (s *Store) GetOperator(ctx context.Context, id string) (*store.Operator, error) {
	ctx, cancel := s.opCtx(ctx)
	defer cancel()
	row := s.db.QueryRowContext(ctx,
		`SELECT id,name,display_name,password_hash,attributes,created_at,updated_at FROM operators WHERE id=?`, id)
	return scanOperator(row.Scan)
}

func (s *Store) DeleteOperator(ctx context.Context, id string) error {
	ctx, cancel := s.opCtx(ctx)
	defer cancel()
	_, err := s.db.ExecContext(ctx, `DELETE FROM operators WHERE id = ?`, id)
	return err
}

func (s *Store) UpsertOperatorToken(ctx context.Context, tok *store.OperatorToken) error {
	ctx, cancel := s.opCtx(ctx)
	defer cancel()
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO operator_tokens(id,operator_id,token_hash,created_at) VALUES(?,?,?,?)
		 ON CONFLICT(id) DO UPDATE SET operator_id=excluded.operator_id, token_hash=excluded.token_hash`,
		tok.ID, tok.OperatorID, tok.TokenHash, tok.CreatedAt.Format(store.TimestampLayout))
	return err
}

func (s *Store) ListOperatorTokens(ctx context.Context) ([]*store.OperatorToken, error) {
	ctx, cancel := s.opCtx(ctx)
	defer cancel()
	rows, err := s.reader.QueryContext(ctx, `SELECT id,operator_id,token_hash,created_at FROM operator_tokens ORDER BY created_at ASC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*store.OperatorToken
	for rows.Next() {
		var tok store.OperatorToken
		var created string
		if err := rows.Scan(&tok.ID, &tok.OperatorID, &tok.TokenHash, &created); err != nil {
			return nil, err
		}
		tok.CreatedAt, _ = time.Parse(store.TimestampLayout, created)
		out = append(out, &tok)
	}
	return out, rows.Err()
}

func (s *Store) DeleteOperatorToken(ctx context.Context, id string) error {
	ctx, cancel := s.opCtx(ctx)
	defer cancel()
	_, err := s.db.ExecContext(ctx, `DELETE FROM operator_tokens WHERE id = ?`, id)
	return err
}

// passwordHashPtr renders an operator's optional password hash: nil stays
// SQL NULL so token-only operators carry no hash at all (and can never log
// in with a password).
func passwordHashPtr(h *string) any {
	if h == nil {
		return nil
	}
	return *h
}

// scanOperator scans one operators row.
func scanOperator(scan func(...any) error) (*store.Operator, error) {
	var o store.Operator
	var created, updated string
	var passwordHash, attrs sql.NullString
	if err := scan(&o.ID, &o.Name, &o.DisplayName, &passwordHash, &attrs, &created, &updated); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, store.ErrNotFound
		}
		return nil, err
	}
	if passwordHash.Valid {
		o.PasswordHash = &passwordHash.String
	}
	parsed, err := store.UnmarshalAttributes(attrs.String)
	if err != nil {
		return nil, err
	}
	o.Attributes = parsed
	o.CreatedAt, _ = time.Parse(store.TimestampLayout, created)
	o.UpdatedAt, _ = time.Parse(store.TimestampLayout, updated)
	return &o, nil
}