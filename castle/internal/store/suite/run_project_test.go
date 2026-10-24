package suite

import (
	"context"
	"testing"
	"time"

	"github.com/brokenbots/castle/castle/internal/store"
)

// TestRunProjectStamp pins project stamping of runs at CreateRun (KB-324 S1):
// assignment-dispatched runs inherit the assignment's project, standalone
// runs stay unscoped, an explicitly stamped run round-trips, and a stamp
// naming a project that does not exist is refused. Read visibility (who may
// read which run) is the lease predicate, covered separately.
func TestRunProjectStamp(t *testing.T) {
	runWithCtx(t, func(t *testing.T, ctx context.Context, s store.Store) {
		now := microUTC(time.Now())
		newProject(t, ctx, s, "proj-stamp", store.ProjectMembershipOpen, now)

		t.Run("assignment run inherits the assignment project", func(t *testing.T) {
			a, created, err := s.CreateWorkflowAssignment(ctx, &store.WorkflowAssignment{
				OwnerCriteriaID: "ov-stamp",
				WorkflowName:    "wf",
				WorkflowSource:  "workflow",
				IdempotencyKey:  "key-stamped",
				ProjectID:       "proj-stamp",
			})
			if err != nil || !created {
				t.Fatalf("CreateWorkflowAssignment: created=%v err=%v", created, err)
			}
			run, err := s.GetRun(ctx, a.RunID)
			if err != nil {
				t.Fatalf("GetRun: %v", err)
			}
			if run.ProjectID != "proj-stamp" {
				t.Fatalf("run project = %q, want proj-stamp", run.ProjectID)
			}
		})

		t.Run("unscoped assignment run stays unscoped", func(t *testing.T) {
			a, created, err := s.CreateWorkflowAssignment(ctx, &store.WorkflowAssignment{
				OwnerCriteriaID: "ov-stamp",
				WorkflowName:    "wf",
				WorkflowSource:  "workflow",
				IdempotencyKey:  "key-unscoped",
			})
			if err != nil || !created {
				t.Fatalf("CreateWorkflowAssignment: created=%v err=%v", created, err)
			}
			run, err := s.GetRun(ctx, a.RunID)
			if err != nil {
				t.Fatalf("GetRun: %v", err)
			}
			if run.ProjectID != "" {
				t.Fatalf("unscoped run project = %q, want empty", run.ProjectID)
			}
		})

		t.Run("explicit project stamp round-trips", func(t *testing.T) {
			newOverseer(t, s, "ov-stamp", now, nil)
			newRun(t, s, &store.Run{
				ID:           "run-stamped",
				OverseerID:   "ov-stamp",
				WorkflowName: "wf",
				Status:       "pending",
				CreatedAt:    now,
				ProjectID:    "proj-stamp",
			})
			run, err := s.GetRun(ctx, "run-stamped")
			if err != nil {
				t.Fatalf("GetRun: %v", err)
			}
			if run.ProjectID != "proj-stamp" {
				t.Fatalf("run project = %q, want proj-stamp", run.ProjectID)
			}
		})

		t.Run("unknown project refused", func(t *testing.T) {
			if err := s.CreateRun(ctx, &store.Run{
				ID:           "run-unknown-project",
				WorkflowName: "wf",
				Status:       "pending",
				CreatedAt:    now,
				ProjectID:    "proj-missing",
			}); err == nil {
				t.Fatal("CreateRun with unknown project_id should fail")
			}
		})
	})
}