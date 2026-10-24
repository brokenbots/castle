// Package relocate implements the FREEZE-AND-IMPORT cutover for KB-317: copy
// a live sqlite castle.db into a freshly-migrated Postgres store.
//
// Contract:
//   - The pg target must be empty (fresh-database-only cutover; new
//     orchestrator tables are born on pg). A non-empty target is refused
//     unless explicitly overridden.
//   - The source is opened read-only by intent (the importer never writes);
//     before and after the copy every source table is re-counted. Any drift
//     between the two passes fails the import: the freeze was not valid and
//     the partially imported target must be discarded.
//   - After the copy, per-table row counts are compared against the source
//     and a row-for-row sample is decoded and compared on both engines.
package relocate

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"reflect"
	"strings"
	"time"

	"github.com/brokenbots/castle/castle/internal/store"

	_ "github.com/jackc/pgx/v5/stdlib"
	_ "modernc.org/sqlite"
)

const (
	// sqliteDriver is registered by modernc.org/sqlite; pgDriver by pgx/v5.
	sqliteDriver = "sqlite"
	pgDriver     = "pgx"
	// pageSize is the keyset batch size for copy and verify scans.
	pageSize  = 1000
	verifyCap = 1000
)

// Kind is the storage transform a column needs between the engines.
// sqlite stores timestamps as RFC3339Nano TEXT and counters as INTEGER;
// pg stores TIMESTAMPTZ and BIGINT.
type Kind int

const (
	KindText Kind = iota
	KindTS        // RFC3339Nano TEXT -> TIMESTAMPTZ
	KindInt       // INTEGER -> BIGINT
	KindJSON      // TEXT JSON payload -> JSONB (semantically equal after round-trip)
)

// column describes one column of a table for reading from sqlite and
// writing to pg.
type column struct {
	name string
	kind Kind
	// null marks columns that are NULLable in sqlite's final schema and are
	// therefore scanned as sql.Null* values.
	null bool
}

// table is one sqlite table to relocate, in FK-safe import order. key is the
// deterministic ordering used for copy paging and verify samplers; pk is the
// target's conflict target for re-runnable (--allow-non-empty) imports;
// keyless tables (only workflow_assignment_labels) order by all columns.
type table struct {
	name string
	key  []string
	pk   []string
	cols []column
}

// Tables is the full relocation set, FK-safe order.
var Tables = []table{
	{
		name: "overseers",
		key:  []string{"id"},
		pk:   []string{"id"},
		cols: []column{
			{"id", KindText, false}, {"name", KindText, false}, {"hostname", KindText, true},
			{"version", KindText, true}, {"token_hash", KindText, false}, {"status", KindText, false},
			{"created_at", KindTS, false}, {"last_seen_at", KindTS, false}, {"labels", KindText, true},
		},
	},
	{
		name: "runs",
		key:  []string{"id"},
		pk:   []string{"id"},
		cols: []column{
			{"id", KindText, false}, {"overseer_id", KindText, true}, {"workflow_name", KindText, false},
			{"workflow_hcl", KindText, false}, {"status", KindText, false}, {"current_step", KindText, false},
			{"last_seq", KindInt, false}, {"created_at", KindTS, false}, {"ended_at", KindTS, true},
			{"variable_scope", KindText, true}, {"pending_signal", KindText, true}, {"paused_at", KindTS, true},
			{"ticket", KindText, true}, {"repo_url", KindText, true}, {"pr_url", KindText, true},
			{"failure_reason", KindText, true}, {"started_at", KindTS, true},
		},
	},
	{
		name: "run_subscriptions",
		key:  []string{"subscriber_id", "run_id"},
		pk:   []string{"subscriber_id", "run_id"},
		cols: []column{
			{"subscriber_id", KindText, false}, {"run_id", KindText, false},
			{"last_seq", KindInt, false}, {"updated_at", KindTS, false},
		},
	},
	{
		name: "run_attempts",
		key:  []string{"run_id", "step", "attempt"},
		pk:   []string{"run_id", "step", "attempt"},
		cols: []column{
			{"run_id", KindText, false}, {"step", KindText, false}, {"attempt", KindInt, false},
			{"started_at", KindTS, false}, {"completed_at", KindTS, true}, {"outcome", KindText, true},
		},
	},
	{
		name: "events",
		key:  []string{"run_id", "seq"},
		// pg's events PK also carries ts; sqlite's is (run_id, seq).
		pk: []string{"run_id", "seq", "ts"},
		cols: []column{
			{"run_id", KindText, false}, {"seq", KindInt, false}, {"type", KindText, false},
			{"ts", KindTS, false}, {"correlation_id", KindText, true}, {"payload", KindJSON, false},
		},
	},
	{
		name: "workflow_assignments",
		key:  []string{"id"},
		pk:   []string{"id"},
		cols: []column{
			{"id", KindText, false}, {"owner_criteria_id", KindText, false}, {"run_id", KindText, false},
			{"workflow_name", KindText, false}, {"workflow_source", KindText, false},
			{"lockfile_source", KindText, true}, {"idempotency_key", KindText, false},
			{"state", KindText, false}, {"terminal_reason", KindText, true},
			{"leased_criteria_id", KindText, true}, {"lease_expires_at", KindTS, true},
			{"created_at", KindTS, false}, {"updated_at", KindTS, false},
		},
	},
	{
		name: "workflow_assignment_labels",
		pk:   []string{"assignment_id", "key"},
		cols: []column{
			{"assignment_id", KindText, false}, {"key", KindText, false}, {"value", KindText, false},
		},
	},
	{
		name: "workflow_assignment_leases",
		key:  []string{"id"},
		pk:   []string{"id"},
		cols: []column{
			{"id", KindText, false}, {"assignment_id", KindText, false}, {"criteria_id", KindText, false},
			{"created_at", KindTS, false}, {"expires_at", KindTS, false},
		},
	},
	{
		name: "workflow_assignment_attempts",
		key:  []string{"assignment_id", "attempt"},
		pk:   []string{"assignment_id", "attempt"},
		cols: []column{
			{"assignment_id", KindText, false}, {"attempt", KindInt, false},
			{"criteria_id", KindText, true}, {"created_at", KindTS, false},
			{"completed_at", KindTS, true}, {"outcome", KindText, true},
		},
	},
	{
		name: "orchestrators",
		key:  []string{"id"},
		pk:   []string{"id"},
		cols: []column{
			{"id", KindText, false}, {"name", KindText, false},
			{"token_hash", KindText, false}, {"created_at", KindTS, false},
		},
	},
	{
		name: "console_users",
		key:  []string{"id"},
		pk:   []string{"id"},
		cols: []column{
			{"id", KindText, false}, {"username", KindText, false}, {"password_hash", KindText, false},
			{"created_at", KindTS, false}, {"updated_at", KindTS, false},
		},
	},
	{
		name: "console_sessions",
		key:  []string{"id"},
		pk:   []string{"id"},
		cols: []column{
			{"id", KindText, false}, {"user_id", KindText, false},
			{"token_hash", KindText, false}, {"created_at", KindTS, false},
		},
	},
	{
		name: "run_archive",
		key:  []string{"archive_key"},
		pk:   []string{"archive_key"},
		cols: []column{
			{"archive_key", KindText, false}, {"run_id", KindText, false}, {"job_name", KindText, false},
			{"final_phase", KindText, false}, {"ticket", KindText, false}, {"record", KindText, false},
			{"operator_id", KindText, false}, {"archived_at", KindTS, false},
			// 0014 artifact blob pointers: rows become object-storage
			// references. Nothing writes them yet, but the freeze-and-import
			// must not silently drop columns the schema carries — the
			// registry-vs-schema test pins this list against both engines.
			{"blob_uri", KindText, false}, {"blob_sha256", KindText, false}, {"blob_size", KindInt, false},
		},
	},
}

// TimestampLayout is the text format sqlite persists timestamps in
// (store.TimestampLayout); relocation parses and re-stamps them.
const TimestampLayout = time.RFC3339Nano

// TableReport is the per-table import outcome surfaced to the operator.
// Imported is the row count this run wrote; Verified is true when the target
// actually holds what the source holds (count equality, no drift on override
// re-runs) and the row-for-row sample compared equal.
type TableReport struct {
	Name     string
	Source   int64
	Imported int64
	// Verified is true when the count and the row-for-row sample matched.
	Verified bool
	// Detail explains a mismatch (count delta or sample row description).
	Detail string
}

// ParseSQLTimestamp converts a persisted sqlite timestamp into the value
// written to pg's TIMESTAMPTZ columns.
func ParseSQLTimestamp(text string) (time.Time, error) {
	t, err := time.Parse(TimestampLayout, text)
	if err != nil {
		return time.Time{}, fmt.Errorf("relocate: parse timestamp %q: %w", text, err)
	}
	return t.UTC(), nil
}

// quoteIdent double-quotes a SQL identifier after validating it, so table
// and column names from the registry (never user input) can't break out.
func quoteIdent(name string) string {
	if !isPlainIdent(name) {
		panic(fmt.Sprintf("relocate: identifier %q is not a plain identifier", name))
	}
	return `"` + name + `"`
}

func isPlainIdent(name string) bool {
	if name == "" {
		return false
	}
	for i, r := range name {
		switch {
		case r >= 'a' && r <= 'z', r == '_':
		// Digits are valid from the second character on (blob_sha256 in
		// the registry is the first digit-bearing column).
		case r >= '0' && r <= '9' && i > 0:
		default:
			return false
		}
	}
	return true
}

// OpenSource opens the sqlite source database. The importer only ever reads;
// the busy timeout mirrors the store's own pragma set.
func OpenSource(path string) (*sql.DB, error) {
	return sql.Open(sqliteDriver, fmt.Sprintf("file:%s?_pragma=busy_timeout(5000)", path))
}

// OpenTarget opens a raw pg handle for the importer. Migrations must already
// have run (open the store with pg.Open first); this package writes raw rows.
func OpenTarget(dsn string) (*sql.DB, error) {
	return sql.Open(pgDriver, dsn)
}

// Import copies every table from src to dst and verifies the result.
// The freeze contract is checked internally: source counts are taken before
// and after the copy and must agree.
//
// Verification against a re-run (--allow-non-empty-pg) is stricter than
// against a fresh target: the copy upserts, so a converged target must be a
// no-op — any target row-count drift during the re-run (a deleted or foreign
// row the copy healed) fails that table's verification instead of silently
// passing. Counts compare against what the target actually holds after the
// copy, so anything the copy cannot account for is surfaced to the operator.
func Import(ctx context.Context, src, dst *sql.DB, verifyRows int64, allowNonEmpty bool) ([]TableReport, error) {
	if verifyRows <= 0 {
		verifyRows = verifyCap
	}

	// Pre-copy target counts: refusal gate for fresh-target imports, and the
	// baseline an override re-run is verified against.
	pre := make(map[string]int64, len(Tables))
	for _, t := range Tables {
		var n int64
		if err := dst.QueryRowContext(ctx,
			`SELECT count(*) FROM `+quoteIdent(t.name)).Scan(&n); err != nil {
			return nil, fmt.Errorf("relocate: probe target %s: %w", t.name, err)
		}
		pre[t.name] = n
	}

	// Refuse a non-empty target: the cutover is born-on-pg for all new
	// orchestrator tables, and a mixed-history target would corrupt replay.
	if !allowNonEmpty {
		for _, t := range Tables {
			if pre[t.name] != 0 {
				return nil, fmt.Errorf(
					"relocate: target table %s already holds %d rows; imports run against an empty pg database only (discard the target and start from a fresh one, or re-run on a converged target with --allow-non-empty-pg)",
					t.name, pre[t.name])
			}
		}
	}

	before, err := countSourceTables(ctx, src)
	if err != nil {
		return nil, err
	}

	imported := make(map[string]int64, len(Tables))
	for _, t := range Tables {
		n, err := copyTable(ctx, src, dst, t)
		if err != nil {
			return nil, fmt.Errorf("relocate: import %s: %w", t.name, err)
		}
		imported[t.name] = n
	}

	// Freeze check: the source must not have changed while copying.
	after, err := countSourceTables(ctx, src)
	if err != nil {
		return nil, err
	}
	for _, t := range Tables {
		if before[t.name] != after[t.name] {
			return nil, fmt.Errorf(
				"relocate: FREEZE violated: source table %s drifted from %d to %d rows during the import; the frozen copy is inconsistent — discard the target database and re-run on a quiesced source",
				t.name, before[t.name], after[t.name])
		}
	}

	// Post-copy actual target row counts. Verification compares the source
	// against what the target actually holds — not against the row count this
	// run wrote — so rows the copy cannot account for (a foreign row, a source
	// row dropped between runs) fail verification instead of passing.
	post := make(map[string]int64, len(Tables))
	for _, t := range Tables {
		var n int64
		if err := dst.QueryRowContext(ctx,
			`SELECT count(*) FROM `+quoteIdent(t.name)).Scan(&n); err != nil {
			return nil, fmt.Errorf("relocate: count target %s after copy: %w", t.name, err)
		}
		post[t.name] = n
	}

	// Verify: count equality, override-run target stability, and a row-for-row
	// decoded sample.
	reports := make([]TableReport, 0, len(Tables))
	for _, t := range Tables {
		rep := TableReport{Name: t.name, Source: after[t.name], Imported: imported[t.name]}
		switch {
		case allowNonEmpty && pre[t.name] != post[t.name]:
			// An override re-run converges an already-imported target, so it
			// must not change its row counts; drift means the state between
			// runs is not the identical source — fail loudly.
			delta := post[t.name] - pre[t.name]
			rep.Detail = fmt.Sprintf(
				"target drifted from %d to %d rows (%+d) during the override re-run; a converged re-import must be a no-op — discard the target and re-run the cutover from a fresh one",
				pre[t.name], post[t.name], delta)
		case rep.Source != post[t.name]:
			rep.Detail = fmt.Sprintf(
				"row count mismatch: source %d, target %d (copied %d)",
				rep.Source, post[t.name], rep.Imported)
		case post[t.name] == 0:
			rep.Verified = true
		default:
			if err := verifySample(ctx, src, dst, t, verifyRows); err != nil {
				rep.Detail = err.Error()
			} else {
				rep.Verified = true
			}
		}
		reports = append(reports, rep)
	}
	return reports, nil
}

func countSourceTables(ctx context.Context, src *sql.DB) (map[string]int64, error) {
	counts := make(map[string]int64, len(Tables))
	for _, t := range Tables {
		var n int64
		if err := src.QueryRowContext(ctx,
			`SELECT count(*) FROM `+quoteIdent(t.name)).Scan(&n); err != nil {
			return nil, fmt.Errorf("relocate: count source %s: %w", t.name, err)
		}
		counts[t.name] = n
	}
	return counts, nil
}

// colList renders the quoted column list shared by read and write SQL.
func colList(cols []column) string {
	names := make([]string, len(cols))
	for i, c := range cols {
		names[i] = quoteIdent(c.name)
	}
	return strings.Join(names, ", ")
}

// copyTable streams one table through keyset pages in source order and
// writes the whole table on a single target transaction. events rows
// pre-create their month partition exactly like the runtime AppendEvent path.
func copyTable(ctx context.Context, src, dst *sql.DB, t table) (int64, error) {
	read := readPageSQL(t)
	write := insertSQL(t)

	tx, err := dst.BeginTx(ctx, nil)
	if err != nil {
		return 0, fmt.Errorf("begin: %w", err)
	}
	stmt, err := tx.PrepareContext(ctx, write)
	if err != nil {
		_ = tx.Rollback()
		return 0, fmt.Errorf("prepare: %w", err)
	}
	defer stmt.Close()

	var total int64
	insert := func(row []any) error {
		// JSON payloads are bound through a ::jsonb cast, and JSONB cannot
		// represent U+0000 (PostgreSQL rejects the \u0000 escape with 22P05):
		// normalize legacy source rows exactly like the runtime AppendEvent
		// paths do, or a single NUL in a copied payload aborts the freeze.
		for j, c := range t.cols {
			if c.kind != KindJSON {
				continue
			}
			if s, ok := row[j].(string); ok {
				row[j] = string(store.NormalizeEventPayload([]byte(s)))
			}
		}
		if t.name == "events" {
			// ts sits at index 3 of the events registry; mirror
			// pg/events.go by ensuring the month partition exists.
			if _, err := tx.ExecContext(ctx,
				`SELECT castle_ensure_event_partition($1::timestamptz)`, row[3]); err != nil {
				return fmt.Errorf("ensure event partition (row %d): %w", total, err)
			}
		}
		if _, err := stmt.ExecContext(ctx, row...); err != nil {
			return fmt.Errorf("insert %s: %w", keysOf(t, row), err)
		}
		return nil
	}

	last := make([]any, len(t.key))
	first := true
	for {
		rows, err := pageRows(ctx, src, t, read, first, last, pageSize)
		if err != nil {
			_ = tx.Rollback()
			return 0, err
		}
		batch, err := drain(rows, t.cols)
		if err != nil {
			_ = tx.Rollback()
			return 0, err
		}
		if len(batch) == 0 {
			break
		}
		for _, row := range batch {
			if err := insert(row); err != nil {
				_ = tx.Rollback()
				return 0, err
			}
			total++
		}
		// Keyless tables have no cursor to advance: the single full scan IS
		// the table. Anything past the first iteration would re-insert it.
		if len(t.key) == 0 {
			break
		}
		last = keyValues(t, batch[len(batch)-1])
		first = false
	}
	if err := tx.Commit(); err != nil {
		return 0, fmt.Errorf("commit: %w", err)
	}
	return total, nil
}

// pageRows applies the keyset predicate: (k1, ...) > last, or no predicate on
// the first page, with an explicit row limit so callers can page by batch
// size or sample by the requested verify-rows count. Keyless tables (label
// rows) always take the uncapped full scan once.
func pageRows(ctx context.Context, src *sql.DB, t table, read string, first bool, last []any, limit int64) (*sql.Rows, error) {
	if len(t.key) == 0 {
		// Full scan, uncapped (count verification would catch a partial
		// copy), in the same all-columns order the target is sampled in.
		return src.QueryContext(ctx, read+" ORDER BY "+orderBy(t, false))
	}
	if first {
		return src.QueryContext(ctx, read+" ORDER BY "+orderBy(t, false)+" LIMIT ?", limit)
	}
	q := read + " WHERE " + keyTuple(t) + " > (" + sqlPlaceholders(1, len(t.key)) + ")" +
		" ORDER BY " + orderBy(t, false) + " LIMIT ?"
	args := append(append([]any{}, last...), limit)
	return src.QueryContext(ctx, q, args...)
}

// keyTuple renders the row-value comparison tuple for a table's key.
func keyTuple(t table) string {
	return colTuple(t.key)
}

// collate renders a byte-wise collation suffix for ORDER BY: sqlite orders
// TEXT by bytes while pg orders it under the database's locale collation, so
// mixed-case keys would page and verify in different orders on the two
// engines. Relocation forces BINARY on sqlite reads and COLLATE "C" on pg
// reads (run_archive.archive_key is already declared COLLATE "C" in the pg
// DDL, so this makes the remaining key columns consistent with it).
// Non-text kinds need no collation: numeric and timestamp orderings are
// deterministic. Run against the column's own kind, so only KindText gets a
// suffix.
func collate(kind Kind, pgDialect bool) string {
	if kind != KindText {
		return ""
	}
	if pgDialect {
		return ` COLLATE "C"`
	}
	return " COLLATE BINARY"
}

// columnKind returns the storage kind of a named column of the table.
func columnKind(t table, name string) Kind {
	for _, c := range t.cols {
		if c.name == name {
			return c.kind
		}
	}
	panic(fmt.Sprintf("relocate: table %s has no column %s", t.name, name))
}

// orderBy renders the deterministic row ordering a table is paged and
// verified in: its key, or all columns for keyless tables. Plain column list,
// not a row-value tuple — sqlite rejects row values in ORDER BY. pgDialect
// selects the byte-wise collation per engine (see collate); the source is
// always sqlite and the target always pg.
func orderBy(t table, pgDialect bool) string {
	if len(t.key) == 0 {
		names := make([]string, len(t.cols))
		for i, c := range t.cols {
			names[i] = quoteIdent(c.name) + collate(c.kind, pgDialect)
		}
		return strings.Join(names, ", ")
	}
	names := make([]string, len(t.key))
	for i, k := range t.key {
		names[i] = quoteIdent(k) + collate(columnKind(t, k), pgDialect)
	}
	return strings.Join(names, ", ")
}

func readPageSQL(t table) string {
	return `SELECT ` + colList(t.cols) + ` FROM ` + quoteIdent(t.name)
}

// insertSQL renders a re-runnable pg INSERT: upserts on the target's primary
// key so an --allow-non-empty re-run converges instead of colliding. JSON
// columns carry an explicit ::jsonb cast so their TEXT source values arrive
// as JSONB.
func insertSQL(t table) string {
	holders := make([]string, len(t.cols))
	assignments := make([]string, 0, len(t.cols))
	for i, c := range t.cols {
		holders[i] = fmt.Sprintf("$%d", i+1)
		if c.kind == KindJSON {
			holders[i] += "::jsonb"
		}
		assignments = append(assignments, fmt.Sprintf("%s = EXCLUDED.%s",
			quoteIdent(c.name), quoteIdent(c.name)))
	}
	target := strings.Join(colNames(t.pk), ", ")
	return `INSERT INTO ` + quoteIdent(t.name) + ` (` + colList(t.cols) + `) VALUES (` +
		strings.Join(holders, ", ") + `) ON CONFLICT (` + target +
		`) DO UPDATE SET ` + strings.Join(assignments, ", ")
}

// colTuple renders a parenthesized quoted column tuple: (a, b).
func colTuple(cols []string) string {
	return "(" + strings.Join(colNames(cols), ", ") + ")"
}

func colNames(cols []string) []string {
	names := make([]string, len(cols))
	for i, c := range cols {
		names[i] = quoteIdent(c)
	}
	return names
}

// sqlPlaceholders renders ?..? (N placeholders) for the sqlite source reads.
func sqlPlaceholders(start, n int) string {
	out := make([]string, n)
	for i := range out {
		out[i] = "?"
	}
	return strings.Join(out, ", ")
}

// placeholders renders $start..$start+n-1 for pg target writes.
func placeholders(start, n int) string {
	out := make([]string, n)
	for i := range out {
		out[i] = fmt.Sprintf("$%d", start+i)
	}
	return strings.Join(out, ", ")
}

// drain scans a page of sqlite rows into the value shapes the pg driver
// accepts: nil for NULL, string for text/json, time.Time for timestamps,
// int64 for counters.
func drain(rows *sql.Rows, cols []column) ([][]any, error) {
	defer rows.Close()
	var out [][]any
	for rows.Next() {
		vals := make([]any, len(cols))
		ptrs := make([]any, len(cols))
		for i, c := range cols {
			switch {
			case c.null && c.kind == KindTS:
				ptrs[i] = new(sql.NullString)
			case c.null && c.kind == KindInt:
				ptrs[i] = new(sql.NullInt64)
			case c.null:
				ptrs[i] = new(sql.NullString)
			case c.kind == KindTS:
				ptrs[i] = new(string)
			case c.kind == KindInt:
				ptrs[i] = new(int64)
			default:
				ptrs[i] = new(string)
			}
		}
		if err := rows.Scan(ptrs...); err != nil {
			return nil, err
		}
		for i, c := range cols {
			switch p := ptrs[i].(type) {
			case *sql.NullString:
				if p.Valid {
					if c.kind == KindTS {
						conv, err := ParseSQLTimestamp(p.String)
						if err != nil {
							return nil, fmt.Errorf("parse %s ts %q: %w", c.name, p.String, err)
						}
						vals[i] = conv
					} else {
						vals[i] = p.String
					}
				}
			case *sql.NullInt64:
				if p.Valid {
					vals[i] = p.Int64
				}
			case *string:
				if c.kind == KindTS {
					conv, err := ParseSQLTimestamp(*p)
					if err != nil {
						return nil, err
					}
					vals[i] = conv
				} else {
					vals[i] = *p
				}
			case *int64:
				vals[i] = *p
			}
		}
		out = append(out, vals)
	}
	return out, rows.Err()
}

func keyValues(t table, row []any) []any {
	out := make([]any, len(t.key))
	for i, k := range t.key {
		for j, c := range t.cols {
			if c.name == k {
				out[i] = row[j]
				break
			}
		}
	}
	return out
}

func keysOf(t table, row []any) string {
	kv := keyValues(t, row)
	parts := make([]string, len(kv))
	for i, v := range kv {
		parts[i] = fmt.Sprintf("%s=%v", t.key[i], v)
	}
	return strings.Join(parts, ",")
}

// verifySample compares the first verifyRows rows of src and dst in the
// table's deterministic order, decoding pg values (time.Time, []byte JSONB)
// into the same canonical shapes before comparing.
func verifySample(ctx context.Context, src, dst *sql.DB, t table, verifyRows int64) error {
	// Both sides read exactly verifyRows rows so the report's claimed
	// coverage always matches what was actually compared (a larger
	// --verify-rows must not silently cap at the copy page size).
	rows, err := pageRows(ctx, src, t, readPageSQL(t), true, nil, verifyRows)
	if err != nil {
		return fmt.Errorf("read source sample: %w", err)
	}
	srcRows, err := drain(rows, t.cols)
	if err != nil {
		return fmt.Errorf("scan source sample: %w", err)
	}

	q := readPageSQL(t) + ` ORDER BY ` + orderBy(t, true) + ` LIMIT $1`
	drows, err := dst.QueryContext(ctx, q, verifyRows)
	if err != nil {
		return fmt.Errorf("read target sample: %w", err)
	}
	dstRows, err := scanTargetSample(drows, t.cols)
	if err != nil {
		return fmt.Errorf("scan target sample: %w", err)
	}

	n := int64(len(srcRows))
	if n > verifyRows {
		n = verifyRows
	}
	if int64(len(dstRows)) < n {
		return fmt.Errorf("sample short: source %d rows, target %d", n, len(dstRows))
	}
	for i := int64(0); i < n; i++ {
		for j, c := range t.cols {
			if !equalValue(srcRows[i][j], dstRows[i][j], c.kind) {
				return fmt.Errorf(
					"row %d (key %v) column %s differs: source %v, target %v",
					i, keysOf(t, srcRows[i]), c.name, srcRows[i][j], dstRows[i][j])
			}
		}
	}
	return nil
}

func scanTargetSample(rows *sql.Rows, cols []column) ([][]any, error) {
	defer rows.Close()
	var out [][]any
	for rows.Next() {
		vals := make([]any, len(cols))
		ptrs := make([]any, len(cols))
		for i, c := range cols {
			switch c.kind {
			case KindTS:
				ptrs[i] = new(sql.NullTime)
			case KindInt:
				ptrs[i] = new(sql.NullInt64)
			default:
				ptrs[i] = new(sql.NullString)
			}
		}
		if err := rows.Scan(ptrs...); err != nil {
			return nil, err
		}
		for i, p := range ptrs {
			switch v := p.(type) {
			case *sql.NullTime:
				if v.Valid {
					vals[i] = v.Time.UTC()
				}
			case *sql.NullInt64:
				if v.Valid {
					vals[i] = v.Int64
				}
			case *sql.NullString:
				if v.Valid {
					vals[i] = v.String
				}
			}
		}
		out = append(out, vals)
	}
	return out, rows.Err()
}

// equalValue compares canonicalized cell values; NULL on either side stays nil.
func equalValue(src, dst any, kind Kind) bool {
	if src == nil || dst == nil {
		return (src == nil) == (dst == nil)
	}
	switch kind {
	case KindTS:
		// pg TIMESTAMPTZ keeps microseconds; sqlite TEXT can carry nanos.
		// Compare at pg resolution — the imported value IS the real one.
		return src.(time.Time).Truncate(time.Microsecond).
			Equal(dst.(time.Time).Truncate(time.Microsecond))
	case KindInt:
		return src.(int64) == dst.(int64)
	case KindJSON:
		// JSONB normalizes key order and whitespace and cannot hold U+0000
		// (both drivers normalize NUL to U+FFFD at the write boundary), so
		// compare normalized decoded values, not raw text — and normalize
		// BOTH sides, since the sqlite source legitimately still carries
		// the raw \u0000 on events rows written before this boundary.
		var a, b any
		if err := json.Unmarshal(store.NormalizeEventPayload([]byte(src.(string))), &a); err != nil {
			return false
		}
		raw, ok := dstValue(dst)
		if !ok {
			return false
		}
		if err := json.Unmarshal(store.NormalizeEventPayload([]byte(raw)), &b); err != nil {
			return false
		}
		return reflect.DeepEqual(a, b)
	default:
		raw, ok := dstValue(dst)
		return ok && raw == src.(string)
	}
}

func dstValue(dst any) (string, bool) {
	switch v := dst.(type) {
	case string:
		return v, true
	case []byte:
		return string(v), true
	default:
		return "", false
	}
}
