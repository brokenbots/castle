package rpc

import (
	"context"
	"testing"
	"time"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/brokenbots/castle/castle/internal/auth"
	"github.com/brokenbots/castle/castle/internal/store"
	pb "github.com/brokenbots/criteria/sdk/pb/criteria/v1"                // import-lint:allow castle service bindings (W08: move to castle-proto)
	"github.com/brokenbots/criteria/sdk/pb/criteria/v1/criteriav1connect" // import-lint:allow castle service bindings (W08: move to castle-proto)

	castlev1 "github.com/brokenbots/castle/castle/gen/castle/v1"
	castlev1connect "github.com/brokenbots/castle/castle/gen/castle/v1/castlev1connect"
)

// consoleWireHarness starts the full HTTP stack with the auth interceptor in
// anon-register mode, registers two agents, mints a run owned by the first,
// and seeds the default console user + enables console login — mirroring what
// main.go does with CASTLE_CONSOLE_USER/CASTLE_CONSOLE_PASSWORD set.
type consoleWireHarness struct {
	ts            *testStack
	oClient       criteriav1connect.CriteriaServiceClient
	cClient       criteriav1connect.ServerServiceClient
	orchClient    criteriav1connect.OrchestratorServiceClient
	consoleClient castlev1connect.ConsoleServiceClient
	agentID       string
	agentToken    string
	otherID       string
	otherToken    string
	runID         string // owned by agentID
	consoleToken  string
}

func newConsoleWireHarness(t *testing.T, allowAnonReads bool) *consoleWireHarness {
	t.Helper()
	ts := newTestStack(t)
	ts.server.EnableConsoleLogin()
	// KB-326 S3: arm the handler-side anon-read gate with the same flag the
	// interceptor runs under — since S3 the interceptor only marks tokenless
	// calls and the handler decides.
	ts.server.AllowAnonymousReads = allowAnonReads
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

	consoleCli := consoleClient(tsrv)
	loginResp, err := consoleCli.Login(ctx, connect.NewRequest(&castlev1.LoginRequest{Username: "operator", Password: "op-password"}))
	if err != nil {
		t.Fatalf("console login over the wire: %v", err)
	}

	return &consoleWireHarness{
		ts:            ts,
		oClient:       oClient,
		cClient:       cClient,
		orchClient:    orchestratorClient(tsrv),
		consoleClient: consoleCli,
		agentID:       regA.Msg.CriteriaId,
		agentToken:    regA.Msg.Token,
		otherID:       regB.Msg.CriteriaId,
		otherToken:    regB.Msg.Token,
		runID:         runResp.Msg.RunId,
		consoleToken:  loginResp.Msg.SessionToken,
	}
}

func (h *consoleWireHarness) authHeader(req connect.AnyRequest) {
	req.Header().Set("Authorization", "Bearer "+h.consoleToken)
}

func TestConsoleWire_LoginOverHTTP(t *testing.T) {
	ts := newTestStack(t)
	tsrv, _, cClient := ts.startServer(t, connect.WithInterceptors(auth.NewInterceptor(ts.store, false)))
	consoleCli := consoleClient(tsrv)

	// Disabled: Login returns Unimplemented with actionable guidance.
	_, err := consoleCli.Login(context.Background(), connect.NewRequest(&castlev1.LoginRequest{Username: "operator", Password: "pw"}))
	if connect.CodeOf(err) != connect.CodeUnimplemented {
		t.Fatalf("expected unimplemented with console login disabled, got %v", err)
	}

	ts.server.EnableConsoleLogin()
	if err := ProvisionConsoleUser(context.Background(), ts.store, "operator", "op-password", nil); err != nil {
		t.Fatalf("provision: %v", err)
	}

	// Wrong password: unauthenticated.
	_, err = consoleCli.Login(context.Background(), connect.NewRequest(&castlev1.LoginRequest{Username: "operator", Password: "nope"}))
	if connect.CodeOf(err) != connect.CodeUnauthenticated {
		t.Fatalf("expected unauthenticated for wrong password, got %v", err)
	}
	// Unknown username: unauthenticated.
	_, err = consoleCli.Login(context.Background(), connect.NewRequest(&castlev1.LoginRequest{Username: "ghost", Password: "op-password"}))
	if connect.CodeOf(err) != connect.CodeUnauthenticated {
		t.Fatalf("expected unauthenticated for unknown username, got %v", err)
	}

	// Correct credentials: token issued and usable as console identity.
	resp, err := consoleCli.Login(context.Background(), connect.NewRequest(&castlev1.LoginRequest{Username: "operator", Password: "op-password"}))
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
// anything outside the run-control surface (CRI-196): assignment/prompt
// writes, CriteriaService writes, OrchestratorService writes, and Register.
func TestConsoleWire_WritesDenied(t *testing.T) {
	h := newConsoleWireHarness(t, false)
	ctx := context.Background()

	denials := map[string]func() error{
		"SendPrompt": func() error {
			req := connect.NewRequest(&pb.SendPromptRequest{RunId: h.runID, Prompt: "nope"})
			h.authHeader(req)
			_, err := h.cClient.SendPrompt(ctx, req)
			return err
		},
		"SubmitWorkflowAssignment": func() error {
			req := connect.NewRequest(&pb.SubmitWorkflowAssignmentRequest{WorkflowName: "wf", WorkflowSource: "nope"})
			h.authHeader(req)
			_, err := h.cClient.SubmitWorkflowAssignment(ctx, req)
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
		"Resume (CriteriaService agent-owned write)": func() error {
			// The agent-owned resume stays out of the console surface: the
			// console delivers pending signals through ServerService.ResumeRun.
			req := connect.NewRequest(&pb.ResumeRequest{RunId: h.runID, Signal: "continue"})
			h.authHeader(req)
			_, err := h.oClient.Resume(ctx, req)
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

// TestConsoleWire_RunControlWritesAccepted is the CRI-196 case: a console
// identity may stop, pause, and resume a run owned by another agent, and the
// issued control messages reach the owning agent. ResumeRun carries the
// payload contract (signal + payload map).
func TestConsoleWire_RunControlWritesAccepted(t *testing.T) {
	h := newConsoleWireHarness(t, false)
	ctx := context.Background()

	// The owning agent subscribes to its control stream with its own token.
	ctrlReq := connect.NewRequest(&pb.ControlSubscribeRequest{CriteriaId: h.agentID})
	ctrlReq.Header().Set("Authorization", "Bearer "+h.agentToken)
	ctrl, err := h.oClient.Control(ctx, ctrlReq)
	if err != nil {
		t.Fatalf("agent control subscribe: %v", err)
	}
	defer ctrl.Close()
	if !ctrl.Receive() {
		t.Fatalf("expected ControlReady, err=%v", ctrl.Err())
	}

	// Console pause on another agent's run is accepted and reaches the agent.
	pauseReq := connect.NewRequest(&pb.PauseRunRequest{RunId: h.runID})
	h.authHeader(pauseReq)
	if _, err := h.cClient.PauseRun(ctx, pauseReq); err != nil {
		t.Fatalf("console PauseRun: %v", err)
	}
	if !ctrl.Receive() {
		t.Fatalf("expected PauseRun control message, err=%v", ctrl.Err())
	}
	if _, ok := ctrl.Msg().Command.(*pb.ControlMessage_PauseRun); !ok {
		t.Fatalf("unexpected control command: %T", ctrl.Msg().Command)
	}

	// Drive the agent-side pause lifecycle: run becomes paused with a pending
	// signal the console can then resume.
	if err := h.ts.store.SetRunPaused(ctx, h.runID, "continue", time.Now().UTC()); err != nil {
		t.Fatalf("set run paused: %v", err)
	}

	// Console resume carries the payload contract (signal + payload map)
	// through the castle-owned ResolveResume decision surface (KB-102 re-home).
	resumeReq := connect.NewRequest(&castlev1.ResolveResumeRequest{
		RunId:   h.runID,
		Signal:  "continue",
		Payload: map[string]string{"decision": "approve"},
	})
	h.authHeader(resumeReq)
	if _, err := h.consoleClient.ResolveResume(ctx, resumeReq); err != nil {
		t.Fatalf("console ResolveResume: %v", err)
	}
	if !ctrl.Receive() {
		t.Fatalf("expected ResumeRun control message, err=%v", ctrl.Err())
	}
	msg := ctrl.Msg()
	cmd, ok := msg.Command.(*pb.ControlMessage_ResumeRun)
	if !ok {
		t.Fatalf("unexpected control command: %T", msg.Command)
	}
	if cmd.ResumeRun.Signal != "continue" {
		t.Fatalf("signal=%s want continue", cmd.ResumeRun.Signal)
	}
	if cmd.ResumeRun.Payload["decision"] != "approve" {
		t.Fatalf("payload not forwarded to agent: %v", cmd.ResumeRun.Payload)
	}

	// Console stop on another agent's run is accepted and reaches the agent.
	stopReq := connect.NewRequest(&pb.StopRunRequest{RunId: h.runID})
	h.authHeader(stopReq)
	if _, err := h.cClient.StopRun(ctx, stopReq); err != nil {
		t.Fatalf("console StopRun: %v", err)
	}
	if !ctrl.Receive() {
		t.Fatalf("expected RunCancel control message, err=%v", ctrl.Err())
	}
	if cancel, ok := ctrl.Msg().Command.(*pb.ControlMessage_RunCancel); !ok {
		t.Fatalf("unexpected control command: %T", ctrl.Msg().Command)
	} else if cancel.RunCancel.RunId != h.runID {
		t.Fatalf("run id=%s want %s", cancel.RunCancel.RunId, h.runID)
	}
}

// TestConsoleWire_ApprovalResumeEndToEnd is the CRI-197 console human-input
// case: the operator answers a run paused for approval (ApprovalCard payload
// contract), the ResumeRun control message reaches the owning agent with the
// signal and decision payload, and the run's paused state is cleared so the
// run no longer reads as paused and a stale console retry is failed_precondition.
func TestConsoleWire_ApprovalResumeEndToEnd(t *testing.T) {
	h := newConsoleWireHarness(t, false)
	ctx := context.Background()

	// The owning agent subscribes to its control stream with its own token.
	ctrlReq := connect.NewRequest(&pb.ControlSubscribeRequest{CriteriaId: h.agentID})
	ctrlReq.Header().Set("Authorization", "Bearer "+h.agentToken)
	ctrl, err := h.oClient.Control(ctx, ctrlReq)
	if err != nil {
		t.Fatalf("agent control subscribe: %v", err)
	}
	defer ctrl.Close()
	if !ctrl.Receive() {
		t.Fatalf("expected ControlReady, err=%v", ctrl.Err())
	}

	// Drive the agent-side approval lifecycle the way the overseer records it:
	// the run pauses with the approval node name as the pending signal.
	const node = "deploy-approval"
	if err := h.ts.store.SetRunPaused(ctx, h.runID, node, time.Now().UTC()); err != nil {
		t.Fatalf("set run paused: %v", err)
	}

	// Console operator approves through the castle-owned ConsoleService
	// ResolveResume decision surface (released ServerService.ResumeRun is
	// run_id-only; KB-102 re-homed the CRI-196 payload contract here).
	resumeReq := connect.NewRequest(&castlev1.ResolveResumeRequest{
		RunId:   h.runID,
		Signal:  node,
		Payload: map[string]string{"decision": "approved"},
	})
	h.authHeader(resumeReq)
	if _, err := h.consoleClient.ResolveResume(ctx, resumeReq); err != nil {
		t.Fatalf("console ResolveResume: %v", err)
	}

	// The control message reaches the owning agent with signal + payload.
	if !ctrl.Receive() {
		t.Fatalf("expected ResumeRun control message, err=%v", ctrl.Err())
	}
	cmd, ok := ctrl.Msg().Command.(*pb.ControlMessage_ResumeRun)
	if !ok {
		t.Fatalf("unexpected control command: %T", ctrl.Msg().Command)
	}
	if cmd.ResumeRun.Signal != node {
		t.Fatalf("signal=%s want %s", cmd.ResumeRun.Signal, node)
	}
	if cmd.ResumeRun.Payload["decision"] != "approved" {
		t.Fatalf("approval decision payload not forwarded to agent: %v", cmd.ResumeRun.Payload)
	}

	// The paused state must be cleared at resume acceptance: status is running
	// and the pending signal is gone.
	run, err := h.ts.store.GetRun(ctx, h.runID)
	if err != nil {
		t.Fatalf("get run: %v", err)
	}
	if run.Status != "running" {
		t.Fatalf("status=%q want running", run.Status)
	}
	if run.PendingSignal != "" {
		t.Fatalf("pending_signal=%q want cleared", run.PendingSignal)
	}

	// A stale console view retrying the same signal gets failed_precondition,
	// which the console renders as an informational "signal already satisfied".
	staleReq := connect.NewRequest(&castlev1.ResolveResumeRequest{RunId: h.runID, Signal: node})
	h.authHeader(staleReq)
	if _, err := h.consoleClient.ResolveResume(ctx, staleReq); connect.CodeOf(err) != connect.CodeFailedPrecondition {
		t.Fatalf("stale console resolve must be failed_precondition, got %v", err)
	}
}

// TestConsoleWire_LogoutInvalidatesImmediately covers the KB-325 acceptance
// path: login -> authenticated call -> Logout -> the same token stops
// authenticating on the very next request. Only the killed session dies; a
// fresh login issues a NEW session that keeps working and whose ledger shows
// the revoked row with its stamp.
func TestConsoleWire_LogoutInvalidatesImmediately(t *testing.T) {
	h := newConsoleWireHarness(t, false)
	ctx := context.Background()

	// Pre-logout: a read with the session token authenticates.
	readReq := connect.NewRequest(&pb.ListRunsRequest{})
	h.authHeader(readReq)
	if _, err := h.cClient.ListRuns(ctx, readReq); err != nil {
		t.Fatalf("live session must authenticate: %v", err)
	}

	// Logout over the wire with the session token.
	logoutReq := connect.NewRequest(&castlev1.LogoutRequest{})
	h.authHeader(logoutReq)
	logout, err := h.consoleClient.Logout(ctx, logoutReq)
	if err != nil {
		t.Fatalf("console Logout: %v", err)
	}
	if logout.Msg.RevokedAt == nil {
		t.Fatal("Logout must return the revocation stamp")
	}

	// The SAME token is unauthenticated on the very next request — both on a
	// read and on a retry of the ledger surface.
	sameTokReq := connect.NewRequest(&castlev1.ListSessionsRequest{})
	h.authHeader(sameTokReq)
	if _, err := h.consoleClient.ListSessions(ctx, sameTokReq); connect.CodeOf(err) != connect.CodeUnauthenticated {
		t.Fatalf("logout must invalidate the session immediately, got %v", err)
	}
	readAgain := connect.NewRequest(&pb.ListRunsRequest{})
	h.authHeader(readAgain)
	if _, err := h.cClient.ListRuns(ctx, readAgain); connect.CodeOf(err) != connect.CodeUnauthenticated {
		t.Fatalf("revoked token must be unauthenticated on reads, got %v", err)
	}

	// Logout with the dead token is unauthenticated, not double-revocation.
	logoutAgain := connect.NewRequest(&castlev1.LogoutRequest{})
	h.authHeader(logoutAgain)
	if _, err := h.consoleClient.Logout(ctx, logoutAgain); connect.CodeOf(err) != connect.CodeUnauthenticated {
		t.Fatalf("retrying Logout with a revoked token must be unauthenticated, got %v", err)
	}

	// A fresh login issues a NEW live session (rotation unaffected) whose
	// ledger lists the killed row as revoked.
	login2, err := h.consoleClient.Login(ctx, connect.NewRequest(&castlev1.LoginRequest{Username: "operator", Password: "op-password"}))
	if err != nil {
		t.Fatalf("re-login after logout: %v", err)
	}
	listReq := connect.NewRequest(&castlev1.ListSessionsRequest{})
	listReq.Header().Set("Authorization", "Bearer "+login2.Msg.SessionToken)
	sessions, err := h.consoleClient.ListSessions(ctx, listReq)
	if err != nil {
		t.Fatalf("ListSessions after re-login: %v", err)
	}
	if sessions.Msg == nil || len(sessions.Msg.Sessions) != 2 {
		t.Fatalf("expected two ledger rows (revoked + live), got %v", sessions.Msg)
	}
	revoked := 0
	for _, s := range sessions.Msg.Sessions {
		if s.RevokedAt != nil {
			revoked++
			if s.Kind != castlev1.SessionKind_SESSION_KIND_CONSOLE {
				t.Fatalf("revoked row kind must be console session, got %v", s.Kind)
			}
		}
	}
	if revoked != 1 {
		t.Fatalf("exactly one row must be revoked, got %d", revoked)
	}

	// A live row's RevokedAt stays nil.
	liveReq := connect.NewRequest(&castlev1.ListSessionsRequest{})
	liveReq.Header().Set("Authorization", "Bearer "+login2.Msg.SessionToken)
	live, err := h.consoleClient.ListSessions(ctx, liveReq)
	if err != nil {
		t.Fatalf("ListSessions as live session: %v", err)
	}
	liveRows := 0
	for _, s := range live.Msg.Sessions {
		if s.RevokedAt == nil {
			liveRows++
			if s.CreatedAt == nil {
				t.Fatal("live row must carry its issue stamp")
			}
		}
	}
	if liveRows != 1 {
		t.Fatalf("exactly one row must be live, got %d", liveRows)
	}
}

// TestConsoleWire_ListSessionsShowsKindAndMembership covers the KB-325
// "sessions visible in ListSessions with kind+membership" acceptance: entries
// carry the console-session credential kind and the issue-time membership
// snapshot — the project set read from the ONE shared membership set at
// login — and logout timestamps show up on the revoked row.
func TestConsoleWire_ListSessionsShowsKindAndMembership(t *testing.T) {
	h := newConsoleWireHarness(t, false)
	ctx := context.Background()

	// Seed one project membership for the console user, then issue a FRESH
	// session so the snapshot is taken with the membership present (the
	// harness login predates the seed and carries an empty snapshot).
	proj := &store.Project{ID: "proj-ledger", MembershipPolicy: store.ProjectMembershipOpen, CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC()}
	if err := h.ts.store.CreateProject(ctx, proj); err != nil {
		t.Fatalf("create project: %v", err)
	}
	if err := h.ts.store.AddProjectMember(ctx, &store.ProjectMember{
		ProjectID: proj.ID, MemberID: ConsoleDefaultUserID, Kind: store.ProjectMemberKindHuman, CreatedAt: time.Now().UTC(),
	}); err != nil {
		t.Fatalf("add console member: %v", err)
	}
	login2, err := h.consoleClient.Login(ctx, connect.NewRequest(&castlev1.LoginRequest{Username: "operator", Password: "op-password"}))
	if err != nil {
		t.Fatalf("second login: %v", err)
	}

	listReq := connect.NewRequest(&castlev1.ListSessionsRequest{})
	listReq.Header().Set("Authorization", "Bearer "+login2.Msg.SessionToken)
	sessions, err := h.consoleClient.ListSessions(ctx, listReq)
	if err != nil {
		t.Fatalf("ListSessions: %v", err)
	}

	foundSnapshot := false
	for _, s := range sessions.Msg.Sessions {
		if s.Kind != castlev1.SessionKind_SESSION_KIND_CONSOLE {
			t.Fatalf("every ledger row is a console session, got kind %v", s.Kind)
		}
		// The fresh session carries the membership snapshot taken at its
		// issue time; the harness's earlier login predates the seed.
		if s.Membership != nil && len(s.Membership) == 1 && s.Membership[0] == proj.ID {
			foundSnapshot = true
		}
	}
	if !foundSnapshot {
		t.Fatalf("no ledger row carries the issue-time membership snapshot: %v", sessions.Msg.Sessions)
	}

	// Logout the fresh session: the ledger row then carries the revocation
	// stamp (and keeps its snapshot — revocation is a tombstone, not a wipe).
	logoutReq := connect.NewRequest(&castlev1.LogoutRequest{})
	logoutReq.Header().Set("Authorization", "Bearer "+login2.Msg.SessionToken)
	revoked, err := h.consoleClient.Logout(ctx, logoutReq)
	if err != nil {
		t.Fatalf("Logout: %v", err)
	}

	// Re-login and inspect the tombstoned row through the new session.
	login3, err := h.consoleClient.Login(ctx, connect.NewRequest(&castlev1.LoginRequest{Username: "operator", Password: "op-password"}))
	if err != nil {
		t.Fatalf("third login: %v", err)
	}
	afterReq := connect.NewRequest(&castlev1.ListSessionsRequest{})
	afterReq.Header().Set("Authorization", "Bearer "+login3.Msg.SessionToken)
	after, err := h.consoleClient.ListSessions(ctx, afterReq)
	if err != nil {
		t.Fatalf("ListSessions after revoke: %v", err)
	}
	tombstoned := false
	for _, s := range after.Msg.Sessions {
		if s.RevokedAt != nil {
			// The tombstoned row keeps its issue-time membership snapshot.
			tombstoned = true
			if len(s.Membership) != 1 || s.Membership[0] != proj.ID {
				t.Fatalf("revocation must keep the membership snapshot, got %v", s.Membership)
			}
			if !s.RevokedAt.AsTime().Equal(revoked.Msg.RevokedAt.AsTime()) {
				t.Fatalf("revocation stamp mismatch: want %v got %v", revoked.Msg.RevokedAt, s.RevokedAt)
			}
		}
	}
	if !tombstoned {
		t.Fatalf("revoked row with stamp must remain in the ledger: %v", after.Msg.Sessions)
	}
}

// TestConsoleWire_ProtocolTokensDeniedOnSessionLedger pins the credential
// class boundary (KB-325): protocol-token callers — agent, orchestrator,
// trigger — have no console login session to end and no ledger row to list,
// so Logout and ListSessions are permission_denied for them regardless of
// token validity.
func TestConsoleWire_ProtocolTokensDeniedOnSessionLedger(t *testing.T) {
	h := newConsoleWireHarness(t, false)
	ctx := context.Background()

	// Agent token (passes the interceptor — agents self-guard their surfaces
	// — so the denial must come from the handler's credential-class check).
	agentLogout := connect.NewRequest(&castlev1.LogoutRequest{})
	agentLogout.Header().Set("Authorization", "Bearer "+h.agentToken)
	if _, err := h.consoleClient.Logout(ctx, agentLogout); connect.CodeOf(err) != connect.CodePermissionDenied {
		t.Fatalf("agent token Logout must be permission_denied, got %v", err)
	}
	agentList := connect.NewRequest(&castlev1.ListSessionsRequest{})
	agentList.Header().Set("Authorization", "Bearer "+h.agentToken)
	if _, err := h.consoleClient.ListSessions(ctx, agentList); connect.CodeOf(err) != connect.CodePermissionDenied {
		t.Fatalf("agent token ListSessions must be permission_denied, got %v", err)
	}

	// Orchestrator and trigger tokens are denied by their allowlists.
	const orchestratorToken = "orchestrator-token-1"
	provisionOrchestratorIdentity(t, h.ts.store, "orchestrator-operator", orchestratorToken)
	orchLogout := connect.NewRequest(&castlev1.LogoutRequest{})
	orchLogout.Header().Set("Authorization", "Bearer "+orchestratorToken)
	if _, err := h.consoleClient.Logout(ctx, orchLogout); connect.CodeOf(err) != connect.CodePermissionDenied {
		t.Fatalf("orchestrator token Logout must be permission_denied, got %v", err)
	}
	orchList := connect.NewRequest(&castlev1.ListSessionsRequest{})
	orchList.Header().Set("Authorization", "Bearer "+orchestratorToken)
	if _, err := h.consoleClient.ListSessions(ctx, orchList); connect.CodeOf(err) != connect.CodePermissionDenied {
		t.Fatalf("orchestrator token ListSessions must be permission_denied, got %v", err)
	}

	// The console session ledger is untouched by denied protocol-token calls:
	// the operator's session still authenticates.
	consoleList := connect.NewRequest(&castlev1.ListSessionsRequest{})
	h.authHeader(consoleList)
	if _, err := h.consoleClient.ListSessions(ctx, consoleList); err != nil {
		t.Fatalf("console ListSessions must keep working, got %v", err)
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
	if _, err := h.consoleClient.Login(ctx, connect.NewRequest(&castlev1.LoginRequest{Username: h.agentID, Password: h.agentToken})); connect.CodeOf(err) != connect.CodeUnauthenticated {
		t.Fatalf("agent credentials must not work on Login, got %v", err)
	}
}

// TestConsoleWire_UnderAnonReads covers the deployed dev-mode configuration:
// anonymous reads stay anonymous, and a presented console token keeps its
// deterministic boundary — reads plus run-control writes (CRI-196) — even on
// the anon-readable surface.
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
	// The owning agent connects so a run-control write can deliver.
	ctrlReq := connect.NewRequest(&pb.ControlSubscribeRequest{CriteriaId: h.agentID})
	ctrlReq.Header().Set("Authorization", "Bearer "+h.agentToken)
	ctrl, err := h.oClient.Control(ctx, ctrlReq)
	if err != nil {
		t.Fatalf("agent control subscribe: %v", err)
	}
	defer ctrl.Close()
	if !ctrl.Receive() {
		t.Fatalf("expected ControlReady, err=%v", ctrl.Err())
	}
	// ...and the console token can perform run-control writes (CRI-196).
	stopReq := connect.NewRequest(&pb.StopRunRequest{RunId: h.runID})
	h.authHeader(stopReq)
	if _, err := h.cClient.StopRun(ctx, stopReq); err != nil {
		t.Fatalf("console StopRun under anon-reads must be accepted, got %v", err)
	}
	// ...nor reach the orchestrator observation surface.
	orchReq := connect.NewRequest(&pb.ListActiveRunsRequest{})
	h.authHeader(orchReq)
	if _, err := h.orchClient.ListActiveRuns(ctx, orchReq); connect.CodeOf(err) != connect.CodePermissionDenied {
		t.Fatalf("console ListActiveRuns must be permission_denied, got %v", err)
	}
}
