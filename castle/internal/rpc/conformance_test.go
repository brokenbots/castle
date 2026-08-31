//go:build conformance

package rpc

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"connectrpc.com/connect"
	"golang.org/x/net/http2"
	"golang.org/x/net/http2/h2c"

	"github.com/brokenbots/castle/castle/internal/auth"
	"github.com/brokenbots/castle/castle/internal/hub"
	"github.com/brokenbots/castle/castle/internal/store"
	"github.com/brokenbots/castle/castle/internal/store/sqlite"
	criteria "github.com/brokenbots/criteria/sdk"
	"github.com/brokenbots/criteria/sdk/conformance"
	pb "github.com/brokenbots/criteria/sdk/pb/criteria/v1"                // import-lint:allow castle service bindings (W08: move to castle-proto)
	"github.com/brokenbots/criteria/sdk/pb/criteria/v1/criteriav1connect" // import-lint:allow castle service bindings (W08: move to castle-proto)
)

// TestCastleConformance runs the full SDK conformance suite against Castle plus
// Castle-specific scenario tests for assignment, bootstrap, ownership,
// restart, and negative authorization. The suite is gated behind the
// "conformance" build tag so it stays out of the default `make test` lane.
// Use `make test-conformance` to run it.
func TestCastleConformance(t *testing.T) {
	t.Run("SDK", func(t *testing.T) {
		conformance.Run(t, &castleSubject{})
	})
	t.Run("Bootstrap", testBootstrap)
	t.Run("Assignment", testAssignment)
	t.Run("Ownership", testOwnership)
	t.Run("Restart", testRestart)
	t.Run("NegativeAuth", testNegativeAuth)
}

// castleSubject implements conformance.Subject backed by a real Castle server
// (fully wired: SQLite store, event hub, auth interceptor, control registry).
// It is analogous to the per-test stack used in auth_negative_test.go and
// other Castle tests.
type castleSubject struct {
	mu             sync.Mutex
	ts             *testStack
	bootstrapToken string
}

// SetUp starts a fresh isolated Castle server with the standard auth interceptor
// (no anon-register). RegisterAgent uses the direct-store path to bypass the
// wire-level bootstrap requirement, so anon-register is not needed. When
// bootstrapToken is non-empty, Register requires a matching X-Server-Bootstrap
// header. The returned HTTP client is h2c-aware so bidi streams work over
// plain HTTP/2 (as in other Castle test helpers).
func (s *castleSubject) SetUp(t *testing.T) (string, *http.Client, func()) {
	t.Helper()
	ts := newTestStack(t)
	opts := []connect.HandlerOption{
		connect.WithInterceptors(auth.NewInterceptor(ts.store, false)),
	}
	if s.bootstrapToken != "" {
		opts = []connect.HandlerOption{
			connect.WithInterceptors(auth.NewInterceptor(ts.store, false, auth.WithBootstrapToken(s.bootstrapToken))),
		}
	}
	srv, _, _ := ts.startServer(t, opts...)
	s.mu.Lock()
	s.ts = ts
	s.mu.Unlock()
	// srv.Close is already registered as t.Cleanup inside startServer.
	return srv.URL, h2cClient(), func() {}
}

// RegisterAgent inserts an overseer record directly into the SQLite store,
// bypassing the Register RPC and its bootstrap requirement. This is the
// standard test-setup path: it does not exercise the Register wire contract
// (that is the Bootstrap scenario tests' job).
func (s *castleSubject) RegisterAgent(t *testing.T, name, token string) string {
	t.Helper()
	return s.RegisterAgentWithLabels(t, name, token, nil)
}

// RegisterAgentWithLabels is like RegisterAgent but attaches the supplied
// agent labels. Used by assignment scenarios that need eligible agents.
func (s *castleSubject) RegisterAgentWithLabels(t *testing.T, name, token string, labels map[string]string) string {
	t.Helper()
	s.mu.Lock()
	ts := s.ts
	s.mu.Unlock()
	if ts == nil {
		t.Fatal("castleSubject.RegisterAgentWithLabels called before SetUp")
	}
	id := "overseer-" + name
	now := time.Now().UTC()
	err := ts.store.CreateOverseer(context.Background(), &store.Overseer{
		ID:         id,
		Name:       name,
		TokenHash:  auth.HashToken(token),
		Status:     "online",
		Labels:     labels,
		CreatedAt:  now,
		LastSeenAt: now,
	})
	if err != nil {
		t.Fatalf("RegisterAgentWithLabels(%s): CreateOverseer: %v", name, err)
	}
	return id
}

// ListRunEvents retrieves events for runID via the ServerService ListRunEvents
// RPC. criteriaToken authenticates the caller. The conformance suite uses this
// to assert persistence without the conformance package importing ServerService.
func (s *castleSubject) ListRunEvents(t *testing.T, baseURL string, client *http.Client, overseerToken, runID string, sinceSeq uint64) []*criteria.Envelope {
	t.Helper()
	cClient := criteriav1connect.NewServerServiceClient(client, baseURL)
	req := connect.NewRequest(&pb.ListRunEventsRequest{
		RunId:    runID,
		SinceSeq: sinceSeq,
		Limit:    1000,
	})
	req.Header().Set("Authorization", "Bearer "+overseerToken)
	resp, err := cClient.ListRunEvents(context.Background(), req)
	if err != nil {
		t.Fatalf("ListRunEvents(run=%s): %v", runID, err)
	}
	return resp.Msg.Events
}

// StopRun sends a StopRun request via the ServerService on behalf of the run
// owner. Returns the RPC error so conformance tests can inspect the error code.
func (s *castleSubject) StopRun(t *testing.T, baseURL string, client *http.Client, ownerToken, runID string) error {
	t.Helper()
	cClient := criteriav1connect.NewServerServiceClient(client, baseURL)
	req := connect.NewRequest(&pb.StopRunRequest{RunId: runID})
	req.Header().Set("Authorization", "Bearer "+ownerToken)
	_, err := cClient.StopRun(context.Background(), req)
	return err
}

const conformanceBootstrapToken = "castle-conformance-bootstrap"

func testBootstrap(t *testing.T) {
	ctx := context.Background()

	t.Run("RegisterWithValidToken", func(t *testing.T) {
		_, _, oClient, _ := startConformanceServer(t, conformanceBootstrapToken)
		req := connect.NewRequest(&pb.RegisterRequest{Name: "bootstrapped-agent"})
		req.Header().Set("X-Server-Bootstrap", conformanceBootstrapToken)
		resp, err := oClient.Register(ctx, req)
		if err != nil {
			t.Fatalf("Register with valid bootstrap token: %v", err)
		}
		if resp.Msg.CriteriaId == "" || resp.Msg.Token == "" {
			t.Fatalf("Register returned empty id/token: %+v", resp.Msg)
		}
	})

	t.Run("RegisterWithWrongToken", func(t *testing.T) {
		_, _, oClient, _ := startConformanceServer(t, conformanceBootstrapToken)
		req := connect.NewRequest(&pb.RegisterRequest{Name: "unauthorized-agent"})
		req.Header().Set("X-Server-Bootstrap", "wrong-token")
		_, err := oClient.Register(ctx, req)
		assertCode(t, err, connect.CodeUnauthenticated)
	})

	t.Run("RegisterMissingHeaderWhenRequired", func(t *testing.T) {
		_, _, oClient, _ := startConformanceServer(t, conformanceBootstrapToken)
		req := connect.NewRequest(&pb.RegisterRequest{Name: "missing-bootstrap"})
		_, err := oClient.Register(ctx, req)
		assertCode(t, err, connect.CodeUnauthenticated)
	})

	t.Run("RegisterDisabledWithoutToken", func(t *testing.T) {
		_, _, oClient, _ := startConformanceServer(t, "")
		req := connect.NewRequest(&pb.RegisterRequest{Name: "no-bootstrap"})
		_, err := oClient.Register(ctx, req)
		assertCode(t, err, connect.CodeUnimplemented)
	})
}

func testAssignment(t *testing.T) {
	ctx := context.Background()
	leaseDuration := 100 * time.Millisecond

	t.Run("SubmitAndDispatchToEligibleAgent", func(t *testing.T) {
		_, _, oClient, sClient := startConformanceServerWithLease(t, conformanceBootstrapToken, leaseDuration)
		owner := registerWithBootstrap(t, oClient, "owner", nil)
		agent := registerWithBootstrap(t, oClient, "agent", map[string]string{"gpu": "true"})

		a := submitAssignment(t, ctx, sClient, owner.token, "dispatch-1", "wf-1", map[string]string{"gpu": "true"})
		if a.State != pb.WorkflowAssignmentState_WORKFLOW_ASSIGNMENT_STATE_QUEUED {
			t.Fatalf("expected initial state queued, got %v", a.State)
		}

		ctrl := openControl(t, oClient, agent.id, agent.token)
		defer ctrl.Close()
		expectControlReady(t, ctrl)
		expectWorkflowAssignment(t, ctrl, a.RunId)

		// Owner can read the leased disposition.
		dispReq := connect.NewRequest(&pb.GetAssignmentDispositionRequest{RunId: a.RunId})
		dispReq.Header().Set("Authorization", "Bearer "+owner.token)
		disp, err := sClient.GetAssignmentDisposition(ctx, dispReq)
		if err != nil {
			t.Fatalf("GetAssignmentDisposition: %v", err)
		}
		if disp.Msg.State != pb.WorkflowAssignmentState_WORKFLOW_ASSIGNMENT_STATE_LEASED {
			t.Fatalf("expected leased disposition, got %v", disp.Msg.State)
		}
		if disp.Msg.LeasedCriteriaId != agent.id {
			t.Fatalf("expected leased to %s, got %s", agent.id, disp.Msg.LeasedCriteriaId)
		}
	})

	t.Run("IdempotentSubmit", func(t *testing.T) {
		_, _, oClient, sClient := startConformanceServer(t, conformanceBootstrapToken)
		owner := registerWithBootstrap(t, oClient, "owner", nil)

		first := submitAssignment(t, ctx, sClient, owner.token, "idempotent-key", "wf-1", nil)
		second := submitAssignment(t, ctx, sClient, owner.token, "idempotent-key", "wf-2", nil)
		if first.RunId != second.RunId {
			t.Fatalf("idempotent submit returned different run ids: %s vs %s", first.RunId, second.RunId)
		}
		if first.State != pb.WorkflowAssignmentState_WORKFLOW_ASSIGNMENT_STATE_QUEUED {
			t.Fatalf("expected queued state, got %v", first.State)
		}
	})

	t.Run("LabelMismatchDoesNotDispatch", func(t *testing.T) {
		_, _, oClient, sClient := startConformanceServer(t, conformanceBootstrapToken)
		owner := registerWithBootstrap(t, oClient, "owner", nil)
		agent := registerWithBootstrap(t, oClient, "agent", map[string]string{"gpu": "false"})

		a := submitAssignment(t, ctx, sClient, owner.token, "mismatch-1", "wf-1", map[string]string{"gpu": "true"})

		ctrl := openControl(t, oClient, agent.id, agent.token)
		defer ctrl.Close()
		expectControlReady(t, ctrl)
		expectNoWorkflowAssignment(t, ctrl, 200*time.Millisecond)

		disp := getDisposition(t, sClient, owner.token, a.RunId)
		if disp.State != pb.WorkflowAssignmentState_WORKFLOW_ASSIGNMENT_STATE_QUEUED {
			t.Fatalf("expected queued after label mismatch, got %v", disp.State)
		}
	})

	t.Run("RequeueAfterDisconnect", func(t *testing.T) {
		ts, _, oClient, sClient := startConformanceServerWithLease(t, conformanceBootstrapToken, leaseDuration)
		owner := registerWithBootstrap(t, oClient, "owner", nil)
		agent := registerWithBootstrap(t, oClient, "agent", map[string]string{"gpu": "true"})

		a := submitAssignment(t, ctx, sClient, owner.token, "requeue-1", "wf-1", map[string]string{"gpu": "true"})

		ctrl := openControl(t, oClient, agent.id, agent.token)
		expectControlReady(t, ctrl)
		expectWorkflowAssignment(t, ctrl, a.RunId)
		if err := ctrl.Close(); err != nil {
			t.Fatalf("close control stream: %v", err)
		}

		// Force lease expiry and redispatch without a timing-only sleep.
		forceExpireAndDispatch(t, ts, ctx)

		reconnect := openControl(t, oClient, agent.id, agent.token)
		defer reconnect.Close()
		expectControlReady(t, reconnect)
		expectWorkflowAssignment(t, reconnect, a.RunId)

		assignment, err := ts.store.GetWorkflowAssignmentByRunID(ctx, a.RunId)
		if err != nil {
			t.Fatalf("get assignment: %v", err)
		}
		if assignment.State != store.WorkflowAssignmentStateLeased {
			t.Fatalf("expected leased after reconnect, got %s", assignment.State)
		}
		if assignment.LeasedCriteriaID != agent.id {
			t.Fatalf("expected re-leased to %s, got %s", agent.id, assignment.LeasedCriteriaID)
		}
	})
}

func testOwnership(t *testing.T) {
	ctx := context.Background()

	t.Run("ServerServiceCrossOwner", func(t *testing.T) {
		_, _, oClient, sClient := startConformanceServer(t, conformanceBootstrapToken)
		owner := registerWithBootstrap(t, oClient, "owner", nil)
		attacker := registerWithBootstrap(t, oClient, "attacker", nil)

		run := createRun(t, oClient, owner.id, owner.token)
		assignment := submitAssignment(t, ctx, sClient, owner.token, "owner-assignment", "wf-1", nil)

		cases := []struct {
			name string
			call func() error
		}{
			{
				name: "StopRun",
				call: func() error {
					req := connect.NewRequest(&pb.StopRunRequest{RunId: run.RunId})
					req.Header().Set("Authorization", "Bearer "+attacker.token)
					_, err := sClient.StopRun(ctx, req)
					return err
				},
			},
			{
				name: "PauseRun",
				call: func() error {
					req := connect.NewRequest(&pb.PauseRunRequest{RunId: run.RunId})
					req.Header().Set("Authorization", "Bearer "+attacker.token)
					_, err := sClient.PauseRun(ctx, req)
					return err
				},
			},
			{
				name: "ResumeRun",
				call: func() error {
					req := connect.NewRequest(&pb.ResumeRunRequest{RunId: run.RunId})
					req.Header().Set("Authorization", "Bearer "+attacker.token)
					_, err := sClient.ResumeRun(ctx, req)
					return err
				},
			},
			{
				name: "InspectRun",
				call: func() error {
					req := connect.NewRequest(&pb.InspectRunRequest{RunId: run.RunId})
					req.Header().Set("Authorization", "Bearer "+attacker.token)
					_, err := sClient.InspectRun(ctx, req)
					return err
				},
			},
			{
				name: "SendPrompt",
				call: func() error {
					req := connect.NewRequest(&pb.SendPromptRequest{RunId: run.RunId, Step: "s1"})
					req.Header().Set("Authorization", "Bearer "+attacker.token)
					_, err := sClient.SendPrompt(ctx, req)
					return err
				},
			},
			{
				name: "GetAssignmentDisposition",
				call: func() error {
					req := connect.NewRequest(&pb.GetAssignmentDispositionRequest{RunId: assignment.RunId})
					req.Header().Set("Authorization", "Bearer "+attacker.token)
					_, err := sClient.GetAssignmentDisposition(ctx, req)
					return err
				},
			},
			{
				name: "ReattachRun",
				call: func() error {
					req := connect.NewRequest(&pb.ReattachRunRequest{RunId: run.RunId, CriteriaId: attacker.id})
					req.Header().Set("Authorization", "Bearer "+attacker.token)
					_, err := oClient.ReattachRun(ctx, req)
					return err
				},
			},
		}
		for _, tc := range cases {
			t.Run(tc.name, func(t *testing.T) {
				assertCode(t, tc.call(), connect.CodePermissionDenied)
			})
		}
	})

	t.Run("ControlWithWrongToken", func(t *testing.T) {
		_, _, oClient, _ := startConformanceServer(t, conformanceBootstrapToken)
		agent := registerWithBootstrap(t, oClient, "agent", nil)
		req := connect.NewRequest(&pb.ControlSubscribeRequest{CriteriaId: agent.id})
		req.Header().Set("Authorization", "Bearer "+"not-the-token")
		ctrl, err := oClient.Control(ctx, req)
		if err != nil {
			t.Fatalf("open control stream: %v", err)
		}
		defer ctrl.Close()
		_ = ctrl.Receive()
		assertCode(t, ctrl.Err(), connect.CodeUnauthenticated)
	})
}

func testRestart(t *testing.T) {
	ctx := context.Background()
	dbPath := filepath.Join(t.TempDir(), "castle-restart.db")

	ts1, srv1, oClient1, _ := startPersistedServer(t, dbPath, conformanceBootstrapToken)
	defer srv1.Close()
	defer ts1.store.Close()

	_ = registerWithBootstrap(t, oClient1, "owner", nil)
	agent := registerWithBootstrap(t, oClient1, "agent", nil)

	run := createRun(t, oClient1, agent.id, agent.token)
	_ = submitEvent(t, oClient1, agent.token, criteria.NewEnvelope(run.RunId, &criteria.WaitEntered{Signal: "go"}))

	runRec, err := ts1.store.GetRun(ctx, run.RunId)
	if err != nil {
		t.Fatalf("get run before restart: %v", err)
	}
	if runRec.Status != "paused" || runRec.PendingSignal != "go" {
		t.Fatalf("expected paused with pending signal before restart, got status=%s signal=%s", runRec.Status, runRec.PendingSignal)
	}

	// Simulate Castle restart: close the first process and reopen on the same DB.
	srv1.Close()
	if err := ts1.store.Close(); err != nil {
		t.Fatalf("close first store: %v", err)
	}

	ts2, srv2, oClient2, _ := startPersistedServer(t, dbPath, conformanceBootstrapToken)
	defer srv2.Close()
	defer ts2.store.Close()

	// The agent can reattach and sees the pending signal.
	reattachReq := connect.NewRequest(&pb.ReattachRunRequest{RunId: run.RunId, CriteriaId: agent.id})
	reattachReq.Header().Set("Authorization", "Bearer "+agent.token)
	reattachResp, err := oClient2.ReattachRun(ctx, reattachReq)
	if err != nil {
		t.Fatalf("reattach after restart: %v", err)
	}
	if !reattachResp.Msg.CanResume {
		t.Fatal("expected can_resume=true after restart")
	}
	if reattachResp.Msg.PendingSignal != "go" {
		t.Fatalf("expected pending signal 'go', got %q", reattachResp.Msg.PendingSignal)
	}

	// Open a fresh control channel before resuming so the ResumeRun command is delivered.
	ctrl := openControl(t, oClient2, agent.id, agent.token)
	defer ctrl.Close()
	expectControlReady(t, ctrl)

	resumeReq := connect.NewRequest(&pb.ResumeRequest{RunId: run.RunId, Signal: "go"})
	resumeReq.Header().Set("Authorization", "Bearer "+agent.token)
	resumeResp, err := oClient2.Resume(ctx, resumeReq)
	if err != nil {
		t.Fatalf("resume after restart: %v", err)
	}
	if !resumeResp.Msg.Accepted {
		t.Fatalf("resume not accepted: %s", resumeResp.Msg.Reason)
	}

	msg := receiveControl(t, ctrl, 2*time.Second)
	resumeMsg := msg.GetResumeRun()
	if resumeMsg == nil {
		t.Fatalf("expected ResumeRun control, got %T", msg.Command)
	}
	if resumeMsg.RunId != run.RunId || resumeMsg.Signal != "go" {
		t.Fatalf("unexpected ResumeRun: %+v", resumeMsg)
	}

	runRec2, err := ts2.store.GetRun(ctx, run.RunId)
	if err != nil {
		t.Fatalf("get run after resume: %v", err)
	}
	if runRec2.Status != "running" || runRec2.PendingSignal != "" {
		t.Fatalf("expected running with no pending signal after resume, got status=%s signal=%s", runRec2.Status, runRec2.PendingSignal)
	}
}

func testNegativeAuth(t *testing.T) {
	ctx := context.Background()

	t.Run("Unauthenticated", func(t *testing.T) {
		_, _, oClient, sClient := startConformanceServer(t, conformanceBootstrapToken)

		cases := []struct {
			name string
			call func() error
		}{
			{
				name: "Register",
				call: func() error {
					_, err := oClient.Register(ctx, connect.NewRequest(&pb.RegisterRequest{Name: "x"}))
					return err
				},
			},
			{
				name: "CreateRun",
				call: func() error {
					_, err := oClient.CreateRun(ctx, connect.NewRequest(&pb.CreateRunRequest{CriteriaId: "x", WorkflowName: "wf"}))
					return err
				},
			},
			{
				name: "ReattachRun",
				call: func() error {
					_, err := oClient.ReattachRun(ctx, connect.NewRequest(&pb.ReattachRunRequest{RunId: "x"}))
					return err
				},
			},
			{
				name: "Heartbeat",
				call: func() error {
					_, err := oClient.Heartbeat(ctx, connect.NewRequest(&pb.HeartbeatRequest{}))
					return err
				},
			},
			{
				name: "Resume",
				call: func() error {
					_, err := oClient.Resume(ctx, connect.NewRequest(&pb.ResumeRequest{RunId: "x", Signal: "s"}))
					return err
				},
			},
			{
				name: "SubmitEvents",
				call: func() error {
					stream := oClient.SubmitEvents(ctx)
					err := stream.Send(criteria.NewEnvelope("x", &pb.StepLog{Step: "s", Stream: pb.LogStream_LOG_STREAM_STDOUT, Chunk: "c"}))
					if err == nil {
						_, err = stream.Receive()
					}
					return err
				},
			},
			{
				name: "Control",
				call: func() error {
					stream, err := oClient.Control(ctx, connect.NewRequest(&pb.ControlSubscribeRequest{CriteriaId: "x"}))
					if err != nil {
						return err
					}
					_ = stream.Receive()
					return stream.Err()
				},
			},
			{
				name: "ListAgents",
				call: func() error {
					_, err := sClient.ListAgents(ctx, connect.NewRequest(&pb.ListAgentsRequest{}))
					return err
				},
			},
			{
				name: "GetAgent",
				call: func() error {
					_, err := sClient.GetAgent(ctx, connect.NewRequest(&pb.GetAgentRequest{CriteriaId: "x"}))
					return err
				},
			},
			{
				name: "ListRuns",
				call: func() error {
					_, err := sClient.ListRuns(ctx, connect.NewRequest(&pb.ListRunsRequest{}))
					return err
				},
			},
			{
				name: "GetRun",
				call: func() error {
					_, err := sClient.GetRun(ctx, connect.NewRequest(&pb.GetRunRequest{RunId: "x"}))
					return err
				},
			},
			{
				name: "ListRunEvents",
				call: func() error {
					_, err := sClient.ListRunEvents(ctx, connect.NewRequest(&pb.ListRunEventsRequest{RunId: "x"}))
					return err
				},
			},
			{
				name: "WatchRun",
				call: func() error {
					stream, err := sClient.WatchRun(ctx, connect.NewRequest(&pb.WatchRunRequest{RunId: "x"}))
					if err != nil {
						return err
					}
					_ = stream.Receive()
					return stream.Err()
				},
			},
			{
				name: "StopRun",
				call: func() error {
					_, err := sClient.StopRun(ctx, connect.NewRequest(&pb.StopRunRequest{RunId: "x"}))
					return err
				},
			},
			{
				name: "PauseRun",
				call: func() error {
					_, err := sClient.PauseRun(ctx, connect.NewRequest(&pb.PauseRunRequest{RunId: "x"}))
					return err
				},
			},
			{
				name: "ResumeRun",
				call: func() error {
					_, err := sClient.ResumeRun(ctx, connect.NewRequest(&pb.ResumeRunRequest{RunId: "x"}))
					return err
				},
			},
			{
				name: "InspectRun",
				call: func() error {
					_, err := sClient.InspectRun(ctx, connect.NewRequest(&pb.InspectRunRequest{RunId: "x"}))
					return err
				},
			},
			{
				name: "SendPrompt",
				call: func() error {
					_, err := sClient.SendPrompt(ctx, connect.NewRequest(&pb.SendPromptRequest{RunId: "x", Step: "s"}))
					return err
				},
			},
			{
				name: "SubmitWorkflowAssignment",
				call: func() error {
					_, err := sClient.SubmitWorkflowAssignment(ctx, connect.NewRequest(&pb.SubmitWorkflowAssignmentRequest{WorkflowName: "wf", WorkflowSource: "hcl", IdempotencyKey: "k"}))
					return err
				},
			},
			{
				name: "GetAssignmentDisposition",
				call: func() error {
					_, err := sClient.GetAssignmentDisposition(ctx, connect.NewRequest(&pb.GetAssignmentDispositionRequest{RunId: "x"}))
					return err
				},
			},
		}
		for _, tc := range cases {
			t.Run(tc.name, func(t *testing.T) {
				assertCode(t, tc.call(), connect.CodeUnauthenticated)
			})
		}
	})

	t.Run("WrongToken", func(t *testing.T) {
		_, _, oClient, sClient := startConformanceServer(t, conformanceBootstrapToken)

		cases := []struct {
			name string
			call func() error
		}{
			{
				name: "CreateRun",
				call: func() error {
					req := connect.NewRequest(&pb.CreateRunRequest{CriteriaId: "x", WorkflowName: "wf"})
					req.Header().Set("Authorization", "Bearer "+"wrong-token")
					_, err := oClient.CreateRun(ctx, req)
					return err
				},
			},
			{
				name: "ListRunEvents",
				call: func() error {
					req := connect.NewRequest(&pb.ListRunEventsRequest{RunId: "x"})
					req.Header().Set("Authorization", "Bearer "+"wrong-token")
					_, err := sClient.ListRunEvents(ctx, req)
					return err
				},
			},
			{
				name: "Control",
				call: func() error {
					req := connect.NewRequest(&pb.ControlSubscribeRequest{CriteriaId: "x"})
					req.Header().Set("Authorization", "Bearer "+"wrong-token")
					stream, err := oClient.Control(ctx, req)
					if err != nil {
						return err
					}
					_ = stream.Receive()
					return stream.Err()
				},
			},
		}
		for _, tc := range cases {
			t.Run(tc.name, func(t *testing.T) {
				assertCode(t, tc.call(), connect.CodeUnauthenticated)
			})
		}
	})
}

// startConformanceServer builds a fresh in-process Castle server for
// scenario tests. If bootstrapToken is non-empty, Register requires a matching
// X-Server-Bootstrap header.
func startConformanceServer(t *testing.T, bootstrapToken string) (*testStack, string, criteriav1connect.CriteriaServiceClient, criteriav1connect.ServerServiceClient) {
	t.Helper()
	return startConformanceServerWithLease(t, bootstrapToken, defaultAssignmentLeaseDuration)
}

// startConformanceServerWithLease is like startConformanceServer but sets a
// short assignment lease duration so disconnect/requeue scenarios stay fast.
func startConformanceServerWithLease(t *testing.T, bootstrapToken string, leaseDuration time.Duration) (*testStack, string, criteriav1connect.CriteriaServiceClient, criteriav1connect.ServerServiceClient) {
	t.Helper()
	ts := newTestStack(t)
	opts := []connect.HandlerOption{
		connect.WithInterceptors(auth.NewInterceptor(ts.store, false)),
	}
	if bootstrapToken != "" {
		opts = []connect.HandlerOption{
			connect.WithInterceptors(auth.NewInterceptor(ts.store, false, auth.WithBootstrapToken(bootstrapToken))),
		}
	}
	srv, oClient, sClient := ts.startServer(t, opts...)
	ts.criteria.SetAssignmentLeaseDuration(leaseDuration)
	ts.server.SetAssignmentLeaseDuration(leaseDuration)
	_ = srv // t.Cleanup registered in startServer closes it.
	return ts, srv.URL, oClient, sClient
}

// startPersistedServer opens a store at dbPath and starts a Castle server over
// it. The caller is responsible for closing srv and st.
func startPersistedServer(t *testing.T, dbPath, bootstrapToken string) (*testStack, *httptest.Server, criteriav1connect.CriteriaServiceClient, criteriav1connect.ServerServiceClient) {
	t.Helper()
	st, err := sqlite.Open(dbPath)
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	h := hub.New()
	controls := NewControlRegistry()
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	criteriaSrv := NewCriteriaServer(st, h, log, controls)
	serverSrv := NewServerServer(st, h, log, controls)

	mux := http.NewServeMux()
	opts := []connect.HandlerOption{
		connect.WithInterceptors(auth.NewInterceptor(st, false, auth.WithBootstrapToken(bootstrapToken))),
	}
	oPath, oHandler := criteriav1connect.NewCriteriaServiceHandler(criteriaSrv, opts...)
	cPath, cHandler := criteriav1connect.NewServerServiceHandler(serverSrv, opts...)
	mux.Handle(oPath, oHandler)
	mux.Handle(cPath, cHandler)

	srv := httptest.NewUnstartedServer(h2c.NewHandler(mux, &http2.Server{}))
	srv.Start()
	client := h2cClient()
	return &testStack{store: st, hub: h, controls: controls, criteria: criteriaSrv, server: serverSrv},
		srv,
		criteriav1connect.NewCriteriaServiceClient(client, srv.URL),
		criteriav1connect.NewServerServiceClient(client, srv.URL)
}

// registerWithBootstrap creates an agent through the Register RPC using the
// configured bootstrap token.
func registerWithBootstrap(t *testing.T, oClient criteriav1connect.CriteriaServiceClient, name string, labels map[string]string) struct {
	id    string
	token string
} {
	t.Helper()
	req := connect.NewRequest(&pb.RegisterRequest{Name: name, Labels: labels})
	req.Header().Set("X-Server-Bootstrap", conformanceBootstrapToken)
	resp, err := oClient.Register(context.Background(), req)
	if err != nil {
		t.Fatalf("register %s: %v", name, err)
	}
	return struct {
		id    string
		token string
	}{id: resp.Msg.CriteriaId, token: resp.Msg.Token}
}

// createRun creates a run owned by the supplied agent.
func createRun(t *testing.T, oClient criteriav1connect.CriteriaServiceClient, criteriaID, token string) *pb.Run {
	t.Helper()
	req := connect.NewRequest(&pb.CreateRunRequest{CriteriaId: criteriaID, WorkflowName: "conformance-wf"})
	req.Header().Set("Authorization", "Bearer "+token)
	resp, err := oClient.CreateRun(context.Background(), req)
	if err != nil {
		t.Fatalf("create run: %v", err)
	}
	return resp.Msg
}

// submitEvent submits a single envelope and returns the ack.
func submitEvent(t *testing.T, oClient criteriav1connect.CriteriaServiceClient, token string, env *criteria.Envelope) *pb.Ack {
	t.Helper()
	stream := oClient.SubmitEvents(context.Background())
	stream.RequestHeader().Set("Authorization", "Bearer "+token)
	if err := stream.Send(env); err != nil {
		t.Fatalf("send event: %v", err)
	}
	ack, err := stream.Receive()
	if err != nil {
		t.Fatalf("receive ack: %v", err)
	}
	return ack
}

// getDisposition reads an assignment disposition as the owner.
func getDisposition(t *testing.T, sClient criteriav1connect.ServerServiceClient, ownerToken, runID string) *pb.GetAssignmentDispositionResponse {
	t.Helper()
	req := connect.NewRequest(&pb.GetAssignmentDispositionRequest{RunId: runID})
	req.Header().Set("Authorization", "Bearer "+ownerToken)
	resp, err := sClient.GetAssignmentDisposition(context.Background(), req)
	if err != nil {
		t.Fatalf("get disposition: %v", err)
	}
	return resp.Msg
}

// forceExpireAndDispatch advances the store clock and redispatches queued
// assignments. It avoids a timing-only sleep for lease expiry.
func forceExpireAndDispatch(t *testing.T, ts *testStack, ctx context.Context) {
	t.Helper()
	ids, err := ts.store.ExpireWorkflowAssignmentLeases(ctx, time.Now().UTC().Add(24*time.Hour))
	if err != nil {
		t.Fatalf("expire leases: %v", err)
	}
	for _, id := range ids {
		a, err := ts.store.GetWorkflowAssignment(ctx, id)
		if err != nil {
			continue
		}
		ts.server.dispatchAssignment(ctx, a)
	}
}

// assertCode fails the test if err does not map to the expected Connect code.
func assertCode(t *testing.T, err error, want connect.Code) {
	t.Helper()
	if err == nil {
		t.Fatalf("expected error code %v, got nil", want)
	}
	if got := connect.CodeOf(err); got != want {
		t.Fatalf("expected code %v, got %v (%v)", want, got, err)
	}
}
