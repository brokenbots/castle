package suite

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/brokenbots/castle/castle/internal/store"
)

func newRun(t *testing.T, s store.Store, r *store.Run) *store.Run {
	t.Helper()
	if r.ID == "" {
		r.ID = fmt.Sprintf("run-%08x", time.Now().UnixNano())
	}
	if err := s.CreateRun(context.Background(), r); err != nil {
		t.Fatalf("CreateRun(%s): %v", r.ID, err)
	}
	return r
}

// TestRunPagingByKeysetCursor covers ListRuns server-side paging: the
// continuation token resumes exactly where the previous page stopped, pages
// neither overlap nor skip rows, an exactly-full final page yields no dead
// token, ties break on id, and a malformed token fails with ErrInvalidCursor.
func TestRunPagingByKeysetCursor(t *testing.T) {
	runWithCtx(t, func(t *testing.T, ctx context.Context, s store.Store) {
		now := microUTC(time.Now())
		newOverseer(t, s, "ov-paging", now, nil)

		// Alternating timestamps exercise both ORDER BY branches: pairs share
		// a created_at so the id tiebreaker decides their relative order.
		base := time.Date(2026, 2, 5, 8, 30, 0, 0, time.UTC)
		wantIDs := make([]string, 0, 7)
		for i := 0; i < 7; i++ {
			id := fmt.Sprintf("run-%02d", i)
			wantIDs = append(wantIDs, id)
			status := "succeeded"
			if i%2 == 1 {
				status = "running"
			}
			newRun(t, s, &store.Run{
				ID:           id,
				OverseerID:   "ov-paging",
				WorkflowName: "wf",
				Status:       status,
				CreatedAt:    base.Add(time.Duration(i%2) * time.Minute),
			})
		}

		t.Run("no limit returns every run with no token", func(t *testing.T) {
			all, next, err := s.ListRuns(ctx, "", "", 0, "")
			if err != nil {
				t.Fatalf("list: %v", err)
			}
			if next != "" {
				t.Fatalf("next token %q, want empty without a limit", next)
			}
			if len(all) != len(wantIDs) {
				t.Fatalf("len=%d want %d", len(all), len(wantIDs))
			}
		})

		t.Run("pages resume at the cursor and cover every run once", func(t *testing.T) {
			seen := map[string]bool{}
			var order []string
			pageToken := ""
			for page := 0; ; page++ {
				rows, next, err := s.ListRuns(ctx, "", "", 3, pageToken)
				if err != nil {
					t.Fatalf("page %d: %v", page, err)
				}
				if page > 0 && len(rows) == 0 {
					t.Fatalf("page %d returned no rows for a live token; dead cursor page", page)
				}
				for _, r := range rows {
					if seen[r.ID] {
						t.Fatalf("run %s returned on two pages", r.ID)
					}
					seen[r.ID] = true
					order = append(order, r.ID)
				}
				if next == "" {
					break
				}
				if page > 100 {
					t.Fatal("paging did not terminate")
				}
				pageToken = next
			}
			if len(seen) != len(wantIDs) {
				t.Fatalf("paged traversal saw %d runs, want %d", len(seen), len(wantIDs))
			}
			// Newest first: later timestamps, then higher id within a tie.
			wantOrder := []string{"run-05", "run-03", "run-01", "run-06", "run-04", "run-02", "run-00"}
			if strings.Join(order, ",") != strings.Join(wantOrder, ",") {
				t.Fatalf("order = %v, want %v", order, wantOrder)
			}
		})

		t.Run("overseer filter pages independently", func(t *testing.T) {
			rows, _, err := s.ListRuns(ctx, "ov-paging", "", 2, "")
			if err != nil {
				t.Fatalf("filtered page: %v", err)
			}
			if len(rows) != 2 {
				t.Fatalf("filtered len = %d, want 2", len(rows))
			}
		})

		t.Run("status filter pages independently", func(t *testing.T) {
			rows, next, err := s.ListRuns(ctx, "", "running", 2, "")
			if err != nil {
				t.Fatalf("first page: %v", err)
			}
			if len(rows) != 2 || rows[0].ID != "run-05" || rows[1].ID != "run-03" {
				t.Fatalf("first page = %v, want run-05,run-03", runIDs(rows))
			}
			rows, next2, err := s.ListRuns(ctx, "", "running", 2, next)
			if err != nil {
				t.Fatalf("second page: %v", err)
			}
			if next2 != "" {
				t.Fatalf("second page returned token %q, want empty when the remainder fits", next2)
			}
			if len(rows) != 1 || rows[0].ID != "run-01" {
				t.Fatalf("second page = %v, want run-01", runIDs(rows))
			}
		})

		t.Run("malformed token is rejected", func(t *testing.T) {
			if _, _, err := s.ListRuns(ctx, "", "", 3, "not-a-cursor"); !errors.Is(err, store.ErrInvalidCursor) {
				t.Fatalf("err = %v, want ErrInvalidCursor", err)
			}
		})
	})
}

func runIDs(runs []*store.Run) []string {
	out := make([]string, 0, len(runs))
	for _, r := range runs {
		out = append(out, r.ID)
	}
	return out
}

// TestRunStartedAtLifecycle covers the started_at contract (CRI-187): a
// pending run has no start instant, the first transition to running stamps it
// and later updates never rewrite it, terminal transitions preserve it, and a
// run created already-running carries the caller-provided instant.
func TestRunStartedAtLifecycle(t *testing.T) {
	runWithCtx(t, func(t *testing.T, ctx context.Context, s store.Store) {
		now := microUTC(time.Now())
		newOverseer(t, s, "ov-started", now, nil)

		r := newRun(t, s, &store.Run{
			ID: "run-start", OverseerID: "ov-started", WorkflowName: "wf", Status: "pending",
			CreatedAt: now,
		})
		if r.StartedAt != nil {
			t.Fatalf("pending run carries StartedAt %+v", r.StartedAt)
		}

		runningAt := microUTC(now.Add(5 * time.Second))
		if err := s.UpdateRun(ctx, &store.Run{ID: "run-start", Status: "running", CurrentStep: "s1", StartedAt: &runningAt, LastSeq: 3}); err != nil {
			t.Fatalf("UpdateRun running: %v", err)
		}
		got, err := s.GetRun(ctx, "run-start")
		if err != nil {
			t.Fatalf("GetRun: %v", err)
		}
		if got.Status != "running" || got.CurrentStep != "s1" || got.LastSeq != 3 {
			t.Fatalf("running transit = %+v", got)
		}
		if got.StartedAt == nil || !got.StartedAt.Equal(runningAt) {
			t.Fatalf("StartedAt = %+v, want %v", got.StartedAt, runningAt)
		}

		// Later update: StartedAt nil means "don't stamp a new one".
		if err := s.UpdateRun(ctx, &store.Run{ID: "run-start", Status: "succeeded", CurrentStep: "s2", LastSeq: 9, EndedAt: &now}); err != nil {
			t.Fatalf("UpdateRun terminal: %v", err)
		}
		got, err = s.GetRun(ctx, "run-start")
		if err != nil {
			t.Fatalf("GetRun terminal: %v", err)
		}
		if got.StartedAt == nil || !got.StartedAt.Equal(runningAt) {
			t.Fatalf("terminal run rewrote StartedAt to %+v, want preserved %v", got.StartedAt, runningAt)
		}
		if got.EndedAt == nil || !got.EndedAt.Equal(now) {
			t.Fatalf("EndedAt = %+v, want %v", got.EndedAt, now)
		}

		// Created already-running carries the caller instant.
		pre := microUTC(now.Add(-1 * time.Minute))
		newRun(t, s, &store.Run{
			ID: "run-prealive", OverseerID: "ov-started", WorkflowName: "wf", Status: "running",
			CreatedAt: now, StartedAt: &pre,
		})
		got, err = s.GetRun(ctx, "run-prealive")
		if err != nil {
			t.Fatalf("GetRun prealive: %v", err)
		}
		if got.StartedAt == nil || !got.StartedAt.Equal(pre) {
			t.Fatalf("pre-alive StartedAt = %+v, want %v", got.StartedAt, pre)
		}
	})
}

// TestRunMetadataPromotion covers SetRunMetadata (CRI-131): non-empty values
// land, empty values leave earlier metadata intact so later partial updates
// from run.metadata events never clear it, and unknown ids are a no-op.
func TestRunMetadataPromotion(t *testing.T) {
	runWithCtx(t, func(t *testing.T, ctx context.Context, s store.Store) {
		now := microUTC(time.Now())
		newOverseer(t, s, "ov-meta", now, nil)
		newRun(t, s, &store.Run{ID: "run-meta", OverseerID: "ov-meta", WorkflowName: "wf", Status: "running", CreatedAt: now})

		if err := s.SetRunMetadata(ctx, "run-meta", "CRI-999", "https://example.com/repo", ""); err != nil {
			t.Fatalf("SetRunMetadata: %v", err)
		}
		got, err := s.GetRun(ctx, "run-meta")
		if err != nil {
			t.Fatalf("GetRun: %v", err)
		}
		if got.Ticket != "CRI-999" || got.RepoURL != "https://example.com/repo" {
			t.Fatalf("metadata = %q/%q, want CRI-999/repo", got.Ticket, got.RepoURL)
		}

		// Empty values preserve; a later update adds the missing piece.
		if err := s.SetRunMetadata(ctx, "run-meta", "", "", "https://example.com/pr"); err != nil {
			t.Fatalf("SetRunMetadata 2: %v", err)
		}
		got, err = s.GetRun(ctx, "run-meta")
		if err != nil {
			t.Fatalf("GetRun 2: %v", err)
		}
		if got.Ticket != "CRI-999" || got.RepoURL != "https://example.com/repo" || got.PRURL != "https://example.com/pr" {
			t.Fatalf("metadata after partial update = %q/%q/%q", got.Ticket, got.RepoURL, got.PRURL)
		}
		if got.Status != "running" {
			t.Fatalf("SetRunMetadata touched status: %q", got.Status)
		}

		if err := s.SetRunMetadata(ctx, "run-unknown", "ignored", "", ""); err != nil {
			t.Fatalf("SetRunMetadata unknown id: %v", err)
		}
	})
}

// TestRunNotFound pins ErrNotFound for unknown run reads.
func TestRunNotFound(t *testing.T) {
	runWithCtx(t, func(t *testing.T, ctx context.Context, s store.Store) {
		if _, err := s.GetRun(ctx, "run-missing"); err != store.ErrNotFound {
			t.Fatalf("GetRun(unknown) = %v, want ErrNotFound", err)
		}
	})
}

// TestRunVariableScopeRoundTrip covers W04: snapshot persists, reads back
// exactly, and reads "" before any snapshot exists.
func TestRunVariableScopeRoundTrip(t *testing.T) {
	runWithCtx(t, func(t *testing.T, ctx context.Context, s store.Store) {
		now := microUTC(time.Now())
		newOverseer(t, s, "ov-vars", now, nil)
		newRun(t, s, &store.Run{ID: "run-vars", OverseerID: "ov-vars", WorkflowName: "wf", Status: "running", CreatedAt: now})

		scope, err := s.GetRunVariableScope(ctx, "run-vars")
		if err != nil || scope != "" {
			t.Fatalf("fresh scope = %q err %v, want empty", scope, err)
		}
		want := `{"count":2,"values":{"a":"1","b":"2"}}`
		if err := s.SetRunVariableScope(ctx, "run-vars", want); err != nil {
			t.Fatalf("SetRunVariableScope: %v", err)
		}
		got, err := s.GetRunVariableScope(ctx, "run-vars")
		if err != nil || got != want {
			t.Fatalf("scope = %q err %v, want %q", got, err, want)
		}
	})
}
