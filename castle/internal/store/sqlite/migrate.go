package sqlite

import (
	"context"
	"database/sql"
	"embed"
	"fmt"
	"io/fs"
	"path"
	"regexp"
	"sort"
	"strconv"
)

//go:embed migrations/*.sql
var migrationsFS embed.FS

var migrationNameRe = regexp.MustCompile(`^(\d{4})_.+\.sql$`)

type migration struct {
	version int
	name    string
	sql     []byte
}

func appliedVersions(ctx context.Context, db *sql.DB) (map[int]struct{}, error) {
	if _, err := db.ExecContext(ctx, `CREATE TABLE IF NOT EXISTS schema_migrations (
		version INTEGER PRIMARY KEY,
		applied_at TIMESTAMP NOT NULL DEFAULT CURRENT_TIMESTAMP
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

func pendingMigrations(fsys embed.FS, applied map[int]struct{}) ([]migration, error) {
	all := make([]migration, 0)
	err := fs.WalkDir(fsys, ".", func(filePath string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if d.IsDir() {
			return nil
		}

		name := path.Base(filePath)
		match := migrationNameRe.FindStringSubmatch(name)
		if match == nil {
			if path.Ext(name) == ".sql" {
				return fmt.Errorf("invalid migration filename %q: expected NNNN_description.sql", filePath)
			}
			return nil
		}

		version, err := strconv.Atoi(match[1])
		if err != nil {
			return fmt.Errorf("parse migration version %q: %w", filePath, err)
		}

		sqlBytes, err := fsys.ReadFile(filePath)
		if err != nil {
			return fmt.Errorf("read migration %q: %w", filePath, err)
		}

		all = append(all, migration{version: version, name: filePath, sql: sqlBytes})
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("discover migrations: %w", err)
	}

	if len(all) == 0 {
		return nil, fmt.Errorf("no migrations found in embedded filesystem")
	}

	sort.Slice(all, func(i, j int) bool {
		return all[i].version < all[j].version
	})

	for i := 1; i < len(all); i++ {
		if all[i].version == all[i-1].version {
			return nil, fmt.Errorf("duplicate migration version %04d in %q and %q", all[i].version, all[i-1].name, all[i].name)
		}
	}

	for i := 1; i < len(all); i++ {
		expected := all[i-1].version + 1
		if all[i].version != expected {
			return nil, fmt.Errorf("migration version gap: missing %04d between %q and %q", expected, all[i-1].name, all[i].name)
		}
	}

	latestApplied := 0
	for version := range applied {
		if version > latestApplied {
			latestApplied = version
		}
	}

	pending := make([]migration, 0, len(all))
	for _, m := range all {
		if m.version > latestApplied {
			pending = append(pending, m)
		}
	}

	return pending, nil
}

func Migrate(ctx context.Context, db *sql.DB) error {
	applied, err := appliedVersions(ctx, db)
	if err != nil {
		return err
	}

	pending, err := pendingMigrations(migrationsFS, applied)
	if err != nil {
		return err
	}

	return applyPendingMigrations(ctx, db, pending)
}

func migrateWithFS(ctx context.Context, db *sql.DB, fsys embed.FS) error {
	applied, err := appliedVersions(ctx, db)
	if err != nil {
		return err
	}
	pending, err := pendingMigrations(fsys, applied)
	if err != nil {
		return err
	}

	return applyPendingMigrations(ctx, db, pending)
}

func applyPendingMigrations(ctx context.Context, db *sql.DB, pending []migration) error {
	for _, m := range pending {
		tx, err := db.BeginTx(ctx, nil)
		if err != nil {
			return fmt.Errorf("begin migration %s: %w", m.name, err)
		}
		if _, err := tx.ExecContext(ctx, string(m.sql)); err != nil {
			_ = tx.Rollback()
			return fmt.Errorf("apply migration %s: %w", m.name, err)
		}
		if _, err := tx.ExecContext(ctx, `INSERT INTO schema_migrations(version) VALUES (?)`, m.version); err != nil {
			_ = tx.Rollback()
			return fmt.Errorf("record migration %s: %w", m.name, err)
		}
		if err := tx.Commit(); err != nil {
			return fmt.Errorf("commit migration %s: %w", m.name, err)
		}
	}

	return nil
}
