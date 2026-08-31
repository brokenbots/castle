package rpc

import (
	"context"
	"fmt"
	"log/slog"
	"testing"
	"time"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/brokenbots/castle/castle/internal/hub"
	"github.com/brokenbots/castle/castle/internal/store/sqlite"
	criteria "github.com/brokenbots/criteria/sdk"
	pb "github.com/brokenbots/criteria/sdk/pb/criteria/v1" // import-lint:allow castle service bindings (W08: move to castle-proto)
)

// TestSubmitEvents_UnknownPayloadArmRejected verifies that SubmitEvents
// returns an explicit error before acknowledgement when an envelope carries
// a payload arm Castle does not recognise (CRI-71). The required contract is
// that unknown payloads are either preserved safely for forward-compatible
// replay or rejected explicitly before acknowledgement; rejection is the path
// taken here because Castle cannot construct a storage/replay envelope for an
// unrecognised oneof arm.
func TestSubmitEvents_UnknownPayloadArmRejected(t *testing.T) {
	ts := newTestStack(t)
	_, oClient, _ := ts.startServer(t)
	overseerID, token := mustRegister(t, oClient)

	createReq := connect.NewRequest(&pb.CreateRunRequest{CriteriaId: overseerID, WorkflowName: "wf"})
	createReq.Header().Set("Authorization", "Bearer "+token)
	runResp, err := oClient.CreateRun(context.Background(), createReq)
	if err != nil {
		t.Fatal(err)
	}
	runID := runResp.Msg.RunId

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	stream := oClient.SubmitEvents(ctx)
	stream.RequestHeader().Set("Authorization", "Bearer "+token)

	// Construct an envelope whose Payload is empty (no oneof arm set). Castle
	// treats a payload-less envelope as unknown and must reject it before ack.
	env := &pb.Envelope{
		SchemaVersion: int32(criteria.SchemaVersion),
		RunId:         runID,
		CorrelationId: "unknown-payload",
		Ts:            timestamppb.Now(),
	}
	if err := stream.Send(env); err != nil {
		// The server may close the stream before the client reads; that is
		// still an explicit rejection.
		if connect.CodeOf(err) != connect.CodeInvalidArgument {
			t.Fatalf("Send: want CodeInvalidArgument, got code=%v err=%v", connect.CodeOf(err), err)
		}
		return
	}
	_, recvErr := stream.Receive()
	if connect.CodeOf(recvErr) != connect.CodeInvalidArgument {
		t.Fatalf("Receive: want CodeInvalidArgument for unknown payload, got code=%v err=%v", connect.CodeOf(recvErr), recvErr)
	}

	// Confirm nothing was persisted for the rejected correlation id.
	events, err := ts.store.ListEvents(context.Background(), runID, 0, 10)
	if err != nil {
		t.Fatal(err)
	}
	for _, ev := range events {
		if ev.CorrelationID == "unknown-payload" {
			t.Fatal("unknown-payload event was persisted despite rejection")
		}
	}
}

// TestAppendEvent_DeduplicationSurvivesRestart proves that sequence and
// correlation-ID deduplication survive a Castle process restart. The same
// envelope submitted before and after restart must receive the same seq and
// must not create a duplicate event row (CRI-71).
func TestAppendEvent_DeduplicationSurvivesRestart(t *testing.T) {
	ctx := context.Background()
	dbPath := t.TempDir() + "/castle-restart.db"

	// First process: create the store, register an agent, create a run, and
	// submit an event with a correlation id.
	s1, err := sqlite.Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	h1 := newTestStackWithStore(t, s1)
	_, oClient1, _ := h1.startServer(t)
	overseerID, token := mustRegister(t, oClient1)

	createReq := connect.NewRequest(&pb.CreateRunRequest{CriteriaId: overseerID, WorkflowName: "wf"})
	createReq.Header().Set("Authorization", "Bearer "+token)
	runResp, err := oClient1.CreateRun(ctx, createReq)
	if err != nil {
		t.Fatal(err)
	}
	runID := runResp.Msg.RunId

	stream1 := oClient1.SubmitEvents(ctx)
	stream1.RequestHeader().Set("Authorization", "Bearer "+token)
	env := criteria.NewEnvelope(runID, &pb.StepLog{Step: "s", Stream: pb.LogStream_LOG_STREAM_STDOUT, Chunk: "before-restart"})
	env.CorrelationId = "dedup-restart"
	if err := stream1.Send(env); err != nil {
		t.Fatal(err)
	}
	ack1, err := stream1.Receive()
	if err != nil {
		t.Fatal(err)
	}
	if ack1.Seq == 0 {
		t.Fatalf("first ack seq=%d, want non-zero", ack1.Seq)
	}
	firstSeq := ack1.Seq
	_ = stream1.CloseRequest()

	// Simulate restart: close the first store and open a fresh stack on the
	// same DB file.
	if err := s1.Close(); err != nil {
		t.Fatal(err)
	}
	s2, err := sqlite.Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	// s2 will be closed by t.Cleanup registered inside newTestStackWithStore.
	h2 := newTestStackWithStore(t, s2)
	_, oClient2, _ := h2.startServer(t)

	// Re-submit the same correlation id from the new process.
	stream2 := oClient2.SubmitEvents(ctx)
	stream2.RequestHeader().Set("Authorization", "Bearer "+token)
	env2 := criteria.NewEnvelope(runID, &pb.StepLog{Step: "s", Stream: pb.LogStream_LOG_STREAM_STDOUT, Chunk: "after-restart"})
	env2.CorrelationId = "dedup-restart"
	if err := stream2.Send(env2); err != nil {
		t.Fatal(err)
	}
	ack2, err := stream2.Receive()
	if err != nil {
		t.Fatal(err)
	}
	if ack2.Seq != firstSeq {
		t.Fatalf("after restart ack seq=%d, want %d (no duplicate)", ack2.Seq, firstSeq)
	}
	_ = stream2.CloseRequest()

	// There must be exactly one persisted event with this correlation id.
	events, err := s2.ListEvents(ctx, runID, 0, 10)
	if err != nil {
		t.Fatal(err)
	}
	count := 0
	for _, ev := range events {
		if ev.CorrelationID == "dedup-restart" {
			count++
		}
	}
	if count != 1 {
		t.Fatalf("expected 1 persisted event for dedup-restart, got %d", count)
	}
}

// TestWatchRun_HistoricalReplayThenLiveNoGapOrDuplicate verifies that a
// WatchRun starting from since_seq=0 replays every persisted event, emits
// WatchReady exactly once after durable replay, then tails live events without
// gaps or duplicates when a published event overlaps with buffered history
// (CRI-71).
func TestWatchRun_HistoricalReplayThenLiveNoGapOrDuplicate(t *testing.T) {
	ts := newTestStack(t)
	_, oClient, cClient := ts.startServer(t)
	overseerID, token := mustRegister(t, oClient)

	createReq := connect.NewRequest(
		&pb.CreateRunRequest{CriteriaId: overseerID, WorkflowName: "wf"})
	createReq.Header().Set("Authorization", "Bearer "+token)
	run, err := oClient.CreateRun(context.Background(), createReq)
	if err != nil {
		t.Fatal(err)
	}
	runID := run.Msg.RunId

	// Seed three persisted events.
	for i := 1; i <= 3; i++ {
		env := &pb.Envelope{
			SchemaVersion: 1,
			RunId:         runID,
			Ts:            timestamppb.Now(),
			Payload:       &pb.Envelope_StepLog{StepLog: &pb.StepLog{Step: "s", Stream: pb.LogStream_LOG_STREAM_STDOUT, Chunk: fmt.Sprintf("hist-%d", i)}},
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

	var seen []uint64
	for i := 0; i < 3; i++ {
		if !watch.Receive() {
			t.Fatalf("expected historical event %d, err=%v", i+1, watch.Err())
		}
		seen = append(seen, watch.Msg().Seq)
	}
	if !watch.Receive() {
		t.Fatalf("expected WatchReady, err=%v", watch.Err())
	}
	if _, ok := watch.Msg().Payload.(*pb.Envelope_WatchReady); !ok {
		t.Fatalf("expected WatchReady after replay, got %T", watch.Msg().Payload)
	}

	// Publish a live event.
	live := &pb.Envelope{
		SchemaVersion: 1,
		RunId:         runID,
		Ts:            timestamppb.Now(),
		Payload:       &pb.Envelope_RunCompleted{RunCompleted: &pb.RunCompleted{Success: true}},
	}
	seq, _, err := ts.store.AppendEvent(context.Background(), mustStoreEvent(t, live))
	if err != nil {
		t.Fatal(err)
	}
	live.Seq = seq
	ts.hub.Publish(live)

	if !watch.Receive() {
		t.Fatalf("expected live terminal event, err=%v", watch.Err())
	}
	seen = append(seen, watch.Msg().Seq)

	// Validate no gaps and no duplicates in the observed sequences.
	seenSet := make(map[uint64]struct{}, len(seen))
	for i, s := range seen {
		if s == 0 {
			t.Fatalf("seq[%d] is zero", i)
		}
		if i > 0 && s <= seen[i-1] {
			t.Fatalf("seq not monotonic at index %d: prev=%d curr=%d", i, seen[i-1], s)
		}
		if _, ok := seenSet[s]; ok {
			t.Fatalf("duplicate seq %d", s)
		}
		seenSet[s] = struct{}{}
	}
	if len(seen) != 4 {
		t.Fatalf("expected 4 observed events, got %d", len(seen))
	}

	// Stream must close after the terminal event.
	if watch.Receive() {
		t.Fatalf("expected stream close after terminal, got seq=%d", watch.Msg().Seq)
	}
}

// newTestStackWithStore builds a testStack using the supplied store. The store
// is closed on test cleanup.
func newTestStackWithStore(t *testing.T, st *sqlite.Store) *testStack {
	t.Helper()
	t.Cleanup(func() { _ = st.Close() })
	h := hub.New()
	controls := NewControlRegistry()
	log := slog.New(slog.NewTextHandler(testWriter{t: t}, nil))
	return &testStack{
		store:    st,
		hub:      h,
		controls: controls,
		criteria: NewCriteriaServer(st, h, log, controls),
		server:   NewServerServer(st, h, log, controls),
	}
}
