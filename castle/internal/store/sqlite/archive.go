package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"github.com/brokenbots/castle/castle/internal/store"
)

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
//
// The upsert is a two-statement transaction carrying the pg profile's
// single-statement contract (see the pg package for the atomic
// INSERT ... ON CONFLICT DO UPDATE WHERE form): sqlite has no xmax signal,
// and RETURNING on an upsert cannot distinguish the insert branch from the
// update branch, so detect the first-writer case with
// INSERT ... ON CONFLICT(archive_key) DO NOTHING (a returned row means this
// call inserted the first record) and fall through to a guarded UPDATE whose
// WHERE reproduces the conflict contract exactly. sqlite's write lock holds
// across the whole transaction, so the pair is race-free: a concurrent
// writer blocks until this transaction commits and then observes the row.
// A zero-row UPDATE is a refused cross-identity overwrite.
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

	var inserted int64
	err = tx.QueryRowContext(ctx,
		`INSERT INTO run_archive(archive_key, run_id, job_name, final_phase, ticket, record, operator_id, archived_at)
		 VALUES(?,?,?,?,?,?,?,?)
		 ON CONFLICT(archive_key) DO NOTHING
		 RETURNING 1`,
		rec.Key, rec.RunID, rec.JobName, rec.FinalPhase, rec.Ticket, rec.Record, rec.OperatorID,
		rec.ArchivedAt.Format(store.TimestampLayout)).Scan(&inserted)
	if err == nil {
		// First archive of the key.
		if err := tx.Commit(); err != nil {
			return false, err
		}
		return true, nil
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return false, err
	}

	// The key is taken; overwrite only when the identity guard allows it.
	res, err := tx.ExecContext(ctx,
		`UPDATE run_archive
		 SET run_id=?, job_name=?, final_phase=?, ticket=?, record=?, operator_id=?, archived_at=?
		 WHERE archive_key=? AND (operator_id='' OR ?='' OR operator_id=?)`,
		rec.RunID, rec.JobName, rec.FinalPhase, rec.Ticket, rec.Record, rec.OperatorID,
		rec.ArchivedAt.Format(store.TimestampLayout),
		rec.Key, rec.OperatorID, rec.OperatorID)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, err
	}
	if n == 0 {
		return false, fmt.Errorf("%w: %s", store.ErrArchiveOperatorConflict, rec.Key)
	}
	if err := tx.Commit(); err != nil {
		return false, err
	}
	return false, nil
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
		key, err := store.DecodeArchiveCursor(pageToken)
		if err != nil {
			return nil, "", err
		}
		afterKey = key
	}
	if limit <= 0 {
		limit = store.ListRunArchivesDefaultLimit
	}
	if limit > store.ListRunArchivesMaxLimit {
		limit = store.ListRunArchivesMaxLimit
	}
	q := `SELECT ` + archiveColumns + ` FROM run_archive WHERE 1=1`
	args := []any{}
	if keyPrefix != "" {
		// Byte-collation range for HasPrefix (SQLite BINARY collation):
		// >= prefix admits every key starting with the prefix (including
		// the prefix itself); < store.PrefixUpperBound excludes everything past
		// the prefix space, so no key from a later range leaks into the
		// page. The upper bound is only unrepresentable when every prefix
		// byte is 0xFF — then the remaining key space IS the suffix space
		// and the lower bound alone is exact.
		q += ` AND archive_key >= ?`
		args = append(args, keyPrefix)
		if upper := store.PrefixUpperBound(keyPrefix); upper != "" {
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
		next = store.EncodeArchiveCursor(out[len(out)-1].Key)
	}
	return out, next, nil
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
