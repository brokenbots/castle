package rpc

import (
	"context"
	"testing"
	"time"

	"connectrpc.com/connect"

	"github.com/brokenbots/overlord/castle/internal/auth"
	"github.com/brokenbots/overlord/castle/internal/store"
	pb "github.com/brokenbots/overlord/shared/pb/overlord/v1"
	"github.com/brokenbots/overlord/shared/pb/overlord/v1/overlordv1connect"
)

// seedPausedRun creates a run in the DB (under a registered overseer) and sets
// its status to "paused" with a pending signal. Returns the run ID.
func seedPausedRun(t *testing.T, stack *testStack, pendingSignal string) (runID, overseerID string) {
	t.Helper()
	ctx := context.Background()
	reg, err := stack.overseer.Register(ctx, connect.NewRequest(&pb.RegisterRequest{Name: "o-resume-" + t.Name()}))
	if err != nil {
		t.Fatalf("Register: %v", err)
	}
	overseerID = reg.Msg.OverseerId
	run := &store.Run{
		ID:           "run-" + overseerID[:8],
		OverseerID:   overseerID,
		WorkflowName: "test",
		WorkflowHCL:  "workflow {}",
		Status:       "running",
		CreatedAt:    time.Now().UTC(),
	}
	if err := stack.store.CreateRun(ctx, run); err != nil {
		t.Fatalf("CreateRun: %v", err)
	}
	if err := stack.store.SetRunPaused(ctx, run.ID, pendingSignal, time.Now().UTC()); err != nil {
		t.Fatalf("SetRunPaused: %v", err)
	}
	return run.ID, overseerID
}

func TestResume_HappyPath(t *testing.T) {
	stack := newTestStack(t)
	ctx := context.Background()
	runID, _ := seedPausedRun(t, stack, "approve")

	req := connect.NewRequest(&pb.ResumeRequest{
		RunId:   runID,
		Signal:  "approve",
		Payload: map[string]string{"actor": "alice"},
	})
	resp, err := stack.overseer.Resume(ctx, req)
	if err != nil {
		t.Fatalf("Resume failed: %v", err)
	}
	if !resp.Msg.Accepted {
		t.Errorf("expected accepted=true, reason=%q", resp.Msg.Reason)
	}
	if resp.Msg.Reason != "ok" {
		t.Errorf("expected reason='ok', got %q", resp.Msg.Reason)
	}

	// Verify run status is cleared back to running.
	run, err := stack.store.GetRun(ctx, runID)
	if err != nil {
		t.Fatalf("GetRun: %v", err)
	}
	if run.Status != "running" {
		t.Errorf("expected status='running' after resume, got %q", run.Status)
	}
	if run.PendingSignal != "" {
		t.Errorf("expected pending_signal cleared, got %q", run.PendingSignal)
	}
}

func TestResume_RunNotPaused(t *testing.T) {
	stack := newTestStack(t)
	ctx := context.Background()

	// Register an overseer.
	reg, err := stack.overseer.Register(ctx, connect.NewRequest(&pb.RegisterRequest{Name: "o-not-paused"}))
	if err != nil {
		t.Fatalf("Register: %v", err)
	}

	// Create a running run (not paused).
	run := &store.Run{
		ID:           "run-not-paused",
		OverseerID:   reg.Msg.OverseerId,
		WorkflowName: "test",
		WorkflowHCL:  "workflow {}",
		Status:       "running",
		CreatedAt:    time.Now().UTC(),
	}
	if err := stack.store.CreateRun(ctx, run); err != nil {
		t.Fatalf("create run: %v", err)
	}

	req := connect.NewRequest(&pb.ResumeRequest{
		RunId:  run.ID,
		Signal: "approve",
	})
	resp, err := stack.overseer.Resume(ctx, req)
	if err != nil {
		t.Fatalf("Resume failed: %v", err)
	}
	if resp.Msg.Accepted {
		t.Error("expected accepted=false for non-paused run")
	}
	if resp.Msg.Reason != "run_not_paused" {
		t.Errorf("expected reason='run_not_paused', got %q", resp.Msg.Reason)
	}
}

func TestResume_SignalMismatch(t *testing.T) {
	stack := newTestStack(t)
	ctx := context.Background()
	runID, _ := seedPausedRun(t, stack, "gate-a")

	req := connect.NewRequest(&pb.ResumeRequest{
		RunId:  runID,
		Signal: "gate-b", // wrong signal
	})
	resp, err := stack.overseer.Resume(ctx, req)
	if err != nil {
		t.Fatalf("Resume failed: %v", err)
	}
	if resp.Msg.Accepted {
		t.Error("expected accepted=false for mismatched signal")
	}
	if resp.Msg.Reason != "signal_mismatch" {
		t.Errorf("expected reason='signal_mismatch', got %q", resp.Msg.Reason)
	}
}

func TestResume_NoPendingSignal(t *testing.T) {
	stack := newTestStack(t)
	ctx := context.Background()

	// Register overseer.
	reg, err := stack.overseer.Register(ctx, connect.NewRequest(&pb.RegisterRequest{Name: "o-no-signal"}))
	if err != nil {
		t.Fatalf("Register: %v", err)
	}

	// Create a run, then set paused with empty signal (edge case).
	run := &store.Run{
		ID:           "run-no-signal",
		OverseerID:   reg.Msg.OverseerId,
		WorkflowName: "test",
		WorkflowHCL:  "workflow {}",
		Status:       "running",
		CreatedAt:    time.Now().UTC(),
	}
	if err := stack.store.CreateRun(ctx, run); err != nil {
		t.Fatalf("create run: %v", err)
	}
	if err := stack.store.SetRunPaused(ctx, run.ID, "", time.Now().UTC()); err != nil {
		t.Fatalf("set paused: %v", err)
	}

	req := connect.NewRequest(&pb.ResumeRequest{
		RunId:  run.ID,
		Signal: "anything",
	})
	resp, err := stack.overseer.Resume(ctx, req)
	if err != nil {
		t.Fatalf("Resume failed: %v", err)
	}
	if resp.Msg.Accepted {
		t.Error("expected accepted=false for no pending signal")
	}
	if resp.Msg.Reason != "no_pending_signal" {
		t.Errorf("expected reason='no_pending_signal', got %q", resp.Msg.Reason)
	}
}

// TestResume_WrongOverseerToken verifies that a caller authenticated as overseer-B
// cannot resume a run owned by overseer-A (CodePermissionDenied).
func TestResume_WrongOverseerToken(t *testing.T) {
	stack := newTestStack(t)
	ctx := context.Background()

	// Start a real HTTP server with the auth interceptor wired in.
	tsrv, overseerClient, _ := stack.startServer(t,
		connect.WithInterceptors(auth.NewInterceptor(stack.store, false, auth.WithAnonRegister())),
	)

	// Register overseer-A (owns the run) and overseer-B (the attacker).
	respA, err := overseerClient.Register(ctx, connect.NewRequest(&pb.RegisterRequest{Name: "overseer-a"}))
	if err != nil {
		t.Fatalf("Register A: %v", err)
	}
	respB, err := overseerClient.Register(ctx, connect.NewRequest(&pb.RegisterRequest{Name: "overseer-b"}))
	if err != nil {
		t.Fatalf("Register B: %v", err)
	}
	tokenB := respB.Msg.Token

	// Create a run owned by overseer-A and pause it.
	run := &store.Run{
		ID:           "run-owned-by-a",
		OverseerID:   respA.Msg.OverseerId,
		WorkflowName: "test",
		WorkflowHCL:  "workflow {}",
		Status:       "running",
		CreatedAt:    time.Now().UTC(),
	}
	if err := stack.store.CreateRun(ctx, run); err != nil {
		t.Fatalf("CreateRun: %v", err)
	}
	if err := stack.store.SetRunPaused(ctx, run.ID, "gate", time.Now().UTC()); err != nil {
		t.Fatalf("SetRunPaused: %v", err)
	}

	// Attempt resume authenticated as overseer-B.
	attackClient := overlordv1connect.NewOverseerServiceClient(h2cClient(), tsrv.URL)
	req := connect.NewRequest(&pb.ResumeRequest{
		RunId:  run.ID,
		Signal: "gate",
	})
	req.Header().Set("Authorization", "Bearer "+tokenB)

	_, resumeErr := attackClient.Resume(ctx, req)
	if connect.CodeOf(resumeErr) != connect.CodePermissionDenied {
		t.Errorf("expected CodePermissionDenied, got %v (err: %v)", connect.CodeOf(resumeErr), resumeErr)
	}
}
