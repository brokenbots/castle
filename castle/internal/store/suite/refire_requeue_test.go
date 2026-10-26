package suite

// KB-343 defect-2 acceptance: a dispatch-layer reap (created_never_started)
// is a transport condition, not a provider stall — the refire pass requeues
// the SAME attempt under the policy's backoff and the family attempt budget
// stays untouched; only a real execution failure consumes a slot.

import (
	"context"
	"testing"
	"time"

	"github.com/brokenbots/castle/castle/internal/store"
	"github.com/brokenbots/castle/castle/internal/store/reap"
)

// TestRefireRequeueCreatedNeverStarted drives the full cycle with the real
// machinery: submit → never-started reap → policy pass (deferred under
// backoff) → policy pass (requeue, budget untouched) → a real stall
// execution failure → the pass refires with the next budget slot.
func TestRefireRequeueCreatedNeverStarted(t *testing.T) {
	runWithCtx(t, func(t *testing.T, ctx context.Context, s store.Store) {
		f := newRefireFixture(t, ctx, s)
		f.policy.Backoff = 30 * time.Second
		f.setPolicy("", "")

		now := f.now
		a := f.submit("owner-requeue", "kb-343-requeue")

		// The dispatch layer never leases: the run is reaped as
		// created_never_started (KB-233 rule 2) — the real writer, not a
		// hand stamp.
		reaped, err := s.ReapNeverStartedRuns(ctx, now, now.Add(time.Minute))
		if err != nil || len(reaped) != 1 || reaped[0].RunID != a.RunID {
			t.Fatalf("reap = %v, %v; want one created_never_started verdict for %s", reaped, err, a.RunID)
		}
		run, err := s.GetRun(ctx, a.RunID)
		if err != nil {
			t.Fatal(err)
		}
		if run.Status != "failed" || run.FailureReason != reap.ReasonCreatedNeverStarted || run.StartedAt != nil {
			t.Fatalf("reaped run = %+v, want failed/created_never_started/started_at NULL", run)
		}
		endedAt := *run.EndedAt

		// Backoff pending: deferred, nothing restored yet.
		outcomes := f.refireAt(endedAt)
		if got := dispositionCounts(t, outcomes); got[store.RefireDispositionDeferred] != 1 {
			t.Fatalf("first pass = %v, want deferred", got)
		}

		// The class fires (failing-first RED: without the dispatch-transport
		// class this pass refired and minted a successor, burning attempt 2
		// of 3 on a transport condition) — requeue with the SAME attempt.
		outcomes = f.refireAt(endedAt.Add(31 * time.Second))
		got := dispositionCounts(t, outcomes)
		if got[store.RefireDispositionRequeue] != 1 || got[store.RefireDispositionRefire] != 0 {
			t.Fatalf("second pass = %v (%+v), want one requeue and zero refires", got, outcomes)
		}
		o := outcomes[requeueIndex(t, outcomes)]
		if o.RunID != a.RunID || o.RefireAttempt != 1 || o.RefireRunID != "" || o.RefireAssignmentID != "" {
			t.Fatalf("requeue outcome = %+v, want the same attempt, no mint", o)
		}

		// The run is back in the leasable bucket: pending, no lease attach,
		// created_at refreshed so the reaper's window restarts.
		run, err = s.GetRun(ctx, a.RunID)
		if err != nil {
			t.Fatal(err)
		}
		if run.Status != "pending" || run.OverseerID != "" || run.StartedAt != nil || run.EndedAt != nil || run.FailureReason != "" {
			t.Fatalf("requeued run = %+v, want pending with the reap stamp fallen away", run)
		}
		if !run.CreatedAt.After(endedAt) {
			t.Fatalf("requeued run created_at %v not refreshed after reap %v", run.CreatedAt, endedAt)
		}
		restored, err := s.GetWorkflowAssignment(ctx, a.ID)
		if err != nil {
			t.Fatal(err)
		}
		if restored.State != store.WorkflowAssignmentStateQueued || restored.TerminalReason != "" ||
			restored.LeasedCriteriaID != "" || restored.LeaseExpiresAt != nil {
			t.Fatalf("restored assignment = %+v, want queued with dead lease and terminal stamp cleared", restored)
		}
		if restored.RefireAttempt != 1 {
			t.Fatalf("restored attempt lineage = %d, want 1 (the original attempt; budget untouched by requeues)", restored.RefireAttempt)
		}

		// Now a REAL execution failure: the family budget starts counting.
		f.failLikeAgentWithReason(a, "run failed", "provider stall: upstream 500")
		outcomes = f.refireAt(endedAt.Add(90 * time.Second))
		got = dispositionCounts(t, outcomes)
		if got[store.RefireDispositionRefire] != 1 {
			t.Fatalf("stall pass = %v (%+v), want refire", got, outcomes)
		}
		o = outcomes[refireIndex(t, outcomes)]
		if o.RefireAttempt != 2 {
			t.Fatalf("minted attempt = %d, want 2 (the requeue cycle consumed no slot)", o.RefireAttempt)
		}
	})
}

// TestRefireRequeueBudgetNotBurnedByRepeatCycles: the requeue cycle can run
// forever without advancing the family attempt — every created_never_started
// reap after a restore classifies again as requeue, and refire_attempt stays
// at the original value after every cycle.
func TestRefireRequeueBudgetNotBurnedByRepeatCycles(t *testing.T) {
	runWithCtx(t, func(t *testing.T, ctx context.Context, s store.Store) {
		f := newRefireFixture(t, ctx, s)
		f.policy.Backoff = time.Second
		f.setPolicy("", "")

		a := f.submit("owner-repeat", "kb-343-repeat")
		// Cursor: each cycle reaps at `at`, requeues at `at+2s` (which
		// refreshes run.created_at), and the next reap's window clears it.
		at := f.now.Add(time.Minute)
		for cycle := 1; cycle <= 3; cycle++ {
			reaped, err := s.ReapNeverStartedRuns(ctx, at, at)
			if err != nil || len(reaped) != 1 || reaped[0].RunID != a.RunID {
				t.Fatalf("cycle %d: reap = %v, %v; want one verdict for %s", cycle, reaped, err, a.RunID)
			}
			outcomes := f.refireAt(at.Add(2 * time.Second))
			got := dispositionCounts(t, outcomes)
			if got[store.RefireDispositionRequeue] != 1 {
				t.Fatalf("cycle %d pass = %v (%+v), want requeue", cycle, got, outcomes)
			}
			if a2, gErr := s.GetWorkflowAssignment(ctx, a.ID); gErr != nil || a2.RefireAttempt != 1 {
				t.Fatalf("cycle %d: refire_attempt = %d/%v, want the original 1 after every requeue cycle", cycle, a2.RefireAttempt, gErr)
			}
			at = at.Add(time.Minute)
		}
	})
}

func requeueIndex(t *testing.T, outcomes []store.RefireOutcome) int {
	t.Helper()
	for i, o := range outcomes {
		if o.Disposition == store.RefireDispositionRequeue {
			return i
		}
	}
	t.Fatal("no requeue outcome")
	return -1
}

func refireIndex(t *testing.T, outcomes []store.RefireOutcome) int {
	t.Helper()
	for i, o := range outcomes {
		if o.Disposition == store.RefireDispositionRefire {
			return i
		}
	}
	t.Fatal("no refire outcome")
	return -1
}