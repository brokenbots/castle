package sqlite

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/brokenbots/castle/castle/internal/store"
)

// reapFixture seeds two agents — one with a fresh heartbeat, one whose
// heartbeat stopped before staleBefore — and runs across the status space so
// reaping is only ever exercised where CRI-142 allows it.
type reapFixture struct {
	s   *Store
	ctx context.Context
	now time.Time
	// staleBefore is the boundary used for the reaper call.
	staleBefore time.Time
}

func mustRun(t *testing.T, s *Store, ctx context.Context, id string) *store.Run {
	t.Helper()
	r, err := s.GetRun(ctx, id)
	if err != nil {
		t.Fatalf("get run %s: %v", id, err)
	}
	return r
}

func newReapFixture(t *testing.T) *reapFixture {
	t.Helper()
	s := tempStore(t)
	now := time.Now().UTC()
	f := &reapFixture{s: s, ctx: context.Background(), now: now, staleBefore: now.Add(-60 * time.Second)}

	freshSeen := now.Add(-5 * time.Second) // heartbeat 5s ago: fresh
	staleSeen := now.Add(-10 * time.Minute)
	for _, o := range []struct {
		id   string
		seen time.Time
	}{
		{"agent-fresh", freshSeen},
		{"agent-stale", staleSeen},
	} {
		if err := s.CreateOverseer(f.ctx, &store.Overseer{
			ID: o.id, Name: o.id, TokenHash: "x", Status: "online",
			CreatedAt: o.seen, LastSeenAt: o.seen,
		}); err != nil {
			t.Fatalf("create overseer %s: %v", o.id, err)
		}
	}
	return f
}

func (f *reapFixture) createRun(t *testing.T, id, agentID, status string) {
	t.Helper()
	if err := f.s.CreateRun(f.ctx, &store.Run{
		ID: id, OverseerID: agentID, WorkflowName: "wf", Status: status, CreatedAt: f.now,
	}); err != nil {
		t.Fatalf("create run %s: %v", id, err)
	}
}

func (f *reapFixture) getRun(t *testing.T, id string) *store.Run {
	t.Helper()
	return mustRun(t, f.s, f.ctx, id)
}

// TestReapStaleAgentRuns covers the CRI-142 heartbeat-staleness reaper: only
// pending/running runs of agents whose heartbeat is stale are stamped failed
// with "agent heartbeat lost"; fresh agents, queued work, paused runs and
// already terminal runs are untouched.
func TestReapStaleAgentRuns(t *testing.T) {
	f := newReapFixture(t)

	// Stale agent: reapable states.
	f.createRun(t, "r-stale-pending", "agent-stale", "pending")
	f.createRun(t, "r-stale-running", "agent-stale", "running")
	// Stale agent: protected states.
	f.createRun(t, "r-stale-paused", "agent-stale", "paused")
	f.createRun(t, "r-stale-succeeded", "agent-stale", "succeeded")
	f.createRun(t, "r-stale-failed", "agent-stale", "failed")
	f.createRun(t, "r-stale-cancelled", "agent-stale", "cancelled")
	// Fresh agent: identical statuses must survive.
	f.createRun(t, "r-fresh-pending", "agent-fresh", "pending")
	f.createRun(t, "r-fresh-running", "agent-fresh", "running")

	reaped, err := f.s.ReapStaleAgentRuns(f.ctx, f.now, f.staleBefore)
	if err != nil {
		t.Fatalf("reap: %v", err)
	}
	if len(reaped) != 2 {
		t.Fatalf("want 2 reaped runs, got %d: %v", len(reaped), reaped)
	}

	for _, id := range []string{"r-stale-pending", "r-stale-running"} {
		r := f.getRun(t, id)
		if r.Status != "failed" {
			t.Errorf("%s: want failed, got %q", id, r.Status)
		}
		if r.FailureReason != "agent heartbeat lost" {
			t.Errorf("%s: failure_reason = %q", id, r.FailureReason)
		}
		if r.EndedAt == nil {
			t.Errorf("%s: ended_at not stamped", id)
		}
	}

	for _, id := range []string{
		"r-stale-paused", "r-stale-succeeded", "r-stale-failed", "r-stale-cancelled",
		"r-fresh-pending", "r-fresh-running",
	} {
		r := f.getRun(t, id)
		if r.FailureReason != "" {
			t.Errorf("%s: failure_reason must stay empty, got %q", id, r.FailureReason)
		}
	}
	if r := f.getRun(t, "r-fresh-pending"); r.Status != "pending" {
		t.Errorf("fresh pending run reaped: %q", r.Status)
	}
	if r := f.getRun(t, "r-stale-paused"); r.Status != "paused" {
		t.Errorf("paused run reaped: %q", r.Status)
	}
	if r := f.getRun(t, "r-stale-succeeded"); r.Status != "succeeded" {
		t.Errorf("terminal run rewritten: %q", r.Status)
	}
}

// TestReapStaleAgentRuns_MarksAssignmentTerminal verifies that reaping a
// leased run also terminates its queued or leased workflow assignment so a
// dead agent's work is never re-dispatched (CRI-142). Queued assignments are
// not touched: their runs have no owning agent yet.
func TestReapStaleAgentRuns_MarksAssignmentTerminal(t *testing.T) {
	f := newReapFixture(t)

	a, created, err := f.s.CreateWorkflowAssignment(f.ctx, &store.WorkflowAssignment{
		OwnerCriteriaID: "agent-stale",
		WorkflowName:    "wf",
		WorkflowSource:  "workflow main {}",
		IdempotencyKey:  "key-1",
		State:           store.WorkflowAssignmentStateQueued,
		CreatedAt:       f.now,
		UpdatedAt:       f.now,
	})
	if err != nil || !created {
		t.Fatalf("create assignment: %v created=%v", err, created)
	}

	// Queued: no owning agent, nothing to reap yet.
	reaped, err := f.s.ReapStaleAgentRuns(f.ctx, f.now, f.staleBefore)
	if err != nil {
		t.Fatalf("reap queued: %v", err)
	}
	if len(reaped) != 0 {
		t.Fatalf("queued run must not be reaped, got %v", reaped)
	}
	if r := f.getRun(t, a.RunID); r.Status != "pending" {
		t.Fatalf("queued run mutated: %q", r.Status)
	}

	// The stale agent leases the work; the run now belongs to it.
	leased, err := f.s.LeaseWorkflowAssignment(f.ctx, "agent-stale", nil, f.now, time.Minute)
	if err != nil {
		t.Fatalf("lease: %v", err)
	}
	if leased.State != store.WorkflowAssignmentStateLeased {
		t.Fatalf("lease state: %q", leased.State)
	}

	reaped, err = f.s.ReapStaleAgentRuns(f.ctx, f.now, f.staleBefore)
	if err != nil {
		t.Fatalf("reap leased: %v", err)
	}
	if len(reaped) != 1 || reaped[0] != a.RunID {
		t.Fatalf("want [%s], got %v", a.RunID, reaped)
	}
	if r := f.getRun(t, a.RunID); r.Status != "failed" || r.FailureReason != "agent heartbeat lost" {
		t.Fatalf("reaped run: status=%q reason=%q", r.Status, r.FailureReason)
	}

	got, err := f.s.GetWorkflowAssignment(f.ctx, a.ID)
	if err != nil {
		t.Fatalf("get assignment: %v", err)
	}
	if got.State != store.WorkflowAssignmentStateTerminal {
		t.Errorf("assignment state = %q, want terminal", got.State)
	}
	if got.TerminalReason != "agent heartbeat lost" {
		t.Errorf("assignment terminal_reason = %q", got.TerminalReason)
	}
}

// TestReapStaleAgentRuns_AgentHeartbeatLostMidRun is the CRI-142 regression
// scenario: an agent registers, heartbeats, starts a run, then dies (pod
// deleted). Once heartbeat staleness exceeds the configured window the run
// must be terminal without any action from the dead agent.
func TestReapStaleAgentRuns_AgentHeartbeatLostMidRun(t *testing.T) {
	s := tempStore(t)
	ctx := context.Background()

	t0 := time.Now().UTC().Add(-2 * time.Minute) // wall-clock start
	if err := s.CreateOverseer(ctx, &store.Overseer{
		ID: "agent-dies", Name: "runner", TokenHash: "x", Status: "online",
		CreatedAt: t0, LastSeenAt: t0,
	}); err != nil {
		t.Fatalf("register: %v", err)
	}
	if err := s.CreateRun(ctx, &store.Run{
		ID: "r-zombie", OverseerID: "agent-dies", WorkflowName: "wf", Status: "pending", CreatedAt: t0,
	}); err != nil {
		t.Fatalf("create run: %v", err)
	}

	// Agent leases the work and emits RunStarted; heartbeats continue briefly.
	if err := s.UpdateOverseerSeen(ctx, "agent-dies", t0.Add(10*time.Second)); err != nil {
		t.Fatalf("heartbeat: %v", err)
	}
	run := mustRun(t, s, ctx, "r-zombie")
	run.Status = "running"
	run.CurrentStep = "s1"
	if err := s.UpdateRun(ctx, run); err != nil {
		t.Fatalf("run started: %v", err)
	}

	// Last heartbeat at t0+30s. The agent dies here; no SubmitEvents terminal
	// stamp ever arrives.
	lastBeat := t0.Add(30 * time.Second)
	if err := s.UpdateOverseerSeen(ctx, "agent-dies", lastBeat); err != nil {
		t.Fatalf("last heartbeat: %v", err)
	}

	// Before expiry the run survives.
	if _, err := s.ReapStaleAgentRuns(ctx, lastBeat.Add(50*time.Second), lastBeat.Add(50*time.Second).Add(-60*time.Second)); err != nil {
		t.Fatalf("reap before expiry: %v", err)
	}
	if r := mustRun(t, s, ctx, "r-zombie"); r.Status != "running" {
		t.Fatalf("run reaped before heartbeat expiry: %q", r.Status)
	}

	// Past expiry (last beat + 61s > 60s staleness): the run goes terminal.
	reapNow := lastBeat.Add(61 * time.Second)
	reaped, err := s.ReapStaleAgentRuns(ctx, reapNow, reapNow.Add(-60*time.Second))
	if err != nil {
		t.Fatalf("reap after expiry: %v", err)
	}
	if len(reaped) != 1 || reaped[0] != "r-zombie" {
		t.Fatalf("want [r-zombie], got %v", reaped)
	}
	r := mustRun(t, s, ctx, "r-zombie")
	if r.Status != "failed" || r.FailureReason != "agent heartbeat lost" || r.EndedAt == nil {
		t.Fatalf("zombie not stamped: status=%q reason=%q ended=%v", r.Status, r.FailureReason, r.EndedAt)
	}
}

// TestCancelRun covers the CRI-142 operator cancel path at the store layer:
// active runs go terminal with the operator reason, terminal runs are never
// rewritten, and unknown ids are ErrNotFound.
func TestCancelRun(t *testing.T) {
	f := newReapFixture(t)

	f.createRun(t, "r-cancel-pending", "agent-fresh", "pending")
	f.createRun(t, "r-cancel-running", "agent-fresh", "running")
	f.createRun(t, "r-done", "agent-fresh", "succeeded")

	cancelledAt := f.now.Add(time.Minute)
	r, err := f.s.CancelRun(f.ctx, "r-cancel-pending", "criteriarun deleted", cancelledAt)
	if err != nil {
		t.Fatalf("cancel pending: %v", err)
	}
	if r.Status != "cancelled" || r.FailureReason != "criteriarun deleted" || r.EndedAt == nil {
		t.Fatalf("cancelled run: status=%q reason=%q ended=%v", r.Status, r.FailureReason, r.EndedAt)
	}
	if got := f.getRun(t, "r-cancel-pending"); got.Status != "cancelled" || got.FailureReason != "criteriarun deleted" {
		t.Fatalf("persisted cancel: status=%q reason=%q", got.Status, got.FailureReason)
	}

	// A default reason is recorded when the operator supplies none.
	if _, err := f.s.CancelRun(f.ctx, "r-cancel-running", "", cancelledAt); err != nil {
		t.Fatalf("cancel running: %v", err)
	}
	if got := f.getRun(t, "r-cancel-running"); got.Status != "cancelled" || got.FailureReason != "" {
		t.Fatalf("cancelled run: status=%q reason=%q", got.Status, got.FailureReason)
	}

	// Terminal states are never overwritten.
	if _, err := f.s.CancelRun(f.ctx, "r-done", "late", cancelledAt); !errors.Is(err, store.ErrRunTerminal) {
		t.Fatalf("cancel succeeded run: want ErrRunTerminal, got %v", err)
	}
	if got := f.getRun(t, "r-done"); got.Status != "succeeded" || got.EndedAt != nil {
		t.Fatalf("terminal run mutated: status=%q ended=%v", got.Status, got.EndedAt)
	}

	if _, err := f.s.CancelRun(f.ctx, "missing", "", cancelledAt); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("cancel unknown run: want ErrNotFound, got %v", err)
	}
}

// TestUpdateRun_PreservesFailureReason verifies the CRI-131-style partial
// update contract extends to failure_reason: a status update that carries no
// reason must not clear an existing one.
func TestUpdateRun_PreservesFailureReason(t *testing.T) {
	f := newReapFixture(t)
	f.createRun(t, "r-reason", "agent-stale", "pending")

	if _, err := f.s.CancelRun(f.ctx, "r-reason", "criteriarun deleted", f.now); err != nil {
		t.Fatalf("cancel: %v", err)
	}

	run := f.getRun(t, "r-reason")
	run.FailureReason = "" // callers holding a stale record must not clobber
	if err := f.s.UpdateRun(f.ctx, run); err != nil {
		t.Fatalf("update: %v", err)
	}
	if got := f.getRun(t, "r-reason"); got.FailureReason != "criteriarun deleted" {
		t.Fatalf("failure_reason clobbered: %q", got.FailureReason)
	}
}

// TestReapStaleAgentRuns_ScanRunsWhileWriterBusy is the CRI-143 structural
// regression: the reaper's staleness scan must run on the dedicated reader
// connection and never queue behind the single serialized writer connection
// that all RPC traffic shares. With the writer pinned by an open transaction
// (as an in-flight RPC write would hold it), the pre-CRI-143 implementation —
// which opened its scan in a transaction on that same pool — blocked for as
// long as the writer stayed busy.
func TestReapStaleAgentRuns_ScanRunsWhileWriterBusy(t *testing.T) {
	f := newReapFixture(t)

	// Hold the writer connection with an open transaction, like an in-flight
	// RPC write would.
	wtx, err := f.s.db.BeginTx(f.ctx, nil)
	if err != nil {
		t.Fatalf("begin writer tx: %v", err)
	}
	defer func() { _ = wtx.Rollback() }()

	type result struct {
		ids []string
		err error
	}
	done := make(chan result, 1)
	go func() {
		ids, err := f.s.ReapStaleAgentRuns(f.ctx, time.Now().UTC(), time.Now().UTC().Add(-time.Minute))
		done <- result{ids, err}
	}()
	select {
	case res := <-done:
		if res.err != nil {
			t.Fatalf("reap with busy writer: %v", res.err)
		}
		if len(res.ids) != 0 {
			t.Fatalf("empty store reaped runs: %v", res.ids)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("reaper scan blocked on the busy writer connection: the scan must run on the reader pool (CRI-143)")
	}
}

// TestReapStaleAgentRuns_UnderConcurrentRPCTraffic is the CRI-143 live
// regression (exit criteria): the reaper runs repeatedly while Register-like
// heartbeats, SubmitEvents-like writes and read RPCs are all active, and no
// non-reaper operation observes a transient fault. The reaper pass also
// completes: the stale agent's zombies are stamped failed with "agent
// heartbeat lost" while the fresh agent's runs stay untouched.
func TestReapStaleAgentRuns_UnderConcurrentRPCTraffic(t *testing.T) {
	f := newReapFixture(t)
	ctx := f.ctx

	f.createRun(t, "r-zombie-1", "agent-stale", "running")
	f.createRun(t, "r-zombie-2", "agent-stale", "pending")
	f.createRun(t, "r-live-1", "agent-fresh", "running")
	f.createRun(t, "r-live-2", "agent-fresh", "running")

	var (
		mu     sync.Mutex
		faults []string
	)
	record := func(op string, err error) {
		mu.Lock()
		defer mu.Unlock()
		faults = append(faults, fmt.Sprintf("%s: %v", op, err))
	}

	stop := make(chan struct{})
	var wg sync.WaitGroup
	run := func(name string, op func(i int)) {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; ; i++ {
				select {
				case <-stop:
					return
				default:
				}
				op(i)
			}
		}()
	}

	// Register/Heartbeat-like traffic: the fresh agent's heartbeat stays live.
	run("heartbeat", func(i int) {
		if err := f.s.UpdateOverseerSeen(ctx, "agent-fresh", time.Now().UTC()); err != nil {
			record("heartbeat", err)
		}
		time.Sleep(time.Millisecond)
	})
	// SubmitEvents-like traffic: ownership read plus a durable event append
	// per envelope, alternating across both live runs.
	run("submit-events", func(i int) {
		runID := "r-live-1"
		if i%2 == 0 {
			runID = "r-live-2"
		}
		if _, err := f.s.GetRun(ctx, runID); err != nil {
			record("submit get-run", err)
			return
		}
		ev := &store.Event{
			SchemaVersion: store.EventSchemaVersion,
			RunID:         runID,
			Type:          "step.log",
			Ts:            time.Now().UTC(),
			CorrelationID: fmt.Sprintf("corr-%d", i),
			Payload:       []byte(`{"step":"s1"}`),
		}
		if _, _, err := f.s.AppendEvent(ctx, ev); err != nil {
			record("submit append-event", err)
		}
	})
	// Read-only RPCs: ListRuns, ListEvents (ListRunEvents), ListOverseers
	// (ListAgents).
	run("reads", func(i int) {
		if _, _, err := f.s.ListRuns(ctx, "", "", 0, ""); err != nil {
			record("list-runs", err)
		}
		if _, err := f.s.ListEvents(ctx, "r-live-1", 0, 0); err != nil {
			record("list-events", err)
		}
		if _, err := f.s.ListOverseers(ctx); err != nil {
			record("list-agents", err)
		}
	})
	// The reaper itself: repeated passes while the traffic above runs.
	var reapPasses atomic.Int64
	run("reaper", func(i int) {
		_, err := f.s.ReapStaleAgentRuns(ctx, time.Now().UTC(), time.Now().UTC().Add(-60*time.Second))
		if err != nil {
			record("reaper", err)
			return
		}
		reapPasses.Add(1)
		time.Sleep(2 * time.Millisecond)
	})

	// Let the reaper collide with RPC traffic for a bounded window.
	time.Sleep(300 * time.Millisecond)
	close(stop)
	wg.Wait()

	if passes := reapPasses.Load(); passes == 0 {
		t.Fatal("reaper completed no successful passes under traffic")
	}
	if n := len(faults); n > 0 {
		if n > 10 {
			faults = faults[:10]
		}
		t.Fatalf("transient faults under concurrent reaping (%d total): %v", n, faults)
	}

	// The reaper stamped exactly the stale agent's zombies failed and left
	// the fresh agent's runs untouched.
	for _, id := range []string{"r-zombie-1", "r-zombie-2"} {
		got := mustRun(t, f.s, ctx, id)
		if got.Status != "failed" {
			t.Fatalf("run %s: status = %q, want failed", id, got.Status)
		}
		if got.FailureReason != "agent heartbeat lost" {
			t.Fatalf("run %s: failure_reason = %q, want %q", id, got.FailureReason, "agent heartbeat lost")
		}
		if got.EndedAt == nil {
			t.Fatalf("run %s: ended_at not stamped", id)
		}
	}
	for _, id := range []string{"r-live-1", "r-live-2"} {
		got := mustRun(t, f.s, ctx, id)
		if got.Status != "running" {
			t.Fatalf("run %s: status = %q, want running (live run clobbered by the reaper)", id, got.Status)
		}
	}
}

// TestReapRunIDs_DropsResolvedCandidatesFromWrite is the CRI-143 re-validation
// regression: when the write-transaction re-validation drops a strict subset
// of the scanned candidates — a run resolved, or its agent heartbeated, between
// the reader scan and the writer transaction — the surviving set is smaller
// and the UPDATE placeholder lists must be derived from the surviving ids. The
// pre-fix code reused the candidate-derived placeholder list, so any partial
// drop failed the whole pass with "missing argument with index N", stamped
// nothing, and returned an error (the exact race the re-validation exists to
// handle).
func TestReapRunIDs_DropsResolvedCandidatesFromWrite(t *testing.T) {
	f := newReapFixture(t)
	ctx := f.ctx

	// Still-active zombies on the stale agent: both must be reaped.
	f.createRun(t, "r-zombie-live", "agent-stale", "running")
	f.createRun(t, "r-zombie-live-2", "agent-stale", "pending")
	// Candidate that resolved before the write transaction.
	f.createRun(t, "r-resolved", "agent-stale", "running")
	if _, err := f.s.CancelRun(ctx, "r-resolved", "criteriarun deleted", f.now); err != nil {
		t.Fatalf("cancel resolved run: %v", err)
	}
	// Candidate whose agent heartbeated back to life between the scan
	// (heartbeat older than staleBefore) and the write (heartbeat fresh): its
	// agent exists but its heartbeat is still stale when the candidate list is
	// assembled, then the heartbeat lands before the write transaction.
	if err := f.s.CreateOverseer(ctx, &store.Overseer{
		ID: "agent-revived", Name: "agent-revived", TokenHash: "x", Status: "online",
		CreatedAt: f.now.Add(-10 * time.Minute), LastSeenAt: f.now.Add(-10 * time.Minute),
	}); err != nil {
		t.Fatalf("create overseer agent-revived: %v", err)
	}
	f.createRun(t, "r-heartbeated", "agent-revived", "running")
	if err := f.s.UpdateOverseerSeen(ctx, "agent-revived", f.now.Add(time.Second)); err != nil {
		t.Fatalf("heartbeat agent-revived: %v", err)
	}

	candidates := []string{"r-zombie-live", "r-zombie-live-2", "r-resolved", "r-heartbeated"}
	reason := "agent heartbeat lost"
	reaped, err := f.s.reapRunIDs(ctx, f.now, f.staleBefore, candidates, reason)
	if err != nil {
		t.Fatalf("reap with partially resolved candidates: %v", err)
	}
	wantReaped := []string{"r-zombie-live", "r-zombie-live-2"}
	if !slices.Equal(reaped, wantReaped) {
		t.Fatalf("reaped = %v, want %v", reaped, wantReaped)
	}

	for _, id := range wantReaped {
		got := f.getRun(t, id)
		if got.Status != "failed" {
			t.Fatalf("run %s: status = %q, want failed", id, got.Status)
		}
		if got.FailureReason != reason {
			t.Fatalf("run %s: failure_reason = %q, want %q", id, got.FailureReason, reason)
		}
		if got.EndedAt == nil {
			t.Fatalf("run %s: ended_at not stamped", id)
		}
	}

	// The resolved run keeps its own terminal state; the reaper never rewrites
	// it (CRI-142 invariant under the scan-then-write split).
	got := f.getRun(t, "r-resolved")
	if got.Status != "cancelled" {
		t.Fatalf("run r-resolved: status = %q, want cancelled", got.Status)
	}
	if got.FailureReason != "criteriarun deleted" {
		t.Fatalf("run r-resolved: failure_reason clobbered: %q", got.FailureReason)
	}
	// The revived run is untouched.
	got = f.getRun(t, "r-heartbeated")
	if got.Status != "running" {
		t.Fatalf("run r-heartbeated: status = %q, want running", got.Status)
	}
	if got.FailureReason != "" {
		t.Fatalf("run r-heartbeated: failure_reason = %q, want empty", got.FailureReason)
	}
}

// TestReapRunIDs_EmptyCandidates pins reapRunIDs' contract on empty input
// (CRI-143 review): with no candidates it must return (nil, nil) without
// opening a transaction — the candidate placeholder list cannot be derived
// from zero ids, and the pre-guard placeholder slicing panicked on an empty
// list ("slice bounds out of range [:-1]").
func TestReapRunIDs_EmptyCandidates(t *testing.T) {
	f := newReapFixture(t)
	for name, candidates := range map[string][]string{"nil": nil, "empty": {}} {
		t.Run(name, func(t *testing.T) {
			reaped, err := f.s.reapRunIDs(f.ctx, f.now, f.staleBefore, candidates, "agent heartbeat lost")
			if err != nil {
				t.Fatalf("reapRunIDs(%s candidates): %v", name, err)
			}
			if reaped != nil {
				t.Fatalf("reaped = %v, want nil", reaped)
			}
		})
	}
}

// TestReapStaleAgentRuns_TerminalTransitionRace drives the public reaper API
// against concurrent run-resolution traffic (the live CRI-143 wedge window):
// every reaper pass races CancelRun transitions on one of the two zombie
// candidates, so re-validation repeatedly sees a partial candidate drop.
// Regardless of interleaving, the reaper must never return an error, must
// always reap the unresolved zombie, and must never rewrite the resolved run.
func TestReapStaleAgentRuns_TerminalTransitionRace(t *testing.T) {
	f := newReapFixture(t)
	ctx := f.ctx

	f.createRun(t, "r-zombie-1", "agent-stale", "running")
	f.createRun(t, "r-zombie-2", "agent-stale", "running")

	var (
		mu     sync.Mutex
		faults []string
	)
	record := func(op string, err error) {
		mu.Lock()
		defer mu.Unlock()
		faults = append(faults, fmt.Sprintf("%s: %v", op, err))
	}

	stop := make(chan struct{})
	var wg sync.WaitGroup
	var reapPasses atomic.Int64

	// Run-resolution traffic against one candidate.
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			select {
			case <-stop:
				return
			default:
			}
			if _, err := f.s.CancelRun(ctx, "r-zombie-2", "criteriarun deleted", time.Now().UTC()); err != nil && !errors.Is(err, store.ErrRunTerminal) {
				record("cancel", err)
			}
			time.Sleep(time.Millisecond)
		}
	}()
	// The reaper itself: repeated public passes over the same candidate set.
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			select {
			case <-stop:
				return
			default:
			}
			if _, err := f.s.ReapStaleAgentRuns(ctx, time.Now().UTC(), time.Now().UTC().Add(-60*time.Second)); err != nil {
				record("reaper", err)
				return
			}
			reapPasses.Add(1)
		}
	}()

	time.Sleep(400 * time.Millisecond)
	close(stop)
	wg.Wait()

	if passes := reapPasses.Load(); passes == 0 {
		t.Fatal("reaper completed no successful passes under resolution traffic")
	}
	if n := len(faults); n > 0 {
		if n > 10 {
			faults = faults[:10]
		}
		t.Fatalf("faults under concurrent resolution (%d total): %v", n, faults)
	}

	// The unresolved zombie is reaped normally.
	got := f.getRun(t, "r-zombie-1")
	if got.Status != "failed" {
		t.Fatalf("run r-zombie-1: status = %q, want failed", got.Status)
	}
	if got.FailureReason != "agent heartbeat lost" {
		t.Fatalf("run r-zombie-1: failure_reason = %q, want %q", got.FailureReason, "agent heartbeat lost")
	}
	if got.EndedAt == nil {
		t.Fatal("run r-zombie-1: ended_at not stamped")
	}
	// The resolved run is terminal via exactly one of the two legitimate
	// paths; terminal runs are never rewritten afterwards.
	got = f.getRun(t, "r-zombie-2")
	if got.Status != "cancelled" && got.Status != "failed" {
		t.Fatalf("run r-zombie-2: status = %q, want cancelled or failed", got.Status)
	}
	if got.FailureReason != "criteriarun deleted" && got.FailureReason != "agent heartbeat lost" {
		t.Fatalf("run r-zombie-2: failure_reason = %q, want one of the two terminal reasons", got.FailureReason)
	}
	if got.EndedAt == nil {
		t.Fatal("run r-zombie-2: ended_at not stamped")
	}
}
