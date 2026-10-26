package suite

import (
	"context"
	"testing"
	"time"

	"github.com/brokenbots/castle/castle/internal/store"
)

// TestListRunsByProject pins the project console listing (KB-340): the
// page is the project's runs newest-first, includeUnscoped adds the legacy
// unscoped runs (the admin arm of the console view), unknown/empty projects
// are the empty set — not an error (the caller surfaces why-empty) — and
// ordering, filtering and cursor semantics match ListRuns exactly.
func TestListRunsByProject(t *testing.T) {
	runWithCtx(t, func(t *testing.T, ctx context.Context, s store.Store) {
		now := microUTC(time.Now())
		newProject(t, ctx, s, "proj-runs", store.ProjectMembershipOpen, now)
		newProject(t, ctx, s, "proj-other", store.ProjectMembershipOpen, now)

		aProj, _, err := s.CreateWorkflowAssignment(ctx, &store.WorkflowAssignment{
			OwnerCriteriaID: "agent-member",
			WorkflowName:    "wf",
			WorkflowSource:  "workflow",
			IdempotencyKey:  "key-proj",
			ProjectID:       "proj-runs",
		})
		if err != nil {
			t.Fatalf("scoped assignment: %v", err)
		}
		aOther, _, err := s.CreateWorkflowAssignment(ctx, &store.WorkflowAssignment{
			OwnerCriteriaID: "agent-other",
			WorkflowName:    "wf",
			WorkflowSource:  "workflow",
			IdempotencyKey:  "key-other",
			ProjectID:       "proj-other",
		})
		if err != nil {
			t.Fatalf("other assignment: %v", err)
		}
		newOverseer(t, s, "agent-owner", now, nil)
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
			ID:           "run-unscoped",
			OverseerID:   "agent-owner",
			WorkflowName: "wf",
			Status:       "running",
			CreatedAt:    now,
		})

		t.Run("project only, sibling and unscoped invisible", func(t *testing.T) {
			runs, _, err := s.ListRunsByProject(ctx, "proj-runs", false, "", 0, "")
			if err != nil {
				t.Fatalf("ListRunsByProject: %v", err)
			}
			got := map[string]bool{}
			for _, r := range runs {
				got[r.ID] = true
				if r.ProjectID != "proj-runs" {
					t.Fatalf("project page leaked run %s of %q", r.ID, r.ProjectID)
				}
			}
			if !got[aProj.RunID] || len(runs) != 1 {
				t.Fatalf("project page = %v, want only %s", got, aProj.RunID)
			}
			if got[aOther.RunID] || got["run-unscoped"] {
				t.Fatalf("project page leaked other-tenant runs: %v", got)
			}
		})

		t.Run("include_unscoped adds the null-project arm", func(t *testing.T) {
			runs, _, err := s.ListRunsByProject(ctx, "proj-runs", true, "", 0, "")
			if err != nil {
				t.Fatalf("ListRunsByProject(include): %v", err)
			}
			got := map[string]bool{}
			for _, r := range runs {
				got[r.ID] = true
			}
			// The arm carries the project's run plus BOTH legacy unscoped
			// runs (the assignment-minted one and the direct one); the
			// sibling project's run stays invisible.
			if !got[aProj.RunID] || !got[aUnscoped.RunID] || !got["run-unscoped"] || len(got) != 3 {
				t.Fatalf("unscoped arm = %v", got)
			}
			if got[aOther.RunID] {
				t.Fatalf("unscoped arm leaked sibling project run %s", aOther.RunID)
			}
		})

		t.Run("unknown and empty projects are the empty set", func(t *testing.T) {
			for _, id := range []string{"no-such-project", ""} {
				runs, _, err := s.ListRunsByProject(ctx, id, false, "", 0, "")
				if err != nil || len(runs) != 0 {
					t.Fatalf("project %q: runs=%v err=%v", id, runs, err)
				}
			}
		})

		t.Run("status filter applies", func(t *testing.T) {
			if err := s.UpdateRun(ctx, &store.Run{ID: aProj.RunID, Status: "running", CurrentStep: "s1"}); err != nil {
				t.Fatalf("status update: %v", err)
			}
			running, _, err := s.ListRunsByProject(ctx, "proj-runs", false, "running", 0, "")
			if err != nil || len(running) != 1 || running[0].ID != aProj.RunID {
				t.Fatalf("status=running: runs=%v err=%v", running, err)
			}
			succeeded, _, err := s.ListRunsByProject(ctx, "proj-runs", false, "succeeded", 0, "")
			if err != nil || len(succeeded) != 0 {
				t.Fatalf("status=succeeded: runs=%v err=%v", succeeded, err)
			}
		})

		t.Run("paging mirrors ListRuns, unscoped arm included", func(t *testing.T) {
			// Three runs in the widened set: page through all of them and
			// expect the token to die exactly after the last run.
			seen := map[string]bool{}
			prevID := ""
			token := ""
			for i := range 3 {
				page, next, err := s.ListRunsByProject(ctx, "proj-runs", true, "", 1, token)
				if err != nil || len(page) != 1 {
					t.Fatalf("page (token=%q): runs=%v err=%v", token, page, err)
				}
				if page[0].ID == prevID {
					t.Fatalf("cursor repeated run %s", prevID)
				}
				prevID = page[0].ID
				seen[page[0].ID] = true
				if i == 2 && next != "" {
					t.Fatalf("page3 issued another token %q — set larger than 3", next)
				}
				if i < 2 && next == "" {
					t.Fatalf("page%d ended the list early", i+1)
				}
				token = next
			}
			if !seen[aProj.RunID] || !seen[aUnscoped.RunID] || !seen["run-unscoped"] || len(seen) != 3 {
				t.Fatalf("paged set = %v", seen)
			}
			if _, _, err := s.ListRunsByProject(ctx, "proj-runs", true, "", 1, "not-a-cursor"); err == nil {
				t.Fatal("malformed page token accepted")
			}
		})
	})
}