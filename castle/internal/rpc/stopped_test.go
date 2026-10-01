package rpc

import (
	"context"
	"testing"
	"time"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/types/known/timestamppb"

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

// stampRunStarted seeds a run's start instant (test scaffold for the
// already-started parked shape: UpdateRun coalesces started_at, so an unset
// stamp is written and an existing one is preserved).
func stampRunStarted(t *testing.T, ts *testStack, runID string) {
	t.Helper()
	r, err := ts.store.GetRun(context.Background(), runID)
	if err != nil {
		t.Fatal(err)
	}
	if r.StartedAt == nil {
		now := time.Now().UTC()
		r.StartedAt = &now
	}
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
// parked, already-started run back to running on the same run id; the resume
// control is delivered with an empty signal because a stopped run never has
// a pending signal. (A parked run whose delivery never landed is covered by
// TestResumeRunRedeliversUnstartedStoppedWork: resume re-queues it into the
// leasable pending bucket instead of faking a start.)
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
	// Parked mid-execution: started_at is already stamped, so resume returns
	// the run to running directly.
	stampRunStarted(t, ts, runID)

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
// disconnected the resume cannot be delivered and the call fails with the
// precondition. The run stays recoverable anyway: a never-started parked run
// has already been re-queued into the leasable pending bucket (reconnect
// redelivery and lease expiry take over from there), so the failed delivery
// cannot strand a running record whose work nobody will ever deliver.
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
	got, err := ts.store.GetRun(context.Background(), run.Msg.RunId)
	if err != nil {
		t.Fatal(err)
	}
	// The re-queue is committed before the delivery attempt, so the failed
	// resume leaves the never-started run in the leasable bucket with
	// started_at NULL — recoverable by reconnect redelivery or lease expiry,
	// not parked as a fake running record.
	if got.Status != "pending" {
		t.Fatalf("status = %q, want pending after failed delivery (re-queued for redelivery)", got.Status)
	}
	if got.StartedAt != nil {
		t.Fatalf("started_at stamped on a never-started run: %v", got.StartedAt)
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

// TestResumeRunRedeliversUnstartedStoppedWork (CRI-207 review R1): a run
// stopped after its assignment was leased but before the agent accepted it
// keeps holding that lease with started_at NULL. A dispatch fire must not
// deliver the parked run's work out of band of the operator, and the resume
// must not stamp a fake start: it re-queues the run into the leasable pending
// bucket, after which the next dispatch fire for the same agent redelivers
// the held assignment and the ordinary protocol round-trip (RunAssignment →
// RunStarted → RunCompleted) carries the run to a terminal state with no
// synthetic event injection. Queued, never-assigned work is covered by
// TestStopRun_NeverAssignedRunNotParked here and the lease-scan tests in the
// store package; the already-started parked shape is covered by
// TestResumeRunFromStopped.
func TestResumeRunRedeliversUnstartedStoppedWork(t *testing.T) {
	ts := newTestStack(t)
	ctx := context.Background()
	_, oClient, _ := ts.startServer(t,
		connect.WithInterceptors(auth.NewInterceptor(ts.store, false, auth.WithAnonRegister())),
	)

	agentID, agentToken := registerAgent(t, ts, "agent-1", map[string]string{"env": "prod"})
	ch, err := ts.controls.Register(agentID)
	if err != nil {
		t.Fatalf("register control channel: %v", err)
	}
	defer ts.controls.Unregister(agentID, ch)

	// Submit real work so dispatch leases the assignment and binds the run to
	// the agent. This fake agent never accepts the delivery — no RunStarted
	// ever arrives — so the run is a leased assignment with started_at NULL,
	// the shape whose work the pre-R1 resume path would silently drop.
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
	if run.OverseerID != agentID || run.StartedAt != nil {
		t.Fatalf("leased run attached to %q with started_at %v, want %q unstarted", run.OverseerID, run.StartedAt, agentID)
	}

	// Operator stop: reaches the connected agent and parks the run while the
	// assignment stays leased to the same agent. A dispatch fire must not
	// deliver the parked run's work: the redelivery scan re-sends only
	// pending runs' leases and the lease scan only picks executable queued
	// work — only the operator resume returns the work to the agent.
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
	ts.criteria.dispatchForAgent(ctx, agentID)
	expectNoCommand(t, ch, 300*time.Millisecond)
	if a, err := ts.store.GetWorkflowAssignmentByRunID(ctx, runID); err != nil {
		t.Fatal(err)
	} else if a.State != store.WorkflowAssignmentStateLeased || a.LeasedCriteriaID != agentID {
		t.Fatalf("parked run's lease disturbed: state=%q leased=%q", a.State, a.LeasedCriteriaID)
	}

	// Operator resume: the resume control is delivered, but the run does not
	// return to running with started_at NULL — resume never fakes a start.
	// It re-queues the run into the leasable pending bucket with started_at
	// still NULL, and the resume itself delivers nothing beyond the signal.
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
	if run.Status != "pending" || run.StartedAt != nil {
		t.Fatalf("resumed run: status=%q started_at=%v, want pending with started_at NULL (leasable again)", run.Status, run.StartedAt)
	}
	if a, err := ts.store.GetWorkflowAssignmentByRunID(ctx, runID); err != nil {
		t.Fatal(err)
	} else if a.State != store.WorkflowAssignmentStateLeased || a.LeasedCriteriaID != agentID {
		t.Fatalf("resume disturbed the held lease: state=%q leased=%q", a.State, a.LeasedCriteriaID)
	}

	// The held lease is redeliverable: the next dispatch fire for the agent —
	// the trigger the control stream open / reconnect invokes — re-delivers
	// the same assignment without a competing re-lease.
	ts.criteria.dispatchForAgent(ctx, agentID)
	expectCommand(t, ch, 2*time.Second, func(msg *pb.ControlMessage) {
		wa := msg.GetWorkflowAssignment()
		if wa == nil || wa.RunId != runID {
			t.Fatalf("expected redelivered assignment for %s, got %T", runID, msg.Command)
		}
	})

	// End-to-end: over the real agent protocol, the agent accepts the
	// redelivered assignment — RunStarted stamps started_at and the run
	// executes to a terminal state with no synthetic event injection.
	submitEvents := oClient.SubmitEvents(ctx)
	submitEvents.RequestHeader().Set("Authorization", "Bearer "+agentToken)
	if err := submitEvents.Send(&pb.Envelope{
		SchemaVersion: 1,
		RunId:         runID,
		CorrelationId: "accept-redelivery",
		Ts:            timestamppb.Now(),
		Payload:       &pb.Envelope_RunStarted{RunStarted: &pb.RunStarted{WorkflowName: "wf", InitialStep: "step-1"}},
	}); err != nil {
		t.Fatalf("send RunStarted: %v", err)
	}
	ack, err := submitEvents.Receive()
	if err != nil {
		t.Fatalf("receive RunStarted ack: %v", err)
	}
	if ack.RunId != runID {
		t.Fatalf("unexpected ack run id: %s", ack.RunId)
	}
	run, err = ts.store.GetRun(ctx, runID)
	if err != nil {
		t.Fatal(err)
	}
	if run.Status != "running" || run.StartedAt == nil {
		t.Fatalf("run after acceptance: status=%q started_at=%v, want running with started_at stamped", run.Status, run.StartedAt)
	}
	if err := submitEvents.Send(&pb.Envelope{
		SchemaVersion: 1,
		RunId:         runID,
		CorrelationId: "complete-redelivery",
		Ts:            timestamppb.Now(),
		Payload:       &pb.Envelope_RunCompleted{RunCompleted: &pb.RunCompleted{Success: true}},
	}); err != nil {
		t.Fatalf("send RunCompleted: %v", err)
	}
	if _, err := submitEvents.Receive(); err != nil {
		t.Fatalf("receive RunCompleted ack: %v", err)
	}
	run, err = ts.store.GetRun(ctx, runID)
	if err != nil {
		t.Fatal(err)
	}
	if run.Status != "succeeded" || run.EndedAt == nil {
		t.Fatalf("run after completion: status=%q ended_at=%v, want succeeded terminal", run.Status, run.EndedAt)
	}
}
