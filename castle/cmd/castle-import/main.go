// Command castle-import performs the KB-317 FREEZE-AND-IMPORT cutover: copy a
// live sqlite castle.db into a freshly-migrated Postgres castle database.
//
// Safety contract (enforced by internal/store/relocate):
//   - the pg target must be empty (born-on-pg cutover);
//   - source tables are re-counted before and after the copy; drift fails the
//     import and the target must be discarded;
//   - per-table row counts and a row-for-row sample are verified on both
//     engines; any mismatch exits non-zero.
package main

import (
	"context"
	"flag"
	"fmt"
	"os"

	"github.com/brokenbots/castle/castle/internal/store/pg"
	"github.com/brokenbots/castle/castle/internal/store/relocate"
)

func main() {
	dbPath := flag.String("db", envOrDefault("CASTLE_DB_PATH", "./castle.db"), "sqlite source database path (or CASTLE_DB_PATH)")
	pgDSN := flag.String("pg-dsn", envOrDefault("CASTLE_PG_DSN", ""), "Postgres target DSN (or CASTLE_PG_DSN)")
	verifyRows := flag.Int64("verify-rows", 1000, "row-for-row sample size per table")
	allowNonEmpty := flag.Bool("allow-non-empty-pg", false, "DESTRUCTIVE: allow a target that already holds rows")
	flag.Parse()

	if *pgDSN == "" {
		fmt.Fprintln(os.Stderr, "castle-import: --pg-dsn (or CASTLE_PG_DSN) is required")
		os.Exit(2)
	}

	// The source store opens read-only by intent; opening via the store
	// package would both migrate and write, so use the raw relocation handle.
	src, err := relocate.OpenSource(*dbPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "castle-import: open source %s: %v\n", *dbPath, err)
		os.Exit(1)
	}
	defer src.Close()

	// pg.Open runs the embedded migrations and closes; the importer then
	// writes raw rows through its own handle.
	target, err := pg.Open(*pgDSN)
	if err != nil {
		fmt.Fprintf(os.Stderr, "castle-import: open+migrate target: %v\n", err)
		os.Exit(1)
	}
	if err := target.Close(); err != nil {
		fmt.Fprintf(os.Stderr, "castle-import: close migration handle: %v\n", err)
		os.Exit(1)
	}
	dst, err := relocate.OpenTarget(*pgDSN)
	if err != nil {
		fmt.Fprintf(os.Stderr, "castle-import: open target handle: %v\n", err)
		os.Exit(1)
	}
	defer dst.Close()

	ctx := context.Background()
	reports, err := relocate.Import(ctx, src, dst, *verifyRows, *allowNonEmpty)
	if err != nil {
		fmt.Fprintf(os.Stderr, "castle-import: %v\n", err)
		os.Exit(1)
	}

	fmt.Printf("%-32s %12s %12s %s\n", "table", "source", "imported", "verified")
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
		fmt.Println(line)
	}
	if failed {
		fmt.Fprintln(os.Stderr, "castle-import: verification FAILED — discard the target database and re-run after fixing the source freeze")
		os.Exit(1)
	}
	fmt.Println("castle-import: all tables imported and verified")
}

func envOrDefault(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}