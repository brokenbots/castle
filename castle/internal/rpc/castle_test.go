package rpc

import (
	"context"
	"errors"
	"log/slog"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/brokenbots/castle/castle/internal/hub"
	"github.com/brokenbots/castle/castle/internal/store"
	"github.com/brokenbots/castle/castle/internal/store/sqlite"
	criteria "github.com/brokenbots/criteria/sdk"
	pb "github.com/brokenbots/criteria/sdk/pb/criteria/v1"                // import-lint:allow castle service bindings (W08: move to castle-proto)
	"github.com/brokenbots/criteria/sdk/pb/criteria/v1/criteriav1connect" // import-lint:allow castle service bindings (W08: move to castle-proto)
)

type recordedLog struct {
	Message string
	Attrs   map[string]any
}

type recordingSlogHandler struct {
	mu   sync.Mutex
	logs []recordedLog
}

func (h *recordingSlogHandler) Enabled(context.Context, slog.Level) bool { return true }

func (h *recordingSlogHandler) Handle(_ context.Context, r slog.Record) error {
	attrs := make(map[string]any)
	r.Attrs(func(a slog.Attr) bool {
		attrs[a.Key] = a.Value.Any()
		return true
	})
	h.mu.Lock()
	h.logs = append(h.logs, recordedLog{Message: r.Message, Attrs: attrs})
	h.mu.Unlock()
	return nil
}

func (h *recordingSlogHandler) WithAttrs(_ []slog.Attr) slog.Handler { return h }
func (h *recordingSlogHandler) WithGroup(_ string) slog.Handler      { return h }

func (h *recordingSlogHandler) snapshot() []recordedLog {
	h.mu.Lock()
	defer h.mu.Unlock()
	out := make([]recordedLog, len(h.logs))
	copy(out, h.logs)
	return out
}

type cursorSpyStore struct {
	store.Store

	mu      sync.Mutex
	upserts int
	lastSeq map[string]uint64
}

func newCursorSpyStore(base store.Store) *cursorSpyStore {
	return &cursorSpyStore{Store: base, lastSeq: make(map[string]uint64)}
}

func (s *cursorSpyStore) UpsertSubscriberCursor(ctx context.Context, subscriberID, runID string, lastSeq uint64) error {
	err := s.Store.UpsertSubscriberCursor(ctx, subscriberID, runID, lastSeq)
	if err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.upserts++
	s.lastSeq[subscriberID+"/"+runID] = lastSeq
	return nil
}

func (s *cursorSpyStore) UpsertCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.upserts
}

func newCursorSpyStack(t *testing.T) (*testStack, *cursorSpyStore) {
	t.Helper()
	baseStore, err := sqlite.Open(filepath.Join(t.TempDir(), "castle.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = baseStore.Close() })

	wrapped := newCursorSpyStore(baseStore)
	h := hub.New()
	controls := NewControlRegistry()
	log := slog.New(slog.NewTextHandler(testWriter{t: t}, nil))
	stack := &testStack{
		store:    wrapped,
		hub:      h,
		controls: controls,
		criteria: NewCriteriaServer(wrapped, h, log, controls),
		server:   NewServerServer(wrapped, h, log, controls),
	}
	return stack, wrapped
}

type testWriter struct{ t *testing.T }

func (w testWriter) Write(p []byte) (int, error) {
	w.t.Logf("%s", p)
	return len(p), nil
}

func TestCastleListRunEventsPaging(t *testing.T) {
	ts := newTestStack(t)
	_, oClient, cClient := ts.startServer(t)
	overseerID, _ := mustRegister(t, oClient)
	run, err := oClient.CreateRun(context.Background(), connect.NewRequest(&pb.CreateRunRequest{CriteriaId: overseerID, WorkflowName: "wf"}))
	if err != nil {
		t.Fatal(err)
	}

	for i := 0; i < 1100; i++ {
		env := &pb.Envelope{
			SchemaVersion: 1,
			RunId:         run.Msg.RunId,
			Ts:            timestamppb.Now(),
			Payload:       &pb.Envelope_StepEntered{StepEntered: &pb.StepEntered{Step: "s", Adapter: "shell", Attempt: 1}},
		}
		if _, _, err := ts.store.AppendEvent(context.Background(), mustStoreEvent(t, env)); err != nil {
			t.Fatal(err)
		}
	}

	resp, err := cClient.ListRunEvents(context.Background(), connect.NewRequest(&pb.ListRunEventsRequest{RunId: run.Msg.RunId, SinceSeq: 0, Limit: 1050}))
	if err != nil {
		t.Fatal(err)
	}
	if got := len(resp.Msg.Events); got != 1050 {
		t.Fatalf("events=%d want 1050", got)
	}
	if resp.Msg.LastSeq != 1050 {
		t.Fatalf("last_seq=%d", resp.Msg.LastSeq)
	}
	if resp.Msg.NextSinceSeq != 1050 {
		t.Fatalf("next_since_seq=%d", resp.Msg.NextSinceSeq)
	}
}

func TestListRunEvents_RejectsOversizedLimit(t *testing.T) {
	ts := newTestStack(t)
	_, oClient, cClient := ts.startServer(t)
	overseerID, _ := mustRegister(t, oClient)
	run, err := oClient.CreateRun(context.Background(), connect.NewRequest(&pb.CreateRunRequest{CriteriaId: overseerID, WorkflowName: "wf"}))
	if err != nil {
		t.Fatal(err)
	}

	_, err = cClient.ListRunEvents(context.Background(), connect.NewRequest(&pb.ListRunEventsRequest{RunId: run.Msg.RunId, SinceSeq: 0, Limit: 3000}))
	if connect.CodeOf(err) != connect.CodeInvalidArgument {
		t.Fatalf("expected invalid argument, got %v", err)
	}
}

func TestListRunEvents_OverThreshold_PagesInternally(t *testing.T) {
	ts := newTestStack(t)
	_, oClient, cClient := ts.startServer(t)
	overseerID, _ := mustRegister(t, oClient)
	run, err := oClient.CreateRun(context.Background(), connect.NewRequest(&pb.CreateRunRequest{CriteriaId: overseerID, WorkflowName: "wf"}))
	if err != nil {
		t.Fatal(err)
	}

	for i := 0; i < 1500; i++ {
		env := &pb.Envelope{
			SchemaVersion: 1,
			RunId:         run.Msg.RunId,
			Ts:            timestamppb.Now(),
			Payload:       &pb.Envelope_StepEntered{StepEntered: &pb.StepEntered{Step: "s", Adapter: "shell", Attempt: 1}},
		}
		if _, _, err := ts.store.AppendEvent(context.Background(), mustStoreEvent(t, env)); err != nil {
			t.Fatal(err)
		}
	}

	resp, err := cClient.ListRunEvents(context.Background(), connect.NewRequest(&pb.ListRunEventsRequest{RunId: run.Msg.RunId, SinceSeq: 0, Limit: 1500}))
	if err != nil {
		t.Fatal(err)
	}
	if got := len(resp.Msg.Events); got != 1500 {
		t.Fatalf("events=%d want 1500", got)
	}
	for i, env := range resp.Msg.Events {
		want := uint64(i + 1)
		if env.Seq != want {
			t.Fatalf("seq[%d]=%d want %d", i, env.Seq, want)
		}
	}
	if resp.Msg.LastSeq != 1500 {
		t.Fatalf("last_seq=%d want 1500", resp.Msg.LastSeq)
	}
	if resp.Msg.NextSinceSeq != 1500 {
		t.Fatalf("next_since_seq=%d want 1500", resp.Msg.NextSinceSeq)
	}
}

func TestWatchRunReplayAndTail(t *testing.T) {
	ts := newTestStack(t)
	_, oClient, cClient := ts.startServer(t)
	overseerID, _ := mustRegister(t, oClient)
	run, err := oClient.CreateRun(context.Background(), connect.NewRequest(&pb.CreateRunRequest{CriteriaId: overseerID, WorkflowName: "wf"}))
	if err != nil {
		t.Fatal(err)
	}
	runID := run.Msg.RunId

	replayEnv := &pb.Envelope{SchemaVersion: 1, RunId: runID, Ts: timestamppb.Now(), Payload: &pb.Envelope_StepEntered{StepEntered: &pb.StepEntered{Step: "r1", Adapter: "shell", Attempt: 1}}}
	seq1, _, err := ts.store.AppendEvent(context.Background(), mustStoreEvent(t, replayEnv))
	if err != nil {
		t.Fatal(err)
	}
	replayEnv.Seq = seq1
	ts.hub.Publish(replayEnv)

	watch, err := cClient.WatchRun(context.Background(), connect.NewRequest(&pb.WatchRunRequest{RunId: runID}))
	if err != nil {
		t.Fatal(err)
	}
	if !watch.Receive() {
		t.Fatalf("expected replay event, err=%v", watch.Err())
	}
	if watch.Msg().Seq != 1 {
		t.Fatalf("replay seq=%d", watch.Msg().Seq)
	}
	// WatchReady is emitted after durable replay and before live tailing.
	if !watch.Receive() {
		t.Fatalf("expected WatchReady, err=%v", watch.Err())
	}
	if _, ok := watch.Msg().Payload.(*pb.Envelope_WatchReady); !ok {
		t.Fatalf("expected WatchReady, got %T", watch.Msg().Payload)
	}

	liveEnv := &pb.Envelope{SchemaVersion: 1, RunId: runID, Ts: timestamppb.Now(), Payload: &pb.Envelope_StepEntered{StepEntered: &pb.StepEntered{Step: "r2", Adapter: "shell", Attempt: 1}}}
	seq2, _, err := ts.store.AppendEvent(context.Background(), mustStoreEvent(t, liveEnv))
	if err != nil {
		t.Fatal(err)
	}
	liveEnv.Seq = seq2
	ts.hub.Publish(liveEnv)
	if !watch.Receive() {
		t.Fatalf("expected live event, err=%v", watch.Err())
	}
	if watch.Msg().Seq != 2 {
		t.Fatalf("live seq=%d", watch.Msg().Seq)
	}

	terminal := criteria.NewEnvelope(runID, &pb.RunFailed{Reason: "x"})
	terminal.Ts = timestamppb.New(time.Now().UTC())
	seq3, _, err := ts.store.AppendEvent(context.Background(), mustStoreEvent(t, terminal))
	if err != nil {
		t.Fatal(err)
	}
	terminal.Seq = seq3
	ts.hub.Publish(terminal)

	if !watch.Receive() {
		t.Fatalf("expected terminal event, err=%v", watch.Err())
	}
	if watch.Msg().Seq != 3 {
		t.Fatalf("terminal seq=%d", watch.Msg().Seq)
	}
	if watch.Receive() {
		t.Fatal("expected stream close after terminal event")
	}
}

func TestWatchRun_TerminalInReplay_ClosesImmediately(t *testing.T) {
	ts := newTestStack(t)
	_, oClient, cClient := ts.startServer(t)
	overseerID, _ := mustRegister(t, oClient)
	run, err := oClient.CreateRun(context.Background(), connect.NewRequest(&pb.CreateRunRequest{CriteriaId: overseerID, WorkflowName: "wf"}))
	if err != nil {
		t.Fatal(err)
	}
	runID := run.Msg.RunId

	replayEnv := &pb.Envelope{SchemaVersion: 1, RunId: runID, Ts: timestamppb.Now(), Payload: &pb.Envelope_StepEntered{StepEntered: &pb.StepEntered{Step: "r1", Adapter: "shell", Attempt: 1}}}
	seq1, _, err := ts.store.AppendEvent(context.Background(), mustStoreEvent(t, replayEnv))
	if err != nil {
		t.Fatal(err)
	}
	replayEnv.Seq = seq1
	ts.hub.Publish(replayEnv)

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	watch, err := cClient.WatchRun(ctx, connect.NewRequest(&pb.WatchRunRequest{RunId: runID}))
	if err != nil {
		t.Fatal(err)
	}

	if !watch.Receive() {
		t.Fatalf("expected replay event 1, err=%v", watch.Err())
	}
	if watch.Msg().Seq != 1 {
		t.Fatalf("replay seq=%d want 1", watch.Msg().Seq)
	}
	if !watch.Receive() {
		t.Fatalf("expected WatchReady after replay, err=%v", watch.Err())
	}
	if _, ok := watch.Msg().Payload.(*pb.Envelope_WatchReady); !ok {
		t.Fatalf("expected WatchReady, got %T", watch.Msg().Payload)
	}

	terminal := criteria.NewEnvelope(runID, &pb.RunFailed{Reason: "x"})
	terminal.Ts = timestamppb.New(time.Now().UTC())
	seq2, _, err := ts.store.AppendEvent(context.Background(), mustStoreEvent(t, terminal))
	if err != nil {
		t.Fatal(err)
	}
	terminal.Seq = seq2
	ts.hub.Publish(terminal)

	if !watch.Receive() {
		t.Fatalf("expected live terminal event, err=%v", watch.Err())
	}
	if watch.Msg().Seq != 2 {
		t.Fatalf("terminal seq=%d want 2", watch.Msg().Seq)
	}
	if watch.Receive() {
		t.Fatal("expected stream close immediately after replayed terminal")
	}
	if err := watch.Err(); err != nil {
		t.Fatalf("expected clean stream close, got err=%v", err)
	}
}

func TestWatchRun_ReplaysFromBuffer(t *testing.T) {
	ts := newTestStack(t)
	_, oClient, cClient := ts.startServer(t)
	overseerID, _ := mustRegister(t, oClient)
	run, err := oClient.CreateRun(context.Background(), connect.NewRequest(&pb.CreateRunRequest{CriteriaId: overseerID, WorkflowName: "wf"}))
	if err != nil {
		t.Fatal(err)
	}
	runID := run.Msg.RunId

	for i := 1; i <= 10; i++ {
		env := &pb.Envelope{
			SchemaVersion: 1,
			RunId:         runID,
			Ts:            timestamppb.Now(),
			Payload:       &pb.Envelope_StepEntered{StepEntered: &pb.StepEntered{Step: "r", Adapter: "shell", Attempt: int32(i)}},
		}
		seq, _, err := ts.store.AppendEvent(context.Background(), mustStoreEvent(t, env))
		if err != nil {
			t.Fatal(err)
		}
		env.Seq = seq
		ts.hub.Publish(env)
	}

	watch, err := cClient.WatchRun(context.Background(), connect.NewRequest(&pb.WatchRunRequest{RunId: runID, SinceSeq: 0}))
	if err != nil {
		t.Fatal(err)
	}

	for i := uint64(1); i <= 10; i++ {
		if !watch.Receive() {
			t.Fatalf("expected seq %d, err=%v", i, watch.Err())
		}
		if watch.Msg().Seq != i {
			t.Fatalf("seq=%d want %d", watch.Msg().Seq, i)
		}
	}
	if !watch.Receive() {
		t.Fatalf("expected WatchReady, err=%v", watch.Err())
	}
	if _, ok := watch.Msg().Payload.(*pb.Envelope_WatchReady); !ok {
		t.Fatalf("expected WatchReady, got %T", watch.Msg().Payload)
	}
	_ = watch.Close()
}

func TestWatchRun_DeDupesBufferVsLive(t *testing.T) {
	ts := newTestStack(t)
	_, oClient, cClient := ts.startServer(t)
	overseerID, _ := mustRegister(t, oClient)
	run, err := oClient.CreateRun(context.Background(), connect.NewRequest(&pb.CreateRunRequest{CriteriaId: overseerID, WorkflowName: "wf"}))
	if err != nil {
		t.Fatal(err)
	}
	runID := run.Msg.RunId

	env := &pb.Envelope{
		SchemaVersion: 1,
		RunId:         runID,
		Ts:            timestamppb.Now(),
		Payload:       &pb.Envelope_StepEntered{StepEntered: &pb.StepEntered{Step: "r1", Adapter: "shell", Attempt: 1}},
	}
	seq, _, err := ts.store.AppendEvent(context.Background(), mustStoreEvent(t, env))
	if err != nil {
		t.Fatal(err)
	}
	env.Seq = seq
	ts.hub.Publish(env)

	watch, err := cClient.WatchRun(context.Background(), connect.NewRequest(&pb.WatchRunRequest{RunId: runID, SinceSeq: 0}))
	if err != nil {
		t.Fatal(err)
	}
	if !watch.Receive() {
		t.Fatalf("expected replay event, err=%v", watch.Err())
	}
	if watch.Msg().Seq != 1 {
		t.Fatalf("replay seq=%d want 1", watch.Msg().Seq)
	}
	if !watch.Receive() {
		t.Fatalf("expected WatchReady, err=%v", watch.Err())
	}
	if _, ok := watch.Msg().Payload.(*pb.Envelope_WatchReady); !ok {
		t.Fatalf("expected WatchReady, got %T", watch.Msg().Payload)
	}

	// Re-publish the same seq as a simulated drain/live overlap; the stream
	// must not emit the same sequence twice.
	ts.hub.Publish(env)

	live := &pb.Envelope{
		SchemaVersion: 1,
		RunId:         runID,
		Ts:            timestamppb.Now(),
		Payload:       &pb.Envelope_RunFailed{RunFailed: &pb.RunFailed{Reason: "done"}},
	}
	seq2, _, err := ts.store.AppendEvent(context.Background(), mustStoreEvent(t, live))
	if err != nil {
		t.Fatal(err)
	}
	live.Seq = seq2
	ts.hub.Publish(live)

	if !watch.Receive() {
		t.Fatalf("expected terminal event, err=%v", watch.Err())
	}
	if watch.Msg().Seq != 2 {
		t.Fatalf("terminal seq=%d want 2", watch.Msg().Seq)
	}
	if watch.Receive() {
		t.Fatal("expected stream close after terminal")
	}
}

func TestWatchRun_ReplaysPersistedWhenBufferEmpty(t *testing.T) {
	ts := newTestStack(t)
	_, oClient, cClient := ts.startServer(t)
	overseerID, _ := mustRegister(t, oClient)
	run, err := oClient.CreateRun(context.Background(), connect.NewRequest(&pb.CreateRunRequest{CriteriaId: overseerID, WorkflowName: "wf"}))
	if err != nil {
		t.Fatal(err)
	}
	runID := run.Msg.RunId

	for i := 1; i <= 3; i++ {
		env := &pb.Envelope{
			SchemaVersion: 1,
			RunId:         runID,
			Ts:            timestamppb.Now(),
			Payload:       &pb.Envelope_StepEntered{StepEntered: &pb.StepEntered{Step: "r", Adapter: "shell", Attempt: int32(i)}},
		}
		if _, _, err := ts.store.AppendEvent(context.Background(), mustStoreEvent(t, env)); err != nil {
			t.Fatal(err)
		}
	}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	watch, err := cClient.WatchRun(ctx, connect.NewRequest(&pb.WatchRunRequest{RunId: runID, SinceSeq: 1}))
	if err != nil {
		t.Fatal(err)
	}

	if !watch.Receive() {
		t.Fatalf("expected persisted seq 2, err=%v", watch.Err())
	}
	if watch.Msg().Seq != 2 {
		t.Fatalf("first replay seq=%d want 2", watch.Msg().Seq)
	}
	if !watch.Receive() {
		t.Fatalf("expected persisted seq 3, err=%v", watch.Err())
	}
	if watch.Msg().Seq != 3 {
		t.Fatalf("second replay seq=%d want 3", watch.Msg().Seq)
	}
	if !watch.Receive() {
		t.Fatalf("expected WatchReady, err=%v", watch.Err())
	}
	if _, ok := watch.Msg().Payload.(*pb.Envelope_WatchReady); !ok {
		t.Fatalf("expected WatchReady, got %T", watch.Msg().Payload)
	}
	_ = watch.Close()
}

func TestWatchRun_WatchReadyAfterPersistedReplay(t *testing.T) {
	ts := newTestStack(t)
	_, oClient, cClient := ts.startServer(t)
	overseerID, _ := mustRegister(t, oClient)
	run, err := oClient.CreateRun(context.Background(), connect.NewRequest(&pb.CreateRunRequest{CriteriaId: overseerID, WorkflowName: "wf"}))
	if err != nil {
		t.Fatal(err)
	}
	runID := run.Msg.RunId

	replayEnv := &pb.Envelope{SchemaVersion: 1, RunId: runID, Ts: timestamppb.Now(), Payload: &pb.Envelope_StepEntered{StepEntered: &pb.StepEntered{Step: "r1", Adapter: "shell", Attempt: 1}}}
	if _, _, err := ts.store.AppendEvent(context.Background(), mustStoreEvent(t, replayEnv)); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	watch, err := cClient.WatchRun(ctx, connect.NewRequest(&pb.WatchRunRequest{RunId: runID}))
	if err != nil {
		t.Fatal(err)
	}

	if !watch.Receive() {
		t.Fatalf("expected replay event before WatchReady, err=%v", watch.Err())
	}
	if _, ok := watch.Msg().Payload.(*pb.Envelope_WatchReady); ok {
		t.Fatal("WatchReady arrived before persisted replay")
	}
	if watch.Msg().Seq != 1 {
		t.Fatalf("replay seq=%d want 1", watch.Msg().Seq)
	}

	if !watch.Receive() {
		t.Fatalf("expected WatchReady after replay, err=%v", watch.Err())
	}
	if _, ok := watch.Msg().Payload.(*pb.Envelope_WatchReady); !ok {
		t.Fatalf("expected WatchReady, got %T", watch.Msg().Payload)
	}
	_ = watch.Close()
}

func TestWatchRun_BufferCapacityLateSubscriberAndEvictionWarning(t *testing.T) {
	recordingHandler := &recordingSlogHandler{}
	log := slog.New(recordingHandler)

	store, err := sqlite.Open(filepath.Join(t.TempDir(), "castle.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })

	h := hub.NewWithBuffer(1024, log)
	controls := NewControlRegistry()
	ts := &testStack{
		store:    store,
		hub:      h,
		controls: controls,
		criteria: NewCriteriaServer(store, h, log, controls),
		server:   NewServerServer(store, h, log, controls),
	}

	_, oClient, cClient := ts.startServer(t)
	overseerID, _ := mustRegister(t, oClient)
	run, err := oClient.CreateRun(context.Background(), connect.NewRequest(&pb.CreateRunRequest{CriteriaId: overseerID, WorkflowName: "wf"}))
	if err != nil {
		t.Fatal(err)
	}
	runID := run.Msg.RunId

	for i := 1; i <= 1500; i++ {
		h.Publish(&pb.Envelope{
			SchemaVersion: 1,
			RunId:         runID,
			Seq:           uint64(i),
			Ts:            timestamppb.Now(),
			Payload:       &pb.Envelope_StepEntered{StepEntered: &pb.StepEntered{Step: "s", Adapter: "shell", Attempt: 1}},
		})
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	watch, err := cClient.WatchRun(ctx, connect.NewRequest(&pb.WatchRunRequest{RunId: runID, SinceSeq: 0}))
	if err != nil {
		t.Fatal(err)
	}

	if !watch.Receive() {
		t.Fatalf("expected WatchReady, err=%v", watch.Err())
	}
	if _, ok := watch.Msg().Payload.(*pb.Envelope_WatchReady); !ok {
		t.Fatalf("expected WatchReady, got %T", watch.Msg().Payload)
	}

	for i := 477; i <= 1500; i++ {
		if !watch.Receive() {
			t.Fatalf("expected seq %d, err=%v", i, watch.Err())
		}
		if watch.Msg().Seq != uint64(i) {
			t.Fatalf("seq=%d want %d", watch.Msg().Seq, i)
		}
	}
	_ = watch.Close()

	warnFound := false
	for _, entry := range recordingHandler.snapshot() {
		if entry.Message != "event buffer rotated" {
			continue
		}
		rid, okRID := entry.Attrs["run_id"].(string)
		if !okRID || rid != runID {
			continue
		}
		if _, ok := entry.Attrs["oldest_seq"]; ok {
			warnFound = true
			break
		}
	}
	if !warnFound {
		t.Fatal("expected event buffer rotated warning with run_id and oldest_seq")
	}
}

func TestWatchRun_CursorResolution_NoPriorCursor(t *testing.T) {
	ts := newTestStack(t)
	_, oClient, cClient := ts.startServer(t)
	overseerID, _ := mustRegister(t, oClient)
	run, err := oClient.CreateRun(context.Background(), connect.NewRequest(&pb.CreateRunRequest{CriteriaId: overseerID, WorkflowName: "wf"}))
	if err != nil {
		t.Fatal(err)
	}
	runID := run.Msg.RunId

	for i := 1; i <= 3; i++ {
		env := &pb.Envelope{SchemaVersion: 1, RunId: runID, Ts: timestamppb.Now(), Payload: &pb.Envelope_StepEntered{StepEntered: &pb.StepEntered{Step: "r", Adapter: "shell", Attempt: int32(i)}}}
		if _, _, err := ts.store.AppendEvent(context.Background(), mustStoreEvent(t, env)); err != nil {
			t.Fatal(err)
		}
	}

	watch, err := cClient.WatchRun(context.Background(), connect.NewRequest(&pb.WatchRunRequest{RunId: runID, SinceSeq: 0, SubscriberId: "sub-a"}))
	if err != nil {
		t.Fatal(err)
	}

	if !watch.Receive() {
		t.Fatalf("expected first replay event, err=%v", watch.Err())
	}
	if watch.Msg().Seq != 1 {
		t.Fatalf("first replay seq=%d want 1", watch.Msg().Seq)
	}
	_ = watch.Close()
}

func TestWatchRun_CursorResolution_WithCursor(t *testing.T) {
	ts := newTestStack(t)
	_, oClient, cClient := ts.startServer(t)
	overseerID, _ := mustRegister(t, oClient)
	run, err := oClient.CreateRun(context.Background(), connect.NewRequest(&pb.CreateRunRequest{CriteriaId: overseerID, WorkflowName: "wf"}))
	if err != nil {
		t.Fatal(err)
	}
	runID := run.Msg.RunId

	for i := 1; i <= 60; i++ {
		env := &pb.Envelope{SchemaVersion: 1, RunId: runID, Ts: timestamppb.Now(), Payload: &pb.Envelope_StepEntered{StepEntered: &pb.StepEntered{Step: "r", Adapter: "shell", Attempt: int32(i)}}}
		if _, _, err := ts.store.AppendEvent(context.Background(), mustStoreEvent(t, env)); err != nil {
			t.Fatal(err)
		}
	}
	if err := ts.store.UpsertSubscriberCursor(context.Background(), "sub-b", runID, 50); err != nil {
		t.Fatalf("seed cursor: %v", err)
	}

	watch, err := cClient.WatchRun(context.Background(), connect.NewRequest(&pb.WatchRunRequest{RunId: runID, SinceSeq: 0, SubscriberId: "sub-b"}))
	if err != nil {
		t.Fatal(err)
	}

	if !watch.Receive() {
		t.Fatalf("expected replay event, err=%v", watch.Err())
	}
	if watch.Msg().Seq != 51 {
		t.Fatalf("first replay seq=%d want 51", watch.Msg().Seq)
	}
	_ = watch.Close()
}

func TestWatchRun_CursorUpdate_Coalesced(t *testing.T) {
	ts, spy := newCursorSpyStack(t)
	_, oClient, cClient := ts.startServer(t)
	overseerID, _ := mustRegister(t, oClient)
	run, err := oClient.CreateRun(context.Background(), connect.NewRequest(&pb.CreateRunRequest{CriteriaId: overseerID, WorkflowName: "wf"}))
	if err != nil {
		t.Fatal(err)
	}
	runID := run.Msg.RunId

	for i := 1; i <= 500; i++ {
		env := &pb.Envelope{SchemaVersion: 1, RunId: runID, Ts: timestamppb.Now(), Payload: &pb.Envelope_StepEntered{StepEntered: &pb.StepEntered{Step: "r", Adapter: "shell", Attempt: int32(i)}}}
		if _, _, err := ts.store.AppendEvent(context.Background(), mustStoreEvent(t, env)); err != nil {
			t.Fatal(err)
		}
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	watch, err := cClient.WatchRun(ctx, connect.NewRequest(&pb.WatchRunRequest{RunId: runID, SubscriberId: "sub-c"}))
	if err != nil {
		t.Fatal(err)
	}

	seen := 0
	for seen < 501 { // 500 replay envelopes + WatchReady
		if !watch.Receive() {
			t.Fatalf("expected message %d, err=%v", seen+1, watch.Err())
		}
		seen++
	}
	_ = watch.Close()

	waitForCursor(t, ts.store, "sub-c", runID, 500, 2*time.Second)
	// Coalescing policy is flush every 100 envelopes or 250ms. Theoretical
	// minimum is 5 batch flushes for 500 replay envelopes; allow a wider ceiling
	// because the 250ms ticker can fire during replay and during the close phase
	// under the race detector.
	const maxExpectedUpserts = 15
	if calls := spy.UpsertCount(); calls > maxExpectedUpserts {
		t.Fatalf("upsert calls=%d want <= %d", calls, maxExpectedUpserts)
	}
}

func TestWatchRun_CursorUpdate_FinalValueFlushedOnClose(t *testing.T) {
	ts := newTestStack(t)
	_, oClient, cClient := ts.startServer(t)
	overseerID, _ := mustRegister(t, oClient)
	run, err := oClient.CreateRun(context.Background(), connect.NewRequest(&pb.CreateRunRequest{CriteriaId: overseerID, WorkflowName: "wf"}))
	if err != nil {
		t.Fatal(err)
	}
	runID := run.Msg.RunId

	for i := 1; i <= 3; i++ {
		env := &pb.Envelope{SchemaVersion: 1, RunId: runID, Ts: timestamppb.Now(), Payload: &pb.Envelope_StepEntered{StepEntered: &pb.StepEntered{Step: "r", Adapter: "shell", Attempt: int32(i)}}}
		if _, _, err := ts.store.AppendEvent(context.Background(), mustStoreEvent(t, env)); err != nil {
			t.Fatal(err)
		}
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	watch, err := cClient.WatchRun(ctx, connect.NewRequest(&pb.WatchRunRequest{RunId: runID, SubscriberId: "sub-d"}))
	if err != nil {
		t.Fatal(err)
	}

	for i := 0; i < 4; i++ { // 3 replay envelopes + WatchReady
		if !watch.Receive() {
			t.Fatalf("expected message %d, err=%v", i+1, watch.Err())
		}
	}
	_ = watch.Close()

	deadline := time.Now().Add(2 * time.Second)
	for {
		seq, found, err := ts.store.GetSubscriberCursor(context.Background(), "sub-d", runID)
		if err != nil {
			t.Fatalf("get cursor: %v", err)
		}
		if found && seq == 3 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for flushed cursor; found=%v seq=%d", found, seq)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// waitForCursor polls the store until the subscriber cursor for the given run
// reaches want or the timeout expires.
func waitForCursor(t *testing.T, st store.Store, subscriberID, runID string, want uint64, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for {
		seq, found, err := st.GetSubscriberCursor(context.Background(), subscriberID, runID)
		if err != nil {
			t.Fatalf("get cursor: %v", err)
		}
		if found && seq == want {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for cursor %d for subscriber %s; found=%v seq=%d", want, subscriberID, found, seq)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// cursorFaultStore wraps a Store and fails exactly one UpsertSubscriberCursor
// call matching a configured sequence number or call count. It is used to
// simulate transient SQLITE_BUSY errors deterministically.
type cursorFaultStore struct {
	store.Store

	mu         sync.Mutex
	calls      int
	failOnSeq  uint64
	failOnCall int
	failed     bool
}

func (s *cursorFaultStore) UpsertSubscriberCursor(ctx context.Context, subscriberID, runID string, lastSeq uint64) error {
	s.mu.Lock()
	s.calls++
	shouldFail := false
	if !s.failed {
		if s.failOnSeq > 0 && lastSeq == s.failOnSeq {
			shouldFail = true
		}
		if s.failOnCall > 0 && s.calls == s.failOnCall {
			shouldFail = true
		}
		if shouldFail {
			s.failed = true
		}
	}
	s.mu.Unlock()

	if shouldFail {
		return errors.New("database is locked (5) (SQLITE_BUSY)")
	}
	return s.Store.UpsertSubscriberCursor(ctx, subscriberID, runID, lastSeq)
}

func (s *cursorFaultStore) Calls() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.calls
}

func (s *cursorFaultStore) Failed() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.failed
}

func newFaultStack(t *testing.T, failOnSeq uint64, failOnCall int) (*testStack, *cursorFaultStore) {
	t.Helper()
	baseStore, err := sqlite.Open(filepath.Join(t.TempDir(), "castle.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = baseStore.Close() })

	wrapped := &cursorFaultStore{Store: baseStore, failOnSeq: failOnSeq, failOnCall: failOnCall}
	h := hub.New()
	controls := NewControlRegistry()
	log := slog.New(slog.NewTextHandler(testWriter{t: t}, nil))
	stack := &testStack{
		store:    wrapped,
		hub:      h,
		controls: controls,
		criteria: NewCriteriaServer(wrapped, h, log, controls),
		server:   NewServerServer(wrapped, h, log, controls),
	}
	return stack, wrapped
}

func TestWatchRun_CursorUpdate_FinalWriteRetriesBusy(t *testing.T) {
	ts, fault := newFaultStack(t, 500, 0)
	_, oClient, cClient := ts.startServer(t)
	overseerID, _ := mustRegister(t, oClient)
	run, err := oClient.CreateRun(context.Background(), connect.NewRequest(&pb.CreateRunRequest{CriteriaId: overseerID, WorkflowName: "wf"}))
	if err != nil {
		t.Fatal(err)
	}
	runID := run.Msg.RunId

	for i := 1; i <= 500; i++ {
		env := &pb.Envelope{SchemaVersion: 1, RunId: runID, Ts: timestamppb.Now(), Payload: &pb.Envelope_StepEntered{StepEntered: &pb.StepEntered{Step: "r", Adapter: "shell", Attempt: int32(i)}}}
		if _, _, err := ts.store.AppendEvent(context.Background(), mustStoreEvent(t, env)); err != nil {
			t.Fatal(err)
		}
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	watch, err := cClient.WatchRun(ctx, connect.NewRequest(&pb.WatchRunRequest{RunId: runID, SubscriberId: "sub-final-busy"}))
	if err != nil {
		t.Fatal(err)
	}

	seen := 0
	for seen < 501 { // 500 replay envelopes + WatchReady
		if !watch.Receive() {
			t.Fatalf("expected message %d, err=%v", seen+1, watch.Err())
		}
		seen++
	}
	_ = watch.Close()

	waitForCursor(t, ts.store, "sub-final-busy", runID, 500, 2*time.Second)

	if !fault.Failed() {
		t.Fatal("expected the final cursor write to be faulted")
	}
}

func TestWatchRun_CursorUpdate_IntermediateWriteRetriesBusy(t *testing.T) {
	ts, fault := newFaultStack(t, 0, 3)
	_, oClient, cClient := ts.startServer(t)
	overseerID, _ := mustRegister(t, oClient)
	run, err := oClient.CreateRun(context.Background(), connect.NewRequest(&pb.CreateRunRequest{CriteriaId: overseerID, WorkflowName: "wf"}))
	if err != nil {
		t.Fatal(err)
	}
	runID := run.Msg.RunId

	for i := 1; i <= 500; i++ {
		env := &pb.Envelope{SchemaVersion: 1, RunId: runID, Ts: timestamppb.Now(), Payload: &pb.Envelope_StepEntered{StepEntered: &pb.StepEntered{Step: "r", Adapter: "shell", Attempt: int32(i)}}}
		if _, _, err := ts.store.AppendEvent(context.Background(), mustStoreEvent(t, env)); err != nil {
			t.Fatal(err)
		}
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	watch, err := cClient.WatchRun(ctx, connect.NewRequest(&pb.WatchRunRequest{RunId: runID, SubscriberId: "sub-intermediate-busy"}))
	if err != nil {
		t.Fatal(err)
	}

	seen := 0
	for seen < 501 { // 500 replay envelopes + WatchReady
		if !watch.Receive() {
			t.Fatalf("expected message %d, err=%v", seen+1, watch.Err())
		}
		seen++
	}
	_ = watch.Close()

	if !fault.Failed() {
		t.Fatal("expected an intermediate cursor write to be faulted")
	}
	// The 3rd coalesced write is expected to land at seq 300. Tolerate the
	// batch boundary shifting by also accepting seq 200 or 400, but ensure we
	// did fault an intermediate rather than the final write.
	if fault.Calls() < 3 {
		t.Fatalf("expected at least 3 upsert calls, got %d", fault.Calls())
	}

	waitForCursor(t, ts.store, "sub-intermediate-busy", runID, 500, 2*time.Second)
}

func TestStopRunConnectedAndDisconnected(t *testing.T) {
	ts := newTestStack(t)
	_, oClient, cClient := ts.startServer(t)
	overseerID, _ := mustRegister(t, oClient)
	run, err := oClient.CreateRun(context.Background(), connect.NewRequest(&pb.CreateRunRequest{CriteriaId: overseerID, WorkflowName: "wf"}))
	if err != nil {
		t.Fatal(err)
	}

	_, err = cClient.StopRun(context.Background(), connect.NewRequest(&pb.StopRunRequest{RunId: run.Msg.RunId, Reason: "stop"}))
	if connect.CodeOf(err) != connect.CodeFailedPrecondition {
		t.Fatalf("expected failed precondition, got %v", err)
	}

	ctrl, err := oClient.Control(context.Background(), connect.NewRequest(&pb.ControlSubscribeRequest{CriteriaId: overseerID}))
	if err != nil {
		t.Fatal(err)
	}

	// First frame is ControlReady; wait for it to confirm the subscription
	// is active before issuing StopRun.
	if !ctrl.Receive() {
		t.Fatalf("expected ControlReady, err=%v", ctrl.Err())
	}
	if _, ok := ctrl.Msg().Command.(*pb.ControlMessage_ControlReady); !ok {
		t.Fatalf("expected ControlReady, got %T", ctrl.Msg().Command)
	}

	_, err = cClient.StopRun(context.Background(), connect.NewRequest(&pb.StopRunRequest{RunId: run.Msg.RunId, Reason: "stop"}))
	if err != nil {
		t.Fatal(err)
	}

	if !ctrl.Receive() {
		t.Fatalf("expected control message, err=%v", ctrl.Err())
	}
	msg := ctrl.Msg()
	cmd, ok := msg.Command.(*pb.ControlMessage_RunCancel)
	if !ok {
		t.Fatalf("unexpected control command: %T", msg.Command)
	}
	if cmd.RunCancel.RunId != run.Msg.RunId {
		t.Fatalf("run id=%s", cmd.RunCancel.RunId)
	}
	_ = ctrl.Close()
}

// drainControlReady subscribes to the Control stream and waits for the
// ControlReady handshake. The caller must close the returned stream.
func drainControlReady(t *testing.T, client criteriav1connect.CriteriaServiceClient, overseerID string) *connect.ServerStreamForClient[pb.ControlMessage] {
	t.Helper()
	ctrl, err := client.Control(context.Background(), connect.NewRequest(&pb.ControlSubscribeRequest{CriteriaId: overseerID}))
	if err != nil {
		t.Fatalf("Control subscribe: %v", err)
	}
	if !ctrl.Receive() {
		t.Fatalf("expected ControlReady, err=%v", ctrl.Err())
	}
	if _, ok := ctrl.Msg().Command.(*pb.ControlMessage_ControlReady); !ok {
		t.Fatalf("expected ControlReady, got %T", ctrl.Msg().Command)
	}
	return ctrl
}

// markRunTerminal updates the run's status in the store to a terminal value.
func markRunTerminal(t *testing.T, st store.Store, runID string, status string) {
	t.Helper()
	r, err := st.GetRun(context.Background(), runID)
	if err != nil {
		t.Fatal(err)
	}
	r.Status = status
	now := time.Now().UTC()
	r.EndedAt = &now
	if err := st.UpdateRun(context.Background(), r); err != nil {
		t.Fatal(err)
	}
}

func TestPauseRunConnectedAndDisconnected(t *testing.T) {
	ts := newTestStack(t)
	_, oClient, cClient := ts.startServer(t)
	overseerID, _ := mustRegister(t, oClient)
	run, err := oClient.CreateRun(context.Background(), connect.NewRequest(&pb.CreateRunRequest{CriteriaId: overseerID, WorkflowName: "wf"}))
	if err != nil {
		t.Fatal(err)
	}

	_, err = cClient.PauseRun(context.Background(), connect.NewRequest(&pb.PauseRunRequest{RunId: run.Msg.RunId}))
	if connect.CodeOf(err) != connect.CodeFailedPrecondition {
		t.Fatalf("expected failed precondition when agent disconnected, got %v", err)
	}

	ctrl := drainControlReady(t, oClient, overseerID)
	defer ctrl.Close()

	_, err = cClient.PauseRun(context.Background(), connect.NewRequest(&pb.PauseRunRequest{RunId: run.Msg.RunId}))
	if err != nil {
		t.Fatal(err)
	}

	if !ctrl.Receive() {
		t.Fatalf("expected control message, err=%v", ctrl.Err())
	}
	msg := ctrl.Msg()
	cmd, ok := msg.Command.(*pb.ControlMessage_PauseRun)
	if !ok {
		t.Fatalf("unexpected control command: %T", msg.Command)
	}
	if cmd.PauseRun.RunId != run.Msg.RunId {
		t.Fatalf("run id=%s", cmd.PauseRun.RunId)
	}
}

func TestPauseRunTerminalRun(t *testing.T) {
	ts := newTestStack(t)
	_, oClient, cClient := ts.startServer(t)
	overseerID, _ := mustRegister(t, oClient)
	run, err := oClient.CreateRun(context.Background(), connect.NewRequest(&pb.CreateRunRequest{CriteriaId: overseerID, WorkflowName: "wf"}))
	if err != nil {
		t.Fatal(err)
	}
	markRunTerminal(t, ts.store, run.Msg.RunId, "cancelled")

	_, err = cClient.PauseRun(context.Background(), connect.NewRequest(&pb.PauseRunRequest{RunId: run.Msg.RunId}))
	if connect.CodeOf(err) != connect.CodeFailedPrecondition {
		t.Fatalf("expected failed precondition for terminal run, got %v", err)
	}
}

func TestResumeRunConnectedAndNotPaused(t *testing.T) {
	ts := newTestStack(t)
	_, oClient, cClient := ts.startServer(t)
	overseerID, _ := mustRegister(t, oClient)
	run, err := oClient.CreateRun(context.Background(), connect.NewRequest(&pb.CreateRunRequest{CriteriaId: overseerID, WorkflowName: "wf"}))
	if err != nil {
		t.Fatal(err)
	}

	// Run is not paused yet.
	_, err = cClient.ResumeRun(context.Background(), connect.NewRequest(&pb.ResumeRunRequest{RunId: run.Msg.RunId}))
	if connect.CodeOf(err) != connect.CodeFailedPrecondition {
		t.Fatalf("expected failed precondition for non-paused run, got %v", err)
	}

	// Mark run paused with a pending signal.
	if err := ts.store.SetRunPaused(context.Background(), run.Msg.RunId, "continue", time.Now().UTC()); err != nil {
		t.Fatal(err)
	}

	ctrl := drainControlReady(t, oClient, overseerID)
	defer ctrl.Close()

	_, err = cClient.ResumeRun(context.Background(), connect.NewRequest(&pb.ResumeRunRequest{RunId: run.Msg.RunId}))
	if err != nil {
		t.Fatal(err)
	}

	if !ctrl.Receive() {
		t.Fatalf("expected control message, err=%v", ctrl.Err())
	}
	msg := ctrl.Msg()
	cmd, ok := msg.Command.(*pb.ControlMessage_ResumeRun)
	if !ok {
		t.Fatalf("unexpected control command: %T", msg.Command)
	}
	if cmd.ResumeRun.RunId != run.Msg.RunId {
		t.Fatalf("run id=%s", cmd.ResumeRun.RunId)
	}
	if cmd.ResumeRun.Signal != "continue" {
		t.Fatalf("signal=%s want continue", cmd.ResumeRun.Signal)
	}
}

func TestResumeRunDisconnected(t *testing.T) {
	ts := newTestStack(t)
	_, oClient, cClient := ts.startServer(t)
	overseerID, _ := mustRegister(t, oClient)
	run, err := oClient.CreateRun(context.Background(), connect.NewRequest(&pb.CreateRunRequest{CriteriaId: overseerID, WorkflowName: "wf"}))
	if err != nil {
		t.Fatal(err)
	}
	if err := ts.store.SetRunPaused(context.Background(), run.Msg.RunId, "continue", time.Now().UTC()); err != nil {
		t.Fatal(err)
	}

	_, err = cClient.ResumeRun(context.Background(), connect.NewRequest(&pb.ResumeRunRequest{RunId: run.Msg.RunId}))
	if connect.CodeOf(err) != connect.CodeFailedPrecondition {
		t.Fatalf("expected failed precondition when agent disconnected, got %v", err)
	}
}

func TestResumeRunTerminalRun(t *testing.T) {
	ts := newTestStack(t)
	_, oClient, cClient := ts.startServer(t)
	overseerID, _ := mustRegister(t, oClient)
	run, err := oClient.CreateRun(context.Background(), connect.NewRequest(&pb.CreateRunRequest{CriteriaId: overseerID, WorkflowName: "wf"}))
	if err != nil {
		t.Fatal(err)
	}
	markRunTerminal(t, ts.store, run.Msg.RunId, "failed")

	_, err = cClient.ResumeRun(context.Background(), connect.NewRequest(&pb.ResumeRunRequest{RunId: run.Msg.RunId}))
	if connect.CodeOf(err) != connect.CodeFailedPrecondition {
		t.Fatalf("expected failed precondition for terminal run, got %v", err)
	}
}

func TestSendPromptConnectedAndDisconnected(t *testing.T) {
	ts := newTestStack(t)
	_, oClient, cClient := ts.startServer(t)
	overseerID, _ := mustRegister(t, oClient)
	run, err := oClient.CreateRun(context.Background(), connect.NewRequest(&pb.CreateRunRequest{CriteriaId: overseerID, WorkflowName: "wf"}))
	if err != nil {
		t.Fatal(err)
	}

	_, err = cClient.SendPrompt(context.Background(), connect.NewRequest(&pb.SendPromptRequest{RunId: run.Msg.RunId, Step: "ask", Prompt: "hello"}))
	if connect.CodeOf(err) != connect.CodeFailedPrecondition {
		t.Fatalf("expected failed precondition when agent disconnected, got %v", err)
	}

	ctrl := drainControlReady(t, oClient, overseerID)
	defer ctrl.Close()

	_, err = cClient.SendPrompt(context.Background(), connect.NewRequest(&pb.SendPromptRequest{RunId: run.Msg.RunId, Step: "ask", Prompt: "hello"}))
	if err != nil {
		t.Fatal(err)
	}

	if !ctrl.Receive() {
		t.Fatalf("expected control message, err=%v", ctrl.Err())
	}
	msg := ctrl.Msg()
	cmd, ok := msg.Command.(*pb.ControlMessage_AgentPrompt)
	if !ok {
		t.Fatalf("unexpected control command: %T", msg.Command)
	}
	if cmd.AgentPrompt.RunId != run.Msg.RunId {
		t.Fatalf("run id=%s", cmd.AgentPrompt.RunId)
	}
	if cmd.AgentPrompt.Step != "ask" {
		t.Fatalf("step=%s want ask", cmd.AgentPrompt.Step)
	}
	if cmd.AgentPrompt.Prompt != "hello" {
		t.Fatalf("prompt=%s want hello", cmd.AgentPrompt.Prompt)
	}
}

func TestSendPromptTerminalRun(t *testing.T) {
	ts := newTestStack(t)
	_, oClient, cClient := ts.startServer(t)
	overseerID, _ := mustRegister(t, oClient)
	run, err := oClient.CreateRun(context.Background(), connect.NewRequest(&pb.CreateRunRequest{CriteriaId: overseerID, WorkflowName: "wf"}))
	if err != nil {
		t.Fatal(err)
	}
	markRunTerminal(t, ts.store, run.Msg.RunId, "succeeded")

	_, err = cClient.SendPrompt(context.Background(), connect.NewRequest(&pb.SendPromptRequest{RunId: run.Msg.RunId, Step: "ask", Prompt: "hello"}))
	if connect.CodeOf(err) != connect.CodeFailedPrecondition {
		t.Fatalf("expected failed precondition for terminal run, got %v", err)
	}
}

func TestSendPromptMissingStep(t *testing.T) {
	ts := newTestStack(t)
	_, oClient, cClient := ts.startServer(t)
	overseerID, _ := mustRegister(t, oClient)
	run, err := oClient.CreateRun(context.Background(), connect.NewRequest(&pb.CreateRunRequest{CriteriaId: overseerID, WorkflowName: "wf"}))
	if err != nil {
		t.Fatal(err)
	}
	ctrl := drainControlReady(t, oClient, overseerID)
	defer ctrl.Close()

	_, err = cClient.SendPrompt(context.Background(), connect.NewRequest(&pb.SendPromptRequest{RunId: run.Msg.RunId, Prompt: "hello"}))
	if connect.CodeOf(err) != connect.CodeInvalidArgument {
		t.Fatalf("expected invalid argument for missing step, got %v", err)
	}
}

func TestInspectRunReturnsStateWithoutMutation(t *testing.T) {
	ts := newTestStack(t)
	_, oClient, cClient := ts.startServer(t)
	overseerID, _ := mustRegister(t, oClient)
	run, err := oClient.CreateRun(context.Background(), connect.NewRequest(&pb.CreateRunRequest{CriteriaId: overseerID, WorkflowName: "wf"}))
	if err != nil {
		t.Fatal(err)
	}

	// Seed a step-entered event and a variable scope.
	env := criteria.NewEnvelope(run.Msg.RunId, &pb.StepEntered{Step: "build", Adapter: "shell", Attempt: 1})
	ev, err := envelopeToEvent(env)
	if err != nil {
		t.Fatal(err)
	}
	_, _, err = ts.store.AppendEvent(context.Background(), ev)
	if err != nil {
		t.Fatal(err)
	}
	if err := ts.store.SetRunVariableScope(context.Background(), run.Msg.RunId, `{"x":1}`); err != nil {
		t.Fatal(err)
	}

	resp, err := cClient.InspectRun(context.Background(), connect.NewRequest(&pb.InspectRunRequest{RunId: run.Msg.RunId, SessionId: "sess-1"}))
	if err != nil {
		t.Fatal(err)
	}
	if resp.Msg.RunId != run.Msg.RunId {
		t.Fatalf("run_id=%s", resp.Msg.RunId)
	}
	if resp.Msg.SessionId != "sess-1" {
		t.Fatalf("session_id=%s", resp.Msg.SessionId)
	}
	if resp.Msg.Adapter != "shell" {
		t.Fatalf("adapter=%s want shell", resp.Msg.Adapter)
	}
	if resp.Msg.CurrentStep != "" {
		t.Fatalf("current_step should be empty for fresh run, got %q", resp.Msg.CurrentStep)
	}
	if resp.Msg.StateJson != `{"x":1}` {
		t.Fatalf("state_json=%s", resp.Msg.StateJson)
	}
	if resp.Msg.LastActivityAt == nil {
		t.Fatal("expected last_activity_at")
	}

	// Inspect must not mutate state.
	r, err := ts.store.GetRun(context.Background(), run.Msg.RunId)
	if err != nil {
		t.Fatal(err)
	}
	if r.VariableScope != `{"x":1}` {
		t.Fatal("InspectRun mutated variable scope")
	}
}

func TestInspectRunTerminalRun(t *testing.T) {
	ts := newTestStack(t)
	_, oClient, cClient := ts.startServer(t)
	overseerID, _ := mustRegister(t, oClient)
	run, err := oClient.CreateRun(context.Background(), connect.NewRequest(&pb.CreateRunRequest{CriteriaId: overseerID, WorkflowName: "wf"}))
	if err != nil {
		t.Fatal(err)
	}
	markRunTerminal(t, ts.store, run.Msg.RunId, "succeeded")

	resp, err := cClient.InspectRun(context.Background(), connect.NewRequest(&pb.InspectRunRequest{RunId: run.Msg.RunId}))
	if err != nil {
		t.Fatal(err)
	}
	if resp.Msg.RunId != run.Msg.RunId {
		t.Fatalf("run_id=%s", resp.Msg.RunId)
	}
	if resp.Msg.LastActivityAt != nil {
		t.Fatal("expected no last_activity_at for terminal run with no events")
	}
}

func TestStopRunTerminalRun(t *testing.T) {
	ts := newTestStack(t)
	_, oClient, cClient := ts.startServer(t)
	overseerID, _ := mustRegister(t, oClient)
	run, err := oClient.CreateRun(context.Background(), connect.NewRequest(&pb.CreateRunRequest{CriteriaId: overseerID, WorkflowName: "wf"}))
	if err != nil {
		t.Fatal(err)
	}
	markRunTerminal(t, ts.store, run.Msg.RunId, "failed")

	_, err = cClient.StopRun(context.Background(), connect.NewRequest(&pb.StopRunRequest{RunId: run.Msg.RunId, Reason: "stop"}))
	if connect.CodeOf(err) != connect.CodeFailedPrecondition {
		t.Fatalf("expected failed precondition for terminal run, got %v", err)
	}
}

func TestControlRegistryEvictsPriorSubscriber(t *testing.T) {
	r := NewControlRegistry()
	first, err := r.Register("o1")
	if err != nil {
		t.Fatal(err)
	}
	second, err := r.Register("o1")
	if err != nil {
		t.Fatal(err)
	}
	if first == second {
		t.Fatal("expected a new channel for re-registration")
	}
	// The prior channel must be closed so the evicted stream returns cleanly.
	select {
	case _, ok := <-first:
		if ok {
			t.Fatal("expected first channel to be closed")
		}
	case <-time.After(time.Second):
		t.Fatal("first channel was not closed on eviction")
	}
	// The new channel must still be usable.
	if err := r.Enqueue("o1", &pb.ControlMessage{}); err != nil {
		t.Fatalf("enqueue on replacement channel: %v", err)
	}
	select {
	case <-second:
	case <-time.After(time.Second):
		t.Fatal("second channel did not receive enqueued message")
	}
	r.Unregister("o1", second)
}

func TestControlRegistryEnqueueErrors(t *testing.T) {
	r := NewControlRegistry()
	if err := r.Enqueue("missing", &pb.ControlMessage{}); err == nil || err != ErrAgentNotConnected {
		t.Fatalf("expected ErrAgentNotConnected, got %v", err)
	}
	ch, err := r.Register("o1")
	if err != nil {
		t.Fatal(err)
	}
	// Fill the buffer without draining.
	for i := 0; i < controlBufferSize; i++ {
		if err := r.Enqueue("o1", &pb.ControlMessage{}); err != nil {
			t.Fatalf("unexpected enqueue error at %d: %v", i, err)
		}
	}
	if err := r.Enqueue("o1", &pb.ControlMessage{}); err == nil || err != ErrControlBacklogFull {
		t.Fatalf("expected ErrControlBacklogFull, got %v", err)
	}
	r.Unregister("o1", ch)
}

