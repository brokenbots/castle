package rpc

import (
	"context"
	"testing"
	"time"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/brokenbots/overlord/shared/events"
	pb "github.com/brokenbots/overlord/shared/pb/overlord/v1"
)

func TestCastleListRunEventsPaging(t *testing.T) {
	ts := newTestStack(t)
	_, oClient, cClient := ts.startServer(t)
	overseerID, _ := mustRegister(t, oClient)
	run, err := oClient.CreateRun(context.Background(), connect.NewRequest(&pb.CreateRunRequest{OverseerId: overseerID, WorkflowName: "wf"}))
	if err != nil {
		t.Fatal(err)
	}

	for i := 0; i < 1100; i++ {
		env, err := toStoreEnvelope(&pb.Envelope{
			SchemaVersion: 1,
			RunId:         run.Msg.RunId,
			Ts:            timestamppb.Now(),
			Payload:       &pb.Envelope_StepEntered{StepEntered: &pb.StepEntered{Step: "s", Adapter: "shell", Attempt: 1}},
		})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := ts.store.AppendEvent(context.Background(), env); err != nil {
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
}

func TestWatchRunReplayAndTail(t *testing.T) {
	ts := newTestStack(t)
	_, oClient, cClient := ts.startServer(t)
	overseerID, _ := mustRegister(t, oClient)
	run, err := oClient.CreateRun(context.Background(), connect.NewRequest(&pb.CreateRunRequest{OverseerId: overseerID, WorkflowName: "wf"}))
	if err != nil {
		t.Fatal(err)
	}
	runID := run.Msg.RunId

	replayEnv, _ := toStoreEnvelope(&pb.Envelope{SchemaVersion: 1, RunId: runID, Ts: timestamppb.Now(), Payload: &pb.Envelope_StepEntered{StepEntered: &pb.StepEntered{Step: "r1", Adapter: "shell", Attempt: 1}}})
	seq1, err := ts.store.AppendEvent(context.Background(), replayEnv)
	if err != nil {
		t.Fatal(err)
	}
	replayEnv.Seq = seq1

	watch, err := cClient.WatchRun(context.Background(), connect.NewRequest(&pb.WatchRunRequest{RunId: runID}))
	if err != nil {
		t.Fatal(err)
	}
	// First frame is WatchReady (headers-flush sentinel); consume and skip.
	if !watch.Receive() {
		t.Fatalf("expected WatchReady, err=%v", watch.Err())
	}
	if _, ok := watch.Msg().Payload.(*pb.Envelope_WatchReady); !ok {
		t.Fatalf("expected WatchReady, got %T", watch.Msg().Payload)
	}
	if !watch.Receive() {
		t.Fatalf("expected replay event, err=%v", watch.Err())
	}
	if watch.Msg().Seq != 1 {
		t.Fatalf("replay seq=%d", watch.Msg().Seq)
	}

	liveEnv, _ := toStoreEnvelope(&pb.Envelope{SchemaVersion: 1, RunId: runID, Ts: timestamppb.Now(), Payload: &pb.Envelope_StepEntered{StepEntered: &pb.StepEntered{Step: "r2", Adapter: "shell", Attempt: 1}}})
	seq2, err := ts.store.AppendEvent(context.Background(), liveEnv)
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

	terminal, _ := events.New(runID, events.TypeRunFailed, events.RunFailed{Reason: "x"})
	terminal.Timestamp = time.Now().UTC()
	seq3, err := ts.store.AppendEvent(context.Background(), terminal)
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

func TestStopRunConnectedAndDisconnected(t *testing.T) {
	ts := newTestStack(t)
	_, oClient, cClient := ts.startServer(t)
	overseerID, _ := mustRegister(t, oClient)
	run, err := oClient.CreateRun(context.Background(), connect.NewRequest(&pb.CreateRunRequest{OverseerId: overseerID, WorkflowName: "wf"}))
	if err != nil {
		t.Fatal(err)
	}

	_, err = cClient.StopRun(context.Background(), connect.NewRequest(&pb.StopRunRequest{RunId: run.Msg.RunId, Reason: "stop"}))
	if connect.CodeOf(err) != connect.CodeFailedPrecondition {
		t.Fatalf("expected failed precondition, got %v", err)
	}

	ctrl, err := oClient.Control(context.Background(), connect.NewRequest(&pb.ControlSubscribeRequest{OverseerId: overseerID}))
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
	if err := r.Enqueue("missing", &pb.ControlMessage{}); err == nil || err != ErrOverseerNotConnected {
		t.Fatalf("expected ErrOverseerNotConnected, got %v", err)
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
