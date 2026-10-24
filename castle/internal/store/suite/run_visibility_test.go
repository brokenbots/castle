package suite

import (
	"context"
	"testing"
	"time"

	"github.com/brokenbots/castle/castle/internal/store"
)

// TestRunVisibilityScope pins the membership read scope (KB-324 S1): a
// criteria caller sees the runs of its memberships, the runs it leases and
// the runs it owns via a minting assignment — nothing else. Unscoped runs
// stay invisible to criteria callers (console-only) and a denied or missing
// run reads as "invisible", which the RPC layer surfaces as NotFound.
func TestRunVisibilityScope(t *testing.T) {
	runWithCtx(t, func(t *testing.T, ctx context.Context, s store.Store) {
		now := microUTC(time.Now())
		newProject(t, ctx, s, "proj-vis", store.ProjectMembershipOpen, now)
		if err := s.AddProjectMember(ctx, &store.ProjectMember{ProjectID: "proj-vis", MemberID: "agent-member", Kind: store.ProjectMemberKindAI, CreatedAt: now}); err != nil {
			t.Fatalf("AddProjectMember: %v", err)
		}
		// Assignments mint runs: member owns a project run, owner (no
		// membership) owns one too, and one lands unscoped.
		aMember, _, err := s.CreateWorkflowAssignment(ctx, &store.WorkflowAssignment{
			OwnerCriteriaID: "agent-member",
			WorkflowName:    "wf",
			WorkflowSource:  "workflow",
			IdempotencyKey:  "key-member",
			ProjectID:       "proj-vis",
		})
		if err != nil {
			t.Fatalf("member assignment: %v", err)
		}
		aOwner, _, err := s.CreateWorkflowAssignment(ctx, &store.WorkflowAssignment{
			OwnerCriteriaID: "agent-owner",
			WorkflowName:    "wf",
			WorkflowSource:  "workflow",
			IdempotencyKey:  "key-owner",
			ProjectID:       "proj-vis",
		})
		if err != nil {
			t.Fatalf("owner assignment: %v", err)
		}
		aUnscoped, _, err := s.CreateWorkflowAssignment(ctx, &store.WorkflowAssignment{
			OwnerCriteriaID: "agent-stranger",
			WorkflowName:    "wf",
			WorkflowSource:  "workflow",
			IdempotencyKey:  "key-unscoped",
		})
		if err != nil {
			t.Fatalf("unscoped assignment: %v", err)
		}
		// A standalone run leased to agent-owner (real overseer FK target).
		newOverseer(t, s, "agent-owner", now, nil)
		newRun(t, s, &store.Run{
			ID:           "run-leased",
			OverseerID:   "agent-owner",
			WorkflowName: "wf",
			Status:       "running",
			CreatedAt:    now,
		})

		t.Run("member sees membership run and own ownership", func(t *testing.T) {
			runs, _, err := s.ListRunsVisibleTo(ctx, "agent-member", "", 0, "")
			if err != nil {
				t.Fatalf("ListRunsVisibleTo: %v", err)
			}
			got := map[string]bool{}
			for _, r := range runs {
				got[r.ID] = true
			}
			if !got[aMember.RunID] {
				t.Errorf("member cannot see own assignment run %s", aMember.RunID)
			}
			if !got[aOwner.RunID] {
				t.Errorf("member cannot see the project run it belongs to")
			}
			if got[aUnscoped.RunID] {
				t.Errorf("member saw an unscoped run through membership scope")
			}
			visible, err := s.IsRunVisibleTo(ctx, aOwner.RunID, "agent-member")
			if err != nil || !visible {
				t.Errorf("IsRunVisibleTo(owner run): (%v, %v)", visible, err)
			}
		})

		t.Run("non-member denied without existence leak", func(t *testing.T) {
			vis, err := s.IsRunVisibleTo(ctx, aMember.RunID, "agent-stranger")
			if err != nil || vis {
				t.Fatalf("stranger IsRunVisibleTo(member run): (%v, %v)", vis, err)
			}
			vis, err = s.IsRunVisibleTo(ctx, aUnscoped.RunID, "agent-member")
			if err != nil || vis {
				t.Fatalf("member IsRunVisibleTo(unscoped): (%v, %v)", vis, err)
			}
			vis, err = s.IsRunVisibleTo(ctx, "run-no-such-thing", "agent-member")
			if err != nil || vis {
				t.Fatalf("IsRunVisibleTo(missing): (%v, %v)", vis, err)
			}
		})

		t.Run("lessee sees its leased run", func(t *testing.T) {
			visible, err := s.IsRunVisibleTo(ctx, "run-leased", "agent-owner")
			if err != nil || !visible {
				t.Fatalf("lessee visibility: (%v, %v)", visible, err)
			}
		})

		t.Run("status and paging mirror ListRuns", func(t *testing.T) {
			runs, _, err := s.ListRunsVisibleTo(ctx, "agent-owner", "running", 0, "")
			if err != nil || len(runs) != 1 || runs[0].ID != "run-leased" {
				t.Fatalf("status filter: runs=%v err=%v", runs, err)
			}
			page1, next, err := s.ListRunsVisibleTo(ctx, "agent-member", "", 1, "")
			if err != nil || len(page1) != 1 || next == "" {
				t.Fatalf("page1: %v next=%q err=%v", page1, next, err)
			}
			page2, next2, err := s.ListRunsVisibleTo(ctx, "agent-member", "", 1, next)
			if err != nil || len(page2) != 1 || next2 != "" {
				t.Fatalf("page2: %v next=%q err=%v", page2, next2, err)
			}
		})
	})
}
