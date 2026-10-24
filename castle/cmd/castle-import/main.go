// Command castle-import performs the KB-317 FREEZE-AND-IMPORT cutover: copy a
// live sqlite castle.db into a freshly-migrated Postgres castle database.
//
// Safety contract (enforced by internal/store/relocate):
//   - the pg target must be empty (born-on-pg cutover) unless overridden;
//   - source tables are re-counted before and after the copy; drift fails the
//     import and the target must be discarded;
//   - per-table row counts, override-run target stability, and a row-for-row
//     sample are verified; any mismatch exits non-zero.
//
// Exit codes: 0 success, 1 operational/verification failure, 2 usage error.
package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"os"

	"github.com/brokenbots/castle/castle/internal/store/pg"
	"github.com/brokenbots/castle/castle/internal/store/relocate"
)

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

func run(args []string, stdout, stderr io.Writer) int {
	fs := flag.NewFlagSet("castle-import", flag.ContinueOnError)
	fs.SetOutput(stderr)
	dbPath := fs.String("db", envOrDefault("CASTLE_DB_PATH", "./castle.db"), "sqlite source database path (or CASTLE_DB_PATH)")
	pgDSN := fs.String("pg-dsn", envOrDefault("CASTLE_PG_DSN", ""), "Postgres target DSN (or CASTLE_PG_DSN)")
	verifyRows := fs.Int64("verify-rows", 1000, "row-for-row sample size per table")
	allowNonEmpty := fs.Bool("allow-non-empty-pg", false, "DESTRUCTIVE: allow a target that already holds rows")
	if err := fs.Parse(args); err != nil {
		// flag.ContinueOnError already printed the usage error to stderr.
		return 2
	}
	if fs.NArg() != 0 {
		fmt.Fprintf(stderr, "castle-import: unexpected positional arguments: %v\n", fs.Args())
		return 2
	}

	if *pgDSN == "" {
		fmt.Fprintln(stderr, "castle-import: --pg-dsn (or CASTLE_PG_DSN) is required")
		return 2
	}

	// The source store opens read-only by intent; opening via the store
	// package would both migrate and write, so use the raw relocation handle.
	src, err := relocate.OpenSource(*dbPath)
	if err != nil {
		fmt.Fprintf(stderr, "castle-import: open source %s: %v\n", *dbPath, err)
		return 1
	}
	defer src.Close()

	// pg.Open runs the embedded migrations and closes; the importer then
	// writes raw rows through its own handle.
	target, err := pg.Open(*pgDSN)
	if err != nil {
		fmt.Fprintf(stderr, "castle-import: open+migrate target: %v\n", err)
		return 1
	}
	if err := target.Close(); err != nil {
		fmt.Fprintf(stderr, "castle-import: close migration handle: %v\n", err)
		return 1
	}
	dst, err := relocate.OpenTarget(*pgDSN)
	if err != nil {
		fmt.Fprintf(stderr, "castle-import: open target handle: %v\n", err)
		return 1
	}
	defer dst.Close()

	ctx := context.Background()
	reports, err := relocate.Import(ctx, src, dst, *verifyRows, *allowNonEmpty)
	if err != nil {
		fmt.Fprintf(stderr, "castle-import: %v\n", err)
		return 1
	}

	fmt.Fprintf(stdout, "%-32s %12s %12s %s\n", "table", "source", "imported", "verified")
	failed := false
	for _, rep := range reports {
		mark := "ok"
		if !rep.Verified {
			mark = "FAILED"
			failed = true
		}
		line := fmt.Sprintf("%-32s %12d %12d %s", rep.Name, rep.Source, rep.Imported, mark)
		if rep.Detail != "" {
			line += " — " + rep.Detail
		}
		fmt.Fprintln(stdout, line)
	}
	if failed {
		fmt.Fprintln(stderr, "castle-import: verification FAILED — discard the target database and re-run after fixing the source freeze")
		return 1
	}
	fmt.Fprintln(stdout, "castle-import: all tables imported and verified")
	return 0
}

func envOrDefault(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}
