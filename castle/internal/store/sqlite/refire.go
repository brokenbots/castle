package sqlite

import (
	"context"
	"database/sql"
	"time"

	"github.com/brokenbots/castle/castle/internal/store"
	"github.com/brokenbots/castle/castle/internal/store/reap"
	"github.com/brokenbots/castle/castle/internal/store/refire"
)

// UpsertRefirePolicy stores the verbatim policy document under (projectID,
// route), preserving created_at across updates (KB-321). The document must
// parse: policy rows are data with guaranteed-decodable contents.
func (s *Store) UpsertRefirePolicy(ctx context.Context, projectID, route, policyJSON string, now time.Time) error {
	ctx, cancel := s.opCtx(ctx)
	defer cancel()
	if _, err := refire.ParsePolicy(policyJSON); err != nil {
		return err
	}
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO refire_policies(project_id, route, policy, created_at, updated_at)
		 VALUES(?,?,?,?,?)
		 ON CONFLICT(project_id, route) DO UPDATE SET policy = excluded.policy, updated_at = excluded.updated_at`,
		projectID, route, policyJSON, now.Format(store.TimestampLayout), now.Format(store.TimestampLayout))
	return err
}

// ListRefirePolicies returns every policy row, or nil when none exist.
func (s *Store) ListRefirePolicies(ctx context.Context) ([]store.RefirePolicyRow, error) {
	ctx, cancel := s.opCtx(ctx)
	defer cancel()
	rows, err := s.reader.QueryContext(ctx,
		`SELECT project_id, route, policy, created_at, updated_at
		 FROM refire_policies ORDER BY project_id ASC, route ASC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []store.RefirePolicyRow
	for rows.Next() {
		var r store.RefirePolicyRow
		var projectID, route string
		var created, updated string
		if err := rows.Scan(&projectID, &route, &r.Policy, &created, &updated); err != nil {
			return nil, err
		}
		// NOT NULL DEFAULT '' key columns scan back as '' here for the
		// default rows (refire.LoadPolicies keeps the same convention).
		r.ProjectID = projectID
		r.Route = route
		r.CreatedAt, _ = time.Parse(store.TimestampLayout, created)
		r.UpdatedAt, _ = time.Parse(store.TimestampLayout, updated)
		out = append(out, r)
	}
	return out, rows.Err()
}

// DeleteRefirePolicy removes the policy row for (projectID, route);
// deleting an unknown key is a no-op.
func (s *Store) DeleteRefirePolicy(ctx context.Context, projectID, route string) error {
	ctx, cancel := s.opCtx(ctx)
	defer cancel()
	_, err := s.db.ExecContext(ctx,
		`DELETE FROM refire_policies WHERE project_id = ? AND route = ?`,
		projectID, route)
	return err
}

// ListRefireCandidates scans the refire-candidate projection on the reader
// connection (never the serialized writer): the newest failed attempt of
// every policy-eligible ending, oldest first, up to limit.
func (s *Store) ListRefireCandidates(ctx context.Context, limit int) ([]store.RefireCandidate, error) {
	ctx, cancel := s.opCtx(ctx)
	defer cancel()
	candidates, err := scanRefireCandidates(ctx, s.reader, reap.SQLite, limit)
	if err != nil {
		return nil, err
	}
	if err := refire.AttachLabels(ctx, s.reader, reap.SQLite, candidates); err != nil {
		return nil, err
	}
	return candidates, nil
}

// RefireFailedRuns evaluates the refire policy for the candidates (see
// ListRefireCandidates, up to limit) and takes the verdicts in one write
// transaction: transient endings requeue successor attempts under the
// attempt/backoff limits, designed-park endings reclassify to the KB-226
// parked shape, cosmetic and unclassified endings are left untouched. now is
// the evaluation instant (drives backoff). The candidate scan is repeated
// inside the transaction, so verdicts land on a consistent snapshot; parked
// rewrites and successor inserts carry status/unique guards, so a racing
// writer can never double-park or mint a twice-refired family (CRI-142
// discipline).
func (s *Store) RefireFailedRuns(ctx context.Context, now time.Time, limit int) ([]store.RefireOutcome, error) {
	ctx, cancel := s.opCtx(ctx)
	defer cancel()
	outcomes, err := retryOnTransient(ctx, reapRetryPolicy, func() ([]store.RefireOutcome, error) {
		return s.refireFailedRunsAttempt(ctx, now, limit)
	})
	if isInterrupt(err) {
		s.recoverWriter(ctx)
	}
	return outcomes, err
}

func (s *Store) refireFailedRunsAttempt(ctx context.Context, now time.Time, limit int) ([]store.RefireOutcome, error) {
	// Bound the pass so a wedged writer connection fails the tick instead of
	// pinning the refire goroutine (CRI-143).
	ctx, cancel := context.WithTimeout(ctx, reapAttemptTimeout)
	defer cancel()

	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback() }()

	policies, err := refire.LoadPolicies(ctx, tx)
	if err != nil {
		return nil, err
	}
	candidates, err := scanRefireCandidates(ctx, tx, reap.SQLite, limit)
	if err != nil {
		return nil, err
	}
	if err := refire.AttachLabels(ctx, tx, reap.SQLite, candidates); err != nil {
		return nil, err
	}
	outcomes, err := refire.EvaluateCandidates(ctx, tx, reap.SQLite, now, policies, candidates)
	if err != nil {
		return nil, err
	}
	return outcomes, tx.Commit()
}

// scanRefireCandidates runs the shared candidate query against runner and
// scans the rows (sqlite timestamps are TEXT).
func scanRefireCandidates(ctx context.Context, runner reap.Runner, d reap.Dialect, limit int) ([]store.RefireCandidate, error) {
	q, args := refire.CandidatesQuery(d, limit)
	rows, err := runner.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []store.RefireCandidate
	for rows.Next() {
		var a store.WorkflowAssignment
		var id, runID, wfName, wfSource, key, state, created, updated string
		var refireAttempt int
		var failureReason, lockfile, termReason, leased, leaseExp, projectID, inputValues, targetMetadata sql.NullString
		var endedAt sql.NullString
		if err := rows.Scan(
			&id, &a.OwnerCriteriaID, &runID, &wfName, &wfSource, &lockfile,
			&key, &state, &termReason, &leased, &leaseExp,
			&created, &updated, &projectID, &inputValues, &targetMetadata,
			&a.RefireRootRunID, &refireAttempt,
			&failureReason, &endedAt,
		); err != nil {
			return nil, err
		}

		a.ID = id
		a.RunID = runID
		a.WorkflowName = wfName
		a.WorkflowSource = wfSource
		a.IdempotencyKey = key
		a.State = state
		a.CreatedAt, _ = time.Parse(store.TimestampLayout, created)
		a.UpdatedAt, _ = time.Parse(store.TimestampLayout, updated)
		a.RefireAttempt = refireAttempt
		if lockfile.Valid {
			a.LockfileSource = lockfile.String
		}
		if termReason.Valid {
			a.TerminalReason = termReason.String
		}
		if leased.Valid {
			a.LeasedCriteriaID = leased.String
		}
		if leaseExp.Valid {
			t, _ := time.Parse(store.TimestampLayout, leaseExp.String)
			a.LeaseExpiresAt = &t
		}
		scanAssignmentMaps(projectID, inputValues, targetMetadata, &a)

		c := store.RefireCandidate{
			Assignment: &a,
			Attempt:    refireAttempt,
			// refireFamilyKey expression result: the family root's run id
			// (originals key their family by their own run id).
			RootRunID: a.RefireRootRunID,
		}
		if c.RootRunID == "" {
			c.RootRunID = runID
		}
		if failureReason.Valid {
			c.FailureReason = failureReason.String
		}
		if endedAt.Valid {
			t, _ := time.Parse(store.TimestampLayout, endedAt.String)
			c.EndedAt = &t
		}
		out = append(out, c)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return out, nil
}
