package rpc

import (
	"context"
	"log/slog"
	"net/http"
	"testing"
	"time"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/brokenbots/overlord/castle/internal/auth"
	pb "github.com/brokenbots/overlord/shared/pb/overlord/v1"
)

func TestE2ELifecycleAndAuth(t *testing.T) {
	ts := newTestStack(t)

	opts := []connect.HandlerOption{connect.WithInterceptors(
		auth.NewLoggingInterceptor(slog.Default()),
		auth.NewInterceptor(ts.store, false),
	)}
	tsrv, oClient, cClient := ts.startServer(t, opts...)

	reg, err := oClient.Register(context.Background(), connect.NewRequest(&pb.RegisterRequest{Name: "o1"}))
	if err != nil {
		t.Fatal(err)
	}
	overseerID, token := reg.Msg.OverseerId, reg.Msg.Token

	// Any non-exempt RPC should reject unauthenticated requests.
	_, err = cClient.GetRun(context.Background(), connect.NewRequest(&pb.GetRunRequest{RunId: "missing"}))
	if connect.CodeOf(err) != connect.CodeUnauthenticated {
		t.Fatalf("expected unauthenticated, got %v", err)
	}

	createReq := connect.NewRequest(&pb.CreateRunRequest{OverseerId: overseerID, WorkflowName: "wf"})
	createReq.Header().Set("Authorization", "Bearer "+token)
	run, err := oClient.CreateRun(context.Background(), createReq)
	if err != nil {
		t.Fatal(err)
	}
	runID := run.Msg.RunId

	watchReq := connect.NewRequest(&pb.WatchRunRequest{RunId: runID})
	watchReq.Header().Set("Authorization", "Bearer "+token)
	watch, err := cClient.WatchRun(context.Background(), watchReq)
	if err != nil {
		t.Fatal(err)
	}
	// Consume the WatchReady headers-flush sentinel before asserting on
	// real events.
	if !watch.Receive() {
		t.Fatalf("expected WatchReady, err=%v", watch.Err())
	}
	if _, ok := watch.Msg().Payload.(*pb.Envelope_WatchReady); !ok {
		t.Fatalf("expected WatchReady, got %T", watch.Msg().Payload)
	}

	submit := oClient.SubmitEvents(context.Background())
	submit.RequestHeader().Set("Authorization", "Bearer "+token)

	evt := &pb.Envelope{
		SchemaVersion: 1,
		RunId:         runID,
		CorrelationId: "corr-1",
		Ts:            timestamppb.Now(),
		Payload:       &pb.Envelope_RunStarted{RunStarted: &pb.RunStarted{WorkflowName: "wf", InitialStep: "s1"}},
	}
	if err := submit.Send(evt); err != nil {
		t.Fatal(err)
	}

	// Publish-before-ack is enforced structurally in the SubmitEvents
	// handler (Hub.Publish runs before Ack.Send). Here we merely verify
	// both sides observe the event: the watcher must receive the live
	// envelope, and the submit stream must return an Ack with a matching
	// seq. Reading watch before ack also rules out the scenario where the
	// ack shipped without any subscriber observing the event.
	type ackOrErr struct {
		ack *pb.Ack
		err error
	}
	ackCh := make(chan ackOrErr, 1)
	go func() {
		ack, err := submit.Receive()
		ackCh <- ackOrErr{ack: ack, err: err}
	}()

	watchDeadline := time.After(5 * time.Second)
	var liveSeq uint64
watchLoop:
	for {
		if !watch.Receive() {
			t.Fatalf("watch stream error: %v", watch.Err())
		}
		msg := watch.Msg()
		switch msg.Payload.(type) {
		case *pb.Envelope_WatchReady:
			t.Fatal("unexpected duplicate WatchReady")
		case *pb.Envelope_RunStarted:
			liveSeq = msg.Seq
			break watchLoop
		default:
			// Ignore other event types the server may emit.
		}
		select {
		case <-watchDeadline:
			t.Fatal("timeout waiting for live run_started event")
		default:
		}
	}

	var ack *pb.Ack
	select {
	case res := <-ackCh:
		if res.err != nil {
			t.Fatalf("submit ack error: %v", res.err)
		}
		ack = res.ack
	case <-time.After(5 * time.Second):
		t.Fatal("timeout waiting for submit ack")
	}

	if ack.Seq == 0 {
		t.Fatal("expected non-zero ack seq")
	}
	if ack.Seq != liveSeq {
		t.Fatalf("ack seq %d does not match live event seq %d", ack.Seq, liveSeq)
	}

	ctrlReq := connect.NewRequest(&pb.ControlSubscribeRequest{OverseerId: overseerID})
	ctrlReq.Header().Set("Authorization", "Bearer "+token)
	ctrl, err := oClient.Control(context.Background(), ctrlReq)
	if err != nil {
		t.Fatal(err)
	}

	// Consume the ControlReady attach-ack before issuing StopRun.
	if !ctrl.Receive() {
		t.Fatalf("expected ControlReady, err=%v", ctrl.Err())
	}
	if _, ok := ctrl.Msg().Command.(*pb.ControlMessage_ControlReady); !ok {
		t.Fatalf("expected ControlReady, got %T", ctrl.Msg().Command)
	}

	stopReq := connect.NewRequest(&pb.StopRunRequest{RunId: runID, Reason: "operator request"})
	stopReq.Header().Set("Authorization", "Bearer "+token)
	if _, err := cClient.StopRun(context.Background(), stopReq); err != nil {
		t.Fatal(err)
	}
	if !ctrl.Receive() {
		t.Fatalf("expected stop control message, err=%v", ctrl.Err())
	}
	if _, ok := ctrl.Msg().Command.(*pb.ControlMessage_RunCancel); !ok {
		t.Fatalf("unexpected control command: %T", ctrl.Msg().Command)
	}

	_ = submit.CloseRequest()
	_ = watch.Close()
	_ = ctrl.Close()

	resp, err := h2cClient().Get(tsrv.URL + "/grpc.reflection.v1.ServerReflection/ServerReflectionInfo")
	if err != nil {
		t.Fatal(err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode == http.StatusNotFound {
		t.Fatalf("reflection handler not mounted: %d", resp.StatusCode)
	}
}
