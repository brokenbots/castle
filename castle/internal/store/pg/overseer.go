package pg

import (
	"context"
	"database/sql"
	"errors"
	"time"

	"github.com/brokenbots/castle/castle/internal/store"
)

func (s *Store) CreateOverseer(ctx context.Context, o *store.Overseer) error {
	ctx, cancel := s.opCtx(ctx)
	defer cancel()
	labels, err := store.MarshalLabels(o.Labels)
	if err != nil {
		return err
	}
	_, err = s.db.ExecContext(ctx,
		`INSERT INTO overseers(id,name,hostname,version,token_hash,status,labels,created_at,last_seen_at) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9)`,
		o.ID, o.Name, o.Hostname, o.Version, o.TokenHash, o.Status, labels, us(o.CreatedAt), us(o.LastSeenAt))
	if err == nil {
		s.freshness.recordWrite(o.CreatedAt)
	}
	return err
}

func (s *Store) GetOverseer(ctx context.Context, id string) (*store.Overseer, error) {
	ctx, cancel := s.opCtx(ctx)
	defer cancel()
	row := s.db.QueryRowContext(ctx, `SELECT id,name,hostname,version,token_hash,status,labels,created_at,last_seen_at FROM overseers WHERE id=$1`, id)
	return scanOverseer(row.Scan)
}

func scanOverseer(scan func(...any) error) (*store.Overseer, error) {
	var o store.Overseer
	var created, seen time.Time
	var labels sql.NullString
	if err := scan(&o.ID, &o.Name, &o.Hostname, &o.Version, &o.TokenHash, &o.Status, &labels, &created, &seen); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, store.ErrNotFound
		}
		return nil, err
	}
	o.CreatedAt = created
	o.LastSeenAt = seen
	o.Labels = store.UnmarshalLabels(labels.String)
	return &o, nil
}

func (s *Store) ListOverseers(ctx context.Context) ([]*store.Overseer, error) {
	ctx, cancel := s.opCtx(ctx)
	defer cancel()
	out, err := s.scanOverseers(ctx, s.reader)
	if err != nil {
		return nil, err
	}
	// KB-223: if the reader view lacks an overseer write the writer already
	// committed, re-read through the authoritative writer handle instead of
	// serving a possibly-pinned view to the auth token resolution path. In
	// the steady state (one pool on pg) this costs nothing.
	readerNewest := newestOverseerCreatedAt(out)
	if s.freshness.newerThan(readerNewest) {
		authoritative, err := s.scanOverseers(ctx, s.db)
		if err != nil {
			// Keep the reader rows; the staleness audit below reports the gap.
			s.log.WarnContext(ctx, "pg: authoritative overseer fallback read failed", "error", err.Error())
		} else {
			out = authoritative
			readerNewest = newestOverseerCreatedAt(out)
			s.freshness.healed(ctx, s.log)
		}
	}
	// Audit the reader view against the writer's newest write (KB-10).
	s.freshness.check(ctx, readerNewest, s.log)
	return out, nil
}

// newestOverseerCreatedAt returns the newest created_at in the view, or zero
// when the view is empty. Callers order scans by created_at DESC, so it is
// simply the first row's timestamp.
func newestOverseerCreatedAt(list []*store.Overseer) time.Time {
	if len(list) == 0 {
		return time.Time{}
	}
	return list[0].CreatedAt
}

// scanOverseers runs the overseers table scan over the given pool handle.
func (s *Store) scanOverseers(ctx context.Context, db *sql.DB) ([]*store.Overseer, error) {
	rows, err := db.QueryContext(ctx, `SELECT id,name,hostname,version,token_hash,status,labels,created_at,last_seen_at FROM overseers ORDER BY created_at DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*store.Overseer
	for rows.Next() {
		o, err := scanOverseer(rows.Scan)
		if err != nil {
			return nil, err
		}
		out = append(out, o)
	}
	return out, rows.Err()
}

func (s *Store) UpdateOverseerSeen(ctx context.Context, id string, ts time.Time) error {
	ctx, cancel := s.opCtx(ctx)
	defer cancel()
	_, err := s.db.ExecContext(ctx, `UPDATE overseers SET last_seen_at=$1, status='online' WHERE id=$2`, us(ts), id)
	return err
}

func (s *Store) UpdateOverseerStatus(ctx context.Context, id, status string) error {
	ctx, cancel := s.opCtx(ctx)
	defer cancel()
	_, err := s.db.ExecContext(ctx, `UPDATE overseers SET status=$1 WHERE id=$2`, status, id)
	return err
}

func (s *Store) MarkOfflineBefore(ctx context.Context, before time.Time) error {
	ctx, cancel := s.opCtx(ctx)
	defer cancel()
	_, err := s.db.ExecContext(ctx, `UPDATE overseers SET status='offline' WHERE last_seen_at < $1 AND status='online'`, us(before))
	return err
}

// OverseerTokenHashPresent reports whether an overseer whose persisted token
// hash equals tokenHash exists in the authoritative writer view. See the
// sqlite profile (KB-223) for the auth-interceptor rationale; the probe stays
// gated on reader-freshness suspicion for parity.
func (s *Store) OverseerTokenHashPresent(ctx context.Context, tokenHash string) (bool, error) {
	if !s.freshness.readerBehind() {
		return false, nil
	}
	ctx, cancel := s.opCtx(ctx)
	defer cancel()
	var count int
	if err := s.db.QueryRowContext(ctx, `SELECT count(*) FROM overseers WHERE token_hash = $1`, tokenHash).Scan(&count); err != nil {
		return false, err
	}
	return count > 0, nil
}
