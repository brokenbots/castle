package rpc

import (
	"context"
	"testing"
	"time"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/brokenbots/castle/castle/internal/auth"
	pb "github.com/brokenbots/criteria/sdk/pb/criteria/v1"                // import-lint:allow castle service bindings (W08: move to castle-proto)
	"github.com/brokenbots/criteria/sdk/pb/criteria/v1/criteriav1connect" // import-lint:allow castle service bindings (W08: move to castle-proto)
)

// consoleWireHarness starts the full HTTP stack with the auth interceptor in
// anon-register mode, registers two agents, mints a run owned by the first,
// and seeds the default console user + enables console login — mirroring what
// main.go does with CASTLE_CONSOLE_USER/CASTLE_CONSOLE_PASSWORD set.
type consoleWireHarness struct {
	ts           *testStack
	oClient      criteriav1connect.CriteriaServiceClient
	cClient      criteriav1connect.ServerServiceClient
	orchClient   criteriav1connect.OrchestratorServiceClient
	agentID      string
	agentToken   string
	otherID      string
	otherToken   string
	runID        string // owned by agentID
	consoleToken string
}

func newConsoleWireHarness(t *testing.T, allowAnonReads bool) *consoleWireHarness {
	t.Helper()
	ts := newTestStack(t)
	ts.server.EnableConsoleLogin()
	if err := ProvisionConsoleUser(context.Background(), ts.store, "operator", "op-password", nil); err != nil {
		t.Fatalf("provision console user: %v", err)
	}
	tsrv, oClient, cClient := ts.startServer(t, connect.WithInterceptors(
		auth.NewInterceptor(ts.store, allowAnonReads, auth.WithAnonRegister()),
	))

	ctx := context.Background()
	regA, err := oClient.Register(ctx, connect.NewRequest(&pb.RegisterRequest{Name: "agent-a"}))
	if err != nil {
		t.Fatalf("register agent-a: %v", err)
	}
	regB, err := oClient.Register(ctx, connect.NewRequest(&pb.RegisterRequest{Name: "agent-b"}))
	if err != nil {
		t.Fatalf("register agent-b: %v", err)
	}

	createReq := connect.NewRequest(&pb.CreateRunRequest{CriteriaId: regA.Msg.CriteriaId, WorkflowName: "wf"})
	createReq.Header().Set("Authorization", "Bearer "+regA.Msg.Token)
	runResp, err := oClient.CreateRun(ctx, createReq)
	if err != nil {
		t.Fatalf("create run: %v", err)
	}

	loginResp, err := cClient.Login(ctx, connect.NewRequest(&pb.LoginRequest{Username: "operator", Password: "op-password"}))
	if err != nil {
		t.Fatalf("console login over the wire: %v", err)
	}

	return &consoleWireHarness{
		ts:           ts,
		oClient:      oClient,
		cClient:      cClient,
		orchClient:   orchestratorClient(tsrv),
		agentID:      regA.Msg.CriteriaId,
		agentToken:   regA.Msg.Token,
		otherID:      regB.Msg.CriteriaId,
		otherToken:   regB.Msg.Token,
		runID:        runResp.Msg.RunId,
		consoleToken: loginResp.Msg.SessionToken,
	}
}

func (h *consoleWireHarness) authHeader(req connect.AnyRequest) {
	req.Header().Set("Authorization", "Bearer "+h.consoleToken)
}

func TestConsoleWire_LoginOverHTTP(t *testing.T) {
	ts := newTestStack(t)
	_, _, cClient := ts.startServer(t, connect.WithInterceptors(auth.NewInterceptor(ts.store, false)))

	// Disabled: Login returns Unimplemented with actionable guidance.
	_, err := cClient.Login(context.Background(), connect.NewRequest(&pb.LoginRequest{Username: "operator", Password: "pw"}))
	if connect.CodeOf(err) != connect.CodeUnimplemented {
		t.Fatalf("expected unimplemented with console login disabled, got %v", err)
	}

	ts.server.EnableConsoleLogin()
	if err := ProvisionConsoleUser(context.Background(), ts.store, "operator", "op-password", nil); err != nil {
		t.Fatalf("provision: %v", err)
	}

	// Wrong password: unauthenticated.
	_, err = cClient.Login(context.Background(), connect.NewRequest(&pb.LoginRequest{Username: "operator", Password: "nope"}))
	if connect.CodeOf(err) != connect.CodeUnauthenticated {
		t.Fatalf("expected unauthenticated for wrong password, got %v", err)
	}
	// Unknown username: unauthenticated.
	_, err = cClient.Login(context.Background(), connect.NewRequest(&pb.LoginRequest{Username: "ghost", Password: "op-password"}))
	if connect.CodeOf(err) != connect.CodeUnauthenticated {
		t.Fatalf("expected unauthenticated for unknown username, got %v", err)
	}

	// Correct credentials: token issued and usable as console identity.
	resp, err := cClient.Login(context.Background(), connect.NewRequest(&pb.LoginRequest{Username: "operator", Password: "op-password"}))
	if err != nil {
		t.Fatalf("login: %v", err)
	}
	readReq := connect.NewRequest(&pb.ListRunsRequest{})
	readReq.Header().Set("Authorization", "Bearer "+resp.Msg.SessionToken)
	if _, err = cClient.ListRuns(context.Background(), readReq); err != nil {
		t.Fatalf("issued token must authenticate: %v", err)
	}
}

// TestConsoleWire_ViewAllObservationSurface is the human console case: a
// console token observes runs owned by OTHER agents — ListRuns/GetRun/
// ListRunEvents/InspectRun/ListAgents — plus the WatchRun stream.
func TestConsoleWire_ViewAllObservationSurface(t *testing.T) {
	h := newConsoleWireHarness(t, false)
	ctx := context.Background()

	// ListRuns must surface the run owned by the OTHER agent (view-all).
	listReq := connect.NewRequest(&pb.ListRunsRequest{})
	h.authHeader(listReq)
	lists, err := h.cClient.ListRuns(ctx, listReq)
	if err != nil {
		t.Fatalf("console ListRuns: %v", err)
	}
	found := false
	for _, r := range lists.Msg.Runs {
		if r.RunId == h.runID {
			found = true
		}
	}
	if !found {
		t.Fatalf("console must see runs owned by other agents; got %d runs", len(lists.Msg.Runs))
	}

	// Seed one durable event so WatchRun replay is deterministic.
	env := &pb.Envelope{SchemaVersion: 1, RunId: h.runID, Ts: timestamppb.New(time.Now().UTC()), Payload: &pb.Envelope_StepEntered{StepEntered: &pb.StepEntered{Step: "s1", Adapter: "shell", Attempt: 1}}}
	seq, _, err := h.ts.store.AppendEvent(ctx, mustStoreEvent(t, env))
	if err != nil {
		t.Fatalf("append event: %v", err)
	}
	env.Seq = seq
	h.ts.hub.Publish(env)

	for name, call := range map[string]func() error{
		"GetAgent": func() error {
			req := connect.NewRequest(&pb.GetAgentRequest{CriteriaId: h.agentID})
			h.authHeader(req)
			_, err := h.cClient.GetAgent(ctx, req)
			return err
		},
		"GetRun": func() error {
			req := connect.NewRequest(&pb.GetRunRequest{RunId: h.runID})
			h.authHeader(req)
			_, err := h.cClient.GetRun(ctx, req)
			return err
		},
		"ListRunEvents": func() error {
			req := connect.NewRequest(&pb.ListRunEventsRequest{RunId: h.runID})
			h.authHeader(req)
			_, err := h.cClient.ListRunEvents(ctx, req)
			return err
		},
		"InspectRun": func() error {
			req := connect.NewRequest(&pb.InspectRunRequest{RunId: h.runID})
			h.authHeader(req)
			_, err := h.cClient.InspectRun(ctx, req)
			return err
		},
		"ListAgents": func() error {
			req := connect.NewRequest(&pb.ListAgentsRequest{})
			h.authHeader(req)
			_, err := h.cClient.ListAgents(ctx, req)
			return err
		},
	} {
		if err := call(); err != nil {
			t.Errorf("console %s on other agent's run: %v", name, err)
		}
	}

	// WatchRun streams for the console identity (replay event + WatchReady).
	watchReq := connect.NewRequest(&pb.WatchRunRequest{RunId: h.runID})
	h.authHeader(watchReq)
	watch, err := h.cClient.WatchRun(ctx, watchReq)
	if err != nil {
		t.Fatalf("console WatchRun: %v", err)
	}
	if !watch.Receive() {
		t.Fatalf("console watch replay: %v", watch.Err())
	}
}

// TestConsoleWire_WritesDenied pins that the console identity cannot mutate
// anything: ServerService writes, CriteriaService writes, OrchestratorService
// writes, and Register.
func TestConsoleWire_WritesDenied(t *testing.T) {
	h := newConsoleWireHarness(t, false)
	ctx := context.Background()

	denials := map[string]func() error{
		"StopRun": func() error {
			req := connect.NewRequest(&pb.StopRunRequest{RunId: h.runID})
			h.authHeader(req)
			_, err := h.cClient.StopRun(ctx, req)
			return err
		},
		"PauseRun": func() error {
			req := connect.NewRequest(&pb.PauseRunRequest{RunId: h.runID})
			h.authHeader(req)
			_, err := h.cClient.PauseRun(ctx, req)
			return err
		},
		"ResumeRun": func() error {
			req := connect.NewRequest(&pb.ResumeRunRequest{RunId: h.runID})
			h.authHeader(req)
			_, err := h.cClient.ResumeRun(ctx, req)
			return err
		},
		"SendPrompt": func() error {
			req := connect.NewRequest(&pb.SendPromptRequest{RunId: h.runID, Prompt: "nope"})
			h.authHeader(req)
			_, err := h.cClient.SendPrompt(ctx, req)
			return err
		},
		"CreateRun (CriteriaService write)": func() error {
			req := connect.NewRequest(&pb.CreateRunRequest{CriteriaId: h.agentID, WorkflowName: "wf"})
			h.authHeader(req)
			_, err := h.oClient.CreateRun(ctx, req)
			return err
		},
		"CancelRun (OrchestratorService write)": func() error {
			req := connect.NewRequest(&pb.CancelRunRequest{RunId: h.runID, Reason: "nope"})
			h.authHeader(req)
			_, err := h.orchClient.CancelRun(ctx, req)
			return err
		},
		"Register (bootstrap write)": func() error {
			req := connect.NewRequest(&pb.RegisterRequest{Name: "sneaky"})
			h.authHeader(req)
			_, err := h.oClient.Register(ctx, req)
			return err
		},
	}
	for name, call := range denials {
		err := call()
		if connect.CodeOf(err) != connect.CodePermissionDenied {
			t.Errorf("console %s: expected permission_denied, got %v", name, err)
		}
	}

	// The denial is not just interceptor-shaped: nothing actually happened.
	run, err := h.ts.store.GetRun(ctx, h.runID)
	if err != nil {
		t.Fatalf("get run: %v", err)
	}
	if run.Status == "paused" || run.Status == "stopped" || run.Status == "cancelled" {
		t.Errorf("run status must be untouched by denied writes, got %q", run.Status)
	}
}

// TestConsoleWire_AgentTokenPathUnchanged pins that agent login (token) and
// agent authorization are unaffected by the console feature.
func TestConsoleWire_AgentTokenPathUnchanged(t *testing.T) {
	h := newConsoleWireHarness(t, false)
	ctx := context.Background()

	// Agent can still create runs and inspect its own run.
	createReq := connect.NewRequest(&pb.CreateRunRequest{CriteriaId: h.agentID, WorkflowName: "wf2"})
	createReq.Header().Set("Authorization", "Bearer "+h.agentToken)
	if _, err := h.oClient.CreateRun(ctx, createReq); err != nil {
		t.Fatalf("agent CreateRun must keep working: %v", err)
	}
	ownReq := connect.NewRequest(&pb.InspectRunRequest{RunId: h.runID})
	ownReq.Header().Set("Authorization", "Bearer "+h.agentToken)
	if _, err := h.cClient.InspectRun(ctx, ownReq); err != nil {
		t.Fatalf("agent InspectRun on own run must keep working: %v", err)
	}

	// ...and is still denied the other agent's run (caller-owns-run for
	// agent identities, unchanged by the console work).
	otherReq := connect.NewRequest(&pb.InspectRunRequest{RunId: h.runID})
	otherReq.Header().Set("Authorization", "Bearer "+h.otherToken)
	if _, err := h.cClient.InspectRun(ctx, otherReq); connect.CodeOf(err) != connect.CodePermissionDenied {
		t.Fatalf("non-owner agent InspectRun must stay permission_denied, got %v", err)
	}

	// Agent tokens do not authenticate as console: a presented agent token on
	// a console read is fine (agent surface), but Login still requires creds.
	if _, err := h.cClient.Login(ctx, connect.NewRequest(&pb.LoginRequest{Username: h.agentID, Password: h.agentToken})); connect.CodeOf(err) != connect.CodeUnauthenticated {
		t.Fatalf("agent credentials must not work on Login, got %v", err)
	}
}

// TestConsoleWire_UnderAnonReads covers the deployed dev-mode configuration:
// anonymous reads stay anonymous, and a presented console token keeps its
// deterministic read-only boundary even on the anon-readable surface.
func TestConsoleWire_UnderAnonReads(t *testing.T) {
	h := newConsoleWireHarness(t, true)
	ctx := context.Background()

	// Anonymous read stays anonymous (CRI-194 unchanged).
	if _, err := h.cClient.ListRuns(ctx, connect.NewRequest(&pb.ListRunsRequest{})); err != nil {
		t.Fatalf("anonymous ListRuns under anon-reads: %v", err)
	}
	// Console token on the read surface still works.
	readReq := connect.NewRequest(&pb.ListRunsRequest{})
	h.authHeader(readReq)
	if _, err := h.cClient.ListRuns(ctx, readReq); err != nil {
		t.Fatalf("console ListRuns under anon-reads: %v", err)
	}
	// ...and the console token still cannot write.
	stopReq := connect.NewRequest(&pb.StopRunRequest{RunId: h.runID})
	h.authHeader(stopReq)
	if _, err := h.cClient.StopRun(ctx, stopReq); connect.CodeOf(err) != connect.CodePermissionDenied {
		t.Fatalf("console StopRun under anon-reads must be permission_denied, got %v", err)
	}
	// ...nor reach the orchestrator observation surface.
	orchReq := connect.NewRequest(&pb.ListActiveRunsRequest{})
	h.authHeader(orchReq)
	if _, err := h.orchClient.ListActiveRuns(ctx, orchReq); connect.CodeOf(err) != connect.CodePermissionDenied {
		t.Fatalf("console ListActiveRuns must be permission_denied, got %v", err)
	}
}