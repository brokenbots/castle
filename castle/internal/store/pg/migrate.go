package pg

import (
	"context"
	"database/sql"
	"embed"
	"fmt"

	"github.com/brokenbots/castle/castle/internal/store/migrate"
)

//go:embed migrations/*.sql
var migrationsFS embed.FS

func appliedVersions(ctx context.Context, db *sql.DB) (map[int]struct{}, error) {
	if _, err := db.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS schema_migrations (
		version INTEGER PRIMARY KEY,
		applied_at TIMESTAMPTZ NOT NULL DEFAULT now()
	)`); err != nil {
		return nil, fmt.Errorf("create schema_migrations: %w", err)
	}

	rows, err := db.QueryContext(ctx, `SELECT version FROM schema_migrations`)
	if err != nil {
		return nil, fmt.Errorf("query schema_migrations: %w", err)
	}
	defer rows.Close()

	applied := make(map[int]struct{})
	for rows.Next() {
		var version int
		if err := rows.Scan(&version); err != nil {
			return nil, fmt.Errorf("scan migration version: %w", err)
		}
		applied[version] = struct{}{}
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate schema_migrations: %w", err)
	}

	return applied, nil
}

// Migrate applies this engine's embedded migrations. Discovery machinery
// (file contract, uniqueness, contiguity) is shared with every engine via
// the migrate package; the pg engine keeps only its own embed of its own
// migrations and its own dialect of the apply step.
func Migrate(ctx context.Context, db *sql.DB) error {
	applied, err := appliedVersions(ctx, db)
	if err != nil {
		return err
	}
	pending, err := migrate.Pending(migrationsFS, applied)
	if err != nil {
		return err
	}

	for _, m := range pending {
		tx, err := db.BeginTx(ctx, nil)
		if err != nil {
			return fmt.Errorf("begin migration %s: %w", m.Name, err)
		}
		// Zero-arg multi-statement files run through the simple query
		// protocol; each migration is one atomic DDL transaction.
		if _, err := tx.ExecContext(ctx, string(m.SQL)); err != nil {
			_ = tx.Rollback()
			return fmt.Errorf("apply migration %s: %w", m.Name, err)
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO schema_migrations(version) VALUES ($1)`, m.Version); err != nil {
			_ = tx.Rollback()
			return fmt.Errorf("record migration %s: %w", m.Name, err)
		}
		if err := tx.Commit(); err != nil {
			return fmt.Errorf("commit migration %s: %w", m.Name, err)
		}
	}

	return nil
}
