package rpc

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
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
// that the client abandons without draining. If any background writer is
// still writing past the store close, it recreates castle.db-wal/-shm
// sidecars in the TempDir and Go's own RemoveAll cleanup fails the run. The
// assertion is on teardown succeeding and the store directory being swept,
// not on store contents.
func TestStackTeardownAbandonedWatchRun(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	dir := t.TempDir()
	s, err := sqlite.Open(filepath.Join(dir, "castle.db"))
	if err != nil {
		t.Fatal(err)
	}
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
	// The handler reached its replay/cursor-writer stage once the first
	// replay event arrives; from here the client abandons the stream —
	// no further receives, only the disconnect that cancels the handler.
	if !watch.Receive() {
		t.Fatalf("expected first replay event, err=%v", watch.Err())
	}
	_ = watch.Close()
	// Give the server-side handler a moment to notice the disconnect (its
	// deferred cursor-writer finalize runs at that point); this is bounded
	// and must not depend on teardown ordering for correctness.
	time.Sleep(50 * time.Millisecond)

	// Explicit teardown mirrors the composite cleanup the stack helpers
	// register (drain → close → sweep); a second teardown from t.Cleanup is
	// an idempotent no-op.
	if err := teardownStack(ts, s, dir); err != nil {
		t.Fatalf("teardown of stack with abandoned watch: %v", err)
	}
	if _, err := os.ReadDir(dir); err != nil && !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("store dir not removed by sweep: %v", err)
	}
}