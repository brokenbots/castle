package sqlite

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

func TestCreateWorkflowAssignment_CreatesRunAndAssignment(t *testing.T) {
	s := tempStore(t)
	ctx := context.Background()
	now := time.Now().UTC()

	owner := "owner-1"
	a := &store.WorkflowAssignment{
		OwnerCriteriaID: owner,
		WorkflowName:    "wf",
		WorkflowSource:  "source hcl",
		LockfileSource:  "lock hcl",
		IdempotencyKey:  "key-1",
		Labels:          map[string]string{"env": "prod"},
		CreatedAt:       now,
		UpdatedAt:       now,
	}

	created, isNew, err := s.CreateWorkflowAssignment(ctx, a)
	if err != nil {
		t.Fatalf("create assignment: %v", err)
	}
	if !isNew {
		t.Fatalf("expected created=true for first submission")
	}
	if created.RunID == "" {
		t.Fatalf("expected run id to be assigned")
	}
	if created.State != store.WorkflowAssignmentStateQueued {
		t.Fatalf("expected state queued, got %s", created.State)
	}

	// The associated run should exist with no overseer and pending status.
	run, err := s.GetRun(ctx, created.RunID)
	if err != nil {
		t.Fatalf("get run: %v", err)
	}
	if run.OverseerID != "" {
		t.Fatalf("expected queued run to have no overseer, got %s", run.OverseerID)
	}
	if run.Status != "pending" {
		t.Fatalf("expected run status pending, got %s", run.Status)
	}
	if run.WorkflowHCL != a.WorkflowSource {
		t.Fatalf("expected workflow hcl to match source")
	}

	// Labels should be persisted and retrievable.
	got, err := s.GetWorkflowAssignment(ctx, created.ID)
	if err != nil {
		t.Fatalf("get assignment: %v", err)
	}
	if len(got.Labels) != 1 || got.Labels["env"] != "prod" {
		t.Fatalf("expected labels to round-trip, got %v", got.Labels)
	}
}

func TestCreateWorkflowAssignment_Idempotent(t *testing.T) {
	s := tempStore(t)
	ctx := context.Background()
	now := time.Now().UTC()

	owner := "owner-1"
	a := &store.WorkflowAssignment{
		OwnerCriteriaID: owner,
		WorkflowName:    "wf",
		WorkflowSource:  "source hcl",
		IdempotencyKey:  "idem-key",
		CreatedAt:       now,
		UpdatedAt:       now,
	}

	first, created, err := s.CreateWorkflowAssignment(ctx, a)
	if err != nil {
		t.Fatalf("first create: %v", err)
	}
	if !created {
		t.Fatalf("expected created=true on first create")
	}

	// Re-submit with the same key: should return the existing run without
	// creating a duplicate.
	a2 := &store.WorkflowAssignment{
		OwnerCriteriaID: owner,
		WorkflowName:    "different-wf",
		WorkflowSource:  "different source",
		IdempotencyKey:  "idem-key",
	}
	second, created2, err := s.CreateWorkflowAssignment(ctx, a2)
	if err != nil {
		t.Fatalf("second create: %v", err)
	}
	if created2 {
		t.Fatalf("expected created=false on idempotent resubmit")
	}
	if second.RunID != first.RunID {
		t.Fatalf("expected same run id, got %s want %s", second.RunID, first.RunID)
	}
	if second.WorkflowName != first.WorkflowName {
		t.Fatalf("expected original workflow name to be preserved, got %s", second.WorkflowName)
	}

	var runCount int
	if err := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM runs`).Scan(&runCount); err != nil {
		t.Fatalf("count runs: %v", err)
	}
	if runCount != 1 {
		t.Fatalf("expected exactly one run row, got %d", runCount)
	}
}

func TestLeaseWorkflowAssignment_RequiresOnlineAgent(t *testing.T) {
	s := tempStore(t)
	ctx := context.Background()
	now := time.Now().UTC()

	owner := "owner-1"
	a := &store.WorkflowAssignment{
		OwnerCriteriaID: owner,
		WorkflowName:    "wf",
		WorkflowSource:  "source",
		IdempotencyKey:  "key-1",
		CreatedAt:       now,
		UpdatedAt:       now,
	}
	if _, _, err := s.CreateWorkflowAssignment(ctx, a); err != nil {
		t.Fatalf("create assignment: %v", err)
	}

	// Create an offline agent.
	if err := s.CreateOverseer(ctx, &store.Overseer{
		ID: "o1", Name: "x", TokenHash: "t", Status: "offline", CreatedAt: now, LastSeenAt: now,
	}); err != nil {
		t.Fatalf("create overseer: %v", err)
	}

	_, err := s.LeaseWorkflowAssignment(ctx, "o1", map[string]string{}, now, time.Minute)
	if !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("expected ErrNotFound for offline agent, got %v", err)
	}
}

func TestLeaseWorkflowAssignment_RequiresLabelMatch(t *testing.T) {
	s := tempStore(t)
	ctx := context.Background()
	now := time.Now().UTC()

	owner := "owner-1"
	a := &store.WorkflowAssignment{
		OwnerCriteriaID: owner,
		WorkflowName:    "wf",
		WorkflowSource:  "source",
		IdempotencyKey:  "key-1",
		Labels:          map[string]string{"gpu": "true"},
		CreatedAt:       now,
		UpdatedAt:       now,
	}
	if _, _, err := s.CreateWorkflowAssignment(ctx, a); err != nil {
		t.Fatalf("create assignment: %v", err)
	}

	if err := s.CreateOverseer(ctx, &store.Overseer{
		ID: "o1", Name: "x", TokenHash: "t", Status: "online", Labels: map[string]string{"gpu": "false"},
		CreatedAt: now, LastSeenAt: now,
	}); err != nil {
		t.Fatalf("create overseer: %v", err)
	}

	_, err := s.LeaseWorkflowAssignment(ctx, "o1", map[string]string{"gpu": "false"}, now, time.Minute)
	if !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("expected ErrNotFound for non-matching labels, got %v", err)
	}

	// A matching agent should succeed.
	if err := s.CreateOverseer(ctx, &store.Overseer{
		ID: "o2", Name: "y", TokenHash: "t2", Status: "online", Labels: map[string]string{"gpu": "true", "zone": "a"},
		CreatedAt: now, LastSeenAt: now,
	}); err != nil {
		t.Fatalf("create overseer: %v", err)
	}
	leased, err := s.LeaseWorkflowAssignment(ctx, "o2", map[string]string{"gpu": "true", "zone": "a"}, now, time.Minute)
	if err != nil {
		t.Fatalf("expected lease success, got %v", err)
	}
	if leased.State != store.WorkflowAssignmentStateLeased {
		t.Fatalf("expected leased state, got %s", leased.State)
	}
	if leased.LeasedCriteriaID != "o2" {
		t.Fatalf("expected leased to o2, got %s", leased.LeasedCriteriaID)
	}

	// The run should now be owned by the leasing agent.
	run, err := s.GetRun(ctx, leased.RunID)
	if err != nil {
		t.Fatalf("get run: %v", err)
	}
	if run.OverseerID != "o2" {
		t.Fatalf("expected run overseer o2, got %s", run.OverseerID)
	}
}

func TestLeaseWorkflowAssignment_ConcurrentClaimsOneWinner(t *testing.T) {
	s := tempStore(t)
	ctx := context.Background()
	now := time.Now().UTC()

	owner := "owner-1"
	a := &store.WorkflowAssignment{
		OwnerCriteriaID: owner,
		WorkflowName:    "wf",
		WorkflowSource:  "source",
		IdempotencyKey:  "key-1",
		Labels:          map[string]string{"gpu": "true"},
		CreatedAt:       now,
		UpdatedAt:       now,
	}
	if _, _, err := s.CreateWorkflowAssignment(ctx, a); err != nil {
		t.Fatalf("create assignment: %v", err)
	}

	// Create two eligible agents.
	for _, id := range []string{"a1", "a2"} {
		if err := s.CreateOverseer(ctx, &store.Overseer{
			ID: id, Name: id, TokenHash: id, Status: "online",
			Labels:    map[string]string{"gpu": "true"},
			CreatedAt: now, LastSeenAt: now,
		}); err != nil {
			t.Fatalf("create overseer %s: %v", id, err)
		}
	}

	const goroutines = 8
	var wins int64
	var mu sync.Mutex
	var winners []string
	var wg sync.WaitGroup

	for i := 0; i < goroutines; i++ {
		wg.Add(1)
		agentID := fmt.Sprintf("a%d", (i%2)+1)
		offset := i
		go func() {
			defer wg.Done()
			// Slightly stagger starts to exercise the transactional guard.
			time.Sleep(time.Duration(offset) * time.Millisecond)
			leased, err := s.LeaseWorkflowAssignment(ctx, agentID, map[string]string{"gpu": "true"}, now, time.Minute)
			if err == nil {
				atomic.AddInt64(&wins, 1)
				mu.Lock()
				winners = append(winners, leased.LeasedCriteriaID)
				mu.Unlock()
			} else if !errors.Is(err, store.ErrNotFound) {
				t.Errorf("unexpected lease error: %v", err)
			}
		}()
	}
	wg.Wait()

	if wins != 1 {
		t.Fatalf("expected exactly one lease winner, got %d (winners=%v)", wins, winners)
	}

	// Subsequent attempts must fail because the assignment is already leased.
	_, err := s.LeaseWorkflowAssignment(ctx, "a1", map[string]string{"gpu": "true"}, now.Add(time.Second), time.Minute)
	if !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("expected ErrNotFound after lease, got %v", err)
	}
}

func TestLeaseWorkflowAssignment_ExpiryReturnsToLeasable(t *testing.T) {
	s := tempStore(t)
	ctx := context.Background()
	now := time.Now().UTC()

	owner := "owner-1"
	a := &store.WorkflowAssignment{
		OwnerCriteriaID: owner,
		WorkflowName:    "wf",
		WorkflowSource:  "source",
		IdempotencyKey:  "key-1",
		CreatedAt:       now,
		UpdatedAt:       now,
	}
	if _, _, err := s.CreateWorkflowAssignment(ctx, a); err != nil {
		t.Fatalf("create assignment: %v", err)
	}

	if err := s.CreateOverseer(ctx, &store.Overseer{
		ID: "o1", Name: "x", TokenHash: "t", Status: "online",
		Labels: map[string]string{}, CreatedAt: now, LastSeenAt: now,
	}); err != nil {
		t.Fatalf("create overseer: %v", err)
	}

	// Lease with a duration that has already expired.
	leaseDuration := -time.Second
	leased, err := s.LeaseWorkflowAssignment(ctx, "o1", map[string]string{}, now, leaseDuration)
	if err != nil {
		t.Fatalf("lease: %v", err)
	}
	if leased.State != store.WorkflowAssignmentStateLeased {
		t.Fatalf("expected leased, got %s", leased.State)
	}

	// A second agent should now be able to lease the same assignment because
	// the prior lease expired atomically during the new lease attempt.
	if err := s.CreateOverseer(ctx, &store.Overseer{
		ID: "o2", Name: "y", TokenHash: "t2", Status: "online",
		Labels: map[string]string{}, CreatedAt: now, LastSeenAt: now,
	}); err != nil {
		t.Fatalf("create overseer: %v", err)
	}
	reLeased, err := s.LeaseWorkflowAssignment(ctx, "o2", map[string]string{}, now.Add(time.Second), time.Minute)
	if err != nil {
		t.Fatalf("expected re-lease after expiry, got %v", err)
	}
	if reLeased.LeasedCriteriaID != "o2" {
		t.Fatalf("expected re-leased to o2, got %s", reLeased.LeasedCriteriaID)
	}
}

func TestMarkWorkflowAssignmentTerminal_PreventsLease(t *testing.T) {
	s := tempStore(t)
	ctx := context.Background()
	now := time.Now().UTC()

	owner := "owner-1"
	a := &store.WorkflowAssignment{
		OwnerCriteriaID: owner,
		WorkflowName:    "wf",
		WorkflowSource:  "source",
		IdempotencyKey:  "key-1",
		Labels:          map[string]string{},
		CreatedAt:       now,
		UpdatedAt:       now,
	}
	created, _, err := s.CreateWorkflowAssignment(ctx, a)
	if err != nil {
		t.Fatalf("create assignment: %v", err)
	}

	if err := s.MarkWorkflowAssignmentTerminal(ctx, created.RunID, "run completed"); err != nil {
		t.Fatalf("mark terminal: %v", err)
	}

	got, err := s.GetWorkflowAssignment(ctx, created.ID)
	if err != nil {
		t.Fatalf("get assignment: %v", err)
	}
	if got.State != store.WorkflowAssignmentStateTerminal {
		t.Fatalf("expected terminal state, got %s", got.State)
	}
	if got.TerminalReason != "run completed" {
		t.Fatalf("expected terminal reason to round-trip, got %s", got.TerminalReason)
	}

	if err := s.CreateOverseer(ctx, &store.Overseer{
		ID: "o1", Name: "x", TokenHash: "t", Status: "online",
		Labels: map[string]string{}, CreatedAt: now, LastSeenAt: now,
	}); err != nil {
		t.Fatalf("create overseer: %v", err)
	}
	_, err = s.LeaseWorkflowAssignment(ctx, "o1", map[string]string{}, now, time.Minute)
	if !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("expected ErrNotFound for terminal assignment, got %v", err)
	}
}

func TestOverseerLabels_RoundTrip(t *testing.T) {
	s := tempStore(t)
	ctx := context.Background()
	now := time.Now().UTC()

	o := &store.Overseer{
		ID: "o1", Name: "x", TokenHash: "t", Status: "online",
		Labels:    map[string]string{"zone": "a", "gpu": "true"},
		CreatedAt: now, LastSeenAt: now,
	}
	if err := s.CreateOverseer(ctx, o); err != nil {
		t.Fatalf("create overseer: %v", err)
	}

	got, err := s.GetOverseer(ctx, "o1")
	if err != nil {
		t.Fatalf("get overseer: %v", err)
	}
	if len(got.Labels) != 2 || got.Labels["zone"] != "a" || got.Labels["gpu"] != "true" {
		t.Fatalf("labels did not round-trip: %v", got.Labels)
	}

	list, err := s.ListOverseers(ctx)
	if err != nil {
		t.Fatalf("list overseers: %v", err)
	}
	if len(list) != 1 || len(list[0].Labels) != 2 {
		t.Fatalf("list did not include labels: %v", list[0].Labels)
	}
}
