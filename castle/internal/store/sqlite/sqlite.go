// Package sqlite is the SQLite implementation of store.Store using the
// pure-Go modernc.org/sqlite driver (no CGO required).
package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
	"sort"
	"strings"
	"time"

	"github.com/google/uuid"
	_ "modernc.org/sqlite"

	"github.com/brokenbots/castle/castle/internal/store"
	"github.com/brokenbots/castle/castle/internal/store/reap"
)

// Compile-time pin: the sqlite profile must satisfy the whole store contract.
var _ store.Store = (*Store)(nil)

type Store struct {
	db *sql.DB
	// reader is a dedicated read handle on the same WAL database (CRI-143).
	// Long scans — the run reaper's staleness scan — run here so they never
	// queue behind or hold the single serialized writer connection that all
	// RPC traffic shares.
	reader *sql.DB
	// freshness lets ListOverseers audit its reader view against the newest
	// write the writer handle has committed (KB-10 staleness telemetry).
	freshness readerFreshness
	log       *slog.Logger
}

// openDSN is the pragma set every pool handle uses. WAL mode still allows
// concurrent readers, and busy_timeout keeps rare cross-handle writer
// collisions waiting instead of failing fast.
const openDSN = "file:%s?_pragma=journal_mode(WAL)&_pragma=foreign_keys(1)&_pragma=busy_timeout(5000)"

// Open opens (or creates) the SQLite store database at path, running the
// embedded migrations before the first caller touches the store. In-memory
// DSNs are rejected: the dedicated reader handle only works for file-backed
// databases (each pooled connection on a :memory: DSN gets its own private,
// empty database, so reads would fail with "no such table" — CRI-143).
func Open(path string) (*Store, error) {
	if path == ":memory:" {
		return nil, fmt.Errorf("sqlite: :memory: stores are not supported; use a file path")
	}
	db, err := sql.Open("sqlite", fmt.Sprintf(openDSN, path))
	if err != nil {
		return nil, err
	}
	if err := Migrate(context.Background(), db); err != nil {
		_ = db.Close()
		return nil, err
	}
	reader, err := sql.Open("sqlite", fmt.Sprintf(openDSN, path))
	if err != nil {
		_ = db.Close()
		return nil, err
	}
	// Serialize all SQLite writers through a single connection. WAL mode still
	// allows concurrent readers, but multiple writer connections can return
	// SQLITE_BUSY under load. A single open connection guarantees writers never
	// conflict and matches the behavior observed when callers explicitly set
	// SetMaxOpenConns(1). The separate reader handle gives the run reaper's
	// scan a connection that never competes with RPC work on this one (CRI-143).
	db.SetMaxOpenConns(1)
	reader.SetMaxOpenConns(1)
	// Disable idle pooling on the reader (KB-10): a pooled reader connection
	// can keep an open read transaction alive (e.g. a driver bug that leaks an
	// active statement, or a scan interrupted mid-read), pinning the WAL
	// snapshot it holds. Everything the reader then returns is frozen at the
	// pin, so agents registered afterwards become invisible to the auth token
	// resolution path and CreateRun rejects their fresh tokens with
	// "unauthenticated: invalid token" — observed live on 2026-09-25. With no
	// idle connections every read opens a fresh connection and therefore
	// reads the current WAL snapshot by construction. Concurrency is
	// unchanged: MaxOpenConns(1) still serializes reader work so reaper scans
	// stay isolated from the writer (CRI-143).
	reader.SetMaxIdleConns(0)
	return &Store{db: db, reader: reader, log: slog.Default()}, nil
}

func (s *Store) Close() error {
	dbErr := s.db.Close()
	// Close the reader after the writer so in-flight RPC writes finish first.
	if err := s.reader.Close(); dbErr == nil {
		return err
	}
	return dbErr
}

// SetMaxOpenConns configures the maximum number of open connections in the
// writer pool. Open defaults the writer to 1 to serialize writers and avoid
// SQLITE_BUSY races (CRI-78); the reader pool stays separate at 1 so reaper
// scans can never queue behind RPC traffic (CRI-143).
func (s *Store) SetMaxOpenConns(n int) { s.db.SetMaxOpenConns(n) }

// storeOpTimeout bounds each store operation executed on a detached context.
// It sits far above the 5s busy_timeout so ordinary writer contention never
// trips it; it only ever caps work that would otherwise run forever because
// its caller is gone.
const storeOpTimeout = 30 * time.Second

// opCtx detaches ctx from its creator (KB-222) before any statement is
// executed. Every public store method funnels its context through this: a
// cancelled gRPC stream (an expired SubmitEvents deadline, a gone client)
// must never reach the driver, because the driver's context watcher responds
// to cancellation by firing sqlite3_interrupt on the WHOLE connection — and
// the writer pool is one serialized connection shared by every RPC
// (CRI-78/CRI-143). A single interrupt landing there historically failed
// every DB-backed call for tens of minutes (KB-222 incident, 2026-09-21).
// Values (request identity, loggers) are preserved; cancellation and
// deadlines are replaced by the local storeOpTimeout bound so detached work
// still terminates. Nested through public->public calls it is idempotent.
func (s *Store) opCtx(ctx context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.WithoutCancel(ctx), storeOpTimeout)
}

// -----------------------------------------------------------------------------
// Projects (KB-318)
// -----------------------------------------------------------------------------

// CreateProject persists a new project. The ID must be a slug shape, the
// membership policy must be one of the known policy values, and queue names
// must be non-empty when present. Settings and info are opaque maps: the
// store persists and returns them, never interprets them.
func (s *Store) CreateProject(ctx context.Context, p *store.Project) error {
	ctx, cancel := s.opCtx(ctx)
	defer cancel()
	if p == nil {
		return errors.New("project required")
	}
	if err := store.ValidateProjectID(p.ID); err != nil {
		return err
	}
	if !store.IsValidProjectMembershipPolicy(p.MembershipPolicy) {
		return fmt.Errorf("%w: %q", store.ErrInvalidProjectPolicy, p.MembershipPolicy)
	}
	queues := make([]string, 0, len(p.Queues))
	for _, q := range p.Queues {
		if q == "" {
			return errors.New("queue names must be non-empty")
		}
		queues = append(queues, q)
	}
	if p.CreatedAt.IsZero() {
		p.CreatedAt = time.Now()
	}
	if p.UpdatedAt.IsZero() {
		p.UpdatedAt = p.CreatedAt
	}
	queuesJSON, err := store.MarshalQueues(queues)
	if err != nil {
		return err
	}
	_, err = s.db.ExecContext(ctx, `
		INSERT INTO projects (id, membership_policy, default_refire_policy, queues, settings, settings_etag, info, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		p.ID,
		p.MembershipPolicy,
		p.DefaultRefirePolicy,
		queuesJSON,
		store.MarshalJSONMap(p.Settings),
		store.SettingsETag(p.Settings),
		store.MarshalJSONMap(p.Info),
		p.CreatedAt.UTC().Format(store.TimestampLayout),
		p.UpdatedAt.UTC().Format(store.TimestampLayout))
	if err != nil {
		if store.IsUniqueViolation(err) {
			return store.ErrProjectExists
		}
		return err
	}
	return nil
}

// scanProject reads one projects row from the given scanner helper.
func scanProject(scan func(dest ...any) error) (*store.Project, error) {
	var p store.Project
	var queues, settings, info sql.NullString
	var created, updated string
	if err := scan(&p.ID, &p.MembershipPolicy, &p.DefaultRefirePolicy,
		&queues, &settings, &info, &created, &updated); err != nil {
		return nil, err
	}
	p.CreatedAt, _ = time.Parse(store.TimestampLayout, created)
	p.UpdatedAt, _ = time.Parse(store.TimestampLayout, updated)
	p.Queues = store.UnmarshalQueues(queues.String)
	p.Settings = store.UnmarshalJSONMap(settings.String)
	p.Info = store.UnmarshalJSONMap(info.String)
	return &p, nil
}

// GetProject loads a project by ID.
func (s *Store) GetProject(ctx context.Context, id string) (*store.Project, error) {
	ctx, cancel := s.opCtx(ctx)
	defer cancel()
	if id == "" {
		return nil, errors.New("id required")
	}
	p, err := scanProject(s.reader.QueryRowContext(ctx, `
		SELECT id, membership_policy, COALESCE(default_refire_policy,''), COALESCE(queues,''),
		       COALESCE(settings,''), COALESCE(info,''), created_at, updated_at
		FROM projects WHERE id=?`, id).Scan)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, store.ErrNotFound
		}
		return nil, err
	}
	return p, nil
}

// ListProjects returns every project ordered by ID.
func (s *Store) ListProjects(ctx context.Context) ([]*store.Project, error) {
	ctx, cancel := s.opCtx(ctx)
	defer cancel()
	rows, err := s.reader.QueryContext(ctx, `
		SELECT id, membership_policy, COALESCE(default_refire_policy,''), COALESCE(queues,''),
		       COALESCE(settings,''), COALESCE(info,''), created_at, updated_at
		FROM projects ORDER BY id ASC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	projects := make([]*store.Project, 0)
	for rows.Next() {
		p, err := scanProject(rows.Scan)
		if err != nil {
			return nil, err
		}
		projects = append(projects, p)
	}
	return projects, rows.Err()
}

// JoinProject records an agent's own membership. Agents may join themselves
// only while the project's membership policy is open; operator-driven
// membership regardless of policy uses AddProjectMember.
func (s *Store) JoinProject(ctx context.Context, projectID, memberID, kind string, now time.Time) error {
	if !store.IsValidProjectMemberKind(kind) {
		return fmt.Errorf("%w: %q", store.ErrInvalidProjectMember, kind)
	}
	return s.joinProject(ctx, projectID, memberID, kind, now, false)
}

// AddProjectMember records operator-driven membership regardless of the
// project's membership policy.
func (s *Store) AddProjectMember(ctx context.Context, m *store.ProjectMember) error {
	if m == nil {
		return errors.New("member required")
	}
	if !store.IsValidProjectMemberKind(m.Kind) {
		return fmt.Errorf("%w: %q", store.ErrInvalidProjectMember, m.Kind)
	}
	if m.CreatedAt.IsZero() {
		m.CreatedAt = time.Now()
	}
	return s.joinProject(ctx, m.ProjectID, m.MemberID, m.Kind, m.CreatedAt, true)
}

// joinProject performs the membership upsert shared by JoinProject and
// AddProjectMember. Re-joining with a different member kind updates the kind;
// the operation is idempotent and never rewrites created_at.
func (s *Store) joinProject(ctx context.Context, projectID, memberID, kind string, now time.Time, bypassPolicy bool) error {
	ctx, cancel := s.opCtx(ctx)
	defer cancel()
	if projectID == "" {
		return errors.New("project id required")
	}
	if memberID == "" {
		return errors.New("member id required")
	}
	ts := now.UTC().Format(store.TimestampLayout)
	if bypassPolicy {
		// Operator path: verify the project exists so a typo'd project ID
		// surfaces as ErrNotFound instead of silently creating an orphan
		// membership the lease scan would never match.
		var one int
		if err := s.db.QueryRowContext(ctx, `SELECT 1 FROM projects WHERE id=?`, projectID).Scan(&one); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return store.ErrNotFound
			}
			return err
		}
		_, err := s.db.ExecContext(ctx, `
			INSERT INTO project_members (project_id, member_id, kind, created_at)
			VALUES (?, ?, ?, ?)
			ON CONFLICT (project_id, member_id) DO UPDATE SET kind=excluded.kind`,
			projectID, memberID, kind, ts)
		return err
	}
	res, err := s.db.ExecContext(ctx, `
		INSERT INTO project_members (project_id, member_id, kind, created_at)
		SELECT p.id, ?, ?, ? FROM projects p
		WHERE p.id = ? AND p.membership_policy = ?
		ON CONFLICT (project_id, member_id) DO UPDATE SET kind=excluded.kind`,
		memberID, kind, ts, projectID, store.ProjectMembershipOpen)
	if err != nil {
		return err
	}
	if affected, _ := res.RowsAffected(); affected == 0 {
		// No row: the project is missing or its membership policy is
		// closed. Re-check to report the precise condition.
		var policy string
		err := s.reader.QueryRowContext(ctx, `SELECT membership_policy FROM projects WHERE id=?`, projectID).Scan(&policy)
		if err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return store.ErrNotFound
			}
			return err
		}
		return store.ErrProjectClosed
	}
	return nil
}

// RemoveProjectMember drops a membership. Removing a member that was never
// joined to the project is not an error.
func (s *Store) RemoveProjectMember(ctx context.Context, projectID, memberID string) error {
	ctx, cancel := s.opCtx(ctx)
	defer cancel()
	if projectID == "" || memberID == "" {
		return errors.New("project id and member id required")
	}
	_, err := s.db.ExecContext(ctx, `DELETE FROM project_members WHERE project_id=? AND member_id=?`, projectID, memberID)
	return err
}

// ListProjectMembers returns the members of a project ordered by member id.
func (s *Store) ListProjectMembers(ctx context.Context, projectID string) ([]*store.ProjectMember, error) {
	ctx, cancel := s.opCtx(ctx)
	defer cancel()
	if projectID == "" {
		return nil, errors.New("project id required")
	}
	rows, err := s.reader.QueryContext(ctx, `
		SELECT project_id, member_id, kind, created_at
		FROM project_members WHERE project_id=? ORDER BY member_id ASC`, projectID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	members := make([]*store.ProjectMember, 0)
	for rows.Next() {
		var m store.ProjectMember
		var created string
		if err := rows.Scan(&m.ProjectID, &m.MemberID, &m.Kind, &created); err != nil {
			return nil, err
		}
		m.CreatedAt, _ = time.Parse(store.TimestampLayout, created)
		members = append(members, &m)
	}
	return members, rows.Err()
}

// IsProjectMember reports whether memberID belongs to projectID.
func (s *Store) IsProjectMember(ctx context.Context, projectID, memberID string) (bool, error) {
	ctx, cancel := s.opCtx(ctx)
	defer cancel()
	if projectID == "" || memberID == "" {
		return false, nil
	}
	var one int
	err := s.reader.QueryRowContext(ctx, `
		SELECT 1 FROM project_members WHERE project_id=? AND member_id=?`, projectID, memberID).Scan(&one)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return false, nil
		}
		return false, err
	}
	return true, nil
}

// ListMemberProjects returns the ids of the projects whose member set
// contains memberID (KB-325), read from the same project_members set the
// lease scan and run read scope consult. An unknown member has no projects.
func (s *Store) ListMemberProjects(ctx context.Context, memberID string) ([]string, error) {
	ctx, cancel := s.opCtx(ctx)
	defer cancel()
	if memberID == "" {
		return nil, nil
	}
	rows, err := s.reader.QueryContext(ctx,
		`SELECT project_id FROM project_members WHERE member_id=? ORDER BY project_id ASC`, memberID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out = append(out, id)
	}
	return out, rows.Err()
}

// GetProjectSettings returns the project's convention-keyed settings doc and
// its content-addressed etag. ErrNotFound when the project does not exist.
func (s *Store) GetProjectSettings(ctx context.Context, projectID string) (map[string]string, string, error) {
	ctx, cancel := s.opCtx(ctx)
	defer cancel()
	if projectID == "" {
		return nil, "", errors.New("project id required")
	}
	var settings, etag sql.NullString
	err := s.reader.QueryRowContext(ctx, `
		SELECT COALESCE(settings,''), COALESCE(settings_etag,'')
		FROM projects WHERE id=?`, projectID).Scan(&settings, &etag)
	if err != nil {
		return nil, "", store.ErrNotFound
	}
	return store.UnmarshalJSONMap(settings.String), etag.String, nil
}

// SetProjectSettings swaps the whole settings doc under the caller's expected
// etag. expectedETag "" is accepted only while the current doc is unset; the
// conditional UPDATE re-checks the etag so a concurrent writer loses the swap
// and gets ErrSettingsConflict instead of clobbering its neighbor.
func (s *Store) SetProjectSettings(ctx context.Context, projectID string, settings map[string]string, expectedETag string) (string, error) {
	ctx, cancel := s.opCtx(ctx)
	defer cancel()
	if projectID == "" {
		return "", errors.New("project id required")
	}
	var current string
	err := s.db.QueryRowContext(ctx, `SELECT COALESCE(settings_etag,'') FROM projects WHERE id=?`, projectID).Scan(&current)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return "", store.ErrNotFound
		}
		return "", err
	}
	if expectedETag != current {
		return "", store.ErrSettingsConflict
	}
	etag := store.SettingsETag(settings)
	res, err := s.db.ExecContext(ctx, `
		UPDATE projects
		SET settings=?, settings_etag=?, updated_at=?
		WHERE id=? AND settings_etag=?`,
		store.MarshalJSONMap(settings), etag,
		time.Now().UTC().Format(store.TimestampLayout),
		projectID, current)
	if err != nil {
		return "", err
	}
	affected, err := res.RowsAffected()
	if err != nil {
		return "", err
	}
	if affected == 0 {
		// Another writer swapped the doc between the read above and this
		// UPDATE. A writer that landed the identical content (identical
		// etag) simply won the race before us: treat it as a no-op.
		var now string
		if err := s.db.QueryRowContext(ctx, `SELECT COALESCE(settings_etag,'') FROM projects WHERE id=?`, projectID).Scan(&now); err != nil {
			return "", err
		}
		if now == etag {
			return etag, nil
		}
		return "", store.ErrSettingsConflict
	}
	return etag, nil
}

func (s *Store) CreateOverseer(ctx context.Context, o *store.Overseer) error {
	ctx, cancel := s.opCtx(ctx)
	defer cancel()
	labels, err := store.MarshalLabels(o.Labels)
	if err != nil {
		return err
	}
	identity, err := store.IdentityJSON(o.Identity)
	if err != nil {
		return err
	}
	_, err = s.db.ExecContext(ctx,
		`INSERT INTO overseers(id,name,hostname,version,token_hash,status,labels,created_at,last_seen_at,identity) VALUES(?,?,?,?,?,?,?,?,?,?)`,
		o.ID, o.Name, o.Hostname, o.Version, o.TokenHash, o.Status, labels, o.CreatedAt.Format(store.TimestampLayout), o.LastSeenAt.Format(store.TimestampLayout), identity)
	if err == nil {
		s.freshness.recordWrite(o.CreatedAt)
	}
	return err
}

func (s *Store) GetOverseer(ctx context.Context, id string) (*store.Overseer, error) {
	ctx, cancel := s.opCtx(ctx)
	defer cancel()
	row := s.db.QueryRowContext(ctx, `SELECT id,name,hostname,version,token_hash,status,labels,created_at,last_seen_at,identity FROM overseers WHERE id=?`, id)
	var o store.Overseer
	var created, seen string
	var labels, identity sql.NullString
	if err := row.Scan(&o.ID, &o.Name, &o.Hostname, &o.Version, &o.TokenHash, &o.Status, &labels, &created, &seen, &identity); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, store.ErrNotFound
		}
		return nil, err
	}
	o.CreatedAt, _ = time.Parse(store.TimestampLayout, created)
	o.LastSeenAt, _ = time.Parse(store.TimestampLayout, seen)
	o.Labels = store.UnmarshalLabels(labels.String)
	o.Identity = store.IdentityFromJSON(identity.String)
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
	// serving a possibly-pinned WAL snapshot to the auth token resolution
	// path. In the steady state the reader is fresh and this costs nothing;
	// while a visibility gap exists it restores correctness in-line.
	readerNewest := newestOverseerCreatedAt(out)
	if s.freshness.newerThan(readerNewest) {
		authoritative, err := s.scanOverseers(ctx, s.db)
		if err != nil {
			// Keep the reader rows; the staleness audit below reports the gap.
			s.log.WarnContext(ctx, "sqlite: authoritative overseer fallback read failed", "error", err.Error())
		} else {
			out = authoritative
			readerNewest = newestOverseerCreatedAt(out)
			s.freshness.healed(ctx, s.log)
		}
	}
	// Audit the reader view against the writer's newest write (KB-10): a
	// reader snapshot pinned before the latest registration is the symptom
	// that left fresh agent tokens rejected as unauthenticated.
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
// ListOverseers services the auth token resolution path through the reader;
// the KB-223 staleness fallback re-runs it through the writer handle.
func (s *Store) scanOverseers(ctx context.Context, db *sql.DB) ([]*store.Overseer, error) {
	rows, err := db.QueryContext(ctx, `SELECT id,name,hostname,version,token_hash,status,labels,created_at,last_seen_at,identity FROM overseers ORDER BY created_at DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*store.Overseer
	for rows.Next() {
		var o store.Overseer
		var created, seen string
		var labels, identity sql.NullString
		if err := rows.Scan(&o.ID, &o.Name, &o.Hostname, &o.Version, &o.TokenHash, &o.Status, &labels, &created, &seen, &identity); err != nil {
			return nil, err
		}
		o.CreatedAt, _ = time.Parse(store.TimestampLayout, created)
		o.LastSeenAt, _ = time.Parse(store.TimestampLayout, seen)
		o.Labels = store.UnmarshalLabels(labels.String)
		o.Identity = store.IdentityFromJSON(identity.String)
		out = append(out, &o)
	}
	return out, rows.Err()
}

func (s *Store) UpdateOverseerSeen(ctx context.Context, id string, ts time.Time) error {
	ctx, cancel := s.opCtx(ctx)
	defer cancel()
	_, err := s.db.ExecContext(ctx, `UPDATE overseers SET last_seen_at=?, status='online' WHERE id=?`, ts.Format(store.TimestampLayout), id)
	return err
}

func (s *Store) UpdateOverseerStatus(ctx context.Context, id, status string) error {
	ctx, cancel := s.opCtx(ctx)
	defer cancel()
	_, err := s.db.ExecContext(ctx, `UPDATE overseers SET status=? WHERE id=?`, status, id)
	return err
}

func (s *Store) MarkOfflineBefore(ctx context.Context, before time.Time) error {
	ctx, cancel := s.opCtx(ctx)
	defer cancel()
	_, err := s.db.ExecContext(ctx, `UPDATE overseers SET status='offline' WHERE last_seen_at < ? AND status='online'`, before.Format(store.TimestampLayout))
	return err
}

// OverseerTokenHashPresent reports whether an overseer whose persisted token
// hash equals tokenHash exists in the authoritative writer view, independent
// of the reader handle's WAL snapshot state. The auth interceptor consults it
// only when a presented token resolved to nothing (KB-223), so a registered
// identity whose row the read path cannot see yet is surfaced as "token not
// yet visible" instead of the misleading generic "invalid token".
//
// The probe is gated on reader-freshness suspicion (staleness.go): in the
// steady state it reports not-present without touching the single serialized
// writer connection, keeping failed-token traffic off the write path — and an
// attacker cannot force the gate open, because only successful CreateOverseer
// calls move the freshness state.
func (s *Store) OverseerTokenHashPresent(ctx context.Context, tokenHash string) (bool, error) {
	if !s.freshness.readerBehind() {
		return false, nil
	}
	ctx, cancel := s.opCtx(ctx)
	defer cancel()
	var count int
	if err := s.db.QueryRowContext(ctx, `SELECT count(*) FROM overseers WHERE token_hash = ?`, tokenHash).Scan(&count); err != nil {
		return false, err
	}
	return count > 0, nil
}

// UpsertOrchestrator inserts or replaces an orchestrator identity (CRI-133).
// On conflict, name and token_hash are replaced — rotating the token
// invalidates previously issued accept tokens — and created_at is preserved.
func (s *Store) UpsertOrchestrator(ctx context.Context, o *store.Orchestrator) error {
	ctx, cancel := s.opCtx(ctx)
	defer cancel()
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO orchestrators(id,name,token_hash,created_at) VALUES(?,?,?,?)
		 ON CONFLICT(id) DO UPDATE SET name=excluded.name, token_hash=excluded.token_hash`,
		o.ID, o.Name, o.TokenHash, o.CreatedAt.Format(store.TimestampLayout))
	return err
}

func (s *Store) ListOrchestrators(ctx context.Context) ([]*store.Orchestrator, error) {
	ctx, cancel := s.opCtx(ctx)
	defer cancel()
	rows, err := s.reader.QueryContext(ctx, `SELECT id,name,token_hash,created_at FROM orchestrators ORDER BY created_at ASC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*store.Orchestrator
	for rows.Next() {
		var o store.Orchestrator
		var created string
		if err := rows.Scan(&o.ID, &o.Name, &o.TokenHash, &created); err != nil {
			return nil, err
		}
		o.CreatedAt, _ = time.Parse(store.TimestampLayout, created)
		out = append(out, &o)
	}
	return out, rows.Err()
}

// DeleteOrchestrator removes the orchestrator identity, revoking its accept
// token (CRI-133). Deleting an unknown ID is a no-op.
func (s *Store) DeleteOrchestrator(ctx context.Context, id string) error {
	ctx, cancel := s.opCtx(ctx)
	defer cancel()
	_, err := s.db.ExecContext(ctx, `DELETE FROM orchestrators WHERE id = ?`, id)
	return err
}

// UpsertConsoleUser inserts or replaces a console user (CRI-195). On
// conflict, username and password_hash are replaced — rotating the seeded
// credentials — and created_at is preserved.
func (s *Store) UpsertConsoleUser(ctx context.Context, u *store.ConsoleUser) error {
	ctx, cancel := s.opCtx(ctx)
	defer cancel()
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO console_users(id,username,password_hash,created_at,updated_at) VALUES(?,?,?,?,?)
		 ON CONFLICT(id) DO UPDATE SET username=excluded.username, password_hash=excluded.password_hash, updated_at=excluded.updated_at`,
		u.ID, u.Username, u.PasswordHash, u.CreatedAt.Format(store.TimestampLayout), u.UpdatedAt.Format(store.TimestampLayout))
	return err
}

func (s *Store) GetConsoleUser(ctx context.Context, username string) (*store.ConsoleUser, error) {
	ctx, cancel := s.opCtx(ctx)
	defer cancel()
	row := s.db.QueryRowContext(ctx,
		`SELECT id,username,password_hash,created_at,updated_at FROM console_users WHERE username=?`, username)
	var u store.ConsoleUser
	var created, updated string
	if err := row.Scan(&u.ID, &u.Username, &u.PasswordHash, &created, &updated); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, store.ErrNotFound
		}
		return nil, err
	}
	u.CreatedAt, _ = time.Parse(store.TimestampLayout, created)
	u.UpdatedAt, _ = time.Parse(store.TimestampLayout, updated)
	return &u, nil
}

// DeleteConsoleUsers removes every console user; the ON DELETE CASCADE on
// console_sessions revokes their sessions with them (CRI-195).
func (s *Store) DeleteConsoleUsers(ctx context.Context) error {
	ctx, cancel := s.opCtx(ctx)
	defer cancel()
	_, err := s.db.ExecContext(ctx, `DELETE FROM console_users`)
	return err
}

func (s *Store) CreateConsoleSession(ctx context.Context, sess *store.ConsoleSession) error {
	ctx, cancel := s.opCtx(ctx)
	defer cancel()
	membership, err := store.MarshalMembership(sess.Membership)
	if err != nil {
		return err
	}
	_, err = s.db.ExecContext(ctx,
		`INSERT INTO console_sessions(id,user_id,token_hash,created_at,revoked_at,membership) VALUES(?,?,?,?,?,?)`,
		sess.ID, sess.UserID, sess.TokenHash, sess.CreatedAt.Format(store.TimestampLayout),
		tsPtr(sess.RevokedAt), membership)
	return err
}

func (s *Store) ListConsoleSessions(ctx context.Context) ([]*store.ConsoleSession, error) {
	ctx, cancel := s.opCtx(ctx)
	defer cancel()
	rows, err := s.reader.QueryContext(ctx, `SELECT id,user_id,token_hash,created_at,revoked_at,membership FROM console_sessions ORDER BY created_at ASC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []*store.ConsoleSession
	for rows.Next() {
		sess, err := scanConsoleSession(rows.Scan)
		if err != nil {
			return nil, err
		}
		out = append(out, sess)
	}
	return out, rows.Err()
}

// RevokeConsoleSession stamps the server-side revocation (KB-325) on the
// ledger row. Idempotent: unknown ids and already-revoked sessions are
// touched again with the FIRST stamp winning (COALESCE keeps the original
// stamp on a re-logout, so the ledger records one revocation instant).
func (s *Store) RevokeConsoleSession(ctx context.Context, sessionID string, at time.Time) error {
	ctx, cancel := s.opCtx(ctx)
	defer cancel()
	_, err := s.db.ExecContext(ctx,
		`UPDATE console_sessions SET revoked_at = COALESCE(revoked_at, ?) WHERE id = ?`,
		at.Format(store.TimestampLayout), sessionID)
	return err
}

// tsPtr renders an optional timestamp as a driver value: nil stays SQL NULL
// so absent stamps (the session ledger's revocation column) stay NULL
// instead of a zero-text value.
func tsPtr(t *time.Time) any {
	if t == nil {
		return nil
	}
	return t.Format(store.TimestampLayout)
}

// scanConsoleSession scans one console_sessions row (the full ledger
// projection: created/revoked stamps + issue-time membership snapshot).
func scanConsoleSession(scan func(...any) error) (*store.ConsoleSession, error) {
	var sess store.ConsoleSession
	var created string
	var revoked sql.NullString
	var membership []byte
	err := scan(&sess.ID, &sess.UserID, &sess.TokenHash, &created, &revoked, &membership)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, store.ErrNotFound
		}
		return nil, err
	}
	sess.CreatedAt, _ = time.Parse(store.TimestampLayout, created)
	if revoked.Valid {
		t, _ := time.Parse(store.TimestampLayout, revoked.String)
		sess.RevokedAt = &t
	}
	sess.Membership = store.UnmarshalMembership(string(membership))
	return &sess, nil
}

func (s *Store) DeleteConsoleSessions(ctx context.Context) error {
	ctx, cancel := s.opCtx(ctx)
	defer cancel()
	_, err := s.db.ExecContext(ctx, `DELETE FROM console_sessions`)
	return err
}

// DeleteConsoleSessionsByUser revokes every session of one console user,
// e.g. when the seeded password is rotated (CRI-195).
func (s *Store) DeleteConsoleSessionsByUser(ctx context.Context, userID string) error {
	ctx, cancel := s.opCtx(ctx)
	defer cancel()
	_, err := s.db.ExecContext(ctx, `DELETE FROM console_sessions WHERE user_id = ?`, userID)
	return err
}

// runColumns is the projection scanned by run row readers; keep in sync with
// scanRun and the migration that last altered the runs table.
const runColumns = "id,overseer_id,workflow_name,workflow_hcl,status,current_step,last_seq,created_at,started_at,ended_at,variable_scope,pending_signal,paused_at,ticket,repo_url,pr_url,failure_reason,agent_identity,project_id"

func (s *Store) CreateRun(ctx context.Context, r *store.Run) error {
	ctx, cancel := s.opCtx(ctx)
	defer cancel()
	var started any
	if r.StartedAt != nil {
		started = r.StartedAt.Format(store.TimestampLayout)
	}
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO runs(id,overseer_id,workflow_name,workflow_hcl,status,current_step,last_seq,created_at,started_at,ticket,repo_url,pr_url,agent_identity,project_id) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		r.ID, r.OverseerID, r.WorkflowName, r.WorkflowHCL, r.Status, r.CurrentStep, r.LastSeq, r.CreatedAt.Format(store.TimestampLayout),
		started, nonEmpty(r.Ticket), nonEmpty(r.RepoURL), nonEmpty(r.PRURL), r.AgentIdentity, nullText(r.ProjectID))
	return err
}

func (s *Store) GetRun(ctx context.Context, id string) (*store.Run, error) {
	ctx, cancel := s.opCtx(ctx)
	defer cancel()
	row := s.db.QueryRowContext(ctx, `SELECT `+runColumns+` FROM runs WHERE id=?`, id)
	return scanRun(row.Scan)
}

// ListRuns pages runs newest-first. limit > 0 fetches at most limit rows and
// returns a continuation token whenever a further row exists (one extra row
// is probed so an exactly-full final page does not hand out a dead token);
// limit <= 0 returns every matching row with no token. The cursor predicate
// mirrors the ORDER BY, so a token stays valid across intervening inserts.
func (s *Store) ListRuns(ctx context.Context, overseerID, status string, limit int, pageToken string) ([]*store.Run, string, error) {
	ctx, cancel := s.opCtx(ctx)
	defer cancel()
	q := `SELECT ` + runColumns + ` FROM runs WHERE 1=1`
	args := []any{}
	if overseerID != "" {
		q += ` AND overseer_id=?`
		args = append(args, overseerID)
	}
	if status != "" {
		q += ` AND status=?`
		args = append(args, status)
	}
	cursorCreated, cursorID, err := store.DecodeRunCursor(pageToken)
	if err != nil {
		return nil, "", err
	}
	if cursorCreated != "" {
		q += ` AND (created_at < ? OR (created_at = ? AND id < ?))`
		args = append(args, cursorCreated, cursorCreated, cursorID)
	}
	q += ` ORDER BY created_at DESC, id DESC`
	if limit > 0 {
		// Probe one row past the page so the token is only issued when a
		// next page actually exists.
		q += ` LIMIT ?`
		args = append(args, limit+1)
	}
	rows, err := s.reader.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, "", err
	}
	defer rows.Close()
	var out []*store.Run
	for rows.Next() {
		r, err := scanRun(rows.Scan)
		if err != nil {
			return nil, "", err
		}
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		return nil, "", err
	}
	var next string
	if limit > 0 && len(out) > limit {
		last := out[limit-1]
		// Format reproduces the stored text exactly (RFC3339Nano round-trips
		// through parse), so the cursor key matches the ORDER BY values.
		next = store.EncodeRunCursor(last.CreatedAt.Format(store.TimestampLayout), last.ID)
		out = out[:limit]
	}
	return out, next, nil
}

// runVisibilityScope is the read-visibility predicate for criteria callers
// (KB-324 S1): the caller sees runs it leases (overseer_id), runs it owns via
// a minting assignment, and runs belonging to a project whose member set
// contains it — the same project_members EXISTS clause the assignment lease
// scan consults. Unscoped runs (project_id NULL) are never returned here;
// they stay console-operator-visible.
func (s *Store) ListRunsVisibleTo(ctx context.Context, criteriaID, overseerID, status string, limit int, pageToken string) ([]*store.Run, string, error) {
	ctx, cancel := s.opCtx(ctx)
	defer cancel()
	q := `SELECT ` + runColumns + ` FROM runs WHERE (
		overseer_id = ?
		OR EXISTS (SELECT 1 FROM workflow_assignments wa
		           WHERE wa.run_id = runs.id AND wa.owner_criteria_id = ?)
		OR EXISTS (SELECT 1 FROM project_members pm
		           WHERE pm.project_id = runs.project_id AND pm.member_id = ?)
	)`
	args := []any{criteriaID, criteriaID, criteriaID}
	if overseerID != "" {
		q += ` AND overseer_id=?`
		args = append(args, overseerID)
	}
	if status != "" {
		q += ` AND status=?`
		args = append(args, status)
	}
	cursorCreated, cursorID, err := store.DecodeRunCursor(pageToken)
	if err != nil {
		return nil, "", err
	}
	if cursorCreated != "" {
		q += ` AND (created_at < ? OR (created_at = ? AND id < ?))`
		args = append(args, cursorCreated, cursorCreated, cursorID)
	}
	q += ` ORDER BY created_at DESC, id DESC`
	if limit > 0 {
		q += ` LIMIT ?`
		args = append(args, limit+1)
	}
	rows, err := s.reader.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, "", err
	}
	defer rows.Close()
	var out []*store.Run
	for rows.Next() {
		r, err := scanRun(rows.Scan)
		if err != nil {
			return nil, "", err
		}
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		return nil, "", err
	}
	var next string
	if limit > 0 && len(out) > limit {
		last := out[limit-1]
		next = store.EncodeRunCursor(last.CreatedAt.Format(store.TimestampLayout), last.ID)
		out = out[:limit]
	}
	return out, next, nil
}

func (s *Store) IsRunVisibleTo(ctx context.Context, runID, criteriaID string) (bool, error) {
	ctx, cancel := s.opCtx(ctx)
	defer cancel()
	var one any
	err := s.db.QueryRowContext(ctx,
		`SELECT 1 FROM runs WHERE id = ? AND (
		overseer_id = ?
		OR EXISTS (SELECT 1 FROM workflow_assignments wa
		           WHERE wa.run_id = runs.id AND wa.owner_criteria_id = ?)
		OR EXISTS (SELECT 1 FROM project_members pm
		           WHERE pm.project_id = runs.project_id AND pm.member_id = ?)
		)
		LIMIT 1`,
		runID, criteriaID, criteriaID, criteriaID).Scan(&one)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return false, nil
		}
		return false, err
	}
	return true, nil
}

func scanRun(scan func(...any) error) (*store.Run, error) {
	var r store.Run
	var created string
	var overseerID sql.NullString
	var started sql.NullString
	var ended sql.NullString
	var variableScope sql.NullString
	var pendingSignal sql.NullString
	var pausedAt sql.NullString
	var ticket sql.NullString
	var repoURL sql.NullString
	var prURL sql.NullString
	var failureReason sql.NullString
	var projectID sql.NullString
	err := scan(&r.ID, &overseerID, &r.WorkflowName, &r.WorkflowHCL, &r.Status, &r.CurrentStep, &r.LastSeq, &created, &started, &ended, &variableScope, &pendingSignal, &pausedAt, &ticket, &repoURL, &prURL, &failureReason, &r.AgentIdentity, &projectID)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, store.ErrNotFound
		}
		return nil, err
	}
	if overseerID.Valid {
		r.OverseerID = overseerID.String
	}
	r.CreatedAt, _ = time.Parse(store.TimestampLayout, created)
	if started.Valid {
		t, _ := time.Parse(store.TimestampLayout, started.String)
		r.StartedAt = &t
	}
	if ended.Valid {
		t, _ := time.Parse(store.TimestampLayout, ended.String)
		r.EndedAt = &t
	}
	if variableScope.Valid {
		r.VariableScope = variableScope.String
	}
	if pendingSignal.Valid {
		r.PendingSignal = pendingSignal.String
	}
	if pausedAt.Valid {
		t, _ := time.Parse(store.TimestampLayout, pausedAt.String)
		r.PausedAt = &t
	}
	if ticket.Valid {
		r.Ticket = ticket.String
	}
	if repoURL.Valid {
		r.RepoURL = repoURL.String
	}
	if prURL.Valid {
		r.PRURL = prURL.String
	}
	if failureReason.Valid {
		r.FailureReason = failureReason.String
	}
	if projectID.Valid {
		r.ProjectID = projectID.String
	}
	return &r, nil
}

func (s *Store) UpdateRun(ctx context.Context, r *store.Run) error {
	ctx, cancel := s.opCtx(ctx)
	defer cancel()
	var ended any
	if r.EndedAt != nil {
		ended = r.EndedAt.Format(store.TimestampLayout)
	}
	var started any
	if r.StartedAt != nil {
		started = r.StartedAt.Format(store.TimestampLayout)
	}
	_, err := s.db.ExecContext(ctx,
		`UPDATE runs SET status=?, current_step=?, last_seq=?, ended_at=?, started_at=COALESCE(started_at, ?), failure_reason=COALESCE(NULLIF(?, ''), failure_reason) WHERE id=?`,
		r.Status, r.CurrentStep, r.LastSeq, ended, started, nonEmpty(r.FailureReason), r.ID)
	return err
}

// SetRunMetadata promotes non-empty metadata values (ticket, repo_url, pr_url)
// onto the run record (CRI-131). Empty values leave the existing column
// untouched so partial updates from later run.metadata events never clear
// earlier metadata. Unknown run ids are a no-op.
func (s *Store) SetRunMetadata(ctx context.Context, runID, ticket, repoURL, prURL string) error {
	ctx, cancel := s.opCtx(ctx)
	defer cancel()
	_, err := s.db.ExecContext(ctx,
		`UPDATE runs SET ticket=COALESCE(NULLIF(?, ''), ticket), repo_url=COALESCE(NULLIF(?, ''), repo_url), pr_url=COALESCE(NULLIF(?, ''), pr_url) WHERE id=?`,
		ticket, repoURL, prURL, runID)
	return err
}

// nonEmpty maps an empty string to SQL NULL so absent run metadata stays NULL
// rather than an empty-text value (CRI-131).
func nonEmpty(s string) any {
	if s == "" {
		return nil
	}
	return s
}

// SetRunVariableScope persists a JSON-encoded variable scope snapshot (W04).
func (s *Store) SetRunVariableScope(ctx context.Context, runID, scope string) error {
	ctx, cancel := s.opCtx(ctx)
	defer cancel()
	_, err := s.db.ExecContext(ctx,
		`UPDATE runs SET variable_scope=? WHERE id=?`, scope, runID)
	return err
}

// GetRunVariableScope returns the stored variable scope JSON for runID.
// Returns ("", nil) when the run exists but has no scope yet (NULL column).
func (s *Store) GetRunVariableScope(ctx context.Context, runID string) (string, error) {
	ctx, cancel := s.opCtx(ctx)
	defer cancel()
	var scope sql.NullString
	err := s.db.QueryRowContext(ctx, `SELECT variable_scope FROM runs WHERE id=?`, runID).Scan(&scope)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return "", store.ErrNotFound
		}
		return "", err
	}
	if !scope.Valid {
		return "", nil
	}
	return scope.String, nil
}

// SetRunPaused marks a run as paused with the given pending signal (W05).
// Guarded against terminal and stopped runs (CRI-207): a late WaitEntered or
// event-driven pause on a run an operator already parked must not flip it out
// of stopped, and terminal runs are never rewritten.
func (s *Store) SetRunPaused(ctx context.Context, runID, pendingSignal string, pausedAt time.Time) error {
	ctx, cancel := s.opCtx(ctx)
	defer cancel()
	_, err := s.db.ExecContext(ctx,
		`UPDATE runs SET status='paused', pending_signal=?, paused_at=? WHERE id=? AND status NOT IN ('succeeded', 'failed', 'cancelled', 'stopped')`,
		pendingSignal, pausedAt.Format(store.TimestampLayout), runID)
	return err
}

// ClearRunPaused clears the pause state and sets status back to running (W05).
// Guarded to paused runs only (CRI-197): a terminal run's pause state is stale
// history, and a late resume must not resurrect it.
func (s *Store) ClearRunPaused(ctx context.Context, runID string) error {
	ctx, cancel := s.opCtx(ctx)
	defer cancel()
	_, err := s.db.ExecContext(ctx,
		`UPDATE runs SET status='running', pending_signal=NULL, paused_at=NULL WHERE id=? AND status='paused'`, runID)
	return err
}

// SetRunStopped parks the run in status stopped (CRI-207): the operator
// requested a stop, and stop is resumable, so the run keeps its id and is not
// terminal — ended_at is untouched. Pause state is dropped: a parked run is
// neither paused nor waiting on a signal. Guarded like CancelRun so terminal
// runs are never rewritten.
func (s *Store) SetRunStopped(ctx context.Context, runID string) error {
	ctx, cancel := s.opCtx(ctx)
	defer cancel()
	_, err := s.db.ExecContext(ctx,
		`UPDATE runs SET status='stopped', pending_signal=NULL, paused_at=NULL WHERE id=? AND status NOT IN ('succeeded', 'failed', 'cancelled')`,
		runID)
	return err
}

// ClearRunStopped moves a stopped run back to running (the resume path,
// CRI-207): the run keeps its identity and checkpoint ownership (criteria is
// checkpoint-owner; castle only points at it). Guarded to stopped runs so the
// operator resume can never clobber a concurrent transition or a terminal run.
func (s *Store) ClearRunStopped(ctx context.Context, runID string) error {
	ctx, cancel := s.opCtx(ctx)
	defer cancel()
	_, err := s.db.ExecContext(ctx,
		`UPDATE runs SET status='running', pending_signal=NULL, paused_at=NULL WHERE id=? AND status='stopped'`, runID)
	return err
}

// MarkRunUnstarted returns a never-started stopped run to the leasable pending
// bucket (CRI-207 resume path): a stopped run whose assignment was leased but
// whose delivery never landed (started_at is NULL) is re-queued as pending so
// the dispatch redelivery scan and the lease-expiry scan — both of which apply
// only to pending runs — can deliver its work again; a resumed record left as
// status running with started_at NULL would qualify for neither and sit
// undelivered forever. started_at is explicitly kept NULL ("unstarted").
//
// requeuedAt refreshes created_at (KB-233 rule 2): the operator-issued re-queue
// is a fresh pending incarnation, and the created_never_started reaper derives
// its window from created_at, so the resumed run must get a fresh window —
// otherwise the next reaper tick would fail a run the operator just resumed
// before the redelivery can land. A re-queued run that still cannot be started
// within that window correctly dies as created_never_started. One conditional
// UPDATE guarded to status='stopped', the same pattern as the pause and
// stopped guards, so it cannot race a competing transition, rewrite a terminal
// or paused run, or resurrect a run that left the parked state.
func (s *Store) MarkRunUnstarted(ctx context.Context, runID string, requeuedAt time.Time) error {
	ctx, cancel := s.opCtx(ctx)
	defer cancel()
	_, err := s.db.ExecContext(ctx,
		`UPDATE runs SET status='pending', started_at=NULL, created_at=? WHERE id=? AND status='stopped'`,
		requeuedAt.Format(store.TimestampLayout), runID)
	return err
}

func (s *Store) ReapStaleAgentRuns(ctx context.Context, now time.Time, staleBefore time.Time) ([]store.RunReapOutcome, error) {
	ctx, cancel := s.opCtx(ctx)
	defer cancel()
	outcomes, err := retryOnTransient(ctx, reapRetryPolicy, func() ([]store.RunReapOutcome, error) {
		return s.reapStaleAgentRunsAttempt(ctx, now, staleBefore)
	})
	if isInterrupt(err) {
		s.recoverWriter(ctx)
	}
	return outcomes, err
}

// recoverWriter forces a health round-trip on the serialized writer pool
// (KB-222). During the 2026-09-21 incident retries alone surfaced
// interrupted (9) for the entire outage window: with every writer sharing one
// pooled connection, only a connection reset clears driver-level corruption,
// and Ping makes database/sql discard any connection the driver judges
// unusable and build a fresh one. Best-effort by design — correctness does
// not depend on it: opCtx keeps stream cancellation from ever reaching
// sqlite3_interrupt, and the driver additionally clears stray interrupt flags
// at statement prepare. Bounded so a wedged pool fails the reaper tick
// instead of pinning it.
func (s *Store) recoverWriter(ctx context.Context) {
	pingCtx, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	_ = s.db.PingContext(pingCtx)
}

// reapStaleAgentRunsAttempt performs one reaping pass. The candidate scan is a
// plain read on the reader connection: with nothing to reap — the common case —
// the writer connection is not touched at all.
func (s *Store) reapStaleAgentRunsAttempt(ctx context.Context, now time.Time, staleBefore time.Time) ([]store.RunReapOutcome, error) {
	// Bound the pass so a wedged writer connection fails the tick instead of
	// pinning the reaper goroutine (CRI-143). Each retry attempt gets a fresh
	// budget.
	ctx, cancel := context.WithTimeout(ctx, reapAttemptTimeout)
	defer cancel()

	// Stopped runs are deliberately absent from the scan (CRI-207): a run an
	// operator parked as stopped has no live agent heartbeat by design and is
	// never reaped regardless of how old its agent's last heartbeat is.
	// Reaper semantics for genuinely dead agents are unchanged.
	ids, err := reap.StaleCandidates(ctx, s.reader, reap.SQLite, staleBefore, nil)
	if err != nil {
		return nil, err
	}
	if len(ids) == 0 {
		return nil, nil
	}

	return s.reapRunIDs(ctx, now, staleBefore, ids, store.ReapReasonAgentHeartbeatLost)
}

// reapRunIDs stamps the given candidates on the serialized writer pool. The
// candidates were picked from a reader snapshot that may already be stale, so
// the transaction re-validates staleness and reapability against current data
// (the same shared candidate predicate, scoped to an IN-list): runs whose
// agent heartbeated or reached a terminal state in between are never
// clobbered (CRI-142 terminal invariants hold even under the scan-then-write
// split). Classification and stamping are delegated to the shared verdict
// stage (KB-226), which reads the candidate's own record inside this
// transaction.
func (s *Store) reapRunIDs(ctx context.Context, now time.Time, staleBefore time.Time, candidates []string, reapReason string) ([]store.RunReapOutcome, error) {
	// Nothing to reap: return before opening the transaction. The candidate
	// placeholder list cannot even be derived from zero ids, so an empty
	// candidate list must not reach the query builders below.
	if len(candidates) == 0 {
		return nil, nil
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback() }()

	// Re-validate inside the write transaction against the same staleness
	// criteria as the scan: only still-active runs of agents whose heartbeat
	// is still older than staleBefore survive into the UPDATE. Stopped runs
	// are reaper-exempt (CRI-207) and never enter this scan either.
	ids, err := reap.StaleCandidates(ctx, tx, reap.SQLite, staleBefore, candidates)
	if err != nil {
		return nil, err
	}
	if len(ids) == 0 {
		_ = tx.Rollback()
		return nil, nil
	}

	outcomes, err := reap.StampVerdicts(ctx, tx, reap.SQLite, now, reapReason, ids)
	if err != nil {
		return nil, err
	}
	return outcomes, tx.Commit()
}

// ReapNeverStartedRuns stamps runs in status pending or running as failed with
// reason "created_never_started" (KB-233 rule 2) when started_at is still NULL
// and created_at is older than createdBefore: a run record that was minted but
// whose execution never began — an orphan-job run minted after a card closed,
// a queued assignment nobody ever leased, or a lease that claimed its run and
// then died before RunStarted. Unlike the CRI-142 heartbeat reaper, the
// verdict is derivable from the run record alone; no heartbeat or assignment
// heuristics participate. Paused and stopped runs (operator-parked, CRI-207)
// are reaper-exempt regardless of age, and terminal runs are never rewritten.
// A never-started candidate whose record ends at a human gate is parked
// instead of failed (KB-226), with the same reclassification semantics as
// ReapStaleAgentRuns: execution demonstrably ran, so only the status stamp was
// lost. Each reaped run's workflow assignment is marked terminal so the dead
// work is never re-dispatched. Returns one RunReapOutcome per stamped run.
//
// Like ReapStaleAgentRuns (CRI-143), the scan runs on the dedicated reader
// connection and the writes go through one short transaction on the serialized
// writer pool; transient sqlite faults are retried with backoff (retry.go).
func (s *Store) ReapNeverStartedRuns(ctx context.Context, now time.Time, createdBefore time.Time) ([]store.RunReapOutcome, error) {
	ctx, cancel := s.opCtx(ctx)
	defer cancel()
	outcomes, err := retryOnTransient(ctx, reapRetryPolicy, func() ([]store.RunReapOutcome, error) {
		return s.reapNeverStartedRunsAttempt(ctx, now, createdBefore)
	})
	if isInterrupt(err) {
		s.recoverWriter(ctx)
	}
	return outcomes, err
}

// reapNeverStartedRunsAttempt performs one rule-2 reaping pass. The candidate
// scan is a plain read on the reader connection: with nothing to reap — the
// common case — the writer connection is not touched at all.
func (s *Store) reapNeverStartedRunsAttempt(ctx context.Context, now time.Time, createdBefore time.Time) ([]store.RunReapOutcome, error) {
	// Bound the pass so a wedged writer connection fails the tick instead of
	// pinning the reaper goroutine (CRI-143). Each retry attempt gets a fresh
	// budget.
	ctx, cancel := context.WithTimeout(ctx, reapAttemptTimeout)
	defer cancel()

	ids, err := reap.NeverStartedCandidates(ctx, s.reader, reap.SQLite, createdBefore, nil)
	if err != nil {
		return nil, err
	}
	if len(ids) == 0 {
		return nil, nil
	}

	return s.reapNeverStartedRunIDs(ctx, now, createdBefore, ids)
}

// reapNeverStartedRunIDs stamps the rule-2 candidates on the serialized writer
// pool. The candidates were picked from a reader snapshot that may already be
// stale, so the transaction re-validates never-started reapability against
// current data: runs that reached RunStarted or a terminal state between the
// scan and the write are never clobbered. Classification and stamping are
// delegated to the shared verdict stage (KB-226), which reads the candidate's
// own record inside this transaction.
func (s *Store) reapNeverStartedRunIDs(ctx context.Context, now time.Time, createdBefore time.Time, candidates []string) ([]store.RunReapOutcome, error) {
	// Nothing to reap: return before opening the transaction. An empty
	// candidate list must not reach the query builders below.
	if len(candidates) == 0 {
		return nil, nil
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback() }()

	// Re-validate inside the write transaction against the same criteria as
	// the scan: only still-unstarted, still-active runs older than
	// createdBefore survive into the UPDATE.
	ids, err := reap.NeverStartedCandidates(ctx, tx, reap.SQLite, createdBefore, candidates)
	if err != nil {
		return nil, err
	}
	if len(ids) == 0 {
		_ = tx.Rollback()
		return nil, nil
	}

	outcomes, err := reap.StampVerdicts(ctx, tx, reap.SQLite, now, reap.ReasonCreatedNeverStarted, ids)
	if err != nil {
		return nil, err
	}
	return outcomes, tx.Commit()
}

// CancelRun stamps runID terminal as "cancelled" with the given reason
// (CRI-142). The conditional update never rewrites an already terminal run,
// so it cannot race a concurrent agent SubmitEvents terminal stamp into a
// regression: whoever wins, the run stays terminal. Unknown run ids return
// ErrNotFound; a run already terminal returns ErrRunTerminal. The updated run
// record is returned on success.
func (s *Store) CancelRun(ctx context.Context, runID, reason string, now time.Time) (*store.Run, error) {
	ctx, cancel := s.opCtx(ctx)
	defer cancel()
	res, err := s.db.ExecContext(ctx,
		`UPDATE runs
		 SET status='cancelled', ended_at=?, failure_reason=COALESCE(NULLIF(?, ''), failure_reason)
		 WHERE id=? AND status NOT IN ('succeeded', 'failed', 'cancelled')`,
		now.Format(store.TimestampLayout), nonEmpty(reason), runID)
	if err != nil {
		return nil, err
	}
	n, err := res.RowsAffected()
	if err != nil {
		return nil, err
	}
	if n == 0 {
		if _, getErr := s.GetRun(ctx, runID); getErr != nil {
			return nil, getErr
		}
		return nil, store.ErrRunTerminal
	}
	return s.GetRun(ctx, runID)
}

func (s *Store) AppendEvent(ctx context.Context, ev *store.Event) (uint64, bool, error) {
	ctx, cancel := s.opCtx(ctx)
	defer cancel()
	if ev == nil {
		return 0, false, errors.New("nil event")
	}
	if ev.RunID == "" {
		return 0, false, errors.New("run_id required")
	}
	if ev.Type == "" {
		return 0, false, errors.New("event type required")
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, false, err
	}
	defer tx.Rollback() //nolint:errcheck

	// Idempotency on (run_id, correlation_id): if an event with this
	// correlation id already exists for this run, return its seq without
	// inserting again. Empty correlation ids skip this check.
	if ev.CorrelationID != "" {
		var existing sql.NullInt64
		if err := tx.QueryRowContext(ctx,
			`SELECT seq FROM events WHERE run_id=? AND correlation_id=? LIMIT 1`,
			ev.RunID, ev.CorrelationID).Scan(&existing); err != nil && !errors.Is(err, sql.ErrNoRows) {
			return 0, false, err
		}
		if existing.Valid {
			// No commit necessary (read-only); release the tx.
			_ = tx.Rollback()
			seq := uint64(existing.Int64)
			ev.Seq = seq
			return seq, false, nil
		}
	}

	var lastSeq sql.NullInt64
	if err := tx.QueryRowContext(ctx, `SELECT MAX(seq) FROM events WHERE run_id=?`, ev.RunID).Scan(&lastSeq); err != nil {
		return 0, false, err
	}
	next := uint64(1)
	if lastSeq.Valid {
		next = uint64(lastSeq.Int64) + 1
	}
	ts := ev.Ts
	if ts.IsZero() {
		ts = time.Now().UTC()
	}
	// Normalize the payload the same way the pg driver does before its
	// JSONB write: every NUL becomes U+FFFD on both engines, so the decoded
	// payload stays equal across them (JSONB cannot hold U+0000; see
	// store.Event.Payload).
	_, err = tx.ExecContext(ctx,
		`INSERT INTO events(run_id,seq,type,ts,correlation_id,payload) VALUES(?,?,?,?,?,?)`,
		ev.RunID, next, ev.Type, ts.Format(store.TimestampLayout), ev.CorrelationID,
		string(store.NormalizeEventPayload(ev.Payload)))
	if err != nil {
		return 0, false, err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE runs SET last_seq=? WHERE id=? AND last_seq < ?`, next, ev.RunID, next); err != nil {
		return 0, false, err
	}
	if err := tx.Commit(); err != nil {
		return 0, false, err
	}
	ev.Seq = next
	ev.Ts = ts
	return next, true, nil
}

func (s *Store) ListEvents(ctx context.Context, runID string, since uint64, limit int) ([]*store.Event, error) {
	ctx, cancel := s.opCtx(ctx)
	defer cancel()
	normalized, err := store.NormalizeListLimit(limit)
	if err != nil {
		return nil, err
	}
	rows, err := s.reader.QueryContext(ctx,
		`SELECT seq,type,ts,correlation_id,payload FROM events WHERE run_id=? AND seq>? ORDER BY seq ASC LIMIT ?`,
		runID, since, normalized)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanEventRows(rows, runID)
}

func (s *Store) ListStepLogs(ctx context.Context, runID, step string, since uint64, limit int) ([]*store.Event, error) {
	ctx, cancel := s.opCtx(ctx)
	defer cancel()
	normalized, err := store.NormalizeListLimit(limit)
	if err != nil {
		return nil, err
	}
	rows, err := s.reader.QueryContext(ctx,
		`SELECT seq,type,ts,correlation_id,payload
		 FROM events
		 WHERE run_id=? AND seq>? AND type='step.log' AND json_extract(payload, '$.step')=?
		 ORDER BY seq ASC LIMIT ?`,
		runID, since, step, normalized)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	return scanEventRows(rows, runID)
}

func (s *Store) UpsertSubscriberCursor(ctx context.Context, subscriberID, runID string, lastSeq uint64) error {
	ctx, cancel := s.opCtx(ctx)
	defer cancel()
	if subscriberID == "" {
		return errors.New("subscriber_id required")
	}
	if runID == "" {
		return errors.New("run_id required")
	}

	_, err := s.db.ExecContext(ctx, `
		INSERT INTO run_subscriptions(subscriber_id, run_id, last_seq, updated_at)
		VALUES (?, ?, ?, CURRENT_TIMESTAMP)
		ON CONFLICT(subscriber_id, run_id) DO UPDATE SET
			last_seq = CASE
				WHEN excluded.last_seq > run_subscriptions.last_seq THEN excluded.last_seq
				ELSE run_subscriptions.last_seq
			END,
			updated_at = CASE
				WHEN excluded.last_seq > run_subscriptions.last_seq THEN CURRENT_TIMESTAMP
				ELSE run_subscriptions.updated_at
			END
	`, subscriberID, runID, lastSeq)
	return err
}

func (s *Store) GetSubscriberCursor(ctx context.Context, subscriberID, runID string) (uint64, bool, error) {
	ctx, cancel := s.opCtx(ctx)
	defer cancel()
	if subscriberID == "" {
		return 0, false, errors.New("subscriber_id required")
	}
	if runID == "" {
		return 0, false, errors.New("run_id required")
	}

	var seq uint64
	err := s.db.QueryRowContext(ctx, `SELECT last_seq FROM run_subscriptions WHERE subscriber_id=? AND run_id=?`, subscriberID, runID).Scan(&seq)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return 0, false, nil
		}
		return 0, false, err
	}
	return seq, true, nil
}

func (s *Store) RecordAttemptStart(ctx context.Context, ra *store.RunAttempt) error {
	ctx, cancel := s.opCtx(ctx)
	defer cancel()
	if ra.RunID == "" || ra.Step == "" || ra.Attempt <= 0 {
		return errors.New("run_id, step, and attempt > 0 required")
	}
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO run_attempts(run_id, step, attempt, started_at)
		VALUES (?, ?, ?, ?)
		ON CONFLICT(run_id, step, attempt) DO NOTHING`,
		ra.RunID, ra.Step, ra.Attempt, ra.StartedAt.UTC().Format(store.TimestampLayout))
	return err
}

func (s *Store) RecordAttemptComplete(ctx context.Context, runID, step string, attempt int, outcome string) error {
	ctx, cancel := s.opCtx(ctx)
	defer cancel()
	if runID == "" || step == "" || attempt <= 0 {
		return errors.New("run_id, step, and attempt > 0 required")
	}
	now := time.Now().UTC().Format(store.TimestampLayout)
	_, err := s.db.ExecContext(ctx, `
		UPDATE run_attempts SET completed_at=?, outcome=?
		WHERE run_id=? AND step=? AND attempt=?`,
		now, outcome, runID, step, attempt)
	return err
}

func (s *Store) GetLatestAttempt(ctx context.Context, runID, step string) (*store.RunAttempt, error) {
	ctx, cancel := s.opCtx(ctx)
	defer cancel()
	if runID == "" || step == "" {
		return nil, errors.New("run_id and step required")
	}
	row := s.db.QueryRowContext(ctx,
		`SELECT run_id, step, attempt, started_at, completed_at, outcome
		 FROM run_attempts WHERE run_id=? AND step=?
		 ORDER BY attempt DESC LIMIT 1`,
		runID, step)
	var ra store.RunAttempt
	var started string
	var completed sql.NullString
	var outcome sql.NullString
	err := row.Scan(&ra.RunID, &ra.Step, &ra.Attempt, &started, &completed, &outcome)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, store.ErrNotFound
		}
		return nil, err
	}
	ra.StartedAt, _ = time.Parse(store.TimestampLayout, started)
	if completed.Valid {
		t, _ := time.Parse(store.TimestampLayout, completed.String)
		ra.CompletedAt = &t
	}
	if outcome.Valid {
		ra.Outcome = outcome.String
	}
	return &ra, nil
}

func scanEvent(seq int64, typ, ts string, corr sql.NullString, payload string, runID string) *store.Event {
	ev := &store.Event{
		SchemaVersion: store.EventSchemaVersion,
		RunID:         runID,
		Seq:           uint64(seq),
		Type:          typ,
		Payload:       []byte(payload),
	}
	if parsed, err := time.Parse(store.TimestampLayout, ts); err == nil {
		ev.Ts = parsed
	}
	if corr.Valid {
		ev.CorrelationID = corr.String
	}
	return ev
}

func scanEventRows(rows *sql.Rows, runID string) ([]*store.Event, error) {
	var out []*store.Event
	for rows.Next() {
		var seq int64
		var ts string
		var payload string
		var corr sql.NullString
		var typ string
		if err := rows.Scan(&seq, &typ, &ts, &corr, &payload); err != nil {
			return nil, err
		}
		out = append(out, scanEvent(seq, typ, ts, corr, payload, runID))
	}
	return out, rows.Err()
}

func scanEventRow(row *sql.Row, runID string) (*store.Event, error) {
	var seq int64
	var ts string
	var payload string
	var corr sql.NullString
	var typ string
	if err := row.Scan(&seq, &typ, &ts, &corr, &payload); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, store.ErrNotFound
		}
		return nil, err
	}
	return scanEvent(seq, typ, ts, corr, payload, runID), nil
}

func (s *Store) GetLatestEvent(ctx context.Context, runID string) (*store.Event, error) {
	ctx, cancel := s.opCtx(ctx)
	defer cancel()
	row := s.db.QueryRowContext(ctx,
		`SELECT seq,type,ts,correlation_id,payload FROM events WHERE run_id=? ORDER BY seq DESC LIMIT 1`,
		runID)
	return scanEventRow(row, runID)
}

func (s *Store) GetLatestStepEnteredEvent(ctx context.Context, runID string) (*store.Event, error) {
	ctx, cancel := s.opCtx(ctx)
	defer cancel()
	row := s.db.QueryRowContext(ctx,
		`SELECT seq,type,ts,correlation_id,payload FROM events WHERE run_id=? AND type='step.entered' ORDER BY seq DESC LIMIT 1`,
		runID)
	return scanEventRow(row, runID)
}

func (s *Store) CreateWorkflowAssignment(ctx context.Context, a *store.WorkflowAssignment) (*store.WorkflowAssignment, bool, error) {
	ctx, cancel := s.opCtx(ctx)
	defer cancel()
	if a == nil {
		return nil, false, errors.New("nil assignment")
	}
	if a.OwnerCriteriaID == "" {
		return nil, false, errors.New("owner_criteria_id required")
	}
	if a.WorkflowName == "" {
		return nil, false, errors.New("workflow_name required")
	}
	if a.WorkflowSource == "" {
		return nil, false, errors.New("workflow_source required")
	}
	if a.IdempotencyKey == "" {
		return nil, false, errors.New("idempotency_key required")
	}
	if a.ID == "" {
		a.ID = uuid.NewString()
	}
	if a.RunID == "" {
		a.RunID = uuid.NewString()
	}
	if a.State == "" {
		a.State = store.WorkflowAssignmentStateQueued
	}
	now := time.Now().UTC()
	if a.CreatedAt.IsZero() {
		a.CreatedAt = now
	}
	if a.UpdatedAt.IsZero() {
		a.UpdatedAt = now
	}

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, false, err
	}
	defer tx.Rollback() //nolint:errcheck

	existing, err := scanAssignmentTx(ctx, tx, `
		SELECT `+assignmentColumns+`
		FROM workflow_assignments
		WHERE owner_criteria_id=? AND idempotency_key=?`, a.OwnerCriteriaID, a.IdempotencyKey)
	if err != nil && !errors.Is(err, store.ErrNotFound) {
		return nil, false, err
	}
	if existing != nil {
		_ = tx.Rollback()
		return existing, false, nil
	}

	if _, err := tx.ExecContext(ctx,
		`INSERT INTO runs(id, overseer_id, workflow_name, workflow_hcl, status, current_step, last_seq, created_at, project_id)
		 VALUES(?, NULL, ?, ?, 'pending', '', 0, ?, ?)`,
		a.RunID, a.WorkflowName, a.WorkflowSource, a.CreatedAt.Format(store.TimestampLayout), nullText(a.ProjectID)); err != nil {
		return nil, false, err
	}

	inputValues, err := store.MarshalLabels(a.InputValues)
	if err != nil {
		return nil, false, err
	}
	targetMetadata, err := store.MarshalLabels(a.TargetMetadata)
	if err != nil {
		return nil, false, err
	}
	if _, err := tx.ExecContext(ctx,
		`INSERT INTO workflow_assignments(id, owner_criteria_id, run_id, workflow_name, workflow_source, lockfile_source,
		                                   idempotency_key, state, created_at, updated_at, project_id, input_values, target_metadata)
		 VALUES(?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		a.ID, a.OwnerCriteriaID, a.RunID, a.WorkflowName, a.WorkflowSource, a.LockfileSource,
		a.IdempotencyKey, a.State, a.CreatedAt.Format(store.TimestampLayout), a.UpdatedAt.Format(store.TimestampLayout),
		nullText(a.ProjectID), inputValues, targetMetadata); err != nil {
		return nil, false, err
	}

	if err := s.insertAssignmentLabelsTx(ctx, tx, a.ID, a.Labels); err != nil {
		return nil, false, err
	}

	if err := tx.Commit(); err != nil {
		return nil, false, err
	}
	return a, true, nil
}

func (s *Store) insertAssignmentLabelsTx(ctx context.Context, tx *sql.Tx, assignmentID string, labels map[string]string) error {
	for k, v := range labels {
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO workflow_assignment_labels(assignment_id, key, value) VALUES(?, ?, ?)`,
			assignmentID, k, v); err != nil {
			return err
		}
	}
	return nil
}

// assignmentColumns is the projection scanned by single-row assignment
// readers; keep in sync with scanAssignment, scanAssignmentTx, and the
// migration that last altered the workflow_assignments table.
const assignmentColumns = `id, owner_criteria_id, run_id, workflow_name, workflow_source,
	lockfile_source, idempotency_key, state, terminal_reason, leased_criteria_id,
	lease_expires_at, created_at, updated_at, project_id, input_values, target_metadata,
	refire_root_run_id, refire_attempt`

func (s *Store) GetWorkflowAssignment(ctx context.Context, id string) (*store.WorkflowAssignment, error) {
	ctx, cancel := s.opCtx(ctx)
	defer cancel()
	if id == "" {
		return nil, errors.New("id required")
	}
	return s.scanAssignment(ctx, s.db.QueryRowContext(ctx, `
		SELECT `+assignmentColumns+` FROM workflow_assignments WHERE id=?`, id))
}

func (s *Store) GetWorkflowAssignmentByRunID(ctx context.Context, runID string) (*store.WorkflowAssignment, error) {
	ctx, cancel := s.opCtx(ctx)
	defer cancel()
	if runID == "" {
		return nil, errors.New("run_id required")
	}
	return s.scanAssignment(ctx, s.db.QueryRowContext(ctx, `
		SELECT `+assignmentColumns+` FROM workflow_assignments WHERE run_id=?`, runID))
}

// nullText maps an empty string to SQL NULL so an unscoped assignment
// satisfies the workflow_assignments.project_id foreign key; non-empty text
// (and the empty-text-equals-empty convention for plain nullable columns)
// passes through unchanged.
func nullText(s string) any {
	if s == "" {
		return nil
	}
	return s
}

func scanAssignmentMaps(projectID sql.NullString, inputValues, targetMetadata sql.NullString, a *store.WorkflowAssignment) {
	if projectID.Valid {
		a.ProjectID = projectID.String
	}
	a.InputValues = store.UnmarshalLabels(inputValues.String)
	a.TargetMetadata = store.UnmarshalLabels(targetMetadata.String)
}

func (s *Store) scanAssignment(ctx context.Context, row *sql.Row) (*store.WorkflowAssignment, error) {
	var a store.WorkflowAssignment
	var created, updated string
	var lockfile, terminalReason, leasedID, expires sql.NullString
	var projectID, inputValues, targetMetadata sql.NullString
	err := row.Scan(
		&a.ID, &a.OwnerCriteriaID, &a.RunID, &a.WorkflowName, &a.WorkflowSource, &lockfile,
		&a.IdempotencyKey, &a.State, &terminalReason, &leasedID, &expires,
		&created, &updated, &projectID, &inputValues, &targetMetadata,
		&a.RefireRootRunID, &a.RefireAttempt)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, store.ErrNotFound
		}
		return nil, err
	}
	a.CreatedAt, _ = time.Parse(store.TimestampLayout, created)
	a.UpdatedAt, _ = time.Parse(store.TimestampLayout, updated)
	if lockfile.Valid {
		a.LockfileSource = lockfile.String
	}
	if terminalReason.Valid {
		a.TerminalReason = terminalReason.String
	}
	if leasedID.Valid {
		a.LeasedCriteriaID = leasedID.String
	}
	if expires.Valid {
		t, _ := time.Parse(store.TimestampLayout, expires.String)
		a.LeaseExpiresAt = &t
	}
	scanAssignmentMaps(projectID, inputValues, targetMetadata, &a)
	labels, err := s.loadAssignmentLabels(ctx, a.ID)
	if err != nil {
		return nil, err
	}
	a.Labels = labels
	return &a, nil
}

func scanAssignmentTx(ctx context.Context, tx *sql.Tx, query string, args ...any) (*store.WorkflowAssignment, error) {
	var a store.WorkflowAssignment
	var created, updated string
	var lockfile, terminalReason, leasedID, expires sql.NullString
	var projectID, inputValues, targetMetadata sql.NullString
	row := tx.QueryRowContext(ctx, query, args...)
	err := row.Scan(
		&a.ID, &a.OwnerCriteriaID, &a.RunID, &a.WorkflowName, &a.WorkflowSource, &lockfile,
		&a.IdempotencyKey, &a.State, &terminalReason, &leasedID, &expires,
		&created, &updated, &projectID, &inputValues, &targetMetadata,
		&a.RefireRootRunID, &a.RefireAttempt)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, store.ErrNotFound
		}
		return nil, err
	}
	a.CreatedAt, _ = time.Parse(store.TimestampLayout, created)
	a.UpdatedAt, _ = time.Parse(store.TimestampLayout, updated)
	if lockfile.Valid {
		a.LockfileSource = lockfile.String
	}
	if terminalReason.Valid {
		a.TerminalReason = terminalReason.String
	}
	if leasedID.Valid {
		a.LeasedCriteriaID = leasedID.String
	}
	if expires.Valid {
		t, _ := time.Parse(store.TimestampLayout, expires.String)
		a.LeaseExpiresAt = &t
	}
	scanAssignmentMaps(projectID, inputValues, targetMetadata, &a)
	return &a, nil
}

func (s *Store) loadAssignmentLabels(ctx context.Context, assignmentID string) (map[string]string, error) {
	rows, err := s.reader.QueryContext(ctx,
		`SELECT key, value FROM workflow_assignment_labels WHERE assignment_id=?`, assignmentID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	m := make(map[string]string)
	for rows.Next() {
		var k, v string
		if err := rows.Scan(&k, &v); err != nil {
			return nil, err
		}
		m[k] = v
	}
	return m, rows.Err()
}

// LeaseWorkflowAssignment atomically expires stale leases, finds a queued
// assignment whose required labels are satisfied by agentLabels, and leases it
// to criteriaID. The lease is written to workflow_assignment_leases and the
// first attempt is recorded in workflow_assignment_attempts. The associated
// run's overseer_id is updated to criteriaID.
func (s *Store) LeaseWorkflowAssignment(ctx context.Context, criteriaID string, agentLabels map[string]string, now time.Time, leaseDuration time.Duration) (*store.WorkflowAssignment, error) {
	ctx, cancel := s.opCtx(ctx)
	defer cancel()
	if criteriaID == "" {
		return nil, errors.New("criteria_id required")
	}
	expiresAt := now.Add(leaseDuration)

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback() //nolint:errcheck

	// Transactionally expire stale leases for unstarted runs back to a leasable
	// queued state. Runs that have already emitted RunStarted (status='running')
	// are intentionally left leased so they can reattach without being requeued.
	if _, err := tx.ExecContext(ctx,
		`UPDATE workflow_assignments
		 SET state=?, leased_criteria_id=NULL, lease_expires_at=NULL, updated_at=?
		 WHERE state=? AND lease_expires_at < ? AND run_id IN (
		     SELECT id FROM runs WHERE status='pending'
		 )`,
		store.WorkflowAssignmentStateQueued, now.Format(store.TimestampLayout),
		store.WorkflowAssignmentStateLeased, now.Format(store.TimestampLayout)); err != nil {
		return nil, err
	}
	// Clear the stale overseer_id for any runs whose lease just expired.
	if _, err := tx.ExecContext(ctx,
		`UPDATE runs
		 SET overseer_id=NULL
		 WHERE id IN (
		     SELECT run_id FROM workflow_assignments
		     WHERE state=? AND lease_expires_at IS NULL AND run_id IN (
		         SELECT id FROM runs WHERE status='pending'
		     )
		 )`,
		store.WorkflowAssignmentStateQueued); err != nil {
		return nil, err
	}

	// Verify the agent is online and read its identity block for the
	// exact-build stamp on the leased run (KB-318).
	var status, identity string
	if err := tx.QueryRowContext(ctx, `SELECT status, COALESCE(identity, '') FROM overseers WHERE id=?`, criteriaID).Scan(&status, &identity); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, store.ErrNotFound
		}
		return nil, err
	}
	if status != "online" {
		return nil, store.ErrNotFound
	}

	// Ensure the agent does not already hold an unstarted lease. This enforces
	// sequential execution: an agent receives the next assignment only after it
	// has accepted (RunStarted) the current one.
	var hasPendingLease int
	if err := tx.QueryRowContext(ctx, `
		SELECT 1 FROM workflow_assignments a
		JOIN runs r ON r.id = a.run_id
		WHERE a.leased_criteria_id=? AND a.state=? AND r.status='pending'
		LIMIT 1`, criteriaID, store.WorkflowAssignmentStateLeased).Scan(&hasPendingLease); err != nil && !errors.Is(err, sql.ErrNoRows) {
		return nil, err
	}
	if hasPendingLease == 1 {
		return nil, store.ErrNotFound
	}

	// Find queued assignments and their required labels, oldest first. Only
	// work whose run is still executable is eligible (CRI-207): the dispatch
	// path must never lease an assignment whose run the operator parked as
	// stopped, or whose run already reached a terminal state — only an
	// explicit ResumeRun moves a stopped run back to running (and back into
	// this scan), so parked work is delivered only through resume. Runs
	// parked at a human gate (awaiting_human, KB-226) are not executable
	// work either: the engine already completed them for a human decision,
	// so re-dispatching would re-run finished work.
	//
	// Project scoping (KB-318) is enforced here, at the store layer: the
	// scan only offers an assignment carrying a project_id to an agent in
	// that project's member set. Unscoped assignments (project_id IS NULL)
	// stay visible to every agent.
	rows, err := tx.QueryContext(ctx, `
		SELECT a.id, a.run_id, a.workflow_name, a.workflow_source, a.lockfile_source,
		       a.idempotency_key, a.created_at, a.updated_at, a.project_id, l.key, l.value
		FROM workflow_assignments a
		JOIN runs r ON r.id = a.run_id
		LEFT JOIN workflow_assignment_labels l ON l.assignment_id = a.id
		WHERE a.state = ?
		  AND r.status NOT IN ('succeeded', 'failed', 'cancelled', 'stopped', '`+store.RunStatusAwaitingHuman+`')
		  AND (
		          a.project_id IS NULL
		          OR EXISTS (
		              SELECT 1 FROM project_members pm
		              WHERE pm.project_id = a.project_id AND pm.member_id = ?
		          )
		      )
		ORDER BY a.created_at ASC, a.id ASC`,
		store.WorkflowAssignmentStateQueued, criteriaID)
	if err != nil {
		return nil, err
	}

	candidates := make(map[string]*assignmentCandidate)
	for rows.Next() {
		var id, runID, wfName, wfSource, idemp, created, updated string
		var lockfile, projectID, lkey, lvalue sql.NullString
		if err := rows.Scan(&id, &runID, &wfName, &wfSource, &lockfile, &idemp,
			&created, &updated, &projectID, &lkey, &lvalue); err != nil {
			rows.Close()
			return nil, err
		}
		c, ok := candidates[id]
		if !ok {
			c = &assignmentCandidate{
				assignment: &store.WorkflowAssignment{
					ID:             id,
					RunID:          runID,
					WorkflowName:   wfName,
					WorkflowSource: wfSource,
					IdempotencyKey: idemp,
					State:          store.WorkflowAssignmentStateQueued,
					CreatedAt:      mustParseTime(created),
					UpdatedAt:      mustParseTime(updated),
					Labels:         make(map[string]string),
				},
				required: make(map[string]string),
			}
			if lockfile.Valid {
				c.assignment.LockfileSource = lockfile.String
			}
			if projectID.Valid {
				c.assignment.ProjectID = projectID.String
			}
			candidates[id] = c
		}
		if lkey.Valid {
			c.required[lkey.String] = lvalue.String
		}
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return nil, err
	}
	rows.Close()

	ordered := make([]*assignmentCandidate, 0, len(candidates))
	for _, c := range candidates {
		ordered = append(ordered, c)
	}
	sort.Slice(ordered, func(i, j int) bool {
		if !ordered[i].assignment.CreatedAt.Equal(ordered[j].assignment.CreatedAt) {
			return ordered[i].assignment.CreatedAt.Before(ordered[j].assignment.CreatedAt)
		}
		return ordered[i].assignment.ID < ordered[j].assignment.ID
	})

	for _, c := range ordered {
		if !labelsSatisfy(agentLabels, c.required) {
			continue
		}
		res, err := tx.ExecContext(ctx,
			`UPDATE workflow_assignments
			 SET state=?, leased_criteria_id=?, lease_expires_at=?, updated_at=?
			 WHERE id=? AND state=?`,
			store.WorkflowAssignmentStateLeased, criteriaID, expiresAt.Format(store.TimestampLayout),
			now.Format(store.TimestampLayout), c.assignment.ID, store.WorkflowAssignmentStateQueued)
		if err != nil {
			return nil, err
		}
		affected, _ := res.RowsAffected()
		if affected == 0 {
			continue
		}
		if _, err := tx.ExecContext(ctx,
			`UPDATE runs SET overseer_id=?, agent_identity=? WHERE id=?`, criteriaID, identity, c.assignment.RunID); err != nil {
			return nil, err
		}
		leaseID := uuid.NewString()
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO workflow_assignment_leases(id, assignment_id, criteria_id, created_at, expires_at)
			 VALUES(?, ?, ?, ?, ?)`,
			leaseID, c.assignment.ID, criteriaID, now.Format(store.TimestampLayout), expiresAt.Format(store.TimestampLayout)); err != nil {
			return nil, err
		}
		var attempt int
		if err := tx.QueryRowContext(ctx,
			`SELECT COALESCE(MAX(attempt),0)+1 FROM workflow_assignment_attempts WHERE assignment_id=?`,
			c.assignment.ID).Scan(&attempt); err != nil {
			return nil, err
		}
		if _, err := tx.ExecContext(ctx,
			`INSERT INTO workflow_assignment_attempts(assignment_id, attempt, criteria_id, created_at)
			 VALUES(?, ?, ?, ?)`,
			c.assignment.ID, attempt, criteriaID, now.Format(store.TimestampLayout)); err != nil {
			return nil, err
		}
		if err := tx.Commit(); err != nil {
			return nil, err
		}
		c.assignment.State = store.WorkflowAssignmentStateLeased
		c.assignment.LeasedCriteriaID = criteriaID
		c.assignment.LeaseExpiresAt = &expiresAt
		c.assignment.Labels = c.required
		return c.assignment, nil
	}

	return nil, store.ErrNotFound
}

type assignmentCandidate struct {
	assignment *store.WorkflowAssignment
	required   map[string]string
}

func labelsSatisfy(agentLabels, required map[string]string) bool {
	for k, v := range required {
		if agentLabels[k] != v {
			return false
		}
	}
	return true
}

func mustParseTime(s string) time.Time {
	t, _ := time.Parse(store.TimestampLayout, s)
	return t
}

// ExpireWorkflowAssignmentLeases transitions leased assignments whose lease
// has expired and whose run has not yet started back to queued. It clears the
// associated run's overseer_id so the assignment can be safely redispatched to
// a new agent. The returned IDs can be used by callers to redispatch the work.
//
// Every statement re-asserts all three expiry predicates — assignment
// state='leased', lease_expires_at < now, and run status='pending' — because a
// run's status is written unguarded and the expiry transaction's later
// statements can observe a newer snapshot (or, on pg, an interleaved writer):
// a run that starts (pending → running) between the scan and the expiry
// UPDATE — the same window LeaseWorkflowAssignment's inline stale-lease sweep
// guards — would otherwise be requeued with a live run. The run's overseer_id
// is cleared only while the run is still pending. (sqlite serializes writers,
// so the pg race cannot interleave here, but both engines enforce one
// contract.)
func (s *Store) ExpireWorkflowAssignmentLeases(ctx context.Context, now time.Time) ([]string, error) {
	ctx, cancel := s.opCtx(ctx)
	defer cancel()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback() //nolint:errcheck

	rows, err := tx.QueryContext(ctx, `
		SELECT a.id
		FROM workflow_assignments a
		JOIN runs r ON r.id = a.run_id
		WHERE a.state = ? AND a.lease_expires_at < ? AND r.status = 'pending'
		ORDER BY a.created_at ASC, a.id ASC`,
		store.WorkflowAssignmentStateLeased, now.Format(store.TimestampLayout))
	if err != nil {
		return nil, err
	}
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return nil, err
		}
		ids = append(ids, id)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return nil, err
	}
	rows.Close()

	if len(ids) == 0 {
		_ = tx.Rollback()
		return nil, nil
	}

	// The re-assertion predicates (state='leased', lease still expired, run
	// still pending) keep the expiry safe when the candidate scan and the
	// UPDATE observe different snapshots: an assignment that was re-queued
	// and re-leased after the scan must not be clobbered here, and a run
	// that started in that window must not be requeued. RETURNING yields
	// only the rows actually transitioned, so the returned ids and the run
	// overseer cleanup below never reference a candidate that was re-leased
	// instead.
	placeholders := strings.Repeat("?,", len(ids))
	placeholders = placeholders[:len(placeholders)-1]

	args := make([]any, 0, len(ids)+3)
	args = append(args, store.WorkflowAssignmentStateQueued, now.Format(store.TimestampLayout))
	for _, id := range ids {
		args = append(args, id)
	}
	args = append(args, now.Format(store.TimestampLayout))
	tRows, err := tx.QueryContext(ctx,
		`UPDATE workflow_assignments
		 SET state=?, leased_criteria_id=NULL, lease_expires_at=NULL, updated_at=?
		 WHERE id IN (`+placeholders+`)
		   AND state='leased' AND lease_expires_at < ?
		   AND run_id IN (SELECT id FROM runs WHERE status='pending')
		 RETURNING id, run_id`,
		args...)
	if err != nil {
		return nil, err
	}
	var transitionedIDs, transitionedRunIDs []string
	for tRows.Next() {
		var id, runID string
		if err := tRows.Scan(&id, &runID); err != nil {
			tRows.Close()
			return nil, err
		}
		transitionedIDs = append(transitionedIDs, id)
		transitionedRunIDs = append(transitionedRunIDs, runID)
	}
	if err := tRows.Err(); err != nil {
		tRows.Close()
		return nil, err
	}
	tRows.Close()

	// Clear overseer_id only for runs of assignments actually transitioned
	// above, and only while the run is still pending; a candidate re-leased
	// between scan and update keeps its overseer, and so does a run that
	// started in that window.
	if len(transitionedRunIDs) > 0 {
		runPlaceholders := strings.Repeat("?,", len(transitionedRunIDs))
		runPlaceholders = runPlaceholders[:len(runPlaceholders)-1]
		runArgs := make([]any, 0, len(transitionedRunIDs))
		for _, runID := range transitionedRunIDs {
			runArgs = append(runArgs, runID)
		}
		if _, err := tx.ExecContext(ctx,
			`UPDATE runs
			 SET overseer_id=NULL
			 WHERE id IN (`+runPlaceholders+`)
			   AND status='pending'`,
			runArgs...); err != nil {
			return nil, err
		}
	}

	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return transitionedIDs, nil
}

// ListLeasedPendingAssignmentsByCriteriaID returns assignments currently
// leased to criteriaID whose run has not yet started. These are in-flight
// leases that should be redelivered to the agent after a Castle restart.
func (s *Store) ListLeasedPendingAssignmentsByCriteriaID(ctx context.Context, criteriaID string) ([]*store.WorkflowAssignment, error) {
	ctx, cancel := s.opCtx(ctx)
	defer cancel()
	if criteriaID == "" {
		return nil, errors.New("criteria_id required")
	}
	rows, err := s.reader.QueryContext(ctx, `
		SELECT a.id, a.run_id, a.workflow_name, a.workflow_source, a.lockfile_source,
		       a.idempotency_key, a.created_at, a.updated_at, a.project_id,
		       a.input_values, a.target_metadata, l.key, l.value
		FROM workflow_assignments a
		JOIN runs r ON r.id = a.run_id
		LEFT JOIN workflow_assignment_labels l ON l.assignment_id = a.id
		WHERE a.leased_criteria_id = ? AND a.state = ? AND r.status = 'pending'
		ORDER BY a.created_at ASC, a.id ASC`,
		criteriaID, store.WorkflowAssignmentStateLeased)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	candidates := make(map[string]*assignmentCandidate)
	for rows.Next() {
		var id, runID, wfName, wfSource, idemp, created, updated string
		var lockfile, projectID, inputValues, targetMetadata, lkey, lvalue sql.NullString
		if err := rows.Scan(&id, &runID, &wfName, &wfSource, &lockfile, &idemp,
			&created, &updated, &projectID, &inputValues, &targetMetadata, &lkey, &lvalue); err != nil {
			return nil, err
		}
		c, ok := candidates[id]
		if !ok {
			c = &assignmentCandidate{
				assignment: &store.WorkflowAssignment{
					ID:             id,
					RunID:          runID,
					WorkflowName:   wfName,
					WorkflowSource: wfSource,
					IdempotencyKey: idemp,
					State:          store.WorkflowAssignmentStateLeased,
					CreatedAt:      mustParseTime(created),
					UpdatedAt:      mustParseTime(updated),
					Labels:         make(map[string]string),
				},
				required: make(map[string]string),
			}
			if lockfile.Valid {
				c.assignment.LockfileSource = lockfile.String
			}
			scanAssignmentMaps(projectID, inputValues, targetMetadata, c.assignment)
			candidates[id] = c
		}
		if lkey.Valid {
			c.required[lkey.String] = lvalue.String
		}
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	ordered := make([]*assignmentCandidate, 0, len(candidates))
	for _, c := range candidates {
		ordered = append(ordered, c)
	}
	sort.Slice(ordered, func(i, j int) bool {
		if !ordered[i].assignment.CreatedAt.Equal(ordered[j].assignment.CreatedAt) {
			return ordered[i].assignment.CreatedAt.Before(ordered[j].assignment.CreatedAt)
		}
		return ordered[i].assignment.ID < ordered[j].assignment.ID
	})

	out := make([]*store.WorkflowAssignment, 0, len(ordered))
	for _, c := range ordered {
		c.assignment.LeasedCriteriaID = criteriaID
		c.assignment.Labels = c.required
		out = append(out, c.assignment)
	}
	return out, nil
}

func (s *Store) RecordWorkflowAssignmentLease(ctx context.Context, lease *store.WorkflowAssignmentLease) error {
	ctx, cancel := s.opCtx(ctx)
	defer cancel()
	if lease == nil || lease.AssignmentID == "" || lease.CriteriaID == "" {
		return errors.New("assignment_id and criteria_id required")
	}
	if lease.ID == "" {
		lease.ID = uuid.NewString()
	}
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO workflow_assignment_leases(id, assignment_id, criteria_id, created_at, expires_at)
		 VALUES(?, ?, ?, ?, ?)`,
		lease.ID, lease.AssignmentID, lease.CriteriaID, lease.CreatedAt.Format(store.TimestampLayout), lease.ExpiresAt.Format(store.TimestampLayout))
	return err
}

func (s *Store) RecordWorkflowAssignmentAttempt(ctx context.Context, attempt *store.WorkflowAssignmentAttempt) error {
	ctx, cancel := s.opCtx(ctx)
	defer cancel()
	if attempt == nil || attempt.AssignmentID == "" || attempt.Attempt <= 0 {
		return errors.New("assignment_id and attempt > 0 required")
	}
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO workflow_assignment_attempts(assignment_id, attempt, criteria_id, created_at)
		 VALUES(?, ?, ?, ?)`,
		attempt.AssignmentID, attempt.Attempt, attempt.CriteriaID, attempt.CreatedAt.Format(store.TimestampLayout))
	return err
}

func (s *Store) CompleteWorkflowAssignmentAttempt(ctx context.Context, assignmentID string, attempt int, outcome string) error {
	ctx, cancel := s.opCtx(ctx)
	defer cancel()
	if assignmentID == "" || attempt <= 0 {
		return errors.New("assignment_id and attempt > 0 required")
	}
	now := time.Now().UTC().Format(store.TimestampLayout)
	_, err := s.db.ExecContext(ctx,
		`UPDATE workflow_assignment_attempts SET completed_at=?, outcome=?
		 WHERE assignment_id=? AND attempt=?`,
		now, outcome, assignmentID, attempt)
	return err
}

func (s *Store) MarkWorkflowAssignmentTerminal(ctx context.Context, runID, reason string) error {
	ctx, cancel := s.opCtx(ctx)
	defer cancel()
	if runID == "" {
		return errors.New("run_id required")
	}
	now := time.Now().UTC()
	_, err := s.db.ExecContext(ctx,
		`UPDATE workflow_assignments
		 SET state=?, terminal_reason=?, updated_at=?
		 WHERE run_id=? AND state IN (?, ?)`,
		store.WorkflowAssignmentStateTerminal, reason, now.Format(store.TimestampLayout),
		runID, store.WorkflowAssignmentStateQueued, store.WorkflowAssignmentStateLeased)
	return err
}
