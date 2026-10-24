package pg

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

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
// The whole upsert is one atomic statement: the DO UPDATE guard reproduces
// the conflict contract exactly (refuse only when the stored identity is
// non-empty, the incoming identity is non-empty, and the two differ), so a
// concurrent writer can never observe an absent row that another writer has
// just claimed. RETURNING (xmax = 0) reports the insert/update branch:
// xmax is 0 on a freshly inserted tuple and holds the current transaction
// id on a tuple updated by this statement. No row is returned when the
// guard refused the overwrite, which is the conflict case.
func (s *Store) PutRunArchiveRecord(ctx context.Context, rec *store.RunArchiveRecord) (bool, error) {
	ctx, cancel := s.opCtx(ctx)
	defer cancel()
	if rec.Key == "" {
		return false, fmt.Errorf("archive key required")
	}
	if rec.Record == "" {
		return false, fmt.Errorf("record body required")
	}
	var created bool
	err := s.db.QueryRowContext(ctx, `
		INSERT INTO run_archive(archive_key, run_id, job_name, final_phase, ticket, record, operator_id, archived_at)
		VALUES($1,$2,$3,$4,$5,$6,$7,$8)
		ON CONFLICT (archive_key) DO UPDATE
		SET run_id=EXCLUDED.run_id,
		    job_name=EXCLUDED.job_name,
		    final_phase=EXCLUDED.final_phase,
		    ticket=EXCLUDED.ticket,
		    record=EXCLUDED.record,
		    operator_id=EXCLUDED.operator_id,
		    archived_at=EXCLUDED.archived_at
		WHERE run_archive.operator_id = '' OR EXCLUDED.operator_id = '' OR run_archive.operator_id = EXCLUDED.operator_id
		RETURNING (xmax = 0)`,
		rec.Key, rec.RunID, rec.JobName, rec.FinalPhase, rec.Ticket, rec.Record, rec.OperatorID, us(rec.ArchivedAt)).Scan(&created)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			// The DO UPDATE guard evaluated false: a different non-empty
			// identity owns this archive key.
			return false, fmt.Errorf("%w: %s", store.ErrArchiveOperatorConflict, rec.Key)
		}
		return false, err
	}
	return created, nil
}

// GetRunArchiveRecord returns the stored archive record for key.
// Returns ErrNotFound when the key has no archive record yet.
func (s *Store) GetRunArchiveRecord(ctx context.Context, key string) (*store.RunArchiveRecord, error) {
	ctx, cancel := s.opCtx(ctx)
	defer cancel()
	rec, err := scanArchiveRow(s.reader.QueryRowContext(ctx,
		`SELECT `+archiveColumns+` FROM run_archive WHERE archive_key = $1`, key))
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
//
// The archive_key column is COLLATE "C", so the byte-range prefix bounds and
// the ORDER BY are byte-ordered — exactly the sqlite BINARY semantics the
// shared PrefixUpperBound pagination helper relies on.
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
	p := &plf{}
	if keyPrefix != "" {
		// Byte-collation range for HasPrefix: >= prefix admits every key
		// starting with the prefix (including the prefix itself); <
		// store.PrefixUpperBound excludes everything past the prefix space,
		// so no key from a later range leaks into the page. The upper bound
		// is only unrepresentable when every prefix byte is 0xFF — then the
		// remaining key space IS the suffix space and the lower bound alone
		// is exact.
		q += ` AND archive_key >= ` + p.next(keyPrefix)
		if upper := store.PrefixUpperBound(keyPrefix); upper != "" {
			q += ` AND archive_key < ` + p.next(upper)
		}
	}
	if afterKey != "" {
		q += ` AND archive_key > ` + p.next(afterKey)
	}
	// Probe one row past the page so the token is only issued when a next
	// page actually exists (same scheme as ListRuns).
	q += ` ORDER BY archive_key ASC LIMIT ` + p.next(limit+1)
	rows, err := s.reader.QueryContext(ctx, q, p.args...)
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
		last := out[limit-1]
		next = store.EncodeArchiveCursor(last.Key)
		out = out[:limit]
	}
	return out, next, nil
}

func scanArchiveRowScan(scan func(...any) error) (*store.RunArchiveRecord, error) {
	var rec store.RunArchiveRecord
	var archivedAt time.Time
	// run_archive non-key columns are NOT NULL DEFAULT '' (byte-identical to
	// the sqlite profile), so they scan straight into the record's strings.
	err := scan(&rec.Key, &rec.RunID, &rec.JobName, &rec.FinalPhase, &rec.Ticket, &rec.Record, &rec.OperatorID, &archivedAt)
	if err != nil {
		return nil, err
	}
	rec.ArchivedAt = archivedAt
	return &rec, nil
}

func scanArchiveRow(row *sql.Row) (*store.RunArchiveRecord, error) {
	return scanArchiveRowScan(row.Scan)
}

func scanArchiveRows(rows *sql.Rows) (*store.RunArchiveRecord, error) {
	return scanArchiveRowScan(rows.Scan)
}
