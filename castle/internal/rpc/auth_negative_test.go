package rpc

import (
	"context"
	"testing"
	"time"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/brokenbots/overlord/castle/internal/auth"
	"github.com/brokenbots/overlord/shared/events"
	pb "github.com/brokenbots/overlord/shared/pb/overlord/v1"
	"github.com/brokenbots/overlord/shared/pb/overlord/v1/overlordv1connect"
)

// ownershipHarness holds the pieces needed for ownership enforcement tests.
type ownershipHarness struct {
	ts          *testStack
	oClient     overlordv1connect.OverseerServiceClient
	cClient     overlordv1connect.CastleServiceClient
	ownerID     string
	ownerToken  string
	attackerID  string
	attackerTok string
	runID       string
}

// newOwnershipHarness starts a server with auth interceptor + anon-register, registers
// two overseers, and creates a run owned by the first overseer.
func newOwnershipHarness(t *testing.T) *ownershipHarness {
	t.Helper()
	ts := newTestStack(t)
	_, oClient, cClient := ts.startServer(t, connect.WithInterceptors(
		auth.NewInterceptor(ts.store, false, auth.WithAnonRegister()),
	))

	ctx := context.Background()
	regA, err := oClient.Register(ctx, connect.NewRequest(&pb.RegisterRequest{Name: "owner"}))
	if err != nil {
		t.Fatalf("register owner: %v", err)
	}
	regB, err := oClient.Register(ctx, connect.NewRequest(&pb.RegisterRequest{Name: "attacker"}))
	if err != nil {
		t.Fatalf("register attacker: %v", err)
	}

	// Mint a run owned by A via CreateRun (authenticated as A).
	createReq := connect.NewRequest(&pb.CreateRunRequest{OverseerId: regA.Msg.OverseerId, WorkflowName: "wf"})
	createReq.Header().Set("Authorization", "Bearer "+regA.Msg.Token)
	runResp, err := oClient.CreateRun(ctx, createReq)
	if err != nil {
		t.Fatalf("create run: %v", err)
	}

	return &ownershipHarness{
		ts:          ts,
		oClient:     oClient,
		cClient:     cClient,
		ownerID:     regA.Msg.OverseerId,
		ownerToken:  regA.Msg.Token,
		attackerID:  regB.Msg.OverseerId,
		attackerTok: regB.Msg.Token,
		runID:       runResp.Msg.RunId,
	}
}

// --- Register bootstrap gate ---

func TestRegister_BootstrapGate_NoTokenConfigured_IsUnimplemented(t *testing.T) {
	ts := newTestStack(t)
	// No bootstrap options → Register must be disabled.
	_, oClient, _ := ts.startServer(t, connect.WithInterceptors(
		auth.NewInterceptor(ts.store, false),
	))
	_, err := oClient.Register(context.Background(), connect.NewRequest(&pb.RegisterRequest{Name: "x"}))
	if connect.CodeOf(err) != connect.CodeUnimplemented {
		t.Fatalf("expected CodeUnimplemented, got code=%v err=%v", connect.CodeOf(err), err)
	}
}

func TestRegister_BootstrapGate_WrongToken_IsUnauthenticated(t *testing.T) {
	ts := newTestStack(t)
	_, oClient, _ := ts.startServer(t, connect.WithInterceptors(
		auth.NewInterceptor(ts.store, false, auth.WithBootstrapToken("correct-token")),
	))
	req := connect.NewRequest(&pb.RegisterRequest{Name: "x"})
	req.Header().Set("X-Castle-Bootstrap", "wrong-token")
	_, err := oClient.Register(context.Background(), req)
	if connect.CodeOf(err) != connect.CodeUnauthenticated {
		t.Fatalf("expected CodeUnauthenticated, got code=%v err=%v", connect.CodeOf(err), err)
	}
}

func TestRegister_BootstrapGate_CorrectToken_Succeeds(t *testing.T) {
	ts := newTestStack(t)
	_, oClient, _ := ts.startServer(t, connect.WithInterceptors(
		auth.NewInterceptor(ts.store, false, auth.WithBootstrapToken("correct-token")),
	))
	req := connect.NewRequest(&pb.RegisterRequest{Name: "x"})
	req.Header().Set("X-Castle-Bootstrap", "correct-token")
	resp, err := oClient.Register(context.Background(), req)
	if err != nil {
		t.Fatalf("Register with correct token: %v", err)
	}
	if resp.Msg.OverseerId == "" {
		t.Fatal("expected non-empty overseer_id")
	}
}

// --- Heartbeat: caller cannot impersonate another overseer via request field ---

func TestOwnership_Heartbeat_CallerIdentityWins(t *testing.T) {
	h := newOwnershipHarness(t)
	ctx := context.Background()

	// Attacker sends Heartbeat claiming to be the owner via the request field.
	// requireCaller must reject this with PermissionDenied because the
	// authenticated caller (attacker) != the requested overseer_id (owner).
	req := connect.NewRequest(&pb.HeartbeatRequest{OverseerId: h.ownerID})
	req.Header().Set("Authorization", "Bearer "+h.attackerTok)
	_, err := h.oClient.Heartbeat(ctx, req)
	if connect.CodeOf(err) != connect.CodePermissionDenied {
		t.Fatalf("expected CodePermissionDenied, got code=%v err=%v", connect.CodeOf(err), err)
	}
}

// --- CreateRun: caller cannot create a run claiming another overseer's identity ---

// TestOwnership_CreateRun_CallerIdentityWins verifies two properties:
//  1. (negative) A caller that sends a mismatched overseer_id in the request field
//     is rejected with PermissionDenied.
//  2. (positive) A caller that sends its own overseer_id (or no field) gets a run
//     recorded under its authenticated identity — not the request field value.
func TestOwnership_CreateRun_CallerIdentityWins(t *testing.T) {
	h := newOwnershipHarness(t)
	ctx := context.Background()

	// Negative case: attacker claims to be the owner via the request's overseer_id field.
	req := connect.NewRequest(&pb.CreateRunRequest{OverseerId: h.ownerID, WorkflowName: "wf"})
	req.Header().Set("Authorization", "Bearer "+h.attackerTok)
	_, err := h.oClient.CreateRun(ctx, req)
	if connect.CodeOf(err) != connect.CodePermissionDenied {
		t.Fatalf("(negative) expected CodePermissionDenied, got code=%v err=%v", connect.CodeOf(err), err)
	}

	// Positive case: caller sends its own overseer_id — run must be owned by caller.
	posReq := connect.NewRequest(&pb.CreateRunRequest{OverseerId: h.attackerID, WorkflowName: "wf"})
	posReq.Header().Set("Authorization", "Bearer "+h.attackerTok)
	runResp, err := h.oClient.CreateRun(ctx, posReq)
	if err != nil {
		t.Fatalf("(positive) CreateRun: %v", err)
	}
	stored, err := h.ts.store.GetRun(ctx, runResp.Msg.RunId)
	if err != nil {
		t.Fatalf("(positive) GetRun: %v", err)
	}
	if stored.OverseerID != h.attackerID {
		t.Errorf("(positive) run.OverseerID=%q want attacker %q; requireCaller may have returned the request field instead of caller identity",
			stored.OverseerID, h.attackerID)
	}
}

// --- ReattachRun: attacker cannot reattach to owner's run ---

func TestOwnership_ReattachRun_OtherOverseer_PermissionDenied(t *testing.T) {
	h := newOwnershipHarness(t)
	ctx := context.Background()

	req := connect.NewRequest(&pb.ReattachRunRequest{RunId: h.runID, OverseerId: h.attackerID})
	req.Header().Set("Authorization", "Bearer "+h.attackerTok)
	_, err := h.oClient.ReattachRun(ctx, req)
	if connect.CodeOf(err) != connect.CodePermissionDenied {
		t.Fatalf("expected CodePermissionDenied, got code=%v err=%v", connect.CodeOf(err), err)
	}
}

// --- SubmitEvents: attacker cannot submit events for owner's run ---

func TestOwnership_SubmitEvents_OtherOverseer_PermissionDenied(t *testing.T) {
	h := newOwnershipHarness(t)
	ctx := context.Background()

	stream := h.oClient.SubmitEvents(ctx)
	stream.RequestHeader().Set("Authorization", "Bearer "+h.attackerTok)

	err := stream.Send(&pb.Envelope{
		SchemaVersion: int32(events.SchemaVersion),
		RunId:         h.runID, // owned by owner, not attacker
		CorrelationId: "neg-1",
		Ts:            timestamppb.Now(),
		Payload:       &pb.Envelope_StepEntered{StepEntered: &pb.StepEntered{Step: "build", Adapter: "shell", Attempt: 1}},
	})
	if err != nil {
		// Send error is also acceptable — the server may close the stream before we read.
		return
	}
	_, recvErr := stream.Receive()
	if connect.CodeOf(recvErr) != connect.CodePermissionDenied {
		t.Fatalf("expected CodePermissionDenied, got code=%v err=%v", connect.CodeOf(recvErr), recvErr)
	}
}

// --- Control: caller cannot subscribe to another overseer's command channel ---

func TestOwnership_Control_CallerIdentityDeterminesChannel(t *testing.T) {
	h := newOwnershipHarness(t)
	ctx := context.Background()

	// Attacker subscribes to Control but passes owner's overseer_id in the request field.
	// requireCaller must reject this with PermissionDenied (caller != request field).
	req := connect.NewRequest(&pb.ControlSubscribeRequest{OverseerId: h.ownerID})
	req.Header().Set("Authorization", "Bearer "+h.attackerTok)
	stream, err := h.oClient.Control(ctx, req)
	if err != nil {
		if connect.CodeOf(err) == connect.CodePermissionDenied {
			return // correct
		}
		t.Fatalf("unexpected error: %v", err)
	}
	// If Control returned a stream, the first Receive must carry the rejection.
	stream.Receive()
	if connect.CodeOf(stream.Err()) != connect.CodePermissionDenied {
		t.Fatalf("expected CodePermissionDenied, got code=%v err=%v", connect.CodeOf(stream.Err()), stream.Err())
	}
}

// --- StopRun: attacker cannot stop owner's run ---

func TestOwnership_StopRun_OtherOverseer_PermissionDenied(t *testing.T) {
	h := newOwnershipHarness(t)
	ctx := context.Background()

	req := connect.NewRequest(&pb.StopRunRequest{RunId: h.runID})
	req.Header().Set("Authorization", "Bearer "+h.attackerTok)
	_, err := h.cClient.StopRun(ctx, req)
	if connect.CodeOf(err) != connect.CodePermissionDenied {
		t.Fatalf("expected CodePermissionDenied, got code=%v err=%v", connect.CodeOf(err), err)
	}
}

// --- Resume: attacker cannot resume owner's run ---

func TestOwnership_Resume_OtherOverseer_PermissionDenied(t *testing.T) {
	h := newOwnershipHarness(t)
	ctx := context.Background()

	// Pause the run.
	if err := h.ts.store.SetRunPaused(ctx, h.runID, "gate", time.Now().UTC()); err != nil {
		t.Fatalf("SetRunPaused: %v", err)
	}

	req := connect.NewRequest(&pb.ResumeRequest{RunId: h.runID, Signal: "gate"})
	req.Header().Set("Authorization", "Bearer "+h.attackerTok)
	_, err := h.oClient.Resume(ctx, req)
	if connect.CodeOf(err) != connect.CodePermissionDenied {
		t.Fatalf("expected CodePermissionDenied, got code=%v err=%v", connect.CodeOf(err), err)
	}
}

// --- helpers ---

// (no test-local helpers required; all helpers live in testhelpers_test.go)
