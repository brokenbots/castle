package pg

import (
	"context"
	"database/sql"
	"errors"
	"time"

	"github.com/brokenbots/castle/castle/internal/store"
)

// GetProjectConfig returns the project's workflow config record doc and its
// content-addressed etag. ErrNotFound when the project does not exist.
func (s *Store) GetProjectConfig(ctx context.Context, projectID string) ([]byte, string, error) {
	ctx, cancel := s.opCtx(ctx)
	defer cancel()
	if projectID == "" {
		return nil, "", errors.New("project id required")
	}
	var doc, etag sql.NullString
	err := s.reader.QueryRowContext(ctx, `
		SELECT COALESCE(workflow_config,''), COALESCE(workflow_config_etag,'')
		FROM projects WHERE id=$1`, projectID).Scan(&doc, &etag)
	if err != nil {
		return nil, "", store.ErrNotFound
	}
	return []byte(doc.String), etag.String, nil
}

// SetProjectConfig swaps the whole workflow config record doc under the
// caller's expected etag. expectedETag "" is accepted only while the current
// doc is unset; the conditional UPDATE re-checks the etag so a concurrent
// writer loses the swap and gets ErrProjectConfigConflict instead of
// clobbering its neighbor. The doc persists in its canonical form so the
// stored bytes render byte-identically regardless of the writer's formatting.
func (s *Store) SetProjectConfig(ctx context.Context, projectID string, doc []byte, expectedETag string) (string, error) {
	ctx, cancel := s.opCtx(ctx)
	defer cancel()
	if projectID == "" {
		return "", errors.New("project id required")
	}
	if len(doc) == 0 {
		doc = nil
	}
	var current string
	err := s.db.QueryRowContext(ctx, `SELECT COALESCE(workflow_config_etag,'') FROM projects WHERE id=$1`, projectID).Scan(&current)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return "", store.ErrNotFound
		}
		return "", err
	}
	if expectedETag != current {
		return "", store.ErrProjectConfigConflict
	}
	canonical := doc
	if len(canonical) > 0 {
		canonical, err = store.CanonicalWorkflowConfigDoc(doc)
		if err != nil {
			return "", err
		}
	}
	etag := store.ProjectConfigETag(canonical)
	stored := []byte("")
	if canonical != nil {
		stored = canonical
	}
	res, err := s.db.ExecContext(ctx, `
		UPDATE projects
		SET workflow_config=$1, workflow_config_etag=$2, updated_at=$3
		WHERE id=$4 AND workflow_config_etag=$5`,
		stored, etag,
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
		if err := s.db.QueryRowContext(ctx, `SELECT COALESCE(workflow_config_etag,'') FROM projects WHERE id=$1`, projectID).Scan(&now); err != nil {
			return "", err
		}
		if now == etag {
			return etag, nil
		}
		return "", store.ErrProjectConfigConflict
	}
	return etag, nil
}
