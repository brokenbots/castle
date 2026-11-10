package suite

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/brokenbots/castle/castle/internal/store"
)

// TestStampRunTerminal_CascadesAssignment pins the KB-373 core invariant
// (CRI-142): stamping a run terminal settles its still queued or leased
// workflow assignments in the SAME transaction, so a settling run can never
// strand an assignment row behind in state='leased'. It also pins the guards:
// terminal and operator-parked stopped runs are never rewritten, unknown ids
// return ErrNotFound, and a non-terminal status is rejected.
func TestStampRunTerminal_CascadesAssignment(t *testing.T) {
	runWithCtx(t, func(t *testing.T, ctx context.Context, s store.Store) {
		now := microUTC(time.Now())
		a1 := newAssignment(t, ctx, s, "owner-1", "key-1", nil, now)
		a2 := newAssignment(t, ctx, s, "owner-1", "key-2", nil, now.Add(time.Millisecond))
		a3 := newAssignment(t, ctx, s, "owner-1", "key-3", nil, now.Add(2*time.Millisecond))
		a4 := newAssignment(t, ctx, s, "owner-1", "key-4", nil, now.Add(3*time.Millisecond))
		newOverseer(t, s, "o1", now, nil)

		// The lease picks the oldest queued assignment: a1 leaves the queue
		// through the leased lifecycle under test, a2/a3 stay queued and a4
		// gets parked by the operator (CRI-207).
		leased, _, err := s.LeaseWorkflowAssignment(ctx, "o1", map[string]string{}, now, 30*time.Minute)
		if err != nil {
			t.Fatalf("lease: %v", err)
		}
		if leased.ID != a1.ID {
			t.Fatalf("leasing picked %s, want %s", leased.ID, a1.ID)
		}

		// A failed run settles its leased assignment in the same transaction.
		run, err := s.StampRunTerminal(ctx, a1.RunID, "failed", "boom", now.Add(time.Minute), store.TerminalReasonRunFailed)
		if err != nil {
			t.Fatalf("StampRunTerminal(failed): %v", err)
		}
		if run.Status != "failed" || run.FailureReason != "boom" || run.EndedAt == nil {
			t.Fatalf("stamped run = %+v", run)
		}
		got, err := s.GetWorkflowAssignment(ctx, a1.ID)
		if err != nil {
			t.Fatalf("GetWorkflowAssignment(a1): %v", err)
		}
		if got.State != store.WorkflowAssignmentStateTerminal || got.TerminalReason != store.TerminalReasonRunFailed {
			t.Fatalf("a1 after stamp = %+v, want terminal with %q", got, store.TerminalReasonRunFailed)
		}

		// Queued rows are covered by the cascade too, with status-mapped
		// reasons for both remaining terminal statuses.
		if _, err := s.StampRunTerminal(ctx, a2.RunID, "succeeded", "", now.Add(time.Minute), store.TerminalReasonRunCompleted); err != nil {
			t.Fatalf("StampRunTerminal(succeeded): %v", err)
		}
		got, err = s.GetWorkflowAssignment(ctx, a2.ID)
		if err != nil {
			t.Fatalf("GetWorkflowAssignment(a2): %v", err)
		}
		if got.State != store.WorkflowAssignmentStateTerminal || got.TerminalReason != store.TerminalReasonRunCompleted {
			t.Fatalf("a2 after stamp = %+v, want terminal with %q", got, store.TerminalReasonRunCompleted)
		}
		if _, err := s.StampRunTerminal(ctx, a3.RunID, "cancelled", "aborted", now.Add(time.Minute), store.TerminalReasonRunCancelled); err != nil {
			t.Fatalf("StampRunTerminal(cancelled): %v", err)
		}
		got, err = s.GetWorkflowAssignment(ctx, a3.ID)
		if err != nil {
			t.Fatalf("GetWorkflowAssignment(a3): %v", err)
		}
		if got.State != store.WorkflowAssignmentStateTerminal || got.TerminalReason != store.TerminalReasonRunCancelled {
			t.Fatalf("a3 after stamp = %+v, want terminal with %q", got, store.TerminalReasonRunCancelled)
		}

		// Terminal stamps are final (CRI-142): restamping a1's run is
		// rejected and leaves the row intact.
		if _, err := s.StampRunTerminal(ctx, a1.RunID, "failed", "boom", now.Add(2*time.Minute), store.TerminalReasonRunFailed); !errors.Is(err, store.ErrRunTerminal) {
			t.Fatalf("restamp terminal run err = %v, want ErrRunTerminal", err)
		}
		if again, err := s.GetRun(ctx, a1.RunID); err != nil || again.Status != "failed" {
			t.Fatalf("restamped run = %+v err=%v, want failed", again, err)
		}

		// An operator-parked stopped run is never rewritten (CRI-207): only
		// an explicit resume returns it to the game.
		if err := s.SetRunStopped(ctx, a4.RunID); err != nil {
			t.Fatalf("SetRunStopped: %v", err)
		}
		if _, err := s.StampRunTerminal(ctx, a4.RunID, "succeeded", "", now.Add(2*time.Minute), store.TerminalReasonRunCompleted); !errors.Is(err, store.ErrRunTerminal) {
			t.Fatalf("stamp stopped run err = %v, want ErrRunTerminal", err)
		}
		if stayed, err := s.GetRun(ctx, a4.RunID); err != nil || stayed.Status != "stopped" {
			t.Fatalf("stopped run after stamp = %+v err=%v, want stopped", stayed, err)
		}

		// Unknown ids are ErrNotFound, and non-terminal statuses are rejected.
		if _, err := s.StampRunTerminal(ctx, "missing-run", "failed", "boom", now, store.TerminalReasonRunFailed); !errors.Is(err, store.ErrNotFound) {
			t.Fatalf("unknown run err = %v, want ErrNotFound", err)
		}
		if _, err := s.StampRunTerminal(ctx, a4.RunID, "running", "", now, ""); err == nil {
			t.Fatal("stamp non-terminal status: expected error")
		}
	})
}

// TestExpireLeases_SweepsTerminalRunLeases is the KB-373 regression test: a
// run that failed while (or before) an assignment was leased must have that
// assignment settled to terminal by the lease-expiry sweeper within one tick
// past lease expiry — the exact lifecycle of the zombie rows in the original
// report (run failed, lease_expires_at days past expiry, never re-leased,
// never terminalized).
func TestExpireLeases_SweepsTerminalRunLeases(t *testing.T) {
	runWithCtx(t, func(t *testing.T, ctx context.Context, s store.Store) {
		now := microUTC(time.Now())
		aFailed := newAssignment(t, ctx, s, "owner-1", "key-failed", nil, now)
		newOverseer(t, s, "o1", now, nil)

		leased, _, err := s.LeaseWorkflowAssignment(ctx, "o1", map[string]string{}, now, 30*time.Minute)
		if err != nil || leased.ID != aFailed.ID {
			t.Fatalf("lease = %+v err=%v, want %s", leased, err, aFailed.ID)
		}

		// Reproduce the zombie: the run settles terminal WITHOUT the dispatch
		// cascade (the pre-fix missed/dropped second write), leaving the
		// assignment leased behind it.
		if err := s.UpdateRun(ctx, &store.Run{ID: aFailed.RunID, Status: "failed", FailureReason: "boom", EndedAt: &now}); err != nil {
			t.Fatalf("UpdateRun(failed): %v", err)
		}

		// A tick before expiry: the live lease is untouched — the sweep must
		// not settle rows whose lease has not lapsed, even with a dead run.
		ids, err := s.ExpireWorkflowAssignmentLeases(ctx, now.Add(5*time.Minute))
		if err != nil {
			t.Fatalf("early sweep: %v", err)
		}
		if len(ids) != 0 {
			t.Fatalf("early sweep ids = %v, want none", ids)
		}
		if got, err := s.GetWorkflowAssignment(ctx, aFailed.ID); err != nil || got.State != store.WorkflowAssignmentStateLeased {
			t.Fatalf("aFailed before expiry = %+v err=%v, want leased", got, err)
		}

		// One sweeper tick past expiry: the row converges to terminal with
		// the run-failure reason and is NOT returned for redispatch.
		ids, err = s.ExpireWorkflowAssignmentLeases(ctx, now.Add(2*time.Hour))
		if err != nil {
			t.Fatalf("sweep: %v", err)
		}
		for _, id := range ids {
			if id == aFailed.ID {
				t.Fatalf("sweep returned terminalized assignment %s for redispatch", id)
			}
		}
		got, err := s.GetWorkflowAssignment(ctx, aFailed.ID)
		if err != nil {
			t.Fatalf("GetWorkflowAssignment(aFailed): %v", err)
		}
		if got.State != store.WorkflowAssignmentStateTerminal || got.TerminalReason != store.TerminalReasonRunFailed {
			t.Fatalf("aFailed after sweep = %+v, want terminal with %q", got, store.TerminalReasonRunFailed)
		}
		if run, err := s.GetRun(ctx, aFailed.RunID); err != nil || run.Status != "failed" || run.FailureReason != "boom" {
			t.Fatalf("aFailed run after sweep = %+v err=%v, want failed/boom", run, err)
		}
	})
}

// TestExpireLeases_SweepsTerminalRunLeasesSiblingEndings covers the succeeded
// and cancelled endings through the same sweeper path so the reason vocabulary
// stays status-mapped in both engines, and pins that a live lease on a
// still-pending run rides through the sweep untouched (the new stage only
// touches expired leases, never displacing queued work).
func TestExpireLeases_SweepsTerminalRunLeasesSiblingEndings(t *testing.T) {
	runWithCtx(t, func(t *testing.T, ctx context.Context, s store.Store) {
		now := microUTC(time.Now())
		aDone := newAssignment(t, ctx, s, "owner-1", "key-done", nil, now)
		aCancelled := newAssignment(t, ctx, s, "owner-1", "key-cancelled", nil, now.Add(time.Millisecond))
		aLive := newAssignment(t, ctx, s, "owner-1", "key-live", nil, now.Add(2*time.Millisecond))
		newOverseer(t, s, "o-done", now, nil)
		newOverseer(t, s, "o-cancelled", now, nil)
		newOverseer(t, s, "o-live", now, nil)

		// Each lease uses its own agent: the store enforces sequential
		// execution, so a second unstarted lease for one agent is rejected.
		for _, tc := range []struct {
			a      *store.WorkflowAssignment
			agent  string
			expiry time.Duration
		}{
			{aDone, "o-done", 30 * time.Minute},
			{aCancelled, "o-cancelled", 30 * time.Minute},
			// aLive's lease outlives the sweep: it must NOT converge.
			{aLive, "o-live", 90 * time.Minute},
		} {
			leased, _, err := s.LeaseWorkflowAssignment(ctx, tc.agent, map[string]string{}, now, tc.expiry)
			if err != nil || leased.ID != tc.a.ID {
				t.Fatalf("lease %s = %+v err=%v, want %s", tc.agent, leased, err, tc.a.ID)
			}
		}

		// Zombie reproduction for the sibling endings: settle the runs
		// without the dispatch cascade.
		done := microUTC(now)
		if err := s.UpdateRun(ctx, &store.Run{ID: aDone.RunID, Status: "succeeded", EndedAt: &done}); err != nil {
			t.Fatalf("UpdateRun(aDone succeeded): %v", err)
		}
		if err := s.UpdateRun(ctx, &store.Run{ID: aCancelled.RunID, Status: "cancelled", EndedAt: &done}); err != nil {
			t.Fatalf("UpdateRun(aCancelled cancelled): %v", err)
		}

		// One sweeper tick past the 30-minute leases (aDone/aCancelled) but
		// before the 90-minute aLive lease: terminal-run leases converge with
		// their status-mapped reasons, requeue work does not leak into the
		// returned ids, and the live pending-run lease is untouched.
		ids, err := s.ExpireWorkflowAssignmentLeases(ctx, now.Add(time.Hour))
		if err != nil {
			t.Fatalf("sweep: %v", err)
		}
		for _, id := range ids {
			if id == aDone.ID || id == aCancelled.ID {
				t.Fatalf("sweep returned terminalized assignment %s for redispatch", id)
			}
		}
		for _, tc := range []struct {
			a      *store.WorkflowAssignment
			status string
			reason string
		}{
			{aDone, "succeeded", store.TerminalReasonRunCompleted},
			{aCancelled, "cancelled", store.TerminalReasonRunCancelled},
		} {
			got, err := s.GetWorkflowAssignment(ctx, tc.a.ID)
			if err != nil {
				t.Fatalf("GetWorkflowAssignment(%s): %v", tc.a.ID, err)
			}
			if got.State != store.WorkflowAssignmentStateTerminal || got.TerminalReason != tc.reason {
				t.Fatalf("%s after sweep = %+v, want terminal with %q", tc.a.ID, got, tc.reason)
			}
			if run, err := s.GetRun(ctx, tc.a.RunID); err != nil || run.Status != tc.status {
				t.Fatalf("%s run after sweep = %+v err=%v, want %s", tc.a.ID, run, err, tc.status)
			}
		}
		if got, err := s.GetWorkflowAssignment(ctx, aLive.ID); err != nil || got.State != store.WorkflowAssignmentStateLeased {
			t.Fatalf("aLive after sweep = %+v err=%v, want leased", got, err)
		}
	})
}
