package pg

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/brokenbots/castle/castle/internal/store"
)

// CreateProject inserts a new project. See the sqlite profile for the exact
// semantics; guards, ordering and error mapping are identical by contract.
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
	now := time.Now().UTC()
	if p.CreatedAt.IsZero() {
		p.CreatedAt = now
	}
	if p.UpdatedAt.IsZero() {
		p.UpdatedAt = now
	}
	queuesJSON, err := store.MarshalQueues(queues)
	if err != nil {
		return err
	}
	_, err = s.db.ExecContext(ctx, `
		INSERT INTO projects (id, membership_policy, default_refire_policy, queues, settings, settings_etag, info, created_at, updated_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)`,
		p.ID,
		p.MembershipPolicy,
		p.DefaultRefirePolicy,
		queuesJSON,
		store.MarshalJSONMap(p.Settings),
		store.SettingsETag(p.Settings),
		store.MarshalJSONMap(p.Info),
		us(p.CreatedAt), us(p.UpdatedAt))
	if err != nil {
		if store.IsUniqueViolation(err) {
			return store.ErrProjectExists
		}
		return err
	}
	return nil
}

// scanProject reads one projects row through the given scanner helper.
func scanProject(scan func(dest ...any) error) (*store.Project, error) {
	var p store.Project
	var queues, settings, info sql.NullString
	var created, updated time.Time
	if err := scan(&p.ID, &p.MembershipPolicy, &p.DefaultRefirePolicy,
		&queues, &settings, &info, &created, &updated); err != nil {
		return nil, err
	}
	p.CreatedAt = created
	p.UpdatedAt = updated
	p.Queues = store.UnmarshalQueues(queues.String)
	p.Settings = store.UnmarshalJSONMap(settings.String)
	p.Info = store.UnmarshalJSONMap(info.String)
	return &p, nil
}

const projectColumns = `id, membership_policy, COALESCE(default_refire_policy,''), COALESCE(queues,''),
	COALESCE(settings,''), COALESCE(info,''), created_at, updated_at`

// GetProject returns the project with the given ID.
func (s *Store) GetProject(ctx context.Context, id string) (*store.Project, error) {
	ctx, cancel := s.opCtx(ctx)
	defer cancel()
	if id == "" {
		return nil, errors.New("id required")
	}
	p, err := scanProject(s.reader.QueryRowContext(ctx,
		`SELECT `+projectColumns+` FROM projects WHERE id=$1`, id).Scan)
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
	rows, err := s.reader.QueryContext(ctx,
		`SELECT `+projectColumns+` FROM projects ORDER BY id ASC`)
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

// JoinProject adds the calling identity to the project as a self-join; it is
// refused while the project's membership policy is closed.
func (s *Store) JoinProject(ctx context.Context, projectID, memberID, kind string, now time.Time) error {
	if !store.IsValidProjectMemberKind(kind) {
		return fmt.Errorf("%w: %q", store.ErrInvalidProjectMember, kind)
	}
	return s.joinProject(ctx, projectID, memberID, kind, now, false)
}

// AddProjectMember adds a member under operator management, regardless of the
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
	ts := us(now)
	if bypassPolicy {
		// Operator path: verify the project exists so a typo'd project ID
		// surfaces as ErrNotFound instead of silently creating an orphan
		// membership the lease scan would never match.
		var one int
		if err := s.db.QueryRowContext(ctx, `SELECT 1 FROM projects WHERE id=$1`, projectID).Scan(&one); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return store.ErrNotFound
			}
			return err
		}
		_, err := s.db.ExecContext(ctx, `
			INSERT INTO project_members (project_id, member_id, kind, created_at)
			VALUES ($1, $2, $3, $4)
			ON CONFLICT (project_id, member_id) DO UPDATE SET kind=excluded.kind`,
			projectID, memberID, kind, ts)
		return err
	}
	res, err := s.db.ExecContext(ctx, `
		INSERT INTO project_members (project_id, member_id, kind, created_at)
		SELECT p.id, $1, $2, $3 FROM projects p
		WHERE p.id = $4 AND p.membership_policy = $5
		ON CONFLICT (project_id, member_id) DO UPDATE SET kind=excluded.kind`,
		memberID, kind, ts, projectID, store.ProjectMembershipOpen)
	if err != nil {
		return err
	}
	if affected, _ := res.RowsAffected(); affected == 0 {
		// No row: the project is missing or its membership policy is
		// closed. Re-check to report the precise condition.
		var policy string
		err := s.reader.QueryRowContext(ctx, `SELECT membership_policy FROM projects WHERE id=$1`, projectID).Scan(&policy)
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

// RemoveProjectMember removes a membership row; removing an unknown
// membership is a no-op.
func (s *Store) RemoveProjectMember(ctx context.Context, projectID, memberID string) error {
	ctx, cancel := s.opCtx(ctx)
	defer cancel()
	if projectID == "" || memberID == "" {
		return errors.New("project id and member id required")
	}
	_, err := s.db.ExecContext(ctx, `DELETE FROM project_members WHERE project_id=$1 AND member_id=$2`, projectID, memberID)
	return err
}

// ListProjectMembers returns the project's member set ordered by member ID.
func (s *Store) ListProjectMembers(ctx context.Context, projectID string) ([]*store.ProjectMember, error) {
	ctx, cancel := s.opCtx(ctx)
	defer cancel()
	if projectID == "" {
		return nil, errors.New("project id required")
	}
	rows, err := s.reader.QueryContext(ctx, `
		SELECT project_id, member_id, kind, created_at
		FROM project_members WHERE project_id=$1 ORDER BY member_id ASC`, projectID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	members := make([]*store.ProjectMember, 0)
	for rows.Next() {
		var m store.ProjectMember
		var created time.Time
		if err := rows.Scan(&m.ProjectID, &m.MemberID, &m.Kind, &created); err != nil {
			return nil, err
		}
		m.CreatedAt = created
		members = append(members, &m)
	}
	return members, rows.Err()
}

// IsProjectMember reports whether memberID belongs to projectID's member set.
func (s *Store) IsProjectMember(ctx context.Context, projectID, memberID string) (bool, error) {
	ctx, cancel := s.opCtx(ctx)
	defer cancel()
	if projectID == "" || memberID == "" {
		return false, nil
	}
	var one int
	err := s.reader.QueryRowContext(ctx, `
		SELECT 1 FROM project_members WHERE project_id=$1 AND member_id=$2`, projectID, memberID).Scan(&one)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return false, nil
		}
		return false, err
	}
	return true, nil
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
		FROM projects WHERE id=$1`, projectID).Scan(&settings, &etag)
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
	err := s.db.QueryRowContext(ctx, `SELECT COALESCE(settings_etag,'') FROM projects WHERE id=$1`, projectID).Scan(&current)
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
		SET settings=$1, settings_etag=$2, updated_at=$3
		WHERE id=$4 AND settings_etag=$5`,
		store.MarshalJSONMap(settings), etag, us(time.Now().UTC()),
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
		if err := s.db.QueryRowContext(ctx, `SELECT COALESCE(settings_etag,'') FROM projects WHERE id=$1`, projectID).Scan(&now); err != nil {
			return "", err
		}
		if now == etag {
			return etag, nil
		}
		return "", store.ErrSettingsConflict
	}
	return etag, nil
}
