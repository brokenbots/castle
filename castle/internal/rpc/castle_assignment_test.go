package rpc

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/brokenbots/castle/castle/internal/auth"
	"github.com/brokenbots/castle/castle/internal/store"
	pb "github.com/brokenbots/criteria/sdk/pb/criteria/v1"
)

func TestSubmitWorkflowAssignment_RequiresAuthentication(t *testing.T) {
	ts := newTestStack(t)
	ctx := context.Background()

	_, err := ts.server.SubmitWorkflowAssignment(ctx, connect.NewRequest(&pb.SubmitWorkflowAssignmentRequest{
		WorkflowName:   "wf",
		WorkflowSource: "hcl",
		IdempotencyKey: "key-1",
	}))
	if err == nil {
		t.Fatalf("expected error for unauthenticated call")
	}
	if connect.CodeOf(err) != connect.CodeUnauthenticated {
		t.Fatalf("expected unauthenticated, got %v", err)
	}
}

func TestSubmitWorkflowAssignment_CreatesAssignmentAndRun(t *testing.T) {
	ts := newTestStack(t)
	ctx := auth.WithCallerCriteriaID(context.Background(), "owner-1")

	resp, err := ts.server.SubmitWorkflowAssignment(ctx, connect.NewRequest(&pb.SubmitWorkflowAssignmentRequest{
		WorkflowName:   "wf",
		WorkflowSource: "hcl",
		IdempotencyKey: "key-1",
		Labels:         map[string]string{"env": "prod"},
	}))
	if err != nil {
		t.Fatalf("submit assignment: %v", err)
	}
	if resp.Msg.RunId == "" {
		t.Fatalf("expected run id in response")
	}
	if resp.Msg.State != pb.WorkflowAssignmentState_WORKFLOW_ASSIGNMENT_STATE_QUEUED {
		t.Fatalf("expected queued state, got %v", resp.Msg.State)
	}

	// The queued run should have no overseer.
	run, err := ts.store.GetRun(ctx, resp.Msg.RunId)
	if err != nil {
		t.Fatalf("get run: %v", err)
	}
	if run.OverseerID != "" {
		t.Fatalf("expected queued run to have no overseer, got %s", run.OverseerID)
	}
}

func TestSubmitWorkflowAssignment_Idempotent(t *testing.T) {
	ts := newTestStack(t)
	ctx := auth.WithCallerCriteriaID(context.Background(), "owner-1")

	req := &pb.SubmitWorkflowAssignmentRequest{
		WorkflowName:   "wf",
		WorkflowSource: "hcl",
		IdempotencyKey: "idem-key",
	}
	first, err := ts.server.SubmitWorkflowAssignment(ctx, connect.NewRequest(req))
	if err != nil {
		t.Fatalf("first submit: %v", err)
	}

	second, err := ts.server.SubmitWorkflowAssignment(ctx, connect.NewRequest(req))
	if err != nil {
		t.Fatalf("second submit: %v", err)
	}
	if second.Msg.RunId != first.Msg.RunId {
		t.Fatalf("expected same run id, got %s want %s", second.Msg.RunId, first.Msg.RunId)
	}

	runs, err := ts.store.ListRuns(ctx, "", "")
	if err != nil {
		t.Fatalf("list runs: %v", err)
	}
	if len(runs) != 1 {
		t.Fatalf("expected exactly one run, got %d", len(runs))
	}
}

func TestSubmitWorkflowAssignment_IdempotencyScopedByOwner(t *testing.T) {
	ts := newTestStack(t)
	ctx := context.Background()

	req := &pb.SubmitWorkflowAssignmentRequest{
		WorkflowName:   "wf",
		WorkflowSource: "hcl",
		IdempotencyKey: "shared-key",
	}
	owner1 := auth.WithCallerCriteriaID(ctx, "owner-a")
	owner2 := auth.WithCallerCriteriaID(ctx, "owner-b")

	r1, err := ts.server.SubmitWorkflowAssignment(owner1, connect.NewRequest(req))
	if err != nil {
		t.Fatalf("owner1 submit: %v", err)
	}
	r2, err := ts.server.SubmitWorkflowAssignment(owner2, connect.NewRequest(req))
	if err != nil {
		t.Fatalf("owner2 submit: %v", err)
	}
	if r1.Msg.RunId == r2.Msg.RunId {
		t.Fatalf("expected different runs for different owners")
	}
}

func TestGetAssignmentDisposition_RequiresOwnership(t *testing.T) {
	ts := newTestStack(t)
	ctx := context.Background()
	owner1 := auth.WithCallerCriteriaID(ctx, "owner-a")
	owner2 := auth.WithCallerCriteriaID(ctx, "owner-b")

	resp, err := ts.server.SubmitWorkflowAssignment(owner1, connect.NewRequest(&pb.SubmitWorkflowAssignmentRequest{
		WorkflowName:   "wf",
		WorkflowSource: "hcl",
		IdempotencyKey: "key-1",
	}))
	if err != nil {
		t.Fatalf("submit: %v", err)
	}

	// Owner of the assignment can read disposition.
	disp, err := ts.server.GetAssignmentDisposition(owner1, connect.NewRequest(&pb.GetAssignmentDispositionRequest{RunId: resp.Msg.RunId}))
	if err != nil {
		t.Fatalf("owner1 get disposition: %v", err)
	}
	if disp.Msg.State != pb.WorkflowAssignmentState_WORKFLOW_ASSIGNMENT_STATE_QUEUED {
		t.Fatalf("expected queued state, got %v", disp.Msg.State)
	}

	// A different caller must be rejected.
	_, err = ts.server.GetAssignmentDisposition(owner2, connect.NewRequest(
		&pb.GetAssignmentDispositionRequest{RunId: resp.Msg.RunId}))
	if err == nil {
		t.Fatalf("expected error for non-owner disposition read")
	}
	if connect.CodeOf(err) != connect.CodePermissionDenied {
		t.Fatalf("expected permission denied, got %v", err)
	}
}

func TestGetAssignmentDisposition_TerminalState(t *testing.T) {
	ts := newTestStack(t)
	ctx := auth.WithCallerCriteriaID(context.Background(), "owner-1")

	resp, err := ts.server.SubmitWorkflowAssignment(ctx, connect.NewRequest(
		&pb.SubmitWorkflowAssignmentRequest{
			WorkflowName:   "wf",
			WorkflowSource: "hcl",
			IdempotencyKey: "key-1",
		}))
	if err != nil {
		t.Fatalf("submit: %v", err)
	}

	if err := ts.store.MarkWorkflowAssignmentTerminal(context.Background(), resp.Msg.RunId, "rejected"); err != nil {
		t.Fatalf("mark terminal: %v", err)
	}

	disp, err := ts.server.GetAssignmentDisposition(ctx, connect.NewRequest(
		&pb.GetAssignmentDispositionRequest{RunId: resp.Msg.RunId}))
	if err != nil {
		t.Fatalf("get disposition: %v", err)
	}
	if disp.Msg.State != pb.WorkflowAssignmentState_WORKFLOW_ASSIGNMENT_STATE_TERMINAL {
		t.Fatalf("expected terminal state, got %v", disp.Msg.State)
	}
}

func TestGetAssignmentDisposition_NotFound(t *testing.T) {
	ts := newTestStack(t)
	ctx := auth.WithCallerCriteriaID(context.Background(), "owner-1")

	_, err := ts.server.GetAssignmentDisposition(ctx, connect.NewRequest(
		&pb.GetAssignmentDispositionRequest{RunId: "no-such-run"}))
	if err == nil {
		t.Fatalf("expected error for missing run")
	}
	if connect.CodeOf(err) != connect.CodeNotFound {
		t.Fatalf("expected not found, got %v", err)
	}
}

func TestSubmitWorkflowAssignment_DispatchesToEligibleConnectedAgent(t *testing.T) {
	ts := newTestStack(t)
	ctx := context.Background()

	// Register an agent with matching labels and subscribe to its Control stream.
	criteriaID, _ := registerAgent(t, ts, "agent-1", map[string]string{"gpu": "true"})
	ch, err := ts.controls.Register(criteriaID)
	if err != nil {
		t.Fatalf("register control channel: %v", err)
	}
	defer ts.controls.Unregister(criteriaID, ch)

	// Submit work that requires the agent's label.
	ownerCtx := auth.WithCallerCriteriaID(ctx, "owner-1")
	resp, err := ts.server.SubmitWorkflowAssignment(ownerCtx, connect.NewRequest(
		&pb.SubmitWorkflowAssignmentRequest{
			WorkflowName:   "wf",
			WorkflowSource: "hcl",
			IdempotencyKey: "key-1",
			Labels:         map[string]string{"gpu": "true"},
		}))
	if err != nil {
		t.Fatalf("submit: %v", err)
	}

	// Wait for the async dispatch to deliver the assignment via Control.
	select {
	case msg := <-ch:
		wa := msg.GetWorkflowAssignment()
		if wa == nil {
			t.Fatalf("expected WorkflowAssignment control message, got %v", msg)
		}
		if wa.RunId != resp.Msg.RunId {
			t.Fatalf("expected run id %s, got %s", resp.Msg.RunId, wa.RunId)
		}
	case <-time.After(2 * time.Second):
		t.Fatalf("timed out waiting for control assignment dispatch")
	}

	// The assignment should now be leased to the agent.
	a, err := ts.store.GetWorkflowAssignmentByRunID(ctx, resp.Msg.RunId)
	if err != nil {
		t.Fatalf("get assignment by run id: %v", err)
	}
	if a.State != store.WorkflowAssignmentStateLeased {
		t.Fatalf("expected leased state, got %s", a.State)
	}
	if a.LeasedCriteriaID != criteriaID {
		t.Fatalf("expected leased to %s, got %s", criteriaID, a.LeasedCriteriaID)
	}
}

// TestSubmitWorkflowAssignment_ConcurrentSubmissionsNoBusy reproduces Shape D
// from CRI-78: two matching agents are connected and two workflow assignments
// are submitted concurrently. Before the fix the second submit frequently
// returned SQLITE_BUSY; after the fix both submits must succeed, create
// distinct runs, and be dispatched to the two agents.
func TestSubmitWorkflowAssignment_ConcurrentSubmissionsNoBusy(t *testing.T) {
	ts := newTestStack(t)
	ctx := context.Background()
	ownerCtx := auth.WithCallerCriteriaID(ctx, "owner-1")

	// Register two online agents with matching labels.
	agentA, _ := registerAgent(t, ts, "agent-a", map[string]string{"env": "prod"})
	chA, err := ts.controls.Register(agentA)
	if err != nil {
		t.Fatalf("register control channel for agent a: %v", err)
	}
	defer ts.controls.Unregister(agentA, chA)

	agentB, _ := registerAgent(t, ts, "agent-b", map[string]string{"env": "prod"})
	chB, err := ts.controls.Register(agentB)
	if err != nil {
		t.Fatalf("register control channel for agent b: %v", err)
	}
	defer ts.controls.Unregister(agentB, chB)

	// Submit two assignments concurrently from the same owner.
	var wg sync.WaitGroup
	var respA, respB *pb.SubmitWorkflowAssignmentResponse
	var errA, errB error

	wg.Add(1)
	go func() {
		defer wg.Done()
		req := connect.NewRequest(&pb.SubmitWorkflowAssignmentRequest{
			WorkflowName:   "valid-alpha",
			WorkflowSource: "hcl",
			IdempotencyKey: "valid-alpha",
			Labels:         map[string]string{"env": "prod"},
		})
		r, err := ts.server.SubmitWorkflowAssignment(ownerCtx, req)
		if err != nil {
			errA = err
			return
		}
		respA = r.Msg
	}()

	wg.Add(1)
	go func() {
		defer wg.Done()
		req := connect.NewRequest(&pb.SubmitWorkflowAssignmentRequest{
			WorkflowName:   "valid-beta",
			WorkflowSource: "hcl",
			IdempotencyKey: "valid-beta",
			Labels:         map[string]string{"env": "prod"},
		})
		r, err := ts.server.SubmitWorkflowAssignment(ownerCtx, req)
		if err != nil {
			errB = err
			return
		}
		respB = r.Msg
	}()

	wg.Wait()

	if isBusyError(errA) {
		t.Fatalf("valid-alpha submit returned busy error: %v", errA)
	}
	if isBusyError(errB) {
		t.Fatalf("valid-beta submit returned busy error: %v", errB)
	}
	if errA != nil {
		t.Fatalf("valid-alpha submit: %v", errA)
	}
	if errB != nil {
		t.Fatalf("valid-beta submit: %v", errB)
	}
	if respA == nil || respB == nil {
		t.Fatal("expected both submissions to return a response")
	}
	if respA.RunId == "" || respB.RunId == "" {
		t.Fatalf("expected both runs to have run ids, got %q and %q", respA.RunId, respB.RunId)
	}
	if respA.RunId == respB.RunId {
		t.Fatalf("expected distinct run ids, got %s", respA.RunId)
	}

	// Wait for async dispatch to deliver both assignments via the agents'
	// Control channels. One assignment may be dispatched to either agent.
	received := make(map[string]string, 2)
	deadline := time.After(3 * time.Second)
	for len(received) < 2 {
		select {
		case msg := <-chA:
			if wa := msg.GetWorkflowAssignment(); wa != nil {
				received[wa.RunId] = agentA
			}
		case msg := <-chB:
			if wa := msg.GetWorkflowAssignment(); wa != nil {
				received[wa.RunId] = agentB
			}
		case <-deadline:
			t.Fatalf("timed out waiting for both assignments to dispatch; received=%v", received)
		}
	}

	if received[respA.RunId] == "" {
		t.Fatalf("assignment for run %s was not dispatched", respA.RunId)
	}
	if received[respB.RunId] == "" {
		t.Fatalf("assignment for run %s was not dispatched", respB.RunId)
	}
}

// TestSubmitWorkflowAssignment_DispatchesOldestQueuedAssignmentOnNewSubmit verifies
// the fix for a stranding bug: when a new submission triggers dispatch, the
// atomic lease may return an older queued assignment rather than the one just
// submitted. That assignment must still be delivered to the leasing agent.
func TestSubmitWorkflowAssignment_DispatchesOldestQueuedAssignmentOnNewSubmit(t *testing.T) {
	ts := newTestStack(t)
	ctx := context.Background()

	// Start a real HTTP server with auth so we can exercise the Control stream.
	_, oClient, cClient := ts.startServer(t,
		connect.WithInterceptors(auth.NewInterceptor(ts.store, false, auth.WithAnonRegister())),
	)

	// Register an owner that will submit work, and an agent that will lease it.
	ownerReg, err := oClient.Register(ctx, connect.NewRequest(&pb.RegisterRequest{Name: "owner-1"}))
	if err != nil {
		t.Fatalf("register owner: %v", err)
	}
	agentReg, err := oClient.Register(ctx, connect.NewRequest(&pb.RegisterRequest{Name: "agent-1", Labels: map[string]string{"gpu": "true"}}))
	if err != nil {
		t.Fatalf("register agent: %v", err)
	}
	agentID := agentReg.Msg.CriteriaId

	submit := func(key, wf string) *pb.SubmitWorkflowAssignmentResponse {
		t.Helper()
		req := connect.NewRequest(&pb.SubmitWorkflowAssignmentRequest{
			WorkflowName:   wf,
			WorkflowSource: "hcl",
			IdempotencyKey: key,
			Labels:         map[string]string{"gpu": "true"},
		})
		req.Header().Set("Authorization", "Bearer "+ownerReg.Msg.Token)
		resp, err := cClient.SubmitWorkflowAssignment(ctx, req)
		if err != nil {
			t.Fatalf("submit %s: %v", key, err)
		}
		return resp.Msg
	}

	// Queue two assignments before any agent is connected.
	a1 := submit("key-1", "wf-1")
	a2 := submit("key-2", "wf-2")

	// Connect the agent. Control sends ControlReady, then dispatchForAgent leases A1.
	ctrlReq := connect.NewRequest(&pb.ControlSubscribeRequest{CriteriaId: agentID})
	ctrlReq.Header().Set("Authorization", "Bearer "+agentReg.Msg.Token)
	ctrl, err := oClient.Control(ctx, ctrlReq)
	if err != nil {
		t.Fatalf("control: %v", err)
	}
	t.Cleanup(func() { _ = ctrl.Close() })

	receive := func() *pb.ControlMessage {
		t.Helper()
		done := make(chan struct{})
		var received bool
		go func() {
			received = ctrl.Receive()
			close(done)
		}()
		select {
		case <-done:
		case <-time.After(2 * time.Second):
			t.Fatalf("timed out waiting for control message")
		}
		if !received {
			t.Fatalf("expected control message, err=%v", ctrl.Err())
		}
		return ctrl.Msg()
	}

	expectWorkflowAssignment := func(wantRunID string) {
		t.Helper()
		msg := receive()
		wa := msg.GetWorkflowAssignment()
		if wa == nil {
			t.Fatalf("expected WorkflowAssignment, got %T", msg.Command)
		}
		if wa.RunId != wantRunID {
			t.Fatalf("expected run id %s, got %s", wantRunID, wa.RunId)
		}
	}

	msg := receive()
	if _, ok := msg.Command.(*pb.ControlMessage_ControlReady); !ok {
		t.Fatalf("expected ControlReady, got %T", msg.Command)
	}
	expectWorkflowAssignment(a1.RunId)

	// Accept A1 so the agent becomes eligible for the next assignment.
	// One agent executes assignments sequentially (CRI-73).
	submitEvents := oClient.SubmitEvents(ctx)
	submitEvents.RequestHeader().Set("Authorization", "Bearer "+agentReg.Msg.Token)
	if err := submitEvents.Send(&pb.Envelope{
		SchemaVersion: 1,
		RunId:         a1.RunId,
		CorrelationId: "accept-a1",
		Ts:            timestamppb.Now(),
		Payload:       &pb.Envelope_RunStarted{RunStarted: &pb.RunStarted{WorkflowName: "wf-1", InitialStep: "s1"}},
	}); err != nil {
		t.Fatalf("send RunStarted: %v", err)
	}
	ack, err := submitEvents.Receive()
	if err != nil {
		t.Fatalf("receive ack: %v", err)
	}
	if ack == nil || ack.RunId != a1.RunId {
		t.Fatalf("unexpected ack: %+v", ack)
	}

	// After A1 starts, Castle should dispatch A2 before anything else.
	expectWorkflowAssignment(a2.RunId)

	// Submitting a third assignment while the agent still holds A2's unstarted
	// lease must not dispatch A3 (sequential execution).
	a3 := submit("key-3", "wf-3")
	expectNoControlMessage := func() {
		t.Helper()
		done := make(chan struct{})
		var received bool
		go func() {
			received = ctrl.Receive()
			close(done)
		}()
		select {
		case <-done:
			if received {
				t.Fatalf("expected no control message while A2 unstarted, got %T", ctrl.Msg().Command)
			}
		case <-time.After(300 * time.Millisecond):
		}
	}
	expectNoControlMessage()

	// Confirm A2 actually transitioned to leased for the agent.
	assignment, err := ts.store.GetWorkflowAssignmentByRunID(ctx, a2.RunId)
	if err != nil {
		t.Fatalf("get assignment: %v", err)
	}
	if assignment.State != store.WorkflowAssignmentStateLeased {
		t.Fatalf("expected A2 leased, got %s", assignment.State)
	}
	if assignment.LeasedCriteriaID != agentID {
		t.Fatalf("expected A2 leased to %s, got %s", agentID, assignment.LeasedCriteriaID)
	}

	// Confirm A3 stayed queued because the agent has not accepted A2 yet.
	a3Stored, err := ts.store.GetWorkflowAssignmentByRunID(ctx, a3.RunId)
	if err != nil {
		t.Fatalf("get A3: %v", err)
	}
	if a3Stored.State != store.WorkflowAssignmentStateQueued {
		t.Fatalf("expected A3 queued, got %s", a3Stored.State)
	}
}

func registerAgent(t *testing.T, ts *testStack, name string, labels map[string]string) (string, string) {
	t.Helper()
	resp, err := ts.criteria.Register(context.Background(), connect.NewRequest(&pb.RegisterRequest{Name: name, Labels: labels}))
	if err != nil {
		t.Fatalf("register agent: %v", err)
	}
	return resp.Msg.CriteriaId, resp.Msg.Token
}

// isBusyError reports whether err is a SQLite writer-lock conflict that must
// never be returned to callers of SubmitWorkflowAssignment.
func isBusyError(err error) bool {
	if err == nil {
		return false
	}
	s := err.Error()
	return strings.Contains(s, "SQLITE_BUSY") ||
		strings.Contains(s, "database is locked") ||
		strings.Contains(s, "517")
}
