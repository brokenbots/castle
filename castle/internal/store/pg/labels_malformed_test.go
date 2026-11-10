package pg_test

import (
	"context"
	"database/sql"
	"testing"
	"time"

	"github.com/brokenbots/castle/castle/internal/store"
	"github.com/brokenbots/castle/castle/internal/store/pg"
	"github.com/brokenbots/castle/castle/internal/store/pg/pgtest"
	"github.com/brokenbots/castle/castle/internal/store/reap"
)

// TestReapNeverStartedRunsPGBadLabelsContinues pins the pg label read's
// failure mode: one malformed overseers.labels row must not abort the whole
// never-started reaper pass. A malformed column reads as "no labels", which
// makes the executor label-ineligible for the candidate's required label and
// lets the undispatchable work reach the created_never_started verdict,
// exactly like the sqlite json_valid guard — the two engines must not differ.
func TestReapNeverStartedRunsPGBadLabelsContinues(t *testing.T) {
	dsn := pgtest.DSN(t)
	s, err := pg.Open(dsn)
	if err != nil {
		t.Fatalf("pg.Open: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	raw, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatalf("open out-of-band handle: %v", err)
	}
	t.Cleanup(func() { _ = raw.Close() })

	const (
		siblingRunID = "run-pgbad-sibling"
		runID        = "run-pgbad-queued"
		overseerID   = "oa-broken"
	)
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Microsecond)
	createdBefore := now.Add(-10 * time.Minute)
	if err := s.CreateOverseer(ctx, &store.Overseer{
		ID: overseerID, Name: overseerID, TokenHash: overseerID, Status: "online",
		Labels:    map[string]string{"gpu": "true"},
		CreatedAt: now, LastSeenAt: now,
	}); err != nil {
		t.Fatalf("CreateOverseer: %v", err)
	}

	// Busy-lane sibling: the executor holds the lane with a running run, so
	// only the label verdict decides the candidate's fate.
	sibling, created, err := s.CreateWorkflowAssignment(ctx, &store.WorkflowAssignment{
		OwnerCriteriaID: "owner-sib", RunID: siblingRunID, WorkflowName: "wf-sib",
		WorkflowSource: "inline", IdempotencyKey: "key-pgbad-sib",
		State:     store.WorkflowAssignmentStateQueued,
		CreatedAt: now.Add(-time.Hour), UpdatedAt: now.Add(-time.Hour),
	})
	if err != nil || !created {
		t.Fatalf("CreateWorkflowAssignment sibling: err=%v created=%v", err, created)
	}
	if _, _, err := s.LeaseWorkflowAssignment(ctx, overseerID, map[string]string{}, now, time.Minute); err != nil {
		t.Fatalf("LeaseWorkflowAssignment: %v", err)
	}
	siblingRun, err := s.GetRun(ctx, siblingRunID)
	if err != nil {
		t.Fatalf("GetRun sibling: %v", err)
	}
	startedAt := now
	siblingRun.Status = "running"
	siblingRun.StartedAt = &startedAt
	if err := s.UpdateRun(ctx, siblingRun); err != nil {
		t.Fatalf("UpdateRun sibling: %v", err)
	}

	// The candidate requires a label the executor would satisfy if its
	// labels column were readable; created before createdBefore so it
	// qualifies for the reap window.
	candidate, created, err := s.CreateWorkflowAssignment(ctx, &store.WorkflowAssignment{
		OwnerCriteriaID: "owner-queued", RunID: runID, WorkflowName: "wf-queued",
		WorkflowSource: "inline", IdempotencyKey: "key-pgbad-queued",
		State:     store.WorkflowAssignmentStateQueued,
		Labels:    map[string]string{"gpu": "true"},
		CreatedAt: now.Add(-30 * time.Minute), UpdatedAt: now.Add(-30 * time.Minute),
	})
	if err != nil || !created {
		t.Fatalf("CreateWorkflowAssignment candidate: err=%v created=%v", err, created)
	}

	// Poison exactly the executor's labels text.
	if _, err := raw.Exec(`UPDATE overseers SET labels = '{not json' WHERE id = $1`, overseerID); err != nil {
		t.Fatalf("poison overseer labels: %v", err)
	}

	reaped, err := s.ReapNeverStartedRuns(ctx, now, createdBefore, now.Add(-60*time.Second), now.Add(-5*time.Minute))
	if err != nil {
		t.Fatalf("ReapNeverStartedRuns with malformed labels row: %v", err)
	}
	if len(reaped) != 1 || reaped[0].RunID != runID || reaped[0].Status != "failed" || reaped[0].Reason != reap.ReasonCreatedNeverStarted {
		t.Fatalf("reaped = %+v, want [%s failed/%s]", reaped, runID, reap.ReasonCreatedNeverStarted)
	}
	run, err := s.GetRun(ctx, runID)
	if err != nil {
		t.Fatalf("GetRun candidate: %v", err)
	}
	if run.Status != "failed" || run.FailureReason != reap.ReasonCreatedNeverStarted || run.StartedAt != nil {
		t.Fatalf("candidate run = %+v, want failed/created_never_started unstarted", run)
	}
	a, err := s.GetWorkflowAssignment(ctx, candidate.ID)
	if err != nil {
		t.Fatalf("GetWorkflowAssignment candidate: %v", err)
	}
	if a.State != store.WorkflowAssignmentStateTerminal || a.TerminalReason != reap.ReasonCreatedNeverStarted {
		t.Fatalf("candidate assignment = state %q reason %q, want terminal/created_never_started", a.State, a.TerminalReason)
	}

	// Containment: the busy sibling was untouched by the pass.
	siblingRun, err = s.GetRun(ctx, siblingRunID)
	if err != nil {
		t.Fatalf("GetRun sibling after: %v", err)
	}
	if siblingRun.Status != "running" || siblingRun.StartedAt == nil {
		t.Fatalf("sibling run = %+v, want running untouched", siblingRun)
	}
	if _, err := s.GetWorkflowAssignment(ctx, sibling.ID); err != nil {
		t.Fatalf("GetWorkflowAssignment sibling: %v", err)
	}
}
