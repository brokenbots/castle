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
// archive_key ASC (the primary key), so a page stays stable across
// concurrent archives of other keys and repeats exactly which keys it
// already served.
type archiveCursor struct {
	Key string `json:"key"`
}

func encodeArchiveCursor(key string) string {
	b, _ := json.Marshal(archiveCursor{Key: key})
	return base64.RawURLEncoding.EncodeToString(b)
}

func decodeArchiveCursor(token string) (key string, err error) {
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
	if c.Key == "" {
		return "", fmt.Errorf("%w: missing key", store.ErrInvalidCursor)
	}
	return c.Key, nil
}

// archiveColumns are the run_archive columns mapped onto
// store.RunArchiveRecord.
const archiveColumns = `archive_key, run_id, job_name, final_phase, ticket, record, operator_id, archived_at`

// PutRunArchiveRecord upserts the terminal-run archive for rec.Key
// (KB-308). Latest wins: an existing row for the same key is overwritten
// in place, so a retried archive leg (archive-before-finalizer with
// backoff) never duplicates history. created reports whether this call
// inserted a new record (first archive) or overwrote an existing one.
// History attribution is first-writer-wins per identity: overwriting a
// record that a different, non-empty identity previously archived fails
// with ErrArchiveOperatorConflict; rec.Key and rec.Record are required.
func (s *Store) PutRunArchiveRecord(ctx context.Context, rec *store.RunArchiveRecord) (bool, error) {
	ctx, cancel := s.opCtx(ctx)
	defer cancel()
	if rec.Key == "" {
		return false, fmt.Errorf("archive key required")
	}
	if rec.Record == "" {
		return false, fmt.Errorf("record body required")
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return false, err
	}
	defer func() { _ = tx.Rollback() }()

	var existed int64
	var stamped string
	if err := tx.QueryRowContext(ctx,
		`SELECT EXISTS(SELECT 1 FROM run_archive WHERE archive_key = ?), COALESCE((SELECT operator_id FROM run_archive WHERE archive_key = ?), '')`,
		rec.Key, rec.Key).Scan(&existed, &stamped); err != nil {
		return false, err
	}
	if stamped != "" && rec.OperatorID != "" && stamped != rec.OperatorID {
		return false, fmt.Errorf("%w: %s", store.ErrArchiveOperatorConflict, rec.Key)
	}
	if existed != 0 {
		_, err = tx.ExecContext(ctx,
			`UPDATE run_archive SET run_id=?, job_name=?, final_phase=?, ticket=?, record=?, operator_id=?, archived_at=? WHERE archive_key=?`,
			rec.RunID, rec.JobName, rec.FinalPhase, rec.Ticket, rec.Record, rec.OperatorID, rec.ArchivedAt.Format(tsLayout), rec.Key)
	} else {
		_, err = tx.ExecContext(ctx,
			`INSERT INTO run_archive(archive_key, run_id, job_name, final_phase, ticket, record, operator_id, archived_at) VALUES(?,?,?,?,?,?,?,?)`,
			rec.Key, rec.RunID, rec.JobName, rec.FinalPhase, rec.Ticket, rec.Record, rec.OperatorID, rec.ArchivedAt.Format(tsLayout))
	}
	if err != nil {
		return false, err
	}
	if err := tx.Commit(); err != nil {
		return false, err
	}
	return existed == 0, nil
}

// GetRunArchiveRecord returns the stored archive record for key.
// Returns ErrNotFound when the key has no archive record yet.
func (s *Store) GetRunArchiveRecord(ctx context.Context, key string) (*store.RunArchiveRecord, error) {
	ctx, cancel := s.opCtx(ctx)
	defer cancel()
	rec, err := scanArchiveRow(s.reader.QueryRowContext(ctx,
		`SELECT `+archiveColumns+` FROM run_archive WHERE archive_key = ?`, key))
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, store.ErrNotFound
		}
		return nil, err
	}
	return rec, nil
}

// ListRunArchives returns stored archive records in ascending archive-key
// order, filtered to keys with the given prefix when non-empty (byte-range
// bounds [prefix, prefix incremented) — exactly HasPrefix, including the
// row whose key equals the prefix itself). limit > 0 bounds the page and
// only issues a continuation token when a further row exists; limit <= 0
// uses the default page size. afterKey continues listing after this key
// (the boundary the pageToken also carries, for direct store callers);
// pageToken wins when set.
func (s *Store) ListRunArchives(ctx context.Context, keyPrefix, afterKey string, limit int, pageToken string) ([]*store.RunArchiveRecord, string, error) {
	ctx, cancel := s.opCtx(ctx)
	defer cancel()
	if pageToken != "" {
		key, err := decodeArchiveCursor(pageToken)
		if err != nil {
			return nil, "", err
		}
		afterKey = key
	}
	if limit <= 0 {
		limit = ListRunArchivesDefaultLimit
	}
	if limit > ListRunArchivesMaxLimit {
		limit = ListRunArchivesMaxLimit
	}
	q := `SELECT ` + archiveColumns + ` FROM run_archive WHERE 1=1`
	args := []any{}
	if keyPrefix != "" {
		// Byte-collation range for HasPrefix (SQLite BINARY collation):
		// >= prefix admits every key starting with the prefix (including
		// the prefix itself); < prefixUpperBound excludes everything past
		// the prefix space, so no key from a later range leaks into the
		// page. The upper bound is only unrepresentable when every prefix
		// byte is 0xFF — then the remaining key space IS the suffix space
		// and the lower bound alone is exact.
		q += ` AND archive_key >= ?`
		args = append(args, keyPrefix)
		if upper := prefixUpperBound(keyPrefix); upper != "" {
			q += ` AND archive_key < ?`
			args = append(args, upper)
		}
	}
	if afterKey != "" {
		q += ` AND archive_key > ?`
		args = append(args, afterKey)
	}
	// Probe one row past the page so the token is only issued when a next
	// page actually exists (same scheme as ListRuns).
	q += ` ORDER BY archive_key ASC LIMIT ?`
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
		next = encodeArchiveCursor(out[len(out)-1].Key)
	}
	return out, next, nil
}

// prefixUpperBound returns the smallest string greater than every string
// with the given prefix: the last byte incremented with big-endian carry
// ("abc" → "abd"). Only reachable impossibility (every byte 0xFF) returns
// "" and callers omit the upper bound — the whole remaining key space is
// then the prefix's suffix space, so no bound can exclude a prefixed key.
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
	if err := row.Scan(&rec.Key, &rec.RunID, &rec.JobName, &rec.FinalPhase, &rec.Ticket, &rec.Record, &rec.OperatorID, &archivedAt); err != nil {
		return nil, err
	}
	rec.ArchivedAt = mustParseTime(archivedAt)
	return &rec, nil
}

func scanArchiveRows(rows *sql.Rows) (*store.RunArchiveRecord, error) {
	var rec store.RunArchiveRecord
	var archivedAt string
	if err := rows.Scan(&rec.Key, &rec.RunID, &rec.JobName, &rec.FinalPhase, &rec.Ticket, &rec.Record, &rec.OperatorID, &archivedAt); err != nil {
		return nil, err
	}
	rec.ArchivedAt = mustParseTime(archivedAt)
	return &rec, nil
}
