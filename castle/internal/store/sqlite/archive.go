package sqlite

import (
	"context"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/brokenbots/castle/castle/internal/store"
)

// PageSize bounds for ListRunArchives pages.
const (
	ListRunArchivesDefaultLimit = 100
	ListRunArchivesMaxLimit     = 1000
)

// archiveCursor is the keyset cursor for ListRunArchives: pages key on
// run_id ASC (the primary key), so a page stays stable across concurrent
// archives of other runs and repeats exactly which ids it already served.
type archiveCursor struct {
	RunID string `json:"run_id"`
}

func encodeArchiveCursor(runID string) string {
	b, _ := json.Marshal(archiveCursor{RunID: runID})
	return base64.RawURLEncoding.EncodeToString(b)
}

func decodeArchiveCursor(token string) (runID string, err error) {
	if token == "" {
		return "", nil
	}
	b, err := base64.RawURLEncoding.DecodeString(token)
	if err != nil {
		return "", fmt.Errorf("%w: %s", store.ErrInvalidCursor, err)
	}
	var c archiveCursor
	if err := json.Unmarshal(b, &c); err != nil {
		return "", fmt.Errorf("%w: %s", store.ErrInvalidCursor, err)
	}
	if c.RunID == "" {
		return "", fmt.Errorf("%w: missing run_id", store.ErrInvalidCursor)
	}
	return c.RunID, nil
}

// archiveColumns are the run_archive columns mapped onto
// store.RunArchiveRecord.
const archiveColumns = `run_id, job_name, final_phase, ticket, record, operator_id, archived_at`

// PutRunArchiveRecord upserts the terminal-run archive for rec.RunID
// (KB-308). Latest wins: an existing row for the same run id is overwritten
// in place, so a retried archive leg (archive-before-finalizer with
// backoff) never duplicates history. created reports whether this call
// inserted a new record (first archive) or overwrote an existing one.
func (s *Store) PutRunArchiveRecord(ctx context.Context, rec *store.RunArchiveRecord) (bool, error) {
	ctx, cancel := s.opCtx(ctx)
	defer cancel()
	if rec.RunID == "" {
		return false, fmt.Errorf("run_id required")
	}
	if rec.Record == "" {
		return false, fmt.Errorf("record body required")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return false, err
	}
	defer func() { _ = tx.Rollback() }()

	var existed bool
	if err := tx.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM run_archive WHERE run_id = ?)`, rec.RunID).Scan(&existed); err != nil {
		return false, err
	}
	if existed {
		_, err = tx.ExecContext(ctx,
			`UPDATE run_archive SET job_name=?, final_phase=?, ticket=?, record=?, operator_id=?, archived_at=? WHERE run_id=?`,
			rec.JobName, rec.FinalPhase, rec.Ticket, rec.Record, rec.OperatorID, rec.ArchivedAt.Format(tsLayout), rec.RunID)
	} else {
		_, err = tx.ExecContext(ctx,
			`INSERT INTO run_archive(run_id, job_name, final_phase, ticket, record, operator_id, archived_at) VALUES(?,?,?,?,?,?,?)`,
			rec.RunID, rec.JobName, rec.FinalPhase, rec.Ticket, rec.Record, rec.OperatorID, rec.ArchivedAt.Format(tsLayout))
	}
	if err != nil {
		return false, err
	}
	if err := tx.Commit(); err != nil {
		return false, err
	}
	return !existed, nil
}

// GetRunArchiveRecord returns the stored archive record for runID.
// Returns ErrNotFound when the run has no archive record yet.
func (s *Store) GetRunArchiveRecord(ctx context.Context, runID string) (*store.RunArchiveRecord, error) {
	ctx, cancel := s.opCtx(ctx)
	defer cancel()
	rec, err := scanArchiveRow(s.reader.QueryRowContext(ctx,
		`SELECT `+archiveColumns+` FROM run_archive WHERE run_id = ?`, runID))
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, store.ErrNotFound
		}
		return nil, err
	}
	return rec, nil
}

// ListRunArchives returns stored archive records in ascending run-id order,
// filtered to run ids with the given prefix when non-empty (byte-range
// bounds [prefix, prefix incremented) — exactly HasPrefix, including the
// row whose id equals the prefix itself). limit > 0 bounds the page and
// only issues a continuation token when a further row exists; limit <= 0
// uses the default page size. afterRunID continues listing after this run
// id (the boundary the pageToken also carries, for direct store callers);
// pageToken wins when set.
func (s *Store) ListRunArchives(ctx context.Context, runIDPrefix, afterRunID string, limit int, pageToken string) ([]*store.RunArchiveRecord, string, error) {
	ctx, cancel := s.opCtx(ctx)
	defer cancel()
	if pageToken != "" {
		id, err := decodeArchiveCursor(pageToken)
		if err != nil {
			return nil, "", err
		}
		afterRunID = id
	}
	if limit <= 0 {
		limit = ListRunArchivesDefaultLimit
	}
	if limit > ListRunArchivesMaxLimit {
		limit = ListRunArchivesMaxLimit
	}
	q := `SELECT ` + archiveColumns + ` FROM run_archive WHERE 1=1`
	args := []any{}
	if runIDPrefix != "" {
		// Byte-collation range for HasPrefix (SQLite BINARY collation):
		// >= prefix admits every id starting with the prefix (including
		// the prefix itself); < prefixUpperBound excludes everything past
		// the prefix space, so no id from a later range leaks into the
		// page. The upper bound is only unrepresentable when every prefix
		// byte is 0xFF — then the remaining id space IS the suffix space
		// and the lower bound alone is exact.
		q += ` AND run_id >= ?`
		args = append(args, runIDPrefix)
		if upper := prefixUpperBound(runIDPrefix); upper != "" {
			q += ` AND run_id < ?`
			args = append(args, upper)
		}
	}
	if afterRunID != "" {
		q += ` AND run_id > ?`
		args = append(args, afterRunID)
	}
	// Probe one row past the page so the token is only issued when a next
	// page actually exists (same scheme as ListRuns).
	q += ` ORDER BY run_id ASC LIMIT ?`
	args = append(args, limit+1)
	rows, err := s.reader.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, "", err
	}
	defer rows.Close()
	var out []*store.RunArchiveRecord
	for rows.Next() {
		rec, err := scanArchiveRows(rows)
		if err != nil {
			return nil, "", err
		}
		out = append(out, rec)
	}
	if err := rows.Err(); err != nil {
		return nil, "", err
	}
	var next string
	if len(out) > limit {
		out = out[:limit]
		next = encodeArchiveCursor(out[len(out)-1].RunID)
	}
	return out, next, nil
}

// prefixUpperBound returns the smallest string greater than every string
// with the given prefix: the last byte incremented with big-endian carry
// ("abc" → "abd"). Only reachable impossibility (every byte 0xFF) returns
// "" and callers omit the upper bound — the whole remaining id space is
// then the prefix's suffix space, so no bound can exclude a prefixed id.
func prefixUpperBound(prefix string) string {
	b := []byte(prefix)
	for i := len(b) - 1; i >= 0; i-- {
		if b[i] == 0xFF {
			continue
		}
		b[i]++
		return string(b[:i+1])
	}
	return ""
}

func scanArchiveRow(row *sql.Row) (*store.RunArchiveRecord, error) {
	var rec store.RunArchiveRecord
	var archivedAt string
	if err := row.Scan(&rec.RunID, &rec.JobName, &rec.FinalPhase, &rec.Ticket, &rec.Record, &rec.OperatorID, &archivedAt); err != nil {
		return nil, err
	}
	rec.ArchivedAt = mustParseTime(archivedAt)
	return &rec, nil
}

func scanArchiveRows(rows *sql.Rows) (*store.RunArchiveRecord, error) {
	var rec store.RunArchiveRecord
	var archivedAt string
	if err := rows.Scan(&rec.RunID, &rec.JobName, &rec.FinalPhase, &rec.Ticket, &rec.Record, &rec.OperatorID, &archivedAt); err != nil {
		return nil, err
	}
	rec.ArchivedAt = mustParseTime(archivedAt)
	return &rec, nil
}
