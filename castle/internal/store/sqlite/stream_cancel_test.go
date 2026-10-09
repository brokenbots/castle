package sqlite

// Regression tests for KB-222 (2026-09-21 incident): an expired gRPC stream
// passed its cancellable context straight into store calls, so the driver's
// context watcher fired sqlite3_interrupt on the shared serialized writer
// connection mid-write. Callers saw the in-flight write die (SQLITE_BUSY with
// the modern driver; interrupted (9) with the v1.33.1 deployed at the time,
// whose pager-lock wait loops abort on interrupt) and on the deployed driver
// the poisoned writer then failed every DB-backed call until restart.
//
// The store now detaches every operation through opCtx, so these tests pin:
//   - an append invoked with an already-expired stream context persists;
//   - an append cancelled mid-flight (queued behind the single serialized
//     writer) persists;
//   - the detached operation context carries a bounded deadline and ignores
//     parent cancellation;
//   - subsequent operations stay healthy afterwards.

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/brokenbots/castle/castle/internal/store"
)

// openSharedFileStore opens a file-backed store whose path is known so a raw
// connection can contend for the writer lock in the tests below.
func openSharedFileStore(t *testing.T) (*Store, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "stream.db")
	s, err := Open(path)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { s.Close() })
	return s, path
}

// seedRunForStream seeds the overseer and run the cancelled-stream tests
// append to.
func seedRunForStream(t *testing.T, s *Store) {
	t.Helper()
	ctx := context.Background()
	now := time.Now().UTC()
	if err := s.CreateOverseer(ctx, &store.Overseer{ID: "ov1", Name: "a", TokenHash: "x", Status: "online", CreatedAt: now, LastSeenAt: now}); err != nil {
		t.Fatalf("seed overseer: %v", err)
	}
	if err := s.CreateRun(ctx, &store.Run{ID: "run1", OverseerID: "ov1", WorkflowName: "wf", Status: "pending", CreatedAt: now}); err != nil {
		t.Fatalf("seed run: %v", err)
	}
}

func TestAppendEventOnExpiredStreamContextPersists(t *testing.T) {
	s, _ := openSharedFileStore(t)
	seedRunForStream(t, s)

	streamCtx, cancel := context.WithCancel(context.Background())
	cancel() // the Connect/gRPC stream deadline has already fired

	seq, inserted, err := s.AppendEvent(streamCtx, &store.Event{
		SchemaVersion: 1,
		RunID:         "run1",
		Type:          "step.log",
		Ts:            time.Now().UTC(),
		Payload:       []byte(`{"x":"y"}`),
	})
	if err != nil {
		t.Fatalf("AppendEvent on expired stream context: %v", err)
	}
	if !inserted || seq == 0 {
		t.Fatalf("AppendEvent inserted=%v seq=%d, want inserted=true seq>0", inserted, seq)
	}

	// The event must be durable, not dropped with the dead stream.
	events, err := s.ListEvents(context.Background(), "run1", 0, 10)
	if err != nil {
		t.Fatalf("ListEvents: %v", err)
	}
	if len(events) != 1 || events[0].Seq != seq {
		t.Fatalf("ListEvents = %d events (first seq %d), want exactly seq %d", len(events), events[0].Seq, seq)
	}
}

func TestAppendEventQueuedBehindPinnedWriterSurvivesStreamCancel(t *testing.T) {
	s, _ := openSharedFileStore(t)
	seedRunForStream(t, s)

	// Pin the store's single serialized writer connection while the
	// incident's expired stream issues its append: the append parks in the
	// database/sql queue for a free writer, exactly the kind of mid-flight
	// wait whose interruption corrupted the shared connection.
	pinned, err := s.db.Conn(context.Background())
	if err != nil {
		t.Fatalf("pin writer conn: %v", err)
	}

	streamCtx, cancel := context.WithCancel(context.Background())
	appendErr := make(chan error, 1)
	seqCh := make(chan uint64, 1)
	insertedCh := make(chan bool, 1)
	go func() {
		seq, inserted, err := s.AppendEvent(streamCtx, &store.Event{
			SchemaVersion: 1,
			RunID:         "run1",
			Type:          "step.log",
			Ts:            time.Now().UTC(),
			Payload:       []byte(`{"x":"y"}`),
		})
		appendErr <- err
		seqCh <- seq
		insertedCh <- inserted
	}()

	// Let the append reach the queue, then fire the stream deadline while it
	// is blocked mid-flight, then release the writer.
	time.Sleep(100 * time.Millisecond)
	cancel()
	time.Sleep(100 * time.Millisecond)
	if err := pinned.Close(); err != nil {
		t.Fatalf("release pinned writer: %v", err)
	}

	if err := <-appendErr; err != nil {
		t.Fatalf("AppendEvent cancelled mid-flight: %v", err)
	}
	if inserted := <-insertedCh; !inserted {
		t.Fatal("AppendEvent mid-cancellation not inserted")
	}
	seq := <-seqCh
	if seq == 0 {
		t.Fatal("AppendEvent mid-cancellation returned zero seq")
	}

	// The event survives the dead stream.
	events, err := s.ListEvents(context.Background(), "run1", 0, 10)
	if err != nil {
		t.Fatalf("ListEvents: %v", err)
	}
	if len(events) != 1 || events[0].Seq != seq {
		t.Fatalf("ListEvents = %d events, want exactly the mid-cancellation append (seq %d)", len(events), seq)
	}

	// Subsequent operations stay healthy — during the incident every such
	// call failed until the pod was restarted.
	for i := 0; i < 5; i++ {
		if _, err := s.GetRun(context.Background(), "run1"); err != nil {
			t.Fatalf("post-expiry GetRun #%d: %v", i, err)
		}
		now := time.Now().UTC()
		if _, _, err := s.AppendEvent(context.Background(), &store.Event{SchemaVersion: 1, RunID: "run1", Type: "criteria.heartbeat", Ts: now}); err != nil {
			t.Fatalf("post-expiry AppendEvent #%d: %v", i, err)
		}
		if err := s.UpdateOverseerSeen(context.Background(), "ov1", now); err != nil {
			t.Fatalf("post-expiry UpdateOverseerSeen #%d: %v", i, err)
		}
	}
}

func TestAppendEventOnAlreadyCancelledContextIsNotRejected(t *testing.T) {
	// Same durability property as the mid-write variant, from the other
	// ordering: the stream context dies before the append is even invoked.
	s, _ := openSharedFileStore(t)
	seedRunForStream(t, s)

	deadlineCtx, cancel := context.WithDeadline(context.Background(), time.Now().Add(-time.Minute))
	defer cancel()

	if _, _, err := s.AppendEvent(deadlineCtx, &store.Event{SchemaVersion: 1, RunID: "run1", Type: "step.log", Ts: time.Now().UTC(), Payload: []byte(`{}`)}); err != nil {
		t.Fatalf("AppendEvent on pre-expired deadline context: %v", err)
	}
	if _, err := s.GetRun(deadlineCtx, "run1"); err != nil {
		t.Fatalf("GetRun on pre-expired deadline context: %v", err)
	}
}

// TestOpCtxIgnoresParentCancellationAndStaysBounded pins the exact mechanism
// the fix relies on: value-preserving detachment from the caller's context
// (an expired gRPC stream deadline must never arm the driver's interrupt) and
// a fresh bounded deadline per operation. Nested calls are idempotent — each
// gets its own full budget, not a shrinking remainder.
func TestOpCtxIgnoresParentCancellationAndStaysBounded(t *testing.T) {
	s, _ := openSharedFileStore(t)

	streamCtx, cancel := context.WithCancel(context.Background())
	defer cancel()

	op, cancelOp := s.opCtx(streamCtx)
	defer cancelOp()
	if err := op.Err(); err != nil {
		t.Fatalf("opCtx error under cancelled parent: %v", err)
	}
	opDeadline, ok := op.Deadline()
	if !ok {
		t.Fatal("opCtx carries no deadline")
	}
	budget := time.Until(opDeadline)
	if budget <= 0 || budget > storeOpTimeout+time.Second {
		t.Fatalf("opCtx budget = %v, want ~%v", budget, storeOpTimeout)
	}

	cancel()
	time.Sleep(10 * time.Millisecond)
	if err := op.Err(); err != nil {
		t.Fatalf("opCtx cancelled after parent cancel: %v", err)
	}

	// Nested detachment is idempotent: the inner context still carries a
	// full fresh budget measured from its own creation.
	inner, cancelInner := s.opCtx(op)
	defer cancelInner()
	innerDeadline, ok := inner.Deadline()
	if !ok {
		t.Fatal("nested opCtx carries no deadline")
	}
	if d := time.Until(innerDeadline); d <= storeOpTimeout-time.Second {
		t.Fatalf("nested opCtx budget = %v, want a fresh ~%v", d, storeOpTimeout)
	}
	if err := inner.Err(); err != nil {
		t.Fatalf("nested opCtx error: %v", err)
	}
}

// TestRecoverWriterIsBestEffort pins the recovery helper's contract (KB-222):
// it never fails, never blocks past its bound on a healthy pool, and leaves
// the store usable. During the incident retries kept surfacing interrupted
// (9) for the whole outage window; Ping is the pool-level reset that forces
// a discarded connection when the driver judges one unusable.
func TestRecoverWriterIsBestEffort(t *testing.T) {
	s, _ := openSharedFileStore(t)
	seedRunForStream(t, s)

	s.recoverWriter(context.Background())

	if _, err := s.GetRun(context.Background(), "run1"); err != nil {
		t.Fatalf("GetRun after recoverWriter: %v", err)
	}
}

// TestReapStaleAgentRunsSanitizesContext pins that the reaper's context never
// propagates caller cancellation into its attempts: reapStaleAgentRunsAttempt
// derives reapAttemptTimeout from the detached opCtx, so a reaper caller whose
// own deadline is fine-grained cannot accidentally unbound (or kill) the
// attempt mid-write.
func TestReapStaleAgentRunsSanitizesContext(t *testing.T) {
	s, _ := openSharedFileStore(t)
	seedRunForStream(t, s)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	// staleBefore == now makes the freshly seeded run stale, so a healthy
	// call must actually do the read-modify-write: pre-fix this failed with
	// "context canceled" from BeginTx, and the reap never ran.
	reaped, err := s.ReapStaleAgentRuns(ctx, time.Now().UTC(), time.Now().UTC())
	if err != nil {
		t.Fatalf("ReapStaleAgentRuns on cancelled context: %v", err)
	}
	if len(reaped) != 1 || reaped[0] != "run1" {
		t.Fatalf("ReapStaleAgentRuns reaped %v, want exactly [run1]", reaped)
	}
}
