package rpc

import (
	"context"
	"testing"
	"time"

	"connectrpc.com/connect"

	"github.com/brokenbots/castle/castle/internal/auth"
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

// expectCommand receives one control message within the given window and
// delegates validation to check.
func expectCommand(t *testing.T, ch <-chan *pb.ControlMessage, within time.Duration, check func(*pb.ControlMessage)) {
	t.Helper()
	select {
	case msg := <-ch:
		check(msg)
	case <-time.After(within):
		t.Fatal("timed out waiting for control message")
	}
}

// expectNoCommand fails if any control message arrives within the quiet
// window, and otherwise passes — used to prove a dispatch path stayed quiet.
func expectNoCommand(t *testing.T, ch <-chan *pb.ControlMessage, quiet time.Duration) {
	t.Helper()
	select {
	case msg := <-ch:
		t.Fatalf("unexpected control command: %T", msg.Command)
	case <-time.After(quiet):
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

// TestApplyRunStatus_StartedDoesNotUnStop (CRI-207 review R1a): a late or
// out-of-order run.started event — e.g. queued work the agent started right
// around the operator's stop — must not flip a parked run back to running or
// stamp started_at. Only an explicit ResumeRun returns a stopped run to
// running; the event itself stays pollable via the event log.
func TestApplyRunStatus_StartedDoesNotUnStop(t *testing.T) {
	ts := newTestStack(t)
	ctx := context.Background()

	reg, err := ts.criteria.Register(ctx, connect.NewRequest(&pb.RegisterRequest{Name: "o-started-guard"}))
	if err != nil {
		t.Fatal(err)
	}
	overseerID := reg.Msg.CriteriaId

	now := time.Now().UTC()
	r := &store.Run{
		ID:           "run-started-guard",
		OverseerID:   overseerID,
		WorkflowName: "wf",
		Status:       "stopped",
		CreatedAt:    now,
	}
	if err := ts.store.CreateRun(ctx, r); err != nil {
		t.Fatal(err)
	}

	ts.criteria.applyRunStatus(ctx, criteria.NewEnvelope(r.ID, &pb.RunStarted{WorkflowName: "wf", InitialStep: "step-1"}))

	got, err := ts.store.GetRun(ctx, r.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != "stopped" {
		t.Fatalf("run.started flipped a parked run: status = %q", got.Status)
	}
	if got.StartedAt != nil {
		t.Fatalf("run.started stamped started_at on a parked run: %v", got.StartedAt)
	}
	if got.CurrentStep != "" {
		t.Fatalf("run.started advanced a parked run's step: %q", got.CurrentStep)
	}
}

// TestStopRun_NeverAssignedRunNotParked (CRI-207 review R1b): a run whose
// assignment is still queued has no attached agent, so no RunCancel can be
// delivered — the stop is rejected and the run stays pending with its
// assignment untouched. A queued, never-dispatched run can never surface as
// a parked-but-executable stranded record this way. (Ownership binds to the
// run's overseeing agent; a queued run has none, so this test drives the RPC
// with the unauthenticated in-process caller that the ownership check
// admits.)
func TestStopRun_NeverAssignedRunNotParked(t *testing.T) {
	ts := newTestStack(t)
	ownerCtx := auth.WithCallerCriteriaID(context.Background(), "owner-1")

	resp, err := ts.server.SubmitWorkflowAssignment(ownerCtx, connect.NewRequest(&pb.SubmitWorkflowAssignmentRequest{
		WorkflowName:   "wf",
		WorkflowSource: "hcl",
		IdempotencyKey: "key-1",
		Labels:         map[string]string{"env": "prod"},
	}))
	if err != nil {
		t.Fatalf("submit assignment: %v", err)
	}

	run, err := ts.store.GetRun(context.Background(), resp.Msg.RunId)
	if err != nil {
		t.Fatal(err)
	}
	if run.OverseerID != "" {
		t.Fatalf("queued run unexpectedly assigned: %q", run.OverseerID)
	}

	_, err = ts.server.StopRun(context.Background(), connect.NewRequest(&pb.StopRunRequest{RunId: resp.Msg.RunId}))
	if connect.CodeOf(err) != connect.CodeFailedPrecondition {
		t.Fatalf("expected FailedPrecondition stopping a never-assigned run, got %v", err)
	}

	run, err = ts.store.GetRun(context.Background(), resp.Msg.RunId)
	if err != nil {
		t.Fatal(err)
	}
	if run.Status != "pending" {
		t.Fatalf("rejected stop parked the run: status = %q", run.Status)
	}
	a, err := ts.store.GetWorkflowAssignmentByRunID(context.Background(), resp.Msg.RunId)
	if err != nil {
		t.Fatal(err)
	}
	if a.State != store.WorkflowAssignmentStateQueued {
		t.Fatalf("rejected stop left the assignment %q, want queued", a.State)
	}
}

// TestStoppedRunNotRedeliveredByDispatch (CRI-207 review R1b/R1c): a run
// stopped after its assignment was leased but before the agent accepted it
// keeps holding that lease — a dispatch fire must not deliver the parked
// run's work again, and the operator resume hands execution back to the same
// agent on the same run id instead of leaving a running record with no
// lease. Queued, never-assigned work is covered by
// TestStopRun_NeverAssignedRunNotParked here and the lease-scan tests in the
// store package.
func TestStoppedRunNotRedeliveredByDispatch(t *testing.T) {
	ts := newTestStack(t)
	ctx := context.Background()

	agentID, _ := registerAgent(t, ts, "agent-1", map[string]string{"env": "prod"})
	ch, err := ts.controls.Register(agentID)
	if err != nil {
		t.Fatalf("register control channel: %v", err)
	}
	defer ts.controls.Unregister(agentID, ch)

	// Submit real work so dispatch delivers the assignment and binds the run
	// to the agent (the lease sets overseer_id). The agent has not accepted
	// yet: the run is leased-pending, never started.
	ownerCtx := auth.WithCallerCriteriaID(ctx, "owner-1")
	submitResp, err := ts.server.SubmitWorkflowAssignment(ownerCtx, connect.NewRequest(&pb.SubmitWorkflowAssignmentRequest{
		WorkflowName:   "wf",
		WorkflowSource: "hcl",
		IdempotencyKey: "key-1",
		Labels:         map[string]string{"env": "prod"},
	}))
	if err != nil {
		t.Fatalf("submit assignment: %v", err)
	}
	runID := submitResp.Msg.RunId

	expectCommand(t, ch, 2*time.Second, func(msg *pb.ControlMessage) {
		wa := msg.GetWorkflowAssignment()
		if wa == nil || wa.RunId != runID {
			t.Fatalf("expected assignment for %s, got %T", runID, msg.Command)
		}
	})
	run, err := ts.store.GetRun(ctx, runID)
	if err != nil {
		t.Fatal(err)
	}
	if run.OverseerID != agentID {
		t.Fatalf("leased run attached to %q, want %q", run.OverseerID, agentID)
	}

	// Operator stop: reaches the connected agent and parks the run while the
	// assignment stays leased to the same agent — the engine-side teardown
	// and any later resume both key on the retained lease.
	runCaller := auth.WithCallerCriteriaID(ctx, agentID)
	if _, err := ts.server.StopRun(runCaller, connect.NewRequest(&pb.StopRunRequest{RunId: runID})); err != nil {
		t.Fatalf("stop leased run: %v", err)
	}
	expectCommand(t, ch, 2*time.Second, func(msg *pb.ControlMessage) {
		cancel := msg.GetRunCancel()
		if cancel == nil || cancel.RunId != runID {
			t.Fatalf("expected RunCancel for %s, got %T", runID, msg.Command)
		}
	})
	if run, err := ts.store.GetRun(ctx, runID); err != nil {
		t.Fatal(err)
	} else if run.Status != "stopped" {
		t.Fatalf("status = %q, want stopped after stop", run.Status)
	}

	// A dispatch fire must not deliver the parked run's work: the redelivery
	// scan re-sends only pending runs' leases, and the lease scan only picks
	// executable queued work.
	ts.criteria.dispatchForAgent(ctx, agentID)
	expectNoCommand(t, ch, 300*time.Millisecond)
	if a, err := ts.store.GetWorkflowAssignmentByRunID(ctx, runID); err != nil {
		t.Fatal(err)
	} else if a.State != store.WorkflowAssignmentStateLeased {
		t.Fatalf("parked run's assignment %q, want leased", a.State)
	}
	if run, err := ts.store.GetRun(ctx, runID); err != nil || run.Status != "stopped" {
		t.Fatalf("parked run disturbed by dispatch: status %v err %v", run, err)
	}

	// Operator resume: the resume control is delivered and the run returns
	// to RUNNING on the same run id; the resume dispatch trigger does not
	// double-deliver the lease the agent already holds.
	if _, err := ts.server.ResumeRun(runCaller, connect.NewRequest(&pb.ResumeRunRequest{RunId: runID})); err != nil {
		t.Fatalf("resume parked run: %v", err)
	}
	expectCommand(t, ch, 2*time.Second, func(msg *pb.ControlMessage) {
		resume := msg.GetResumeRun()
		if resume == nil || resume.RunId != runID || resume.Signal != "" {
			t.Fatalf("expected empty-signal RunResume for %s, got %T", runID, msg.Command)
		}
	})
	expectNoCommand(t, ch, 300*time.Millisecond)
	run, err = ts.store.GetRun(ctx, runID)
	if err != nil {
		t.Fatal(err)
	}
	if run.Status != "running" || run.ID != runID {
		t.Fatalf("resumed run: status=%q id=%s, want running on same id", run.Status, run.ID)
	}
	if run.StartedAt != nil {
		t.Fatalf("resume faked a start: started_at=%v", run.StartedAt)
	}

	// The restored run participates in ordinary execution: its own start
	// event stamps started_at and stays running (the stopped guard only
	// protects parked runs).
	ts.criteria.applyRunStatus(ctx, criteria.NewEnvelope(runID, &pb.RunStarted{WorkflowName: "wf", InitialStep: "step-1"}))
	run, err = ts.store.GetRun(ctx, runID)
	if err != nil {
		t.Fatal(err)
	}
	if run.Status != "running" || run.StartedAt == nil {
		t.Fatalf("resumed run did not execute: status=%q started_at=%v", run.Status, run.StartedAt)
	}
}
