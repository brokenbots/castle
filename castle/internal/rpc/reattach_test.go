package rpc

import (
	"context"
	"strings"
	"testing"
	"time"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/brokenbots/castle/castle/internal/auth"
	"github.com/brokenbots/castle/castle/internal/store"
	criteria "github.com/brokenbots/criteria/sdk"
	pb "github.com/brokenbots/criteria/sdk/pb/criteria/v1" // import-lint:allow castle service bindings (W08: move to castle-proto)
)

// createRunAtStep creates a run in "running" status with currentStep set.
func createRunAtStep(t *testing.T, ts *testStack, overseerID, step string) string {
	t.Helper()
	ctx := context.Background()
	r := &store.Run{
		ID:           "run-" + step + "-" + overseerID[:8],
		OverseerID:   overseerID,
		WorkflowName: "test-workflow",
		Status:       "running",
		CurrentStep:  step,
		CreatedAt:    time.Now().UTC(),
	}
	if err := ts.store.CreateRun(ctx, r); err != nil {
		t.Fatal(err)
	}
	return r.ID
}

func TestReattachRun_RunningReturnsCurrentStep(t *testing.T) {
	ts := newTestStack(t)
	ctx := context.Background()

	// Register an overseer.
	reg, err := ts.criteria.Register(ctx, connect.NewRequest(&pb.RegisterRequest{Name: "o-reattach"}))
	if err != nil {
		t.Fatal(err)
	}
	overseerID := reg.Msg.CriteriaId

	// Create a run in running state at step "build".
	runID := createRunAtStep(t, ts, overseerID, "build")

	// Record an attempt so GetLatestAttempt returns something.
	if raErr := ts.store.RecordAttemptStart(ctx, &store.RunAttempt{
		RunID:     runID,
		Step:      "build",
		Attempt:   1,
		StartedAt: time.Now().UTC(),
	}); raErr != nil {
		t.Fatal(raErr)
	}

	resp, err := ts.criteria.ReattachRun(ctx, connect.NewRequest(&pb.ReattachRunRequest{
		RunId:      runID,
		CriteriaId: overseerID,
	}))
	if err != nil {
		t.Fatalf("ReattachRun: %v", err)
	}
	if !resp.Msg.CanResume {
		t.Fatal("expected can_resume=true for running run")
	}
	if resp.Msg.CurrentStep != "build" {
		t.Fatalf("current_step=%q want %q", resp.Msg.CurrentStep, "build")
	}
	if resp.Msg.Attempt != 1 {
		t.Fatalf("attempt=%d want 1", resp.Msg.Attempt)
	}
	if resp.Msg.Status != "running" {
		t.Fatalf("status=%q want running", resp.Msg.Status)
	}
}

func TestReattachRun_TerminalReturnsCannotResume(t *testing.T) {
	ts := newTestStack(t)
	ctx := context.Background()

	reg, err := ts.criteria.Register(ctx, connect.NewRequest(&pb.RegisterRequest{Name: "o-terminal"}))
	if err != nil {
		t.Fatal(err)
	}
	overseerID := reg.Msg.CriteriaId

	// Create a succeeded run.
	now := time.Now().UTC()
	r := &store.Run{
		ID:           "run-terminal",
		OverseerID:   overseerID,
		WorkflowName: "wf",
		Status:       "succeeded",
		CreatedAt:    now,
		EndedAt:      &now,
	}
	if err := ts.store.CreateRun(ctx, r); err != nil {
		t.Fatal(err)
	}

	resp, err := ts.criteria.ReattachRun(ctx, connect.NewRequest(&pb.ReattachRunRequest{
		RunId:      r.ID,
		CriteriaId: overseerID,
	}))
	if err != nil {
		t.Fatalf("ReattachRun: %v", err)
	}
	if resp.Msg.CanResume {
		t.Fatal("expected can_resume=false for terminal run")
	}
	if resp.Msg.Status != "succeeded" {
		t.Fatalf("status=%q want succeeded", resp.Msg.Status)
	}
}

func TestReattachRun_WrongOwner_Rejected(t *testing.T) {
	ts := newTestStack(t)
	ctx := context.Background()

	// Register two overseers.
	reg1, _ := ts.criteria.Register(ctx, connect.NewRequest(&pb.RegisterRequest{Name: "o-owner"}))
	reg2, _ := ts.criteria.Register(ctx, connect.NewRequest(&pb.RegisterRequest{Name: "o-other"}))
	ownerID := reg1.Msg.CriteriaId
	otherID := reg2.Msg.CriteriaId

	runID := createRunAtStep(t, ts, ownerID, "deploy")

	// Other overseer tries to reattach — inject its identity as the caller.
	callerCtx := auth.WithCallerCriteriaID(ctx, otherID)
	_, err := ts.criteria.ReattachRun(callerCtx, connect.NewRequest(&pb.ReattachRunRequest{
		RunId:      runID,
		CriteriaId: otherID,
	}))
	if connect.CodeOf(err) != connect.CodePermissionDenied {
		t.Fatalf("expected CodePermissionDenied, got code=%v err=%v", connect.CodeOf(err), err)
	}
}

func TestReattachRun_MissingArgs_ReturnsError(t *testing.T) {
	ts := newTestStack(t)
	ctx := context.Background()

	// run_id is required — a missing run_id must be rejected.
	_, err := ts.criteria.ReattachRun(ctx, connect.NewRequest(&pb.ReattachRunRequest{CriteriaId: "x"}))
	if err == nil {
		t.Fatal("expected error for missing run_id")
	}

	// run_id present but run does not exist — must also be rejected (NotFound).
	_, err = ts.criteria.ReattachRun(ctx, connect.NewRequest(&pb.ReattachRunRequest{RunId: "nonexistent"}))
	if err == nil {
		t.Fatal("expected error for nonexistent run")
	}
}

func TestReattachRun_RunNotFound(t *testing.T) {
	ts := newTestStack(t)
	ctx := context.Background()

	_, err := ts.criteria.ReattachRun(ctx, connect.NewRequest(&pb.ReattachRunRequest{
		RunId:      "nonexistent-run",
		CriteriaId: "nonexistent-overseer",
	}))
	if err == nil {
		t.Fatal("expected error for nonexistent run")
	}
}

func TestRunAttempts_RecordAndComplete(t *testing.T) {
	ts := newTestStack(t)
	ctx := context.Background()

	reg, _ := ts.criteria.Register(ctx, connect.NewRequest(&pb.RegisterRequest{Name: "o-attempts"}))
	overseerID := reg.Msg.CriteriaId
	runID := createRunAtStep(t, ts, overseerID, "test")

	// Record attempt 1 start.
	if err := ts.store.RecordAttemptStart(ctx, &store.RunAttempt{
		RunID:     runID,
		Step:      "test",
		Attempt:   1,
		StartedAt: time.Now().UTC(),
	}); err != nil {
		t.Fatal(err)
	}

	latest, err := ts.store.GetLatestAttempt(ctx, runID, "test")
	if err != nil {
		t.Fatal(err)
	}
	if latest.Attempt != 1 {
		t.Fatalf("latest attempt=%d want 1", latest.Attempt)
	}
	if latest.CompletedAt != nil {
		t.Fatal("expected completed_at to be nil before completion")
	}

	// Complete it.
	if err := ts.store.RecordAttemptComplete(ctx, runID, "test", 1, "success"); err != nil {
		t.Fatal(err)
	}
	latest, err = ts.store.GetLatestAttempt(ctx, runID, "test")
	if err != nil {
		t.Fatal(err)
	}
	if latest.Outcome != "success" {
		t.Fatalf("outcome=%q want success", latest.Outcome)
	}
	if latest.CompletedAt == nil {
		t.Fatal("expected completed_at to be set after completion")
	}
}

func TestSubmitEvents_StepEntered_RecordsAttempt(t *testing.T) {
	ts := newTestStack(t)
	_, oClient, _ := ts.startServer(t)
	overseerID, token := mustRegister(t, oClient)

	createReq := connect.NewRequest(&pb.CreateRunRequest{CriteriaId: overseerID, WorkflowName: "wf", WorkflowHash: "h"})
	createReq.Header().Set("Authorization", "Bearer "+token)
	runResp, err := oClient.CreateRun(context.Background(), createReq)
	if err != nil {
		t.Fatal(err)
	}
	runID := runResp.Msg.RunId

	stream := oClient.SubmitEvents(context.Background())
	stream.RequestHeader().Set("Authorization", "Bearer "+token)

	// Send StepEntered event.
	if err := stream.Send(&pb.Envelope{
		SchemaVersion: int32(criteria.SchemaVersion),
		RunId:         runID,
		CorrelationId: "se-1",
		Ts:            timestamppb.Now(),
		Payload:       &pb.Envelope_StepEntered{StepEntered: &pb.StepEntered{Step: "build", Adapter: "shell", Attempt: 1}},
	}); err != nil {
		t.Fatal(err)
	}
	// Receive ack.
	if _, err := stream.Receive(); err != nil {
		t.Fatal(err)
	}
	_ = stream.CloseRequest()

	// Verify attempt was recorded in the store.
	latest, err := ts.store.GetLatestAttempt(context.Background(), runID, "build")
	if err != nil {
		t.Fatalf("GetLatestAttempt: %v", err)
	}
	if latest.Attempt != 1 {
		t.Fatalf("attempt=%d want 1", latest.Attempt)
	}
}

func TestSubmitEvents_StepResumed_StoredAndFannedOut(t *testing.T) {
	ts := newTestStack(t)
	_, oClient, _ := ts.startServer(t)
	overseerID, token := mustRegister(t, oClient)

	createReq := connect.NewRequest(&pb.CreateRunRequest{CriteriaId: overseerID, WorkflowName: "wf", WorkflowHash: "h"})
	createReq.Header().Set("Authorization", "Bearer "+token)
	runResp, err := oClient.CreateRun(context.Background(), createReq)
	if err != nil {
		t.Fatal(err)
	}
	runID := runResp.Msg.RunId

	stream := oClient.SubmitEvents(context.Background())
	stream.RequestHeader().Set("Authorization", "Bearer "+token)

	// Send StepResumed event.
	if err := stream.Send(&pb.Envelope{
		SchemaVersion: int32(criteria.SchemaVersion),
		RunId:         runID,
		CorrelationId: "sr-1",
		Ts:            timestamppb.Now(),
		Payload:       &pb.Envelope_StepResumed{StepResumed: &pb.StepResumed{Step: "build", Attempt: 2, Reason: "overseer_restart"}},
	}); err != nil {
		t.Fatal(err)
	}
	ack, err := stream.Receive()
	if err != nil {
		t.Fatal(err)
	}
	_ = stream.CloseRequest()

	// Verify the event is persisted and retrievable.
	evts, err := ts.store.ListEvents(context.Background(), runID, 0, 10)
	if err != nil {
		t.Fatal(err)
	}
	var found bool
	for _, e := range evts {
		if e.Seq == ack.Seq {
			var sr pb.StepResumed
			if err := protojson.Unmarshal(e.Payload, &sr); err != nil {
				t.Fatalf("unmarshal StepResumed payload: %v", err)
			}
			if sr.Step == "build" && sr.Attempt == 2 {
				found = true
			}
		}
	}
	if !found {
		t.Fatal("StepResumed event not found in persisted events")
	}
}

func TestReattachRun_ReturnsVariableScopeAndPendingSignal(t *testing.T) {
	ts := newTestStack(t)
	ctx := context.Background()

	reg, err := ts.criteria.Register(ctx, connect.NewRequest(&pb.RegisterRequest{Name: "o-reattach-scope"}))
	if err != nil {
		t.Fatal(err)
	}
	overseerID := reg.Msg.CriteriaId
	runID := createRunAtStep(t, ts, overseerID, "wait-step")

	scopeJSON := `{"var":{"x":"42"},"steps":{"wait-step":{"out":"yes"}}}`
	if err := ts.store.SetRunVariableScope(ctx, runID, scopeJSON); err != nil {
		t.Fatal(err)
	}
	// Append an event so runs.last_seq is non-zero and can be validated.
	env := &pb.Envelope{
		SchemaVersion: 1,
		RunId:         runID,
		Ts:            timestamppb.Now(),
		Payload:       &pb.Envelope_StepLog{StepLog: &pb.StepLog{Step: "wait-step", Stream: pb.LogStream_LOG_STREAM_STDOUT, Chunk: "setup"}},
		CorrelationId: "reattach-seq",
	}
	seq, _, err := ts.store.AppendEvent(ctx, mustStoreEvent(t, env))
	if err != nil {
		t.Fatal(err)
	}

	pausedAt := time.Now().UTC()
	if err := ts.store.SetRunPaused(ctx, runID, "resume-signal", pausedAt); err != nil {
		t.Fatal(err)
	}

	resp, err := ts.criteria.ReattachRun(ctx, connect.NewRequest(&pb.ReattachRunRequest{
		RunId:      runID,
		CriteriaId: overseerID,
	}))
	if err != nil {
		t.Fatalf("ReattachRun: %v", err)
	}
	if !resp.Msg.CanResume {
		t.Fatal("expected can_resume=true for paused run")
	}
	if resp.Msg.CurrentStep != "wait-step" {
		t.Fatalf("current_step=%q want wait-step", resp.Msg.CurrentStep)
	}
	if resp.Msg.VariableScope != scopeJSON {
		t.Fatalf("variable_scope=%q want %q", resp.Msg.VariableScope, scopeJSON)
	}
	if resp.Msg.PendingSignal != "resume-signal" {
		t.Fatalf("pending_signal=%q want resume-signal", resp.Msg.PendingSignal)
	}
	if resp.Msg.LastSeq != seq {
		t.Fatalf("last_seq=%d want %d", resp.Msg.LastSeq, seq)
	}
}

func TestReattachRun_FlushesPendingScopeBeforeReturning(t *testing.T) {
	ts := newTestStack(t)
	ctx := context.Background()

	reg, err := ts.criteria.Register(ctx, connect.NewRequest(&pb.RegisterRequest{Name: "o-reattach-flush"}))
	if err != nil {
		t.Fatal(err)
	}
	overseerID := reg.Msg.CriteriaId
	runID := createRunAtStep(t, ts, overseerID, "build")

	// Queue a scope mutation but do not flush it manually.
	ts.criteria.scope.Enqueue(runID, func(scope map[string]interface{}) {
		varMap, _ := scope["var"].(map[string]interface{})
		if varMap == nil {
			varMap = map[string]interface{}{}
		}
		varMap["flushed"] = "true"
		scope["var"] = varMap
	})

	resp, err := ts.criteria.ReattachRun(ctx, connect.NewRequest(&pb.ReattachRunRequest{
		RunId:      runID,
		CriteriaId: overseerID,
	}))
	if err != nil {
		t.Fatalf("ReattachRun: %v", err)
	}
	if !resp.Msg.CanResume {
		t.Fatal("expected can_resume=true")
	}
	if resp.Msg.VariableScope == "" {
		t.Fatal("expected flushed variable scope to be returned")
	}
	if !strings.Contains(resp.Msg.VariableScope, "flushed") {
		t.Fatalf("variable_scope does not contain flushed mutation: %q", resp.Msg.VariableScope)
	}
}
