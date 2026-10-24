package suite

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/brokenbots/castle/castle/internal/store"
)

func newAssignment(t *testing.T, ctx context.Context, s store.Store, owner, idem string, labels map[string]string, now time.Time) *store.WorkflowAssignment {
	t.Helper()
	a, created, err := s.CreateWorkflowAssignment(ctx, &store.WorkflowAssignment{
		OwnerCriteriaID: owner,
		WorkflowName:    "wf",
		WorkflowSource:  "source hcl",
		IdempotencyKey:  idem,
		Labels:          labels,
		CreatedAt:       now,
		UpdatedAt:       now,
	})
	if err != nil {
		t.Fatalf("CreateWorkflowAssignment(%s): %v", idem, err)
	}
	if !created {
		t.Fatalf("CreateWorkflowAssignment(%s): expected a new assignment", idem)
	}
	return a
}

// TestCreateWorkflowAssignmentLifecycle covers the durable submission leg:
// the queued run is born atomically with the assignment (no overseer, pending
// status), labels round-trip, and resubmission under the same
// (owner, idempotency) key returns the original record instead of a duplicate.
func TestCreateWorkflowAssignmentLifecycle(t *testing.T) {
	runWithCtx(t, func(t *testing.T, ctx context.Context, s store.Store) {
		now := microUTC(time.Now())
		a := newAssignment(t, ctx, s, "owner-1", "key-1", map[string]string{"env": "prod"}, now)

		if a.RunID == "" || a.State != store.WorkflowAssignmentStateQueued {
			t.Fatalf("created assignment = %+v", a)
		}
		run, err := s.GetRun(ctx, a.RunID)
		if err != nil {
			t.Fatalf("GetRun: %v", err)
		}
		if run.Status != "pending" || run.OverseerID != "" || run.WorkflowHCL != "source hcl" {
			t.Fatalf("queued run = %+v", run)
		}

		got, err := s.GetWorkflowAssignment(ctx, a.ID)
		if err != nil {
			t.Fatalf("GetWorkflowAssignment: %v", err)
		}
		if got.State != store.WorkflowAssignmentStateQueued || len(got.Labels) != 1 || got.Labels["env"] != "prod" {
			t.Fatalf("assignment round-trip = %+v labels=%v", got, got.Labels)
		}
		byRun, err := s.GetWorkflowAssignmentByRunID(ctx, a.RunID)
		if err != nil || byRun.ID != a.ID {
			t.Fatalf("GetWorkflowAssignmentByRunID = %v, %v", byRun, err)
		}

		// Idempotent resubmission: different payload, same key.
		second, created, err := s.CreateWorkflowAssignment(ctx, &store.WorkflowAssignment{
			OwnerCriteriaID: "owner-1",
			WorkflowName:    "different-wf",
			WorkflowSource:  "different source",
			IdempotencyKey:  "key-1",
		})
		if err != nil {
			t.Fatalf("idempotent resubmit: %v", err)
		}
		if created || second.ID != a.ID || second.RunID != a.RunID {
			t.Fatalf("idempotent resubmit = created=%v %+v", created, second)
		}
		if second.WorkflowName != "wf" || second.WorkflowSource != "source hcl" {
			t.Fatalf("original payload must be preserved, got %q/%q", second.WorkflowName, second.WorkflowSource)
		}

		// Same key under a different owner is a distinct submission.
		if _, created, err := s.CreateWorkflowAssignment(ctx, &store.WorkflowAssignment{
			OwnerCriteriaID: "owner-2", WorkflowName: "wf", WorkflowSource: "s", IdempotencyKey: "key-1",
		}); err != nil || !created {
			t.Fatalf("same key, other owner: created=%v err=%v", created, err)
		}

		runs, _, err := s.ListRuns(ctx, "", "", 0, "")
		if err != nil {
			t.Fatalf("ListRuns: %v", err)
		}
		if len(runs) != 2 {
			t.Fatalf("run rows = %d, want 2 (one per owner)", len(runs))
		}
	})
}

// TestLeaseRequiresOnlineAgentAndLabelMatch pins the eligibility gates: an
// offline agent or one whose labels do not satisfy the assignment's labels
// gets ErrNotFound, and a matching online agent leases the oldest eligible
// assignment and takes ownership of its run.
func TestLeaseRequiresOnlineAgentAndLabelMatch(t *testing.T) {
	runWithCtx(t, func(t *testing.T, ctx context.Context, s store.Store) {
		now := microUTC(time.Now())
		newAssignment(t, ctx, s, "owner-1", "key-1", map[string]string{"gpu": "true"}, now)

		if err := s.CreateOverseer(ctx, &store.Overseer{ID: "o-off", Name: "x", TokenHash: "t", Status: "offline", CreatedAt: now, LastSeenAt: now}); err != nil {
			t.Fatalf("CreateOverseer(offline): %v", err)
		}
		if _, err := s.LeaseWorkflowAssignment(ctx, "o-off", map[string]string{}, now, time.Minute); !errors.Is(err, store.ErrNotFound) {
			t.Fatalf("offline agent lease err = %v, want ErrNotFound", err)
		}

		if err := s.CreateOverseer(ctx, &store.Overseer{ID: "o2", Name: "y", TokenHash: "t2", Status: "online", Labels: map[string]string{"gpu": "true", "zone": "a"}, CreatedAt: now, LastSeenAt: now}); err != nil {
			t.Fatalf("CreateOverseer(matching): %v", err)
		}
		if _, err := s.LeaseWorkflowAssignment(ctx, "o2", map[string]string{"gpu": "false"}, now, time.Minute); !errors.Is(err, store.ErrNotFound) {
			t.Fatalf("label mismatch lease err = %v, want ErrNotFound", err)
		}

		leased, err := s.LeaseWorkflowAssignment(ctx, "o2", map[string]string{"gpu": "true", "zone": "a"}, now, time.Minute)
		if err != nil {
			t.Fatalf("matching lease: %v", err)
		}
		if leased.State != store.WorkflowAssignmentStateLeased || leased.LeasedCriteriaID != "o2" || leased.LeaseExpiresAt == nil {
			t.Fatalf("leased assignment = %+v", leased)
		}
		run, err := s.GetRun(ctx, leased.RunID)
		if err != nil {
			t.Fatalf("GetRun: %v", err)
		}
		if run.OverseerID != "o2" {
			t.Fatalf("run overseer = %q, want o2", run.OverseerID)
		}

		// The agent's lease record and attempt ledger both grew by one.
		if err := s.RecordWorkflowAssignmentLease(ctx, &store.WorkflowAssignmentLease{AssignmentID: leased.ID, CriteriaID: "o2", CreatedAt: now, ExpiresAt: now.Add(time.Minute)}); err != nil {
			t.Fatalf("RecordWorkflowAssignmentLease: %v", err)
		}
		if err := s.RecordWorkflowAssignmentAttempt(ctx, &store.WorkflowAssignmentAttempt{AssignmentID: leased.ID, Attempt: 2, CriteriaID: "o2", CreatedAt: now}); err != nil {
			t.Fatalf("RecordWorkflowAssignmentAttempt: %v", err)
		}
		if err := s.CompleteWorkflowAssignmentAttempt(ctx, leased.ID, 2, "accepted"); err != nil {
			t.Fatalf("CompleteWorkflowAssignmentAttempt: %v", err)
		}

		// Input validation parity.
		if err := s.RecordWorkflowAssignmentAttempt(ctx, &store.WorkflowAssignmentAttempt{AssignmentID: leased.ID, Attempt: 0}); err == nil {
			t.Fatalf("attempt 0 must be rejected")
		}
		if err := s.RecordWorkflowAssignmentAttempt(ctx, nil); err == nil {
			t.Fatalf("nil attempt must be rejected")
		}
	})
}

// TestLeaseSingleWinnerAndSequentialGuards reproduces the concurrent-claim
// race: eight goroutines across two agents race on one queued assignment and
// exactly one may win. Afterwards both agents are refused — the winner holds
// an unstarted lease (sequential-execution guard) and the assignment itself
// is no longer queued.
func TestLeaseSingleWinnerAndSequentialGuards(t *testing.T) {
	runWithCtx(t, func(t *testing.T, ctx context.Context, s store.Store) {
		now := microUTC(time.Now())
		a := newAssignment(t, ctx, s, "owner-1", "key-1", map[string]string{"gpu": "true"}, now)
		for _, id := range []string{"a1", "a2"} {
			if err := s.CreateOverseer(ctx, &store.Overseer{ID: id, Name: id, TokenHash: id, Status: "online", Labels: map[string]string{"gpu": "true"}, CreatedAt: now, LastSeenAt: now}); err != nil {
				t.Fatalf("CreateOverseer(%s): %v", id, err)
			}
		}

		const goroutines = 8
		var wins int64
		var mu sync.Mutex
		winners := map[string]bool{}
		var wg sync.WaitGroup
		for i := 0; i < goroutines; i++ {
			wg.Add(1)
			agent := fmt.Sprintf("a%d", (i%2)+1)
			go func() {
				defer wg.Done()
				leased, err := s.LeaseWorkflowAssignment(ctx, agent, map[string]string{"gpu": "true"}, now, time.Minute)
				if err == nil {
					atomic.AddInt64(&wins, 1)
					mu.Lock()
					winners[leased.LeasedCriteriaID] = true
					mu.Unlock()
				} else if !errors.Is(err, store.ErrNotFound) {
					t.Errorf("unexpected lease error: %v", err)
				}
			}()
		}
		wg.Wait()
		if wins != 1 {
			t.Fatalf("wins = %d (winners=%v), want exactly 1", wins, winners)
		}

		// Winner retrying while its lease is unstarted: sequential guard.
		if _, err := s.LeaseWorkflowAssignment(ctx, "a1", map[string]string{"gpu": "true"}, now.Add(time.Second), time.Minute); !errors.Is(err, store.ErrNotFound) {
			t.Fatalf("winner retry err = %v, want ErrNotFound", err)
		}
		// The other agent cannot pick up the assignment either: it is leased.
		if _, err := s.LeaseWorkflowAssignment(ctx, "a2", map[string]string{"gpu": "true"}, now.Add(time.Second), time.Minute); !errors.Is(err, store.ErrNotFound) {
			t.Fatalf("second agent lease err = %v, want ErrNotFound", err)
		}
		_ = a
	})
}

// TestLeaseExpiryRequeues pins the expired-lease fast path: a lease that is
// already stale when the next lease attempt runs is expired atomically and
// the assignment re-leases to the new agent.
func TestLeaseExpiryRequeues(t *testing.T) {
	runWithCtx(t, func(t *testing.T, ctx context.Context, s store.Store) {
		now := microUTC(time.Now())
		newAssignment(t, ctx, s, "owner-1", "key-1", nil, now)
		for _, id := range []string{"o1", "o2"} {
			if err := s.CreateOverseer(ctx, &store.Overseer{ID: id, Name: id, TokenHash: id, Status: "online", CreatedAt: now, LastSeenAt: now}); err != nil {
				t.Fatalf("CreateOverseer(%s): %v", id, err)
			}
		}

		first, err := s.LeaseWorkflowAssignment(ctx, "o1", map[string]string{}, now, -time.Second)
		if err != nil {
			t.Fatalf("stale lease: %v", err)
		}
		if first.State != store.WorkflowAssignmentStateLeased {
			t.Fatalf("state = %q, want leased", first.State)
		}
		again, err := s.LeaseWorkflowAssignment(ctx, "o2", map[string]string{}, now.Add(time.Second), time.Minute)
		if err != nil {
			t.Fatalf("re-lease after expiry: %v", err)
		}
		if again.ID != first.ID || again.LeasedCriteriaID != "o2" {
			t.Fatalf("re-lease = %+v, want same assignment on o2", again)
		}
	})
}

// TestExpireLeasesAndRedelivery covers Castle-restart recovery: leases still
// pending on redelivery are listed per agent; once a run has started the
// lease stops being redelivered but stays leased even past expiry; expired
// leases on still-pending runs return to the queue and drop their run's
// overseer so the work can be redispatched.
func TestExpireLeasesAndRedelivery(t *testing.T) {
	runWithCtx(t, func(t *testing.T, ctx context.Context, s store.Store) {
		now := microUTC(time.Now())
		a1 := newAssignment(t, ctx, s, "owner-1", "key-1", nil, now)
		a2 := newAssignment(t, ctx, s, "owner-1", "key-2", nil, now.Add(time.Millisecond))
		if err := s.CreateOverseer(ctx, &store.Overseer{ID: "o1", Name: "x", TokenHash: "t", Status: "online", CreatedAt: now, LastSeenAt: now}); err != nil {
			t.Fatalf("CreateOverseer: %v", err)
		}

		if _, err := s.LeaseWorkflowAssignment(ctx, "o1", map[string]string{}, a1.CreatedAt, time.Hour); err != nil {
			t.Fatalf("lease a1: %v", err)
		}
		// a2 stays queued: an agent with an unstarted lease gets nothing.

		// Redelivery: o1's live lease is pending and listed once.
		redeliver, err := s.ListLeasedPendingAssignmentsByCriteriaID(ctx, "o1")
		if err != nil || len(redeliver) != 1 || redeliver[0].ID != a1.ID {
			t.Fatalf("redelivery = %+v err=%v, want a1", redeliver, err)
		}

		// a1's run starts: its lease leaves redelivery but survives expiry.
		if err := s.UpdateRun(ctx, &store.Run{ID: a1.RunID, Status: "running"}); err != nil {
			t.Fatalf("UpdateRun(a1 running): %v", err)
		}
		redeliver, err = s.ListLeasedPendingAssignmentsByCriteriaID(ctx, "o1")
		if err != nil || len(redeliver) != 0 {
			t.Fatalf("redelivery after start = %+v err=%v, want none", redeliver, err)
		}
		expired, err := s.ExpireWorkflowAssignmentLeases(ctx, now.Add(2*time.Hour))
		if err != nil || len(expired) != 0 {
			t.Fatalf("expiry with started run = %v err=%v, want none", expired, err)
		}
		if a1Leased, err := s.GetWorkflowAssignment(ctx, a1.ID); err != nil || a1Leased.State != store.WorkflowAssignmentStateLeased {
			t.Fatalf("started-run lease = %+v err=%v, want leased", a1Leased, err)
		}

		// a2 takes a lease that is already stale: expiry requeues it and
		// clears the run's overseer so the work can be redispatched.
		if _, err := s.LeaseWorkflowAssignment(ctx, "o1", map[string]string{}, a2.CreatedAt, -time.Second); err != nil {
			t.Fatalf("lease a2 stale: %v", err)
		}
		expired, err = s.ExpireWorkflowAssignmentLeases(ctx, now.Add(2*time.Hour))
		if err != nil || len(expired) != 1 || expired[0] != a2.ID {
			t.Fatalf("expired = %v err=%v, want a2", expired, err)
		}
		run, err := s.GetRun(ctx, a2.RunID)
		if err != nil {
			t.Fatalf("GetRun(a2): %v", err)
		}
		if run.OverseerID != "" {
			t.Fatalf("expired run overseer = %q, want empty", run.OverseerID)
		}
		if gotA2, err := s.GetWorkflowAssignment(ctx, a2.ID); err != nil || gotA2.State != store.WorkflowAssignmentStateQueued || gotA2.LeasedCriteriaID != "" {
			t.Fatalf("expired assignment = %+v err=%v, want queued with no holder", gotA2, err)
		}

		redeliver, err = s.ListLeasedPendingAssignmentsByCriteriaID(ctx, "o1")
		if err != nil || len(redeliver) != 0 {
			t.Fatalf("redelivery after expiry = %+v err=%v, want none", redeliver, err)
		}
	})
}

// TestMarkAssignmentTerminalAndAttempts pins the completion bookkeeping:
// terminal assignments keep their reason and can no longer be leased, and
// the attempt ledger's completion call is a no-op-safe update.
func TestMarkAssignmentTerminalAndAttempts(t *testing.T) {
	runWithCtx(t, func(t *testing.T, ctx context.Context, s store.Store) {
		now := microUTC(time.Now())
		a := newAssignment(t, ctx, s, "owner-1", "key-1", nil, now)
		a2 := newAssignment(t, ctx, s, "owner-1", "key-2", nil, now.Add(time.Millisecond))
		if err := s.CreateOverseer(ctx, &store.Overseer{ID: "o1", Name: "x", TokenHash: "t", Status: "online", CreatedAt: now, LastSeenAt: now}); err != nil {
			t.Fatalf("CreateOverseer: %v", err)
		}

		if err := s.MarkWorkflowAssignmentTerminal(ctx, a.RunID, "run completed"); err != nil {
			t.Fatalf("MarkWorkflowAssignmentTerminal: %v", err)
		}
		got, err := s.GetWorkflowAssignment(ctx, a.ID)
		if err != nil {
			t.Fatalf("GetWorkflowAssignment: %v", err)
		}
		if got.State != store.WorkflowAssignmentStateTerminal || got.TerminalReason != "run completed" {
			t.Fatalf("terminal = %+v", got)
		}
		// Terminal marking on an already-terminal run is idempotent-safe.
		if err := s.MarkWorkflowAssignmentTerminal(ctx, a.RunID, "run completed"); err != nil {
			t.Fatalf("re-terminal: %v", err)
		}

		// a2 stays leasable: terminal marking is scoped to the given run.
		a2Leased, err := s.LeaseWorkflowAssignment(ctx, "o1", map[string]string{}, now, time.Minute)
		if err != nil || a2Leased.ID != a2.ID {
			t.Fatalf("lease of remaining assignment = %+v err=%v, want a2", a2Leased, err)
		}
		// Completing a non-existent attempt is a no-op, not an error.
		if err := s.CompleteWorkflowAssignmentAttempt(ctx, "no-such-assignment", 1, "x"); err != nil {
			t.Fatalf("complete unknown attempt = %v, want nil", err)
		}
	})
}

// TestExpireLeasesLoseRaceToLease pins the expiry contract under a writer
// race: with agent A holding an expired lease, expiry racing a re-lease to
// agent B must never clobber B's lease (the pg READ COMMITTED
// check-then-update hole), never leave a queued assignment's run overseer
// pointing at a live lease holder, and never report a transition that
// leaves the run overseer cleared for a leased assignment. sqlite has a
// single writer so the race never interleaves there, but both engines must
// uphold the same contract.
func TestExpireLeasesLoseRaceToLease(t *testing.T) {
	runWithCtx(t, func(t *testing.T, ctx context.Context, s store.Store) {
		now := microUTC(time.Now())
		for _, id := range []string{"oa-race", "ob-race"} {
			if err := s.CreateOverseer(ctx, &store.Overseer{ID: id, Name: id, TokenHash: id, Status: "online", CreatedAt: now, LastSeenAt: now}); err != nil {
				t.Fatalf("CreateOverseer(%s): %v", id, err)
			}
		}
		for i := 0; i < 25; i++ {
			a := newAssignment(t, ctx, s, "owner-race", fmt.Sprintf("key-race-%d", i), nil, now)
			// Agent A leases with an already-expired lease so expiry can be
			// driven deterministically with an explicit now; no sleeping.
			if _, err := s.LeaseWorkflowAssignment(ctx, "oa-race", map[string]string{}, now, -time.Second); err != nil {
				t.Fatalf("iteration %d: lease to A: %v", i, err)
			}

			var (
				wg      sync.WaitGroup
				expired []string
				expErr  error
				// leaseErr distinguishes "B acquired" (nil) from "B ran
				// before expiry requeued the work" (ErrNotFound).
				leaseErr error
			)
			start := make(chan struct{})
			wg.Add(2)
			go func() {
				defer wg.Done()
				<-start
				expired, expErr = s.ExpireWorkflowAssignmentLeases(ctx, now.Add(time.Minute))
			}()
			go func() {
				defer wg.Done()
				<-start
				_, leaseErr = s.LeaseWorkflowAssignment(ctx, "ob-race", map[string]string{}, now.Add(time.Minute), time.Hour)
			}()
			close(start)
			wg.Wait()

			if expErr != nil {
				t.Fatalf("iteration %d: ExpireWorkflowAssignmentLeases: %v", i, expErr)
			}
			if leaseErr != nil && !errors.Is(leaseErr, store.ErrNotFound) {
				t.Fatalf("iteration %d: LeaseWorkflowAssignment: %v", i, leaseErr)
			}

			got, err := s.GetWorkflowAssignment(ctx, a.ID)
			if err != nil {
				t.Fatalf("iteration %d: GetWorkflowAssignment: %v", i, err)
			}
			run, err := s.GetRun(ctx, a.RunID)
			if err != nil {
				t.Fatalf("iteration %d: GetRun: %v", i, err)
			}

			switch {
			case leaseErr == nil:
				// B holds the lease: it must have survived expiry with its
				// run overseer intact, whichever goroutine ran first.
				if got.State != store.WorkflowAssignmentStateLeased || got.LeasedCriteriaID != "ob-race" {
					t.Fatalf("iteration %d: re-lease succeeded but assignment ended %q on %q, want leased on ob-race", i, got.State, got.LeasedCriteriaID)
				}
				if got.LeaseExpiresAt == nil || !got.LeaseExpiresAt.After(now.Add(time.Minute)) {
					t.Fatalf("iteration %d: live lease missing/expired after re-lease: %+v", i, got)
				}
				if run.OverseerID != "ob-race" {
					t.Fatalf("iteration %d: live lease on ob-race but run overseer = %q", i, run.OverseerID)
				}
			default:
				// B never acquired (it ran before expiry requeued the work):
				// the assignment must have been requeued (by expiry or by the
				// lease path's own stale-lease sweep) with a cleared overseer.
				if got.State != store.WorkflowAssignmentStateQueued {
					t.Fatalf("iteration %d: no live lease but assignment stuck in %q", i, got.State)
				}
				if run.OverseerID != "" {
					t.Fatalf("iteration %d: queued assignment still holds run overseer %q", i, run.OverseerID)
				}
			}
			// Expiry may legitimately report nothing (the re-lease path's own
			// stale-lease sweep can requeue the expired lease before the
			// expiry scan observes it), but a reported transition must
			// describe this iteration's assignment — never a foreign row.
			for _, id := range expired {
				if id != a.ID {
					t.Fatalf("iteration %d: expiry transitioned a foreign assignment %s, want none or %s", i, id, a.ID)
				}
			}
			// Terminalize this iteration's assignment so the next iteration
			// starts with an empty queue pool: LeaseWorkflowAssignment leases
			// the oldest queued assignment, so leftover queued work from
			// earlier iterations would steal leases and make the race
			// non-targeted.
			if err := s.MarkWorkflowAssignmentTerminal(ctx, a.RunID, "test-cleanup"); err != nil {
				t.Fatalf("iteration %d: MarkWorkflowAssignmentTerminal: %v", i, err)
			}
		}
	})
}
