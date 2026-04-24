package rpc

import (
	"context"
	"testing"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/brokenbots/overlord/shared/events"
	pb "github.com/brokenbots/overlord/shared/pb/overlord/v1"
)

func TestOverseerUnaryMethods(t *testing.T) {
	ts := newTestStack(t)
	ctx := context.Background()

	reg, err := ts.overseer.Register(ctx, connect.NewRequest(&pb.RegisterRequest{Name: "o1", Labels: map[string]string{"hostname": "h1", "version": "v1"}}))
	if err != nil {
		t.Fatal(err)
	}
	if reg.Msg.OverseerId == "" || reg.Msg.Token == "" {
		t.Fatal("expected register to return overseer id and token")
	}

	if _, err := ts.overseer.Heartbeat(ctx, connect.NewRequest(&pb.HeartbeatRequest{OverseerId: reg.Msg.OverseerId})); err != nil {
		t.Fatal(err)
	}

	runResp, err := ts.overseer.CreateRun(ctx, connect.NewRequest(&pb.CreateRunRequest{OverseerId: reg.Msg.OverseerId, WorkflowName: "wf", WorkflowHash: "h"}))
	if err != nil {
		t.Fatal(err)
	}
	if runResp.Msg.RunId == "" || runResp.Msg.Status != "pending" {
		t.Fatalf("unexpected run response: %+v", runResp.Msg)
	}
}

func TestSubmitEventsStream(t *testing.T) {
	ts := newTestStack(t)
	_, oClient, _ := ts.startServer(t)
	overseerID, token := mustRegister(t, oClient)

	createReq := connect.NewRequest(&pb.CreateRunRequest{OverseerId: overseerID, WorkflowName: "wf", WorkflowHash: "hash"})
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
			SchemaVersion: int32(events.SchemaVersion),
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
			SchemaVersion: int32(events.SchemaVersion),
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
