package suite

import (
	"context"
	"testing"
	"time"

	"github.com/brokenbots/castle/castle/internal/store"
)

// TestListRunsUnscoped pins the anonymous-read store surface (KB-326 S3):
// ListRunsUnscoped mirrors ListRuns' paging and filters but only ever
// returns legacy unscoped runs — the anonymous listing may not reveal that
// project-scoped runs exist.
func TestListRunsUnscoped(t *testing.T) {
	runWithCtx(t, func(t *testing.T, ctx context.Context, s store.Store) {
		now := microUTC(time.Now())
		newProject(t, ctx, s, "proj-unscoped", store.ProjectMembershipOpen, now)
		if err := s.AddProjectMember(ctx, &store.ProjectMember{ProjectID: "proj-unscoped", MemberID: "agent-member", Kind: store.ProjectMemberKindAI, CreatedAt: now}); err != nil {
			t.Fatalf("AddProjectMember: %v", err)
		}
		aScoped, _, err := s.CreateWorkflowAssignment(ctx, &store.WorkflowAssignment{
			OwnerCriteriaID: "agent-member",
			WorkflowName:    "wf",
			WorkflowSource:  "workflow",
			IdempotencyKey:  "key-scoped",
			ProjectID:       "proj-unscoped",
		})
		if err != nil {
			t.Fatalf("scoped assignment: %v", err)
		}
		newOverseer(t, s, "agent-owner", now, nil)
		if _, _, err = s.CreateWorkflowAssignment(ctx, &store.WorkflowAssignment{
			OwnerCriteriaID: "agent-owner",
			WorkflowName:    "wf",
			WorkflowSource:  "workflow",
			IdempotencyKey:  "key-owner",
			ProjectID:       "proj-unscoped",
		}); err != nil {
			t.Fatalf("owner assignment: %v", err)
		}
		aUnscoped, _, err := s.CreateWorkflowAssignment(ctx, &store.WorkflowAssignment{
			OwnerCriteriaID: "agent-owner",
			WorkflowName:    "wf",
			WorkflowSource:  "workflow",
			IdempotencyKey:  "key-unscoped",
		})
		if err != nil {
			t.Fatalf("unscoped assignment: %v", err)
		}
		// Direct CreateRun rows are unscoped by construction.
		newRun(t, s, &store.Run{
			ID:           "run-minted",
			OverseerID:   "agent-owner",
			WorkflowName: "wf",
			Status:       "running",
			CreatedAt:    now,
		})

		t.Run("unscoped only, scoped invisible", func(t *testing.T) {
			runs, _, err := s.ListRunsUnscoped(ctx, "", "", 0, "")
			if err != nil {
				t.Fatalf("ListRunsUnscoped: %v", err)
			}
			got := map[string]bool{}
			for _, r := range runs {
				got[r.ID] = true
			}
			if !got[aUnscoped.RunID] || !got["run-minted"] {
				t.Fatalf("unscoped listing misses unscoped runs: %v", got)
			}
			if got[aScoped.RunID] {
				t.Fatalf("unscoped listing leaked the scoped run %s", aScoped.RunID)
			}
			// Parity with the unfiltered whole-space listing.
			allRuns, _, err := s.ListRuns(ctx, "", "", 0, "")
			if err != nil {
				t.Fatalf("ListRuns: %v", err)
			}
			if len(allRuns) != 4 {
				t.Fatalf("ListRuns returned %d runs, want 4 (2 scoped + 2 unscoped)", len(allRuns))
			}
			for _, r := range allRuns {
				if r.ProjectID != "" && got[r.ID] {
					t.Fatalf("scoped run %s also in unscoped listing", r.ID)
				}
			}
		})

		t.Run("overseer and status filters apply", func(t *testing.T) {
			// Only the direct CreateRun row carries agent-owner as its
			// overseer; assignment-minted runs own via owner_criteria_id
			// instead (their overseer lands at lease time).
			runs, _, err := s.ListRunsUnscoped(ctx, "agent-owner", "", 0, "")
			if err != nil || len(runs) != 1 || runs[0].ID != "run-minted" {
				t.Fatalf("overseer filter: runs=%v err=%v", runs, err)
			}
			runs, _, err = s.ListRunsUnscoped(ctx, "", "running", 0, "")
			if err != nil || len(runs) != 1 || runs[0].ID != "run-minted" {
				t.Fatalf("status filter: runs=%v err=%v", runs, err)
			}
			runs, _, err = s.ListRunsUnscoped(ctx, "no-such-agent", "", 0, "")
			if err != nil || len(runs) != 0 {
				t.Fatalf("unknown overseer: runs=%v err=%v", runs, err)
			}
		})

		t.Run("paging mirrors ListRuns", func(t *testing.T) {
			page1, next, err := s.ListRunsUnscoped(ctx, "", "", 1, "")
			if err != nil || len(page1) != 1 || next == "" {
				t.Fatalf("page1: runs=%v next=%q err=%v", page1, next, err)
			}
			page2, next2, err := s.ListRunsUnscoped(ctx, "", "", 1, next)
			if err != nil || len(page2) != 1 || next2 != "" {
				t.Fatalf("page2: runs=%v next=%q err=%v", page2, next2, err)
			}
			if page1[0].ID == page2[0].ID {
				t.Fatalf("cursor repeated the same run: %v then %v", page1[0].ID, page2[0].ID)
			}
			if _, _, err := s.ListRunsUnscoped(ctx, "", "", 1, "not-a-cursor"); err == nil {
				t.Fatal("malformed page token accepted")
			}
		})
	})
}
