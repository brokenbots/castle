package pg

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/brokenbots/castle/castle/internal/store"
)

// plf is a placeholder factory for building pg queries: each call returns the
// next numbered placeholder and appends the value, keeping argument order and
// placeholder numbering in lockstep. Placeholders must start at $1 and form a
// dense sequence with no gaps in every statement: the extended protocol
// parses parameter references as a contiguous range (a statement that only
// references $3+ fails to parse, 42P18), so reuse a fresh plf per statement.
type plf struct {
	args []any
}

func (p *plf) next(v any) string {
	p.args = append(p.args, v)
	return fmt.Sprintf("$%d", len(p.args))
}

// runColumns is the projection scanned by run row readers; keep in sync with
// scanRun and the migration that last altered the runs table.
const runColumns = "id,overseer_id,workflow_name,workflow_hcl,status,current_step,last_seq,created_at,started_at,ended_at,variable_scope,pending_signal,paused_at,ticket,repo_url,pr_url,failure_reason"

func (s *Store) CreateRun(ctx context.Context, r *store.Run) error {
	ctx, cancel := s.opCtx(ctx)
	defer cancel()
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO runs(id,overseer_id,workflow_name,workflow_hcl,status,current_step,last_seq,created_at,started_at,ticket,repo_url,pr_url) VALUES($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12)`,
		r.ID, r.OverseerID, r.WorkflowName, r.WorkflowHCL, r.Status, r.CurrentStep, r.LastSeq, us(r.CreatedAt),
		timePtr(r.StartedAt), nonEmpty(r.Ticket), nonEmpty(r.RepoURL), nonEmpty(r.PRURL))
	return err
}

func (s *Store) GetRun(ctx context.Context, id string) (*store.Run, error) {
	ctx, cancel := s.opCtx(ctx)
	defer cancel()
	row := s.db.QueryRowContext(ctx, `SELECT `+runColumns+` FROM runs WHERE id=$1`, id)
	return scanRun(row.Scan)
}

// ListRuns pages runs newest-first. limit > 0 fetches at most limit rows and
// returns a continuation token whenever a further row exists (one extra row
// is probed so an exactly-full final page does not hand out a dead token);
// limit <= 0 returns every matching row with no token. The cursor predicate
// mirrors the ORDER BY, so a token stays valid across intervening inserts.
// Postgres stores created_at at microsecond precision; the shared cursor
// helpers encode/decode that precision losslessly, so the comparison against
// the stored column is exact.
func (s *Store) ListRuns(ctx context.Context, overseerID, status string, limit int, pageToken string) ([]*store.Run, string, error) {
	ctx, cancel := s.opCtx(ctx)
	defer cancel()
	q := `SELECT ` + runColumns + ` FROM runs WHERE 1=1`
	p := &plf{}
	if overseerID != "" {
		q += " AND overseer_id=" + p.next(overseerID)
	}
	if status != "" {
		q += " AND status=" + p.next(status)
	}
	cursorCreated, cursorID, err := store.DecodeRunCursor(pageToken)
	if err != nil {
		return nil, "", err
	}
	if cursorCreated != "" {
		cursorTS, err := store.DecodeRunCursorCreated(cursorCreated)
		if err != nil {
			return nil, "", err
		}
		// Tuple comparison mirrors ORDER BY created_at DESC, id DESC, with
		// the timestamp bound parsed to the same TIMESTAMPTZ domain as the
		// stored column.
		q += " AND (created_at, id) < (" + p.next(cursorTS) + "::timestamptz, " + p.next(cursorID) + ")"
	}
	q += " ORDER BY created_at DESC, id DESC"
	if limit > 0 {
		// Probe one row past the page so the token is only issued when a
		// next page actually exists.
		q += " LIMIT " + p.next(limit+1)
	}
	rows, err := s.reader.QueryContext(ctx, q, p.args...)
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
		next = store.EncodeRunCursorFromTime(last.CreatedAt, last.ID)
		out = out[:limit]
	}
	return out, next, nil
}

func scanRun(scan func(...any) error) (*store.Run, error) {
	var r store.Run
	var created time.Time
	var overseerID sql.NullString
	var started, ended, pausedAt sql.NullTime
	var variableScope, pendingSignal, ticket, repoURL, prURL, failureReason sql.NullString
	err := scan(&r.ID, &overseerID, &r.WorkflowName, &r.WorkflowHCL, &r.Status, &r.CurrentStep, &r.LastSeq, &created, &started, &ended, &variableScope, &pendingSignal, &pausedAt, &ticket, &repoURL, &prURL, &failureReason)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, store.ErrNotFound
		}
		return nil, err
	}
	if overseerID.Valid {
		r.OverseerID = overseerID.String
	}
	r.CreatedAt = created
	if started.Valid {
		t := started.Time
		r.StartedAt = &t
	}
	if ended.Valid {
		t := ended.Time
		r.EndedAt = &t
	}
	if variableScope.Valid {
		r.VariableScope = variableScope.String
	}
	if pendingSignal.Valid {
		r.PendingSignal = pendingSignal.String
	}
	if pausedAt.Valid {
		t := pausedAt.Time
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
	return &r, nil
}

func (s *Store) UpdateRun(ctx context.Context, r *store.Run) error {
	ctx, cancel := s.opCtx(ctx)
	defer cancel()
	_, err := s.db.ExecContext(ctx,
		`UPDATE runs SET status=$1, current_step=$2, last_seq=$3, ended_at=$4, started_at=COALESCE(started_at, $5), failure_reason=COALESCE(NULLIF($6, ''), failure_reason) WHERE id=$7`,
		r.Status, r.CurrentStep, r.LastSeq, timePtr(r.EndedAt), timePtr(r.StartedAt), nonEmpty(r.FailureReason), r.ID)
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
		`UPDATE runs SET ticket=COALESCE(NULLIF($1, ''), ticket), repo_url=COALESCE(NULLIF($2, ''), repo_url), pr_url=COALESCE(NULLIF($3, ''), pr_url) WHERE id=$4`,
		ticket, repoURL, prURL, runID)
	return err
}

// SetRunVariableScope persists a JSON-encoded variable scope snapshot (W04).
func (s *Store) SetRunVariableScope(ctx context.Context, runID, scope string) error {
	ctx, cancel := s.opCtx(ctx)
	defer cancel()
	_, err := s.db.ExecContext(ctx,
		`UPDATE runs SET variable_scope=$1 WHERE id=$2`, scope, runID)
	return err
}

// GetRunVariableScope returns the stored variable scope JSON for runID.
// Returns ("", nil) when the run exists but has no scope yet (NULL column).
func (s *Store) GetRunVariableScope(ctx context.Context, runID string) (string, error) {
	ctx, cancel := s.opCtx(ctx)
	defer cancel()
	var scope sql.NullString
	err := s.db.QueryRowContext(ctx, `SELECT variable_scope FROM runs WHERE id=$1`, runID).Scan(&scope)
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
// Guarded against terminal and stopped runs (CRI-207).
func (s *Store) SetRunPaused(ctx context.Context, runID, pendingSignal string, pausedAt time.Time) error {
	ctx, cancel := s.opCtx(ctx)
	defer cancel()
	_, err := s.db.ExecContext(ctx,
		`UPDATE runs SET status='paused', pending_signal=$1, paused_at=$2 WHERE id=$3 AND status NOT IN ('succeeded', 'failed', 'cancelled', 'stopped')`,
		pendingSignal, us(pausedAt), runID)
	return err
}

// ClearRunPaused clears the pause state and sets status back to running (W05).
// Guarded to paused runs only (CRI-197).
func (s *Store) ClearRunPaused(ctx context.Context, runID string) error {
	ctx, cancel := s.opCtx(ctx)
	defer cancel()
	_, err := s.db.ExecContext(ctx,
		`UPDATE runs SET status='running', pending_signal=NULL, paused_at=NULL WHERE id=$1 AND status='paused'`, runID)
	return err
}

// SetRunStopped parks the run in status stopped (CRI-207).
func (s *Store) SetRunStopped(ctx context.Context, runID string) error {
	ctx, cancel := s.opCtx(ctx)
	defer cancel()
	_, err := s.db.ExecContext(ctx,
		`UPDATE runs SET status='stopped', pending_signal=NULL, paused_at=NULL WHERE id=$1 AND status NOT IN ('succeeded', 'failed', 'cancelled')`,
		runID)
	return err
}

// ClearRunStopped moves a stopped run back to running (the resume path,
// CRI-207).
func (s *Store) ClearRunStopped(ctx context.Context, runID string) error {
	ctx, cancel := s.opCtx(ctx)
	defer cancel()
	_, err := s.db.ExecContext(ctx,
		`UPDATE runs SET status='running', pending_signal=NULL, paused_at=NULL WHERE id=$1 AND status='stopped'`, runID)
	return err
}

// MarkRunUnstarted returns a never-started stopped run to the leasable pending
// bucket (CRI-207 resume path, KB-233 rule 2 fresh-window semantics). See the
// sqlite profile for the full invariant commentary — the guards are identical.
func (s *Store) MarkRunUnstarted(ctx context.Context, runID string, requeuedAt time.Time) error {
	ctx, cancel := s.opCtx(ctx)
	defer cancel()
	_, err := s.db.ExecContext(ctx,
		`UPDATE runs SET status='pending', started_at=NULL, created_at=$1 WHERE id=$2 AND status='stopped'`,
		us(requeuedAt), runID)
	return err
}
