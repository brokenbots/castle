package rpc

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/brokenbots/castle/castle/internal/store/sqlite"
	pb "github.com/brokenbots/criteria/sdk/pb/criteria/v1" // import-lint:allow castle service bindings (W08: move to castle-proto)
)

// TestStackTeardownAbandonedWatchRun pins the teardown race behind the
// CI-only failure "TempDir RemoveAll cleanup: ... directory not empty"
// (TestApplyRunStatus_ResumeRestoresTerminalHandling, identical SHA green on
// one runner and red on another). The shape it constructs: a WAL-mode store
// in a t.TempDir, a run with persisted events, and a WatchRun stream with a
// SubscriberId — which starts the debounced cursor writer on the server —
// that the client abandons without draining, plus a variable-scope mutation
// still pending in the coalescer when teardown runs. If the stack teardown
// drops either (no drain, or a drain that discards pending work), the test
// fails: the writer leaks a sidecar file into the TempDir and Go's own
// RemoveAll cleanup fails the run with "directory not empty", or the drained
// mutation is not persisted. The final assertions are on teardown succeeding
// and the store contents, not on how the failure surfaces.
func TestStackTeardownAbandonedWatchRun(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	dir := t.TempDir()
	dbPath := filepath.Join(dir, "castle.db")
	s, err := sqlite.Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	// newTestStackOnStore registers no teardown cleanup — what runs at test
	// cleanup is tsrv.Close (registered by startServer), which waits for the
	// outstanding watch handler to finish after the client disconnects. This
	// test performs the ordered teardown explicitly so its stages are
	// asserted: drain → close → sidecar-leak check → persistence → sweep.
	ts := newTestStackOnStore(t, s, discardLogger())
	_, oClient, cClient := ts.startServer(t)

	overseerID, _ := mustRegister(t, oClient)
	run, err := oClient.CreateRun(ctx, connect.NewRequest(&pb.CreateRunRequest{CriteriaId: overseerID, WorkflowName: "wf"}))
	if err != nil {
		t.Fatal(err)
	}
	runID := run.Msg.RunId

	env := &pb.Envelope{SchemaVersion: 1, RunId: runID, Ts: timestamppb.Now(), Payload: &pb.Envelope_StepEntered{StepEntered: &pb.StepEntered{Step: "r", Adapter: "shell", Attempt: 1}}}
	if _, _, err := ts.store.AppendEvent(ctx, mustStoreEvent(t, env)); err != nil {
		t.Fatal(err)
	}

	watch, err := cClient.WatchRun(ctx, connect.NewRequest(&pb.WatchRunRequest{RunId: runID, SinceSeq: 0, SubscriberId: "sub-abandoned"}))
	if err != nil {
		t.Fatal(err)
	}
	// The handler has reached its replay/cursor-writer stage once the first
	// replay event arrives; from here the client abandons the stream — no
	// further receives, only the disconnect that cancels the handler.
	if !watch.Receive() {
		t.Fatalf("expected first replay event, err=%v", watch.Err())
	}
	_ = watch.Close()
	// Give the server-side handler a moment to notice the disconnect, so its
	// deferred cursor-writer finalize has run before the drain; bounded and
	// must not depend on teardown ordering for correctness.
	time.Sleep(50 * time.Millisecond)

	// Queue a scope mutation that is still pending when teardown runs (same
	// shape as reattach_test.go). It is debounced for scopeFlushInterval, so
	// the coalescer's timer cannot fire before the drain below — persistence
	// must come from the drain, or nothing will persist it at all.
	ts.criteria.scope.Enqueue(runID, func(scope map[string]interface{}) {
		scope["var"] = map[string]interface{}{"drained": "true"}
	})

	// Stage 1: drain + close, with no directory side effects while the store
	// is still inspectable.
	if err := teardownStackStop(ts, s); err != nil {
		t.Fatalf("drain and close: %v", err)
	}
	// Stage 2: no leaked WAL sidecars — after a clean close the driver
	// deletes castle.db-wal/-shm, so a straggler writer shows up here.
	if leaked := leakedStoreSidecars(dir, filepath.Base(dbPath)); len(leaked) > 0 {
		t.Fatalf("leaked WAL sidecars after store close: %v", leaked)
	}
	// Stage 3: the drained coalescer mutation must be persisted to the store
	// (reads fail if DrainBackgroundWriters is removed or turned into a
	// no-op, and if a Stop implementation discards pending work).
	ro, err := sqlite.Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = ro.Close() }()
	scopeJSON, err := ro.GetRunVariableScope(ctx, runID)
	if err != nil {
		t.Fatalf("GetRunVariableScope after teardown: %v", err)
	}
	if !strings.Contains(scopeJSON, "drained") || !strings.Contains(scopeJSON, "true") {
		t.Fatalf("drained scope mutation not persisted, scope=%q", scopeJSON)
	}
	// Stage 4: sweep so the surrounding TempDir removal cannot fail with
	// "directory not empty"; the directory must be gone (or empty) after.
	if err := sweepStoreDir(dir, 5, 20*time.Millisecond); err != nil {
		t.Fatalf("sweep store dir: %v", err)
	}
	if _, err := os.ReadDir(dir); err != nil && !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("store dir not removed by sweep: %v", err)
	}
}
