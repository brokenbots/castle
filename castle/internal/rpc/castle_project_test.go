package rpc

import (
	"context"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"connectrpc.com/connect"

	castlev1 "github.com/brokenbots/castle/castle/gen/castle/v1"
	castlev1connect "github.com/brokenbots/castle/castle/gen/castle/v1/castlev1connect"
	"github.com/brokenbots/castle/castle/internal/auth"
	"github.com/brokenbots/castle/castle/internal/store"
	pb "github.com/brokenbots/criteria/sdk/pb/criteria/v1"
	criteriav1connect "github.com/brokenbots/criteria/sdk/pb/criteria/v1/criteriav1connect"
)

// projectTestStack opens an anon-registration test server and returns the
// stack with criteria-, server- and project-service clients bound to it.
// projectTestStack opens an anon-registration test server and returns the
// stack plus criteria- and server-service clients bound to it (the
// project-service client is built with projectClient(tsrv)).
func projectTestStack(t *testing.T) (*testStack, *httptest.Server, criteriav1connect.CriteriaServiceClient, criteriav1connect.ServerServiceClient) {
	t.Helper()
	ts := newTestStack(t)
	tsrv, oClient, sClient := ts.startServer(t,
		connect.WithInterceptors(auth.NewInterceptor(ts.store, false, auth.WithAnonRegister())),
	)
	return ts, tsrv, oClient, sClient
}

// bearer returns a connect request carrying the given agent token.
func bearer[T any](msg *T, token string) *connect.Request[T] {
	req := connect.NewRequest(msg)
	req.Header().Set("Authorization", "Bearer "+token)
	return req
}

// registerProjectAgent registers an agent presenting the KB-318 identity
// block and (when non-empty) the self-join project header.
func registerProjectAgent(t *testing.T, ctx context.Context, oClient criteriav1connect.CriteriaServiceClient, name, project string) (string, string) {
	t.Helper()
	req := connect.NewRequest(&pb.RegisterRequest{Name: name, Labels: map[string]string{"gpu": "true"}})
	h := req.Header()
	h.Set(auth.HeaderAgentVersion, "2.7.1")
	h.Set(auth.HeaderAgentRepoURL, "https://github.com/example/agent")
	h.Set(auth.HeaderAgentBuildDate, "2026-05-11T00:00:00Z")
	h.Add(auth.HeaderAgentSDKVersion, "adapter:9.9.9")
	h.Add(auth.HeaderAgentSDKVersion, "criteria:v0.9.1")
	h.Add(auth.HeaderAgentExtra, "packager:oci")
	h.Add(auth.HeaderAgentExtra, "packager:vex")
	h.Add(auth.HeaderAgentExtra, "node:edge-runner")
	if project != "" {
		h.Set(auth.HeaderAgentProject, project)
	}
	resp, err := oClient.Register(ctx, req)
	if err != nil {
		t.Fatalf("register %s: %v", name, err)
	}
	return resp.Msg.CriteriaId, resp.Msg.Token
}

// TestProjectRegisterIdentityAndJoin covers the registration half of the
// acceptance chain: the identity block persists on the overseer row, the
// agent self-joins the project, the member set records it as an AI operator,
// and the identity echo rides GetAgent without touching stored labels.
func TestProjectRegisterIdentityAndJoin(t *testing.T) {
	ts, tsrv, oClient, sClient := projectTestStack(t)
	ctx := context.Background()

	// A previously registered agent is today's operator for the project
	// surface (full user auth arrives later; agent-identity-only now).
	opReg, err := oClient.Register(ctx, connect.NewRequest(&pb.RegisterRequest{Name: "op"}))
	if err != nil {
		t.Fatalf("register operator: %v", err)
	}
	pClient := projectClient(tsrv)
	createProject(t, ctx, pClient, opReg.Msg.Token, "squad-alpha", castlev1.ProjectMembershipPolicy_PROJECT_MEMBERSHIP_POLICY_OPEN)

	agentID, _ := registerProjectAgent(t, ctx, oClient, "worker", "squad-alpha")

	// Persisted identity: every block field survives, stored labels untouched.
	o, err := ts.store.GetOverseer(ctx, agentID)
	if err != nil {
		t.Fatalf("get overseer: %v", err)
	}
	if o.Identity == nil {
		t.Fatal("expected identity block persisted on overseer row")
	}
	if o.Identity.Version != "2.7.1" || o.Identity.RepoURL != "https://github.com/example/agent" || o.Identity.BuildDate != "2026-05-11T00:00:00Z" {
		t.Fatalf("identity scalars not persisted: %+v", o.Identity)
	}
	// Repeated sdk-version headers append; canonical persistence sorts by
	// component name.
	wantSDK := []store.AgentSDKVersion{{Name: "adapter", Version: "9.9.9"}, {Name: "criteria", Version: "v0.9.1"}}
	if len(o.Identity.SDKVersions) != len(wantSDK) || o.Identity.SDKVersions[0] != wantSDK[0] || o.Identity.SDKVersions[1] != wantSDK[1] {
		t.Fatalf("sdk versions = %+v, want %+v", o.Identity.SDKVersions, wantSDK)
	}
	// Repeated extra keys collapse to their last presented value.
	if len(o.Identity.Extra) != 2 || o.Identity.Extra["packager"] != "vex" || o.Identity.Extra["node"] != "edge-runner" {
		t.Fatalf("extra not persisted: %+v", o.Identity.Extra)
	}
	if _, leaked := o.Labels[auth.IdentityEchoLabel]; leaked {
		t.Fatal("stored labels must never contain the identity echo")
	}

	// Member set: registration joined the agent as an AI operator.
	members := listProjectMembers(t, ctx, pClient, opReg.Msg.Token, "squad-alpha")
	if len(members) != 1 || members[0].MemberId != agentID || members[0].Kind != castlev1.ProjectMemberKind_PROJECT_MEMBER_KIND_AI {
		t.Fatalf("project members = %+v, want [%s/ai]", members, agentID)
	}

	// Identity echo on GetAgent: the persisted block renders through the
	// reserved label; the agent's own labels still carry through.
	resp, err := sClient.GetAgent(ctx, bearer(&pb.GetAgentRequest{CriteriaId: agentID}, opReg.Msg.Token))
	if err != nil {
		t.Fatalf("get agent: %v", err)
	}
	block, err := store.IdentityJSON(o.Identity)
	if err != nil {
		t.Fatalf("identity json: %v", err)
	}
	if resp.Msg.Labels[auth.IdentityEchoLabel] != block {
		t.Fatalf("identity echo = %q, want %q", resp.Msg.Labels[auth.IdentityEchoLabel], block)
	}
	if resp.Msg.Labels["gpu"] != "true" {
		t.Fatalf("agent labels lost routing entry: %v", resp.Msg.Labels)
	}
}

// TestProjectAssignmentScopesDispatchToMembers covers the dispatch half of
// the acceptance chain: a non-member never leases a project assignment, a
// member does, conventions merge into the delivered labels under the
// reserved prefixes, replay is idempotent, and the run lifecycle streams.
func TestProjectAssignmentScopesDispatchToMembers(t *testing.T) {
	ts, tsrv, oClient, _ := projectTestStack(t)
	ctx := context.Background()

	pClient := projectClient(tsrv)
	opReg, err := oClient.Register(ctx, connect.NewRequest(&pb.RegisterRequest{Name: "op"}))
	if err != nil {
		t.Fatalf("register operator: %v", err)
	}
	createProject(t, ctx, pClient, opReg.Msg.Token, "build-pool", castlev1.ProjectMembershipPolicy_PROJECT_MEMBERSHIP_POLICY_OPEN)

	memberID, memberToken := registerProjectAgent(t, ctx, oClient, "member", "build-pool")
	nonMemberID, nonMemberToken := registerProjectAgent(t, ctx, oClient, "outsider", "")

	// Watch the non-member's control stream while the assignment queues: a
	// project assignment must never be delivered to a non-member.
	nonMemberCtrl := openControl(t, oClient, nonMemberID, nonMemberToken)
	defer nonMemberCtrl.Close()
	expectControlReady(t, nonMemberCtrl)

	submit := func() *castlev1.SubmitProjectAssignmentResponse {
		t.Helper()
		req := bearer(&castlev1.SubmitProjectAssignmentRequest{
			ProjectId:      "build-pool",
			WorkflowName:   "ship-it",
			WorkflowSource: "hcl-source",
			IdempotencyKey: "k1",
			Labels:         map[string]string{"gpu": "true"},
			InputValues:    map[string]string{"region": "eu-1"},
			TargetMetadata: map[string]string{"adapter.shell.image": "registry/img:1.2.3"},
		}, memberToken)
		resp, err := pClient.SubmitProjectAssignment(ctx, req)
		if err != nil {
			t.Fatalf("submit project assignment: %v", err)
		}
		return resp.Msg
	}
	first := submit()
	if first.State != pb.WorkflowAssignmentState_WORKFLOW_ASSIGNMENT_STATE_QUEUED {
		t.Fatalf("state = %v, want QUEUED", first.State)
	}

	// No lease from the non-member: its stream stays idle and the assignment
	// stays queued, still scoped to the project and owned by the submitter.
	expectNoWorkflowAssignment(t, nonMemberCtrl, 500*time.Millisecond)
	a, err := ts.store.GetWorkflowAssignmentByRunID(ctx, first.RunId)
	if err != nil {
		t.Fatalf("get assignment: %v", err)
	}
	if a.State != store.WorkflowAssignmentStateQueued || a.ProjectID != "build-pool" || a.OwnerCriteriaID != memberID {
		t.Fatalf("assignment = state %s project %s owner %s, want queued/build-pool/%s", a.State, a.ProjectID, a.OwnerCriteriaID, memberID)
	}

	// Members lease within the pool: connecting the member agent triggers its
	// dispatch and delivers the assignment with merged conventions.
	memberCtrl := openControl(t, oClient, memberID, memberToken)
	defer memberCtrl.Close()
	expectControlReady(t, memberCtrl)
	msg := receiveControl(t, memberCtrl, 2*time.Second)
	wa := msg.GetWorkflowAssignment()
	if wa == nil {
		t.Fatalf("expected WorkflowAssignment, got %T", msg.Command)
	}
	if wa.RunId != first.RunId || wa.WorkflowName != "ship-it" {
		t.Fatalf("delivered %s/%q, want %s/ship-it", wa.RunId, wa.WorkflowName, first.RunId)
	}
	for key, want := range map[string]string{
		"gpu":                                 "true",
		"criteria.input.region":               "eu-1",
		"criteria.target.adapter.shell.image": "registry/img:1.2.3",
	} {
		if got := wa.Labels[key]; got != want {
			t.Fatalf("delivered label %s = %q, want %q (labels=%v)", key, got, want, wa.Labels)
		}
	}

	// Idempotent replay: the same owner+key returns the same run untouched.
	replay := submit()
	if replay.RunId != first.RunId || !replay.CreatedAt.AsTime().Equal(first.CreatedAt.AsTime()) {
		t.Fatalf("replay not idempotent: run %s createdAt %v, want %s/%v", replay.RunId, replay.CreatedAt, first.RunId, first.CreatedAt)
	}

	// The run lifecycle belongs to the operator: RunStarted streams through
	// the events path and flips the run to running for the agent owner.
	submitRunStarted(t, oClient, memberToken, first.RunId, "ship-it", "s1")
	run, err := ts.store.GetRun(ctx, first.RunId)
	if err != nil {
		t.Fatalf("get run: %v", err)
	}
	if run.Status != "running" || run.OverseerID != memberID {
		t.Fatalf("run = %s owned by %s, want running/%s", run.Status, run.OverseerID, memberID)
	}
	expectNoWorkflowAssignment(t, nonMemberCtrl, 300*time.Millisecond)
}

// TestSubmitProjectAssignmentValidation pins the request-shape failures:
// required fields, unknown project, and convention size caps (shape-only
// validation; values are never interpreted).
func TestSubmitProjectAssignmentValidation(t *testing.T) {
	_, tsrv, oClient, _ := projectTestStack(t)
	ctx := context.Background()

	pClient := projectClient(tsrv)
	opReg, err := oClient.Register(ctx, connect.NewRequest(&pb.RegisterRequest{Name: "op"}))
	if err != nil {
		t.Fatalf("register operator: %v", err)
	}
	token := opReg.Msg.Token
	createProject(t, ctx, pClient, token, "validated", castlev1.ProjectMembershipPolicy_PROJECT_MEMBERSHIP_POLICY_OPEN)

	cases := []struct {
		name     string
		req      *castlev1.SubmitProjectAssignmentRequest
		wantCode connect.Code
	}{
		{
			name:     "missing project_id",
			req:      &castlev1.SubmitProjectAssignmentRequest{WorkflowName: "w", WorkflowSource: "hcl", IdempotencyKey: "k"},
			wantCode: connect.CodeInvalidArgument,
		},
		{
			name:     "missing workflow",
			req:      &castlev1.SubmitProjectAssignmentRequest{ProjectId: "validated", WorkflowSource: "hcl", IdempotencyKey: "k"},
			wantCode: connect.CodeInvalidArgument,
		},
		{
			name:     "missing idempotency_key",
			req:      &castlev1.SubmitProjectAssignmentRequest{ProjectId: "validated", WorkflowName: "w", WorkflowSource: "hcl"},
			wantCode: connect.CodeInvalidArgument,
		},
		{
			name:     "unknown project",
			req:      &castlev1.SubmitProjectAssignmentRequest{ProjectId: "ghost", WorkflowName: "w", WorkflowSource: "hcl", IdempotencyKey: "k"},
			wantCode: connect.CodeNotFound,
		},
		{
			name: "oversized input value",
			req: &castlev1.SubmitProjectAssignmentRequest{ProjectId: "validated", WorkflowName: "w", WorkflowSource: "hcl", IdempotencyKey: "k",
				InputValues: map[string]string{"big": strings.Repeat("x", 17*1024)}},
			wantCode: connect.CodeInvalidArgument,
		},
		{
			name:     "malformed project slug",
			req:      &castlev1.SubmitProjectAssignmentRequest{ProjectId: "Bad Slug!", WorkflowName: "w", WorkflowSource: "hcl", IdempotencyKey: "k"},
			wantCode: connect.CodeInvalidArgument,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := pClient.SubmitProjectAssignment(ctx, bearer(tc.req, token))
			if err == nil || connect.CodeOf(err) != tc.wantCode {
				t.Fatalf("got %v, want %v", err, tc.wantCode)
			}
		})
	}
}

// TestRegistrationMetadataRejected pins the wire-level rejection of malformed
// identity/project metadata: misconfigured agents fail fast at registration
// instead of persisting a truncated identity block.
func TestRegistrationMetadataRejected(t *testing.T) {
	_, tsrv, oClient, _ := projectTestStack(t)
	ctx := context.Background()

	pClient := projectClient(tsrv)
	opReg, err := oClient.Register(ctx, connect.NewRequest(&pb.RegisterRequest{Name: "op"}))
	if err != nil {
		t.Fatalf("register operator: %v", err)
	}
	createProject(t, ctx, pClient, opReg.Msg.Token, "closed-hold", castlev1.ProjectMembershipPolicy_PROJECT_MEMBERSHIP_POLICY_CLOSED)

	cases := []struct {
		name     string
		header   string
		value    string
		wantCode connect.Code
	}{
		{"malformed sdk pair", auth.HeaderAgentSDKVersion, "nocolon", connect.CodeInvalidArgument},
		{"oversized version header", auth.HeaderAgentVersion, strings.Repeat("x", 513), connect.CodeInvalidArgument},
		{"malformed project slug", auth.HeaderAgentProject, "Bad Slug!", connect.CodeInvalidArgument},
		{"unknown project", auth.HeaderAgentProject, "no-such-project", connect.CodeNotFound},
		{"closed project", auth.HeaderAgentProject, "closed-hold", connect.CodePermissionDenied},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req := connect.NewRequest(&pb.RegisterRequest{Name: "bad"})
			req.Header().Set(tc.header, tc.value)
			resp, err := oClient.Register(ctx, req)
			if err == nil {
				t.Fatalf("expected %v, got criteria_id %s", tc.wantCode, resp.Msg.CriteriaId)
			}
			if connect.CodeOf(err) != tc.wantCode {
				t.Fatalf("got %v, want %v", connect.CodeOf(err), tc.wantCode)
			}
		})
	}
}

func createProject(t *testing.T, ctx context.Context, pClient castlev1connect.ProjectServiceClient, token, id string, policy castlev1.ProjectMembershipPolicy) *castlev1.Project {
	t.Helper()
	resp, err := pClient.CreateProject(ctx, bearer(&castlev1.CreateProjectRequest{Project: &castlev1.Project{
		Id:               id,
		MembershipPolicy: policy,
	}}, token))
	if err != nil {
		t.Fatalf("create project %s: %v", id, err)
	}
	return resp.Msg.Project
}

func listProjectMembers(t *testing.T, ctx context.Context, pClient castlev1connect.ProjectServiceClient, token, projectID string) []*castlev1.ProjectMember {
	t.Helper()
	resp, err := pClient.ListProjectMembers(ctx, bearer(&castlev1.ListProjectMembersRequest{ProjectId: projectID}, token))
	if err != nil {
		t.Fatalf("list project members: %v", err)
	}
	return resp.Msg.Members
}
