package rpc

import (
	"context"
	"fmt"
	"testing"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/types/known/timestamppb"

	pb "github.com/brokenbots/criteria/sdk/pb/criteria/v1" // import-lint:allow castle service bindings (W08: move to castle-proto)
	criteria "github.com/brokenbots/criteria/sdk"
)

func TestOverseerUnaryMethods(t *testing.T) {
	ts := newTestStack(t)
	ctx := context.Background()

	reg, err := ts.criteria.Register(ctx, connect.NewRequest(&pb.RegisterRequest{Name: "o1", Labels: map[string]string{"hostname": "h1", "version": "v1"}}))
	if err != nil {
		t.Fatal(err)
	}
	if reg.Msg.CriteriaId == "" || reg.Msg.Token == "" {
		t.Fatal("expected register to return overseer id and token")
	}

	if _, err := ts.criteria.Heartbeat(ctx, connect.NewRequest(&pb.HeartbeatRequest{CriteriaId: reg.Msg.CriteriaId})); err != nil {
		t.Fatal(err)
	}

	runResp, err := ts.criteria.CreateRun(ctx, connect.NewRequest(&pb.CreateRunRequest{CriteriaId: reg.Msg.CriteriaId, WorkflowName: "wf", WorkflowHash: "h"}))
	if err != nil {
		t.Fatal(err)
	}
	if runResp.Msg.RunId == "" || runResp.Msg.Status != "pending" {
		t.Fatalf("unexpected run response: %+v", runResp.Msg)
	}
}

func TestSubmitEventsStream_ReplayPagesPersistedEvents(t *testing.T) {
	ts := newTestStack(t)
	_, oClient, _ := ts.startServer(t)
	overseerID, token := mustRegister(t, oClient)

	createReq := connect.NewRequest(&pb.CreateRunRequest{CriteriaId: overseerID, WorkflowName: "wf", WorkflowHash: "hash"})
	createReq.Header().Set("Authorization", "Bearer "+token)
	runResp, err := oClient.CreateRun(context.Background(), createReq)
	if err != nil {
		t.Fatal(err)
	}
	runID := runResp.Msg.RunId

	for i := 0; i < 1500; i++ {
		env := &pb.Envelope{
			SchemaVersion: int32(criteria.SchemaVersion),
			RunId:         runID,
			CorrelationId: fmt.Sprintf("seed-%d", i),
			Ts:            timestamppb.Now(),
			Payload:       &pb.Envelope_StepEntered{StepEntered: &pb.StepEntered{Step: "seed", Adapter: "shell", Attempt: 1}},
		}
		if _, _, err := ts.store.AppendEvent(context.Background(), mustStoreEvent(t, env)); err != nil {
			t.Fatal(err)
		}
	}

	stream := oClient.SubmitEvents(context.Background())
	stream.RequestHeader().Set("Authorization", "Bearer "+token)
	stream.RequestHeader().Set("since_seq", "0")
	if err := stream.Send(&pb.Envelope{
		SchemaVersion: int32(criteria.SchemaVersion),
		RunId:         runID,
		CorrelationId: "live-event",
		Ts:            timestamppb.Now(),
		Payload:       &pb.Envelope_StepEntered{StepEntered: &pb.StepEntered{Step: "live", Adapter: "shell", Attempt: 1}},
	}); err != nil {
		t.Fatal(err)
	}

	replayCount := 0
	for {
		ack, err := stream.Receive()
		if err != nil {
			t.Fatal(err)
		}
		if ack.CorrelationId == "live-event" {
			if ack.Seq != 1501 {
				t.Fatalf("live ack seq=%d want 1501", ack.Seq)
			}
			break
		}
		replayCount++
	}
	if replayCount != 1500 {
		t.Fatalf("replay ack count=%d want 1500", replayCount)
	}
	_ = stream.CloseRequest()
}

func TestSubmitEventsStream(t *testing.T) {
	ts := newTestStack(t)
	_, oClient, _ := ts.startServer(t)
	overseerID, token := mustRegister(t, oClient)

	createReq := connect.NewRequest(&pb.CreateRunRequest{CriteriaId: overseerID, WorkflowName: "wf", WorkflowHash: "hash"})
	createReq.Header().Set("Authorization", "Bearer "+token)
	runResp, err := oClient.CreateRun(context.Background(), createReq)
	if err != nil {
		t.Fatal(err)
	}
	runID := runResp.Msg.RunId

	t.Run("happy path", func(t *testing.T) {
		stream := oClient.SubmitEvents(context.Background())
		stream.RequestHeader().Set("Authorization", "Bearer "+token)
		err := stream.Send(&pb.Envelope{
			SchemaVersion: int32(criteria.SchemaVersion),
			RunId:         runID,
			CorrelationId: "c1",
			Ts:            timestamppb.Now(),
			Payload:       &pb.Envelope_StepEntered{StepEntered: &pb.StepEntered{Step: "step1", Adapter: "shell", Attempt: 1}},
		})
		if err != nil {
			t.Fatal(err)
		}
		ack, err := stream.Receive()
		if err != nil {
			t.Fatal(err)
		}
		if ack.Seq != 1 || ack.CorrelationId != "c1" {
			t.Fatalf("unexpected ack: %+v", ack)
		}
		_ = stream.CloseRequest()
	})

	t.Run("schema version mismatch", func(t *testing.T) {
		stream := oClient.SubmitEvents(context.Background())
		stream.RequestHeader().Set("Authorization", "Bearer "+token)
		err := stream.Send(&pb.Envelope{
			SchemaVersion: 2,
			RunId:         runID,
			Ts:            timestamppb.Now(),
			Payload:       &pb.Envelope_StepEntered{StepEntered: &pb.StepEntered{Step: "bad", Adapter: "shell", Attempt: 1}},
		})
		if err != nil {
			t.Fatal(err)
		}
		_, err = stream.Receive()
		if connect.CodeOf(err) != connect.CodeFailedPrecondition {
			t.Fatalf("expected failed precondition, got %v", err)
		}
	})

	t.Run("reconnect with since_seq replay", func(t *testing.T) {
		stream := oClient.SubmitEvents(context.Background())
		stream.RequestHeader().Set("Authorization", "Bearer "+token)
		stream.RequestHeader().Set("since_seq", "0")
		err := stream.Send(&pb.Envelope{
			SchemaVersion: int32(criteria.SchemaVersion),
			RunId:         runID,
			CorrelationId: "c2",
			Ts:            timestamppb.Now(),
			Payload:       &pb.Envelope_StepEntered{StepEntered: &pb.StepEntered{Step: "step2", Adapter: "shell", Attempt: 1}},
		})
		if err != nil {
			t.Fatal(err)
		}
		ack1, err := stream.Receive()
		if err != nil {
			t.Fatal(err)
		}
		ack2, err := stream.Receive()
		if err != nil {
			t.Fatal(err)
		}
		if ack1.Seq != 1 {
			t.Fatalf("expected replay seq 1, got %d", ack1.Seq)
		}
		if ack2.Seq != 2 || ack2.CorrelationId != "c2" {
			t.Fatalf("unexpected second ack: %+v", ack2)
		}
		_ = stream.CloseRequest()
	})
}

// TestSubmitEvents_DurationWaitDoesNotPause is a DB-layer regression test that
// confirms a WaitEntered event with Mode="duration" (Signal="") does NOT set
// the run to "paused". Previously the WaitEntered handler lacked the Signal
// guard and would call SetRunPaused unconditionally, breaking duration waits.
func TestSubmitEvents_DurationWaitDoesNotPause(t *testing.T) {
	ts := newTestStack(t)
	_, oClient, _ := ts.startServer(t)
	overseerID, token := mustRegister(t, oClient)

	createReq := connect.NewRequest(&pb.CreateRunRequest{CriteriaId: overseerID, WorkflowName: "wf", WorkflowHash: "hash"})
	createReq.Header().Set("Authorization", "Bearer "+token)
	runResp, err := oClient.CreateRun(context.Background(), createReq)
	if err != nil {
		t.Fatal(err)
	}
	runID := runResp.Msg.RunId

	stream := oClient.SubmitEvents(context.Background())
	stream.RequestHeader().Set("Authorization", "Bearer "+token)
	if err := stream.Send(&pb.Envelope{
		SchemaVersion: int32(criteria.SchemaVersion),
		RunId:         runID,
		CorrelationId: "dur-1",
		Ts:            timestamppb.Now(),
		Payload:       &pb.Envelope_WaitEntered{WaitEntered: &pb.WaitEntered{Node: "pause", Mode: "duration", Duration: "30s", Signal: ""}},
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := stream.Receive(); err != nil {
		t.Fatal(err)
	}
	_ = stream.CloseRequest()

	run, err := ts.store.GetRun(context.Background(), runID)
	if err != nil {
		t.Fatal(err)
	}
	if run.Status == "paused" {
		t.Errorf("duration-mode WaitEntered must not set run status to paused, got %q", run.Status)
	}
}
