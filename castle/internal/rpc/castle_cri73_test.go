package rpc

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"connectrpc.com/connect"
	"golang.org/x/net/http2"
	"golang.org/x/net/http2/h2c"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/brokenbots/castle/castle/internal/auth"
	"github.com/brokenbots/castle/castle/internal/hub"
	"github.com/brokenbots/castle/castle/internal/store"
	"github.com/brokenbots/castle/castle/internal/store/sqlite"
	pb "github.com/brokenbots/criteria/sdk/pb/criteria/v1"
	criteriav1connect "github.com/brokenbots/criteria/sdk/pb/criteria/v1/criteriav1connect"
)

// TestCRI73_SingleAgentSequential verifies that one agent receives the next
// queued assignment only after it has emitted RunStarted for the current one.
func TestCRI73_SingleAgentSequential(t *testing.T) {
	ts := newTestStack(t)
	ctx := context.Background()
	_, oClient, sClient := ts.startServer(t,
		connect.WithInterceptors(auth.NewInterceptor(ts.store, false, auth.WithAnonRegister())),
	)

	ownerReg, err := oClient.Register(ctx, connect.NewRequest(&pb.RegisterRequest{Name: "owner"}))
	if err != nil {
		t.Fatalf("register owner: %v", err)
	}
	agentReg, err := oClient.Register(ctx, connect.NewRequest(&pb.RegisterRequest{Name: "agent", Labels: map[string]string{"gpu": "true"}}))
	if err != nil {
		t.Fatalf("register agent: %v", err)
	}
	agentID, agentToken := agentReg.Msg.CriteriaId, agentReg.Msg.Token

	submit := func(key, wf string) *pb.SubmitWorkflowAssignmentResponse {
		t.Helper()
		req := connect.NewRequest(&pb.SubmitWorkflowAssignmentRequest{
			WorkflowName:   wf,
			WorkflowSource: "hcl",
			IdempotencyKey: key,
			Labels:         map[string]string{"gpu": "true"},
		})
		req.Header().Set("Authorization", "Bearer "+ownerReg.Msg.Token)
		resp, err := sClient.SubmitWorkflowAssignment(ctx, req)
		if err != nil {
			t.Fatalf("submit %s: %v", key, err)
		}
		return resp.Msg
	}

	a1 := submit("seq-1", "wf-1")
	a2 := submit("seq-2", "wf-2")

	ctrl := openControl(t, oClient, agentID, agentToken)
	defer ctrl.Close()
	expectControlReady(t, ctrl)
	expectWorkflowAssignment(t, ctrl, a1.RunId)

	submitRunStarted(t, oClient, agentToken, a1.RunId, "wf-1", "s1")
	expectWorkflowAssignment(t, ctrl, a2.RunId)

	// A2 is leased; A1 is running and still owned by the agent.
	run, err := ts.store.GetRun(ctx, a1.RunId)
	if err != nil {
		t.Fatalf("get run: %v", err)
	}
	if run.Status != "running" {
		t.Fatalf("expected A1 running, got %s", run.Status)
	}
	if run.OverseerID != agentID {
		t.Fatalf("expected A1 owned by %s, got %s", agentID, run.OverseerID)
	}
}

// TestCRI73_TwoAgentsConcurrentDistinctLabels verifies that two agents with
// different matching labels each receive their own distinct assignment and do
// not steal each other's work.
func TestCRI73_TwoAgentsConcurrentDistinctLabels(t *testing.T) {
	ts := newTestStack(t)
	ctx := context.Background()
	_, oClient, sClient := ts.startServer(t,
		connect.WithInterceptors(auth.NewInterceptor(ts.store, false, auth.WithAnonRegister())),
	)

	ownerReg, err := oClient.Register(ctx, connect.NewRequest(&pb.RegisterRequest{Name: "owner"}))
	if err != nil {
		t.Fatalf("register owner: %v", err)
	}
	agentProd, err := oClient.Register(ctx, connect.NewRequest(&pb.RegisterRequest{Name: "agent-prod", Labels: map[string]string{"env": "prod"}}))
	if err != nil {
		t.Fatalf("register prod agent: %v", err)
	}
	agentDev, err := oClient.Register(ctx, connect.NewRequest(&pb.RegisterRequest{Name: "agent-dev", Labels: map[string]string{"env": "dev"}}))
	if err != nil {
		t.Fatalf("register dev agent: %v", err)
	}

	submit := func(key, wf, env string) *pb.SubmitWorkflowAssignmentResponse {
		t.Helper()
		req := connect.NewRequest(&pb.SubmitWorkflowAssignmentRequest{
			WorkflowName:   wf,
			WorkflowSource: "hcl",
			IdempotencyKey: key,
			Labels:         map[string]string{"env": env},
		})
		req.Header().Set("Authorization", "Bearer "+ownerReg.Msg.Token)
		resp, err := sClient.SubmitWorkflowAssignment(ctx, req)
		if err != nil {
			t.Fatalf("submit %s: %v", key, err)
		}
		return resp.Msg
	}

	aProd := submit("prod-1", "wf-prod", "prod")
	aDev := submit("dev-1", "wf-dev", "dev")

	ctrlProd := openControl(t, oClient, agentProd.Msg.CriteriaId, agentProd.Msg.Token)
	defer ctrlProd.Close()
	ctrlDev := openControl(t, oClient, agentDev.Msg.CriteriaId, agentDev.Msg.Token)
	defer ctrlDev.Close()

	expectControlReady(t, ctrlProd)
	expectControlReady(t, ctrlDev)

	expectWorkflowAssignment(t, ctrlProd, aProd.RunId)
	expectWorkflowAssignment(t, ctrlDev, aDev.RunId)

	// Ensure neither assignment leaked to the wrong agent.
	prodA, err := ts.store.GetWorkflowAssignmentByRunID(ctx, aProd.RunId)
	if err != nil {
		t.Fatalf("get prod assignment: %v", err)
	}
	if prodA.LeasedCriteriaID != agentProd.Msg.CriteriaId {
		t.Fatalf("prod assignment leased to %s, want %s", prodA.LeasedCriteriaID, agentProd.Msg.CriteriaId)
	}
	devA, err := ts.store.GetWorkflowAssignmentByRunID(ctx, aDev.RunId)
	if err != nil {
		t.Fatalf("get dev assignment: %v", err)
	}
	if devA.LeasedCriteriaID != agentDev.Msg.CriteriaId {
		t.Fatalf("dev assignment leased to %s, want %s", devA.LeasedCriteriaID, agentDev.Msg.CriteriaId)
	}
}

// TestCRI73_DisconnectBeforeRunStartedRequeues verifies that an unstarted
// lease is returned to the queue after the lease expires and is then dispatched
// again to a reconnecting agent.
func TestCRI73_DisconnectBeforeRunStartedRequeues(t *testing.T) {
	ts := newTestStack(t)
	ctx := context.Background()
	_, oClient, sClient := ts.startServer(t,
		connect.WithInterceptors(auth.NewInterceptor(ts.store, false, auth.WithAnonRegister())),
	)

	// Use a short lease so the test does not wait for the production expiry loop.
	ts.criteria.SetAssignmentLeaseDuration(200 * time.Millisecond)
	ts.server.SetAssignmentLeaseDuration(200 * time.Millisecond)

	ownerReg, err := oClient.Register(ctx, connect.NewRequest(&pb.RegisterRequest{Name: "owner"}))
	if err != nil {
		t.Fatalf("register owner: %v", err)
	}
	agentReg, err := oClient.Register(ctx, connect.NewRequest(&pb.RegisterRequest{Name: "agent", Labels: map[string]string{"gpu": "true"}}))
	if err != nil {
		t.Fatalf("register agent: %v", err)
	}
	agentID, agentToken := agentReg.Msg.CriteriaId, agentReg.Msg.Token

	a1 := submitAssignment(t, ctx, sClient, ownerReg.Msg.Token, "requeue-1", "wf-1", map[string]string{"gpu": "true"})

	ctrl := openControl(t, oClient, agentID, agentToken)
	expectControlReady(t, ctrl)
	expectWorkflowAssignment(t, ctrl, a1.RunId)

	// Disconnect before accepting the assignment.
	if err := ctrl.Close(); err != nil {
		t.Fatalf("close control stream: %v", err)
	}

	// Wait for the lease to expire, then explicitly run expiry/redispatch.
	time.Sleep(300 * time.Millisecond)
	if err := ts.server.ExpireLeasesNow(ctx); err != nil {
		t.Fatalf("expire leases: %v", err)
	}

	reconnect := openControl(t, oClient, agentID, agentToken)
	defer reconnect.Close()
	expectControlReady(t, reconnect)
	expectWorkflowAssignment(t, reconnect, a1.RunId)

	// The same assignment was requeued and then re-leased to the same agent.
	assignment, err := ts.store.GetWorkflowAssignmentByRunID(ctx, a1.RunId)
	if err != nil {
		t.Fatalf("get assignment: %v", err)
	}
	if assignment.State != store.WorkflowAssignmentStateLeased {
		t.Fatalf("expected requeued assignment leased again, got %s", assignment.State)
	}
	if assignment.LeasedCriteriaID != agentID {
		t.Fatalf("expected assignment leased to %s, got %s", agentID, assignment.LeasedCriteriaID)
	}
}

// TestCRI73_DisconnectAfterRunStartedReattaches verifies that a run which has
// already emitted RunStarted is not requeued or dispatched to another agent
// after disconnect. The original agent can reattach; another agent receives
// nothing.
func TestCRI73_DisconnectAfterRunStartedReattaches(t *testing.T) {
	ts := newTestStack(t)
	ctx := context.Background()
	_, oClient, sClient := ts.startServer(t,
		connect.WithInterceptors(auth.NewInterceptor(ts.store, false, auth.WithAnonRegister())),
	)

	ownerReg, err := oClient.Register(ctx, connect.NewRequest(&pb.RegisterRequest{Name: "owner"}))
	if err != nil {
		t.Fatalf("register owner: %v", err)
	}
	agentReg, err := oClient.Register(ctx, connect.NewRequest(&pb.RegisterRequest{Name: "agent", Labels: map[string]string{"gpu": "true"}}))
	if err != nil {
		t.Fatalf("register agent: %v", err)
	}
	agentID, agentToken := agentReg.Msg.CriteriaId, agentReg.Msg.Token

	a1 := submitAssignment(t, ctx, sClient, ownerReg.Msg.Token, "reattach-1", "wf-1", map[string]string{"gpu": "true"})

	ctrl := openControl(t, oClient, agentID, agentToken)
	expectControlReady(t, ctrl)
	expectWorkflowAssignment(t, ctrl, a1.RunId)

	// Accept the assignment.
	submitRunStarted(t, oClient, agentToken, a1.RunId, "wf-1", "s1")

	// Disconnect the original agent.
	if err := ctrl.Close(); err != nil {
		t.Fatalf("close control stream: %v", err)
	}

	// A second eligible agent must not receive the running assignment.
	otherReg, err := oClient.Register(ctx, connect.NewRequest(&pb.RegisterRequest{Name: "other", Labels: map[string]string{"gpu": "true"}}))
	if err != nil {
		t.Fatalf("register other: %v", err)
	}
	otherCtrl := openControl(t, oClient, otherReg.Msg.CriteriaId, otherReg.Msg.Token)
	defer otherCtrl.Close()
	expectControlReady(t, otherCtrl)
	expectNoWorkflowAssignment(t, otherCtrl, 300*time.Millisecond)

	// The run is still running and still owned by the original agent.
	run, err := ts.store.GetRun(ctx, a1.RunId)
	if err != nil {
		t.Fatalf("get run: %v", err)
	}
	if run.Status != "running" {
		t.Fatalf("expected run running after disconnect, got %s", run.Status)
	}
	if run.OverseerID != agentID {
		t.Fatalf("expected run still owned by %s, got %s", agentID, run.OverseerID)
	}

	// The original agent can reattach to the same run.
	reattachReq := connect.NewRequest(&pb.ReattachRunRequest{RunId: a1.RunId, CriteriaId: agentID})
	reattachReq.Header().Set("Authorization", "Bearer "+agentToken)
	resp, err := oClient.ReattachRun(ctx, reattachReq)
	if err != nil {
		t.Fatalf("reattach: %v", err)
	}
	if !resp.Msg.CanResume {
		t.Fatal("expected can_resume=true for running run")
	}
}

// TestCRI73_CastleRestartRestoresLeases verifies that a Castle restart
// redelivers in-flight unstarted leases to the reconnecting agent and leaves
// still-queued assignments in the queue.
func TestCRI73_CastleRestartRestoresLeases(t *testing.T) {
	ctx := context.Background()

	// Use a stable database path so the new server can recover the same state.
	dbPath := filepath.Join(t.TempDir(), "castle.db")
	st, err := sqlite.Open(dbPath)
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	h := hub.New()
	controls := NewControlRegistry()
	criteriaServer := NewCriteriaServer(st, h, discardLogger(), controls)
	serverServer := NewServerServer(st, h, discardLogger(), controls)

	mux := http.NewServeMux()
	oPath, oHandler := criteriav1connect.NewCriteriaServiceHandler(criteriaServer,
		connect.WithInterceptors(auth.NewInterceptor(st, false, auth.WithAnonRegister())),
	)
	cPath, cHandler := criteriav1connect.NewServerServiceHandler(serverServer,
		connect.WithInterceptors(auth.NewInterceptor(st, false, auth.WithAnonRegister())),
	)
	mux.Handle(oPath, oHandler)
	mux.Handle(cPath, cHandler)

	srv := httptest.NewUnstartedServer(h2c.NewHandler(mux, &http2.Server{}))
	srv.Start()
	defer srv.Close()

	client := h2cClient()
	oClient := criteriav1connect.NewCriteriaServiceClient(client, srv.URL)
	sClient := criteriav1connect.NewServerServiceClient(client, srv.URL)

	ownerReg, err := oClient.Register(ctx, connect.NewRequest(&pb.RegisterRequest{Name: "owner"}))
	if err != nil {
		t.Fatalf("register owner: %v", err)
	}
	agentReg, err := oClient.Register(ctx, connect.NewRequest(&pb.RegisterRequest{Name: "agent", Labels: map[string]string{"gpu": "true"}}))
	if err != nil {
		t.Fatalf("register agent: %v", err)
	}
	agentID, agentToken := agentReg.Msg.CriteriaId, agentReg.Msg.Token

	a1 := submitAssignment(t, ctx, sClient, ownerReg.Msg.Token, "restart-1", "wf-1", map[string]string{"gpu": "true"})
	a2 := submitAssignment(t, ctx, sClient, ownerReg.Msg.Token, "restart-2", "wf-2", map[string]string{"gpu": "true"})

	ctrl := openControl(t, oClient, agentID, agentToken)
	expectControlReady(t, ctrl)
	expectWorkflowAssignment(t, ctrl, a1.RunId)

	// Do not accept A1. Simulate a Castle restart by tearing down the server and
	// creating a new one over the same store.
	if err := ctrl.Close(); err != nil {
		t.Fatalf("close control stream: %v", err)
	}
	srv.Close()
	st.Close()

	st2, err := sqlite.Open(dbPath)
	if err != nil {
		t.Fatalf("reopen db: %v", err)
	}
	defer st2.Close()
	h2 := hub.New()
	controls2 := NewControlRegistry()
	criteriaServer2 := NewCriteriaServer(st2, h2, discardLogger(), controls2)
	serverServer2 := NewServerServer(st2, h2, discardLogger(), controls2)

	mux2 := http.NewServeMux()
	oPath2, oHandler2 := criteriav1connect.NewCriteriaServiceHandler(criteriaServer2,
		connect.WithInterceptors(auth.NewInterceptor(st2, false, auth.WithAnonRegister())),
	)
	cPath2, cHandler2 := criteriav1connect.NewServerServiceHandler(serverServer2,
		connect.WithInterceptors(auth.NewInterceptor(st2, false, auth.WithAnonRegister())),
	)
	mux2.Handle(oPath2, oHandler2)
	mux2.Handle(cPath2, cHandler2)

	srv2 := httptest.NewUnstartedServer(h2c.NewHandler(mux2, &http2.Server{}))
	srv2.Start()
	defer srv2.Close()

	oClient2 := criteriav1connect.NewCriteriaServiceClient(client, srv2.URL)

	ctrl2 := openControl(t, oClient2, agentID, agentToken)
	defer ctrl2.Close()
	expectControlReady(t, ctrl2)

	// The unstarted lease for A1 should be redelivered without re-leasing.
	expectWorkflowAssignment(t, ctrl2, a1.RunId)

	// A2 is still queued because A1 is still an unstarted lease.
	assignment, err := st2.GetWorkflowAssignmentByRunID(ctx, a2.RunId)
	if err != nil {
		t.Fatalf("get A2: %v", err)
	}
	if assignment.State != store.WorkflowAssignmentStateQueued {
		t.Fatalf("expected A2 queued after restart, got %s", assignment.State)
	}
}

func openControl(t *testing.T, client criteriav1connect.CriteriaServiceClient, criteriaID, token string) *connect.ServerStreamForClient[pb.ControlMessage] {
	t.Helper()
	ctx := context.Background()
	req := connect.NewRequest(&pb.ControlSubscribeRequest{CriteriaId: criteriaID})
	req.Header().Set("Authorization", "Bearer "+token)
	ctrl, err := client.Control(ctx, req)
	if err != nil {
		t.Fatalf("control stream: %v", err)
	}
	return ctrl
}

func receiveControl(t *testing.T, ctrl *connect.ServerStreamForClient[pb.ControlMessage], timeout time.Duration) *pb.ControlMessage {
	t.Helper()
	done := make(chan struct{})
	var received bool
	go func() {
		received = ctrl.Receive()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(timeout):
		t.Fatalf("timed out waiting for control message")
	}
	if !received {
		t.Fatalf("expected control message, err=%v", ctrl.Err())
	}
	return ctrl.Msg()
}

func expectControlReady(t *testing.T, ctrl *connect.ServerStreamForClient[pb.ControlMessage]) {
	t.Helper()
	msg := receiveControl(t, ctrl, 2*time.Second)
	if _, ok := msg.Command.(*pb.ControlMessage_ControlReady); !ok {
		t.Fatalf("expected ControlReady, got %T", msg.Command)
	}
}

func expectWorkflowAssignment(t *testing.T, ctrl *connect.ServerStreamForClient[pb.ControlMessage], wantRunID string) {
	t.Helper()
	msg := receiveControl(t, ctrl, 2*time.Second)
	wa := msg.GetWorkflowAssignment()
	if wa == nil {
		t.Fatalf("expected WorkflowAssignment, got %T", msg.Command)
	}
	if wa.RunId != wantRunID {
		t.Fatalf("expected run id %s, got %s", wantRunID, wa.RunId)
	}
}

func expectNoWorkflowAssignment(t *testing.T, ctrl *connect.ServerStreamForClient[pb.ControlMessage], timeout time.Duration) {
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
			if wa := ctrl.Msg().GetWorkflowAssignment(); wa != nil {
				t.Fatalf("expected no WorkflowAssignment, got run id %s", wa.RunId)
			}
		}
	case <-time.After(timeout):
	}
}

func submitAssignment(t *testing.T, ctx context.Context, client criteriav1connect.ServerServiceClient, ownerToken, key, wf string, labels map[string]string) *pb.SubmitWorkflowAssignmentResponse {
	t.Helper()
	req := connect.NewRequest(&pb.SubmitWorkflowAssignmentRequest{
		WorkflowName:   wf,
		WorkflowSource: "hcl",
		IdempotencyKey: key,
		Labels:         labels,
	})
	req.Header().Set("Authorization", "Bearer "+ownerToken)
	resp, err := client.SubmitWorkflowAssignment(ctx, req)
	if err != nil {
		t.Fatalf("submit %s: %v", key, err)
	}
	return resp.Msg
}

func submitRunStarted(t *testing.T, client criteriav1connect.CriteriaServiceClient, token, runID, workflowName, initialStep string) {
	t.Helper()
	stream := client.SubmitEvents(context.Background())
	stream.RequestHeader().Set("Authorization", "Bearer "+token)
	if err := stream.Send(&pb.Envelope{
		SchemaVersion: 1,
		RunId:         runID,
		CorrelationId: "runstarted-" + runID,
		Ts:            timestamppb.Now(),
		Payload:       &pb.Envelope_RunStarted{RunStarted: &pb.RunStarted{WorkflowName: workflowName, InitialStep: initialStep}},
	}); err != nil {
		t.Fatalf("send RunStarted: %v", err)
	}
	ack, err := stream.Receive()
	if err != nil {
		t.Fatalf("receive ack: %v", err)
	}
	if ack == nil || ack.RunId != runID {
		t.Fatalf("unexpected ack: %+v", ack)
	}
}

func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}
