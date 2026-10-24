// Package migrate implements the embedded-FS migration discovery shared by
// every store engine.
//
// Each engine owns its embedded migrations directory (castle/internal/store/sqlite/migrations,
// castle/internal/store/pg/migrations) and its own dialect-specific runner;
// this package carries only the walk/numbering/validity machinery so all
// engines follow the same file contract: NNNN_description.sql, strictly
// monotonic 4-digit versions with no gaps or duplicates, forward-only. There
// is no cross-engine magic: no engine ever reads another engine's directory,
// and neither engine replays the other's migration history.
package migrate

import (
	"fmt"
	"io/fs"
	"path"
	"regexp"
	"sort"
	"strconv"
)

// Migration is one discovered migration file.
type Migration struct {
	// Version is the 4-digit monotonic version parsed from the filename.
	Version int
	// Name is the full FS path of the file, for error messages.
	Name string
	// SQL is the raw file content.
	SQL []byte
}

var migrationNameRe = regexp.MustCompile(`^(\d{4})_.+\.sql$`)

// Discover walks fsys and returns every migration in version order,
// validating the file contract: every .sql file must match
// NNNN_description.sql, versions must be unique and contiguous from the
// lowest version, and there must be at least one migration.
func Discover(fsys fs.FS) ([]Migration, error) {
	all := make([]Migration, 0)
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

		sqlBytes, err := fs.ReadFile(fsys, filePath)
		if err != nil {
			return fmt.Errorf("read migration %q: %w", filePath, err)
		}

		all = append(all, Migration{Version: version, Name: filePath, SQL: sqlBytes})
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("discover migrations: %w", err)
	}

	if len(all) == 0 {
		return nil, fmt.Errorf("no migrations found in embedded filesystem")
	}

	sort.Slice(all, func(i, j int) bool {
		return all[i].Version < all[j].Version
	})

	for i := 1; i < len(all); i++ {
		if all[i].Version == all[i-1].Version {
			return nil, fmt.Errorf("duplicate migration version %04d in %q and %q", all[i].Version, all[i-1].Name, all[i].Name)
		}
	}

	for i := 1; i < len(all); i++ {
		expected := all[i-1].Version + 1
		if all[i].Version != expected {
			return nil, fmt.Errorf("migration version gap: missing %04d between %q and %q", expected, all[i-1].Name, all[i].Name)
		}
	}

	return all, nil
}

// Pending returns the discovered migrations not yet in applied.
func Pending(fsys fs.FS, applied map[int]struct{}) ([]Migration, error) {
	all, err := Discover(fsys)
	if err != nil {
		return nil, err
	}

	pending := make([]Migration, 0, len(all))
	for _, m := range all {
		if _, ok := applied[m.Version]; !ok {
			pending = append(pending, m)
		}
	}

	return pending, nil
}
