package pg_test

import (
	"context"
	"database/sql"
	"testing"
	"time"

	"github.com/brokenbots/castle/castle/internal/store"
	"github.com/brokenbots/castle/castle/internal/store/pg"
	"github.com/brokenbots/castle/castle/internal/store/pg/pgtest"
)

// TestExpireLeasesReassertRunStatus is the regression test for the
// check-then-update hole in ExpireWorkflowAssignmentLeases that the
// candidate scan cannot close by itself: under READ COMMITTED each
// statement sees a fresh snapshot, and a run's status is written
// unguarded, so a run that starts (pending → running) between the
// candidate scan and the expiry UPDATE — inside the very transaction —
// must still not be requeued. The hook seam fires exactly in that window,
// making the interleaving deterministic.
//
// The out-of-band UPDATE must use a second connection, not the store's
// pool: at hook time the expiry transaction has only SELECTed, so it holds
// no row lock on runs and the competing write cannot deadlock.
func TestExpireLeasesReassertRunStatus(t *testing.T) {
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
		runID      = "run-backstop"
		overseerID = "oa-backstop"
	)
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Microsecond)
	if err := s.CreateOverseer(ctx, &store.Overseer{ID: overseerID, Name: overseerID, TokenHash: overseerID, Status: "online", CreatedAt: now, LastSeenAt: now}); err != nil {
		t.Fatalf("CreateOverseer: %v", err)
	}
	// CreateWorkflowAssignment creates the pending run atomically.
	a, created, err := s.CreateWorkflowAssignment(ctx, &store.WorkflowAssignment{
		OwnerCriteriaID: "owner-back", RunID: runID, WorkflowName: "wf-back",
		WorkflowSource: "inline", LockfileSource: "lock-back", IdempotencyKey: "key-back",
		State: store.WorkflowAssignmentStateQueued, CreatedAt: now, UpdatedAt: now,
	})
	if err != nil || !created {
		t.Fatalf("CreateWorkflowAssignment: err=%v created=%v", err, created)
	}

	// Agent A leases with an already-expired lease so expiry is driven by an
	// explicit now; no sleeping.
	if _, err := s.LeaseWorkflowAssignment(ctx, overseerID, map[string]string{}, now, -time.Second); err != nil {
		t.Fatalf("lease to A: %v", err)
	}

	// In the hook window — the candidate scan sees the run as still pending,
	// the expiry UPDATE runs after — start the run on a separate connection.
	restore := pg.InstallExpireLeasesHook(func() {
		if _, err := raw.Exec(`UPDATE runs SET status='running', started_at=$1 WHERE id=$2`, now, runID); err != nil {
			t.Errorf("out-of-band run start in expiry window: %v", err)
		}
	})
	defer restore()

	ids, err := s.ExpireWorkflowAssignmentLeases(ctx, now.Add(time.Minute))
	if err != nil {
		t.Fatalf("ExpireWorkflowAssignmentLeases: %v", err)
	}
	for _, id := range ids {
		if id == a.ID {
			t.Fatalf("expiry requeued the assignment whose run is running: %v", ids)
		}
	}

	got, err := s.GetWorkflowAssignment(ctx, a.ID)
	if err != nil {
		t.Fatalf("GetWorkflowAssignment: %v", err)
	}
	if got.State != store.WorkflowAssignmentStateLeased || got.LeasedCriteriaID != overseerID {
		t.Fatalf("assignment was cleared despite started run: state=%q holder=%q", got.State, got.LeasedCriteriaID)
	}
	if got.LeaseExpiresAt == nil || !got.LeaseExpiresAt.Equal(now.Add(-time.Second)) {
		t.Fatalf("original expired lease was clobbered: %+v", got.LeaseExpiresAt)
	}

	run, err := s.GetRun(ctx, runID)
	if err != nil {
		t.Fatalf("GetRun: %v", err)
	}
	if run.Status != "running" {
		t.Fatalf("run status = %q, want running (the live run must keep its state)", run.Status)
	}
	if run.OverseerID != overseerID {
		t.Fatalf("live run's overseer_id = %q, want %q (must not be blanked)", run.OverseerID, overseerID)
	}
}
