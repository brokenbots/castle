package rpc

import (
	"context"
	"testing"
	"time"

	"connectrpc.com/connect"

	"github.com/brokenbots/castle/castle/internal/store"
	criteria "github.com/brokenbots/criteria/sdk"
	pb "github.com/brokenbots/criteria/sdk/pb/criteria/v1" // import-lint:allow castle service bindings (W08: move to castle-proto)
)

// markRunStatus moves a run to the given status in the store (test scaffold
// for parked-state scenarios).
func markRunStatus(t *testing.T, ts *testStack, runID, status string) {
	t.Helper()
	r, err := ts.store.GetRun(context.Background(), runID)
	if err != nil {
		t.Fatal(err)
	}
	r.Status = status
	if err := ts.store.UpdateRun(context.Background(), r); err != nil {
		t.Fatal(err)
	}
}

// TestStopRunParksRunStopped (CRI-207): an operator StopRun delivers the
// RunCancel control to the owning agent and immediately parks the run in
// status stopped on the same run id — non-terminal (ended_at stays unset), so
// the run reads as resumable even before the agent's teardown lands. Stopping
// a paused run drops the pause state.
func TestStopRunParksRunStopped(t *testing.T) {
	ts := newTestStack(t)
	_, oClient, cClient := ts.startServer(t)
	overseerID, _ := mustRegister(t, oClient)
	run, err := oClient.CreateRun(context.Background(), connect.NewRequest(&pb.CreateRunRequest{CriteriaId: overseerID, WorkflowName: "wf"}))
	if err != nil {
		t.Fatal(err)
	}
	runID := run.Msg.RunId

	ctrl := drainControlReady(t, oClient, overseerID)
	defer ctrl.Close()

	resp, err := cClient.StopRun(context.Background(), connect.NewRequest(&pb.StopRunRequest{RunId: runID, Reason: "hotfix"}))
	if err != nil {
		t.Fatalf("StopRun: %v", err)
	}
	if resp.Msg.IssuedAt == nil {
		t.Fatal("expected issued_at on a successful stop")
	}

	if !ctrl.Receive() {
		t.Fatalf("expected RunCancel control, err=%v", ctrl.Err())
	}
	cmd, ok := ctrl.Msg().Command.(*pb.ControlMessage_RunCancel)
	if !ok {
		t.Fatalf("unexpected control command: %T", ctrl.Msg().Command)
	}
	if cmd.RunCancel.RunId != runID {
		t.Fatalf("run id=%s", cmd.RunCancel.RunId)
	}
	_ = ctrl.Close()

	got, err := ts.store.GetRun(context.Background(), runID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != "stopped" {
		t.Fatalf("status = %q, want stopped", got.Status)
	}
	if got.ID != runID {
		t.Fatalf("run id changed to %s", got.ID)
	}
	if got.EndedAt != nil {
		t.Fatalf("ended_at stamped on a resumable park: %v", got.EndedAt)
	}

	// Stopping a paused run parks it and drops the pause state.
	run2, err := oClient.CreateRun(context.Background(), connect.NewRequest(&pb.CreateRunRequest{CriteriaId: overseerID, WorkflowName: "wf"}))
	if err != nil {
		t.Fatal(err)
	}
	if err := ts.store.SetRunPaused(context.Background(), run2.Msg.RunId, "deploy", time.Now().UTC()); err != nil {
		t.Fatalf("SetRunPaused: %v", err)
	}
	ctrl2 := drainControlReady(t, oClient, overseerID)
	defer ctrl2.Close()
	if _, err := cClient.StopRun(context.Background(), connect.NewRequest(&pb.StopRunRequest{RunId: run2.Msg.RunId})); err != nil {
		t.Fatalf("StopRun paused: %v", err)
	}
	got2, err := ts.store.GetRun(context.Background(), run2.Msg.RunId)
	if err != nil {
		t.Fatal(err)
	}
	if got2.Status != "stopped" || got2.PendingSignal != "" || got2.PausedAt != nil {
		t.Fatalf("stopped-from-paused: status=%q signal=%q paused_at=%v", got2.Status, got2.PendingSignal, got2.PausedAt)
	}
}

// TestStopRunAlreadyStopped (CRI-207): a second stop on an already parked run
// is a precondition failure and does not re-deliver a control command.
func TestStopRunAlreadyStopped(t *testing.T) {
	ts := newTestStack(t)
	_, oClient, cClient := ts.startServer(t)
	overseerID, _ := mustRegister(t, oClient)
	run, err := oClient.CreateRun(context.Background(), connect.NewRequest(&pb.CreateRunRequest{CriteriaId: overseerID, WorkflowName: "wf"}))
	if err != nil {
		t.Fatal(err)
	}
	ctrl := drainControlReady(t, oClient, overseerID)
	defer ctrl.Close()

	if _, err := cClient.StopRun(context.Background(), connect.NewRequest(&pb.StopRunRequest{RunId: run.Msg.RunId})); err != nil {
		t.Fatalf("first StopRun: %v", err)
	}
	// Drain the first RunCancel so the backlog is empty for the second call.
	if !ctrl.Receive() {
		t.Fatalf("expected RunCancel control, err=%v", ctrl.Err())
	}
	_ = ctrl.Close()

	_, err = cClient.StopRun(context.Background(), connect.NewRequest(&pb.StopRunRequest{RunId: run.Msg.RunId}))
	if connect.CodeOf(err) != connect.CodeFailedPrecondition {
		t.Fatalf("expected failed precondition for an already-stopped run, got %v", err)
	}
}

// TestResumeRunFromStopped (CRI-207): resume from the console/API moves a
// parked run back to running on the same run id; the resume control is
// delivered with an empty signal because a stopped run never has a pending
// signal.
func TestResumeRunFromStopped(t *testing.T) {
	ts := newTestStack(t)
	_, oClient, cClient := ts.startServer(t)
	overseerID, _ := mustRegister(t, oClient)
	run, err := oClient.CreateRun(context.Background(), connect.NewRequest(&pb.CreateRunRequest{CriteriaId: overseerID, WorkflowName: "wf"}))
	if err != nil {
		t.Fatal(err)
	}
	runID := run.Msg.RunId
	markRunStatus(t, ts, runID, "stopped")

	// A signal-bearing resume is rejected: a stopped run has no pending signal.
	_, err = cClient.ResumeRun(context.Background(), connect.NewRequest(&pb.ResumeRunRequest{RunId: runID, Signal: "deploy"}))
	if connect.CodeOf(err) != connect.CodeFailedPrecondition {
		t.Fatalf("expected failed precondition for signal on stopped run, got %v", err)
	}

	ctrl := drainControlReady(t, oClient, overseerID)
	defer ctrl.Close()

	resp, err := cClient.ResumeRun(context.Background(), connect.NewRequest(&pb.ResumeRunRequest{RunId: runID}))
	if err != nil {
		t.Fatalf("ResumeRun: %v", err)
	}
	if resp.Msg.IssuedAt == nil {
		t.Fatal("expected issued_at on a successful resume")
	}

	if !ctrl.Receive() {
		t.Fatalf("expected ResumeRun control, err=%v", ctrl.Err())
	}
	cmd, ok := ctrl.Msg().Command.(*pb.ControlMessage_ResumeRun)
	if !ok {
		t.Fatalf("unexpected control command: %T", ctrl.Msg().Command)
	}
	if cmd.ResumeRun.RunId != runID {
		t.Fatalf("run id=%s", cmd.ResumeRun.RunId)
	}
	if cmd.ResumeRun.Signal != "" {
		t.Fatalf("resume signal = %q, want empty", cmd.ResumeRun.Signal)
	}
	_ = ctrl.Close()

	got, err := ts.store.GetRun(context.Background(), runID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != "running" {
		t.Fatalf("status = %q, want running after resume", got.Status)
	}
	if got.ID != runID {
		t.Fatalf("run id changed to %s", got.ID)
	}
}

// TestResumeRunFromStoppedAgentDisconnected (CRI-207): with the agent
// disconnected the resume cannot be delivered and the run stays stopped and
// resumable for a later attempt.
func TestResumeRunFromStoppedAgentDisconnected(t *testing.T) {
	ts := newTestStack(t)
	_, oClient, cClient := ts.startServer(t)
	overseerID, _ := mustRegister(t, oClient)
	run, err := oClient.CreateRun(context.Background(), connect.NewRequest(&pb.CreateRunRequest{CriteriaId: overseerID, WorkflowName: "wf"}))
	if err != nil {
		t.Fatal(err)
	}
	markRunStatus(t, ts, run.Msg.RunId, "stopped")

	_, err = cClient.ResumeRun(context.Background(), connect.NewRequest(&pb.ResumeRunRequest{RunId: run.Msg.RunId}))
	if connect.CodeOf(err) != connect.CodeFailedPrecondition {
		t.Fatalf("expected failed precondition when agent disconnected, got %v", err)
	}
	if got, err := ts.store.GetRun(context.Background(), run.Msg.RunId); err != nil {
		t.Fatal(err)
	} else if got.Status != "stopped" {
		t.Fatalf("status = %q, want stopped after failed delivery", got.Status)
	}
}

// TestControlWritesRejectStoppedRun (CRI-207): an operator cannot drive a
// parked run back into work via pause or prompt controls — resume is the only
// lever out of stopped.
func TestControlWritesRejectStoppedRun(t *testing.T) {
	ts := newTestStack(t)
	_, oClient, cClient := ts.startServer(t)
	overseerID, _ := mustRegister(t, oClient)
	run, err := oClient.CreateRun(context.Background(), connect.NewRequest(&pb.CreateRunRequest{CriteriaId: overseerID, WorkflowName: "wf"}))
	if err != nil {
		t.Fatal(err)
	}
	markRunStatus(t, ts, run.Msg.RunId, "stopped")
	ctrl := drainControlReady(t, oClient, overseerID)
	defer ctrl.Close()

	_, err = cClient.PauseRun(context.Background(), connect.NewRequest(&pb.PauseRunRequest{RunId: run.Msg.RunId}))
	if connect.CodeOf(err) != connect.CodeFailedPrecondition {
		t.Fatalf("PauseRun on stopped run: want failed precondition, got %v", err)
	}
	_, err = cClient.SendPrompt(context.Background(), connect.NewRequest(&pb.SendPromptRequest{RunId: run.Msg.RunId, Step: "main"}))
	if connect.CodeOf(err) != connect.CodeFailedPrecondition {
		t.Fatalf("SendPrompt on stopped run: want failed precondition, got %v", err)
	}
}

// TestReattachRun_StoppedReturnsCannotResume (CRI-207): a parked run answers
// reattach with can_resume=false so crash-recovery re-registration never
// starts a second executor for the same run; the operator resume path owns
// un-parking.
func TestReattachRun_StoppedReturnsCannotResume(t *testing.T) {
	ts := newTestStack(t)
	ctx := context.Background()

	reg, err := ts.criteria.Register(ctx, connect.NewRequest(&pb.RegisterRequest{Name: "o-stopped"}))
	if err != nil {
		t.Fatal(err)
	}
	overseerID := reg.Msg.CriteriaId

	now := time.Now().UTC()
	r := &store.Run{
		ID:           "run-stopped",
		OverseerID:   overseerID,
		WorkflowName: "wf",
		Status:       "stopped",
		CreatedAt:    now,
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
		t.Fatal("expected can_resume=false for stopped run")
	}
	if resp.Msg.Status != "stopped" {
		t.Fatalf("status=%q want stopped", resp.Msg.Status)
	}
}

// TestApplyRunStatus_StoppedRunStaysParked (CRI-207): run.failed /
// run.completed events from a stopping agent's teardown phase must not flip
// an operator-parked run out of stopped — the operator stop is authoritative,
// and only an explicit resume returns the run to running.
func TestApplyRunStatus_StoppedRunStaysParked(t *testing.T) {
	ts := newTestStack(t)
	ctx := context.Background()

	reg, err := ts.criteria.Register(ctx, connect.NewRequest(&pb.RegisterRequest{Name: "o-parked"}))
	if err != nil {
		t.Fatal(err)
	}
	overseerID := reg.Msg.CriteriaId

	now := time.Now().UTC()
	r := &store.Run{
		ID:            "run-parked",
		OverseerID:    overseerID,
		WorkflowName:  "wf",
		Status:        "stopped",
		CreatedAt:     now,
		FailureReason: "",
	}
	if err := ts.store.CreateRun(ctx, r); err != nil {
		t.Fatal(err)
	}

	// RunFailed from the stopping engine's teardown must not terminalize.
	ts.criteria.applyRunStatus(ctx, criteria.NewEnvelope(r.ID, &pb.RunFailed{Reason: "cancelled: requested by operator"}))
	got, err := ts.store.GetRun(ctx, r.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != "stopped" {
		t.Fatalf("RunFailed flipped a stopped run: status = %q", got.Status)
	}
	if got.EndedAt != nil {
		t.Fatalf("RunFailed stamped ended_at on a parked run: %v", got.EndedAt)
	}

	// RunCompleted must not terminalize either.
	ts.criteria.applyRunStatus(ctx, criteria.NewEnvelope(r.ID, &pb.RunCompleted{Success: true}))
	got, err = ts.store.GetRun(ctx, r.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != "stopped" {
		t.Fatalf("RunCompleted flipped a stopped run: status = %q", got.Status)
	}
	if got.EndedAt != nil {
		t.Fatalf("RunCompleted stamped ended_at on a parked run: %v", got.EndedAt)
	}
}

// TestApplyRunStatus_ResumeRestoresTerminalHandling (CRI-207 sanity): after
// the operator resumes a parked run, ordinary agent events once again apply —
// RunCompleted after resume still terminalizes the run.
func TestApplyRunStatus_ResumeRestoresTerminalHandling(t *testing.T) {
	ts := newTestStack(t)
	ctx := context.Background()

	reg, err := ts.criteria.Register(ctx, connect.NewRequest(&pb.RegisterRequest{Name: "o-resumed"}))
	if err != nil {
		t.Fatal(err)
	}
	overseerID := reg.Msg.CriteriaId

	now := time.Now().UTC()
	r := &store.Run{
		ID:           "run-resumed",
		OverseerID:   overseerID,
		WorkflowName: "wf",
		Status:       "stopped",
		CreatedAt:    now,
	}
	if err := ts.store.CreateRun(ctx, r); err != nil {
		t.Fatal(err)
	}

	if err := ts.store.ClearRunStopped(ctx, r.ID); err != nil {
		t.Fatalf("resume: %v", err)
	}
	ts.criteria.applyRunStatus(ctx, criteria.NewEnvelope(r.ID, &pb.RunCompleted{Success: true}))
	got, err := ts.store.GetRun(ctx, r.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != "succeeded" {
		t.Fatalf("status = %q, want succeeded after resume", got.Status)
	}
	if got.EndedAt == nil {
		t.Fatal("ended_at not stamped after resume")
	}
}