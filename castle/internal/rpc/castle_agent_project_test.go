package rpc

import (
	"context"
	"testing"
	"time"

	"connectrpc.com/connect"

	castlev1 "github.com/brokenbots/castle/castle/gen/castle/v1"
	castlev1connect "github.com/brokenbots/castle/castle/gen/castle/v1/castlev1connect"
	"github.com/brokenbots/castle/castle/internal/auth"
	"github.com/brokenbots/castle/castle/internal/provision"
	"github.com/brokenbots/castle/castle/internal/store"
	pb "github.com/brokenbots/criteria/sdk/pb/criteria/v1"
	criteriav1connect "github.com/brokenbots/criteria/sdk/pb/criteria/v1/criteriav1connect"
)

// registerAgentCredential registers an agent through a per-project agent
// credential (KB-337): the credential rides the Authorization header, the
// optional project header exercises the fail-closed cross-check (empty means
// the credential alone binds), and withBlock controls whether an identity
// block is presented.
func registerAgentCredential(t *testing.T, ctx context.Context, oClient criteriav1connect.CriteriaServiceClient, project, token, headerProject string, withBlock bool) (string, string) {
	t.Helper()
	req := connect.NewRequest(&pb.RegisterRequest{Name: project + "-runner", Labels: map[string]string{"gpu": "true"}})
	h := req.Header()
	h.Set("Authorization", "Bearer "+token)
	if withBlock {
		h.Set(auth.HeaderAgentVersion, "2.7.1")
		h.Set(auth.HeaderAgentRepoURL, "https://github.com/example/agent")
		h.Set(auth.HeaderAgentBuildDate, "2026-05-11T00:00:00Z")
	}
	if headerProject != "" {
		h.Set(auth.HeaderAgentProject, headerProject)
	}
	resp, err := oClient.Register(ctx, req)
	if err != nil {
		t.Fatalf("register %s agent with its credential: %v", project, err)
	}
	return resp.Msg.CriteriaId, resp.Msg.Token
}

// submitProjectAssignment queues one project assignment as an operator.
func submitProjectAssignment(t *testing.T, ctx context.Context, pClient castlev1connect.ProjectServiceClient, token, project, key string) *castlev1.SubmitProjectAssignmentResponse {
	t.Helper()
	req := bearer(&castlev1.SubmitProjectAssignmentRequest{
		ProjectId:      project,
		WorkflowName:   "ship-it",
		WorkflowSource: "hcl-source",
		IdempotencyKey: key,
		Labels:         map[string]string{"gpu": "true"},
		InputValues:    map[string]string{"region": "eu-1"},
		TargetMetadata: map[string]string{"adapter.shell.image": "registry/img:1.2.3"},
	}, token)
	resp, err := pClient.SubmitProjectAssignment(ctx, req)
	if err != nil {
		t.Fatalf("submit assignment %s: %v", key, err)
	}
	return resp.Msg
}

// TestAgentProjectCredentialEndToEnd is the P0.1 acceptance chain: a fake
// agent registered with its project's credential joins the project, consumes
// the assignment through the Control stream (route_name + payload +
// target_metadata conventions), mints a run stamped with the project id, and
// a second project's agent can neither see the run nor lease the work.
func TestAgentProjectCredentialEndToEnd(t *testing.T) {
	ts, tsrv, oClient, sClient := projectTestStack(t)
	ctx := context.Background()
	pClient := projectClient(tsrv)

	opReg, err := oClient.Register(ctx, connect.NewRequest(&pb.RegisterRequest{Name: "op"}))
	if err != nil {
		t.Fatalf("register operator: %v", err)
	}

	// Provision the per-project credentials, then ledger the projects: alpha
	// is OPEN, beta is CLOSED — a project's credential must join it either
	// way (provisioning the credential was the admission act).
	now := time.Now().UTC()
	entries := []provision.AgentProjectToken{
		{Project: "cred-alpha", Token: "tok-alpha"},
		{Project: "cred-beta", Token: "tok-beta"},
	}
	if err := provision.ApplyAgentProjectTokens(ctx, ts.store, entries, now); err != nil {
		t.Fatalf("provision credentials: %v", err)
	}
	createProject(t, ctx, pClient, opReg.Msg.Token, "cred-alpha", castlev1.ProjectMembershipPolicy_PROJECT_MEMBERSHIP_POLICY_OPEN)
	createProject(t, ctx, pClient, opReg.Msg.Token, "cred-beta", castlev1.ProjectMembershipPolicy_PROJECT_MEMBERSHIP_POLICY_CLOSED)

	alphaID, alphaToken := registerAgentCredential(t, ctx, oClient, "cred-alpha", "tok-alpha", "", true)
	betaID, betaToken := registerAgentCredential(t, ctx, oClient, "cred-beta", "tok-beta", "", false)

	// Both agents joined their own project — beta through a CLOSED policy,
	// binding-only (no self-join header presented).
	members := listProjectMembers(t, ctx, pClient, opReg.Msg.Token, "cred-alpha")
	if len(members) != 1 || members[0].MemberId != alphaID || members[0].Kind != castlev1.ProjectMemberKind_PROJECT_MEMBER_KIND_AI {
		t.Fatalf("alpha members = %+v, want [%s/ai]", members, alphaID)
	}
	betaMembers := listProjectMembers(t, ctx, pClient, opReg.Msg.Token, "cred-beta")
	if len(betaMembers) != 1 || betaMembers[0].MemberId != betaID || betaMembers[0].Kind != castlev1.ProjectMemberKind_PROJECT_MEMBER_KIND_AI {
		t.Fatalf("beta members (closed policy) = %+v, want [%s/ai]", betaMembers, betaID)
	}

	// The identity block presented alongside the credential persists.
	o, err := ts.store.GetOverseer(ctx, alphaID)
	if err != nil {
		t.Fatalf("get overseer: %v", err)
	}
	if o.Identity == nil || o.Identity.Version != "2.7.1" || o.Identity.RepoURL != "https://github.com/example/agent" {
		t.Fatalf("identity block = %+v, want version 2.7.1 + repo", o.Identity)
	}

	// Queue alpha work. The submission mints the run already stamped with
	// the project id (runs.project_id from the assignment row).
	first := submitProjectAssignment(t, ctx, pClient, opReg.Msg.Token, "cred-alpha", "k1")
	if first.State != pb.WorkflowAssignmentState_WORKFLOW_ASSIGNMENT_STATE_QUEUED {
		t.Fatalf("state = %v, want QUEUED", first.State)
	}
	run, err := ts.store.GetRun(ctx, first.RunId)
	if err != nil {
		t.Fatalf("get run: %v", err)
	}
	if run.ProjectID != "cred-alpha" {
		t.Fatalf("run project = %q, want cred-alpha (assignment stamping)", run.ProjectID)
	}

	// The member agent consumes the assignment via Control: delivered with
	// merged conventions (route_name, input payload, target metadata).
	alphaCtrl := openControl(t, oClient, alphaID, alphaToken)
	defer alphaCtrl.Close()
	expectControlReady(t, alphaCtrl)
	msg := receiveControl(t, alphaCtrl, 2*time.Second)
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

	// The agent owns the run: RunStarted flips it to running for the agent,
	// keeping the project stamp.
	submitRunStarted(t, oClient, alphaToken, first.RunId, "ship-it", "s1")
	run, err = ts.store.GetRun(ctx, first.RunId)
	if err != nil {
		t.Fatalf("get run after start: %v", err)
	}
	if run.Status != "running" || run.OverseerID != alphaID || run.ProjectID != "cred-alpha" {
		t.Fatalf("run = %s/%s/%s, want running/%s/cred-alpha", run.Status, run.OverseerID, run.ProjectID, alphaID)
	}

	// A second queued assignment stays invisible to the beta agent: no
	// control delivery, no lease.
	second := submitProjectAssignment(t, ctx, pClient, opReg.Msg.Token, "cred-alpha", "k2")
	betaCtrl := openControl(t, oClient, betaID, betaToken)
	defer betaCtrl.Close()
	expectControlReady(t, betaCtrl)
	expectNoWorkflowAssignment(t, betaCtrl, 500*time.Millisecond)
	if _, err := ts.store.GetWorkflowAssignmentByRunID(ctx, second.RunId); err != nil {
		t.Fatalf("get k2 assignment: %v", err)
	}

	// Beta cannot read alpha's run: membership-scoped reads answer NotFound
	// so the run's existence does not leak.
	if _, err := sClient.GetRun(ctx, bearer(&pb.GetRunRequest{RunId: first.RunId}, betaToken)); connect.CodeOf(err) != connect.CodeNotFound {
		t.Fatalf("beta read of alpha run = %v, want NotFound", err)
	}

	// Heartbeat fail closed: a credential may never update another
	// project's agent row. The presented credential resolves to its own
	// operator identity (which owns no overseer row), so targeting another
	// project's agent is an explicit PermissionDenied before any write.
	before, err := ts.store.GetOverseer(ctx, alphaID)
	if err != nil {
		t.Fatalf("get overseer before heartbeat: %v", err)
	}
	if _, err := oClient.Heartbeat(ctx, bearer(&pb.HeartbeatRequest{CriteriaId: alphaID}, "tok-beta")); connect.CodeOf(err) != connect.CodePermissionDenied {
		t.Fatalf("heartbeat with foreign credential = %v, want PermissionDenied", err)
	}
	after, err := ts.store.GetOverseer(ctx, alphaID)
	if err != nil {
		t.Fatalf("get overseer after heartbeat: %v", err)
	}
	if !after.LastSeenAt.Equal(before.LastSeenAt) {
		t.Fatalf("foreign-credential heartbeat moved LastSeenAt %v -> %v", before.LastSeenAt, after.LastSeenAt)
	}

	// Cross-project registration is refused on the wire: alpha's credential
	// claiming beta (and beta's claiming alpha) are PermissionDenied before
	// any join.
	for _, tc := range []struct{ cred, header string }{
		{cred: "tok-alpha", header: "cred-beta"},
		{cred: "tok-beta", header: "cred-alpha"},
	} {
		req := connect.NewRequest(&pb.RegisterRequest{Name: "imposter"})
		h := req.Header()
		h.Set("Authorization", "Bearer "+tc.cred)
		h.Set(auth.HeaderAgentProject, tc.header)
		if _, err := oClient.Register(ctx, req); connect.CodeOf(err) != connect.CodePermissionDenied {
			t.Fatalf("credential %s header %s: got %v, want PermissionDenied", tc.cred, tc.header, err)
		}
	}

	// Fail closed on a claim without a project row: provisioning the secret
	// does not conjure the project ledger entry, so the registration answers
	// NotFound instead of joining anything.
	if err := provision.ProvisionProjectAgentCredential(ctx, ts.store, "ghost-pool", "tok-ghost", now); err != nil {
		t.Fatalf("provision ghost credential: %v", err)
	}
	ghostReq := connect.NewRequest(&pb.RegisterRequest{Name: "ghost-runner"})
	ghostReq.Header().Set("Authorization", "Bearer tok-ghost")
	if _, err := oClient.Register(ctx, ghostReq); connect.CodeOf(err) != connect.CodeNotFound {
		t.Fatalf("ghost-project registration = %v, want NotFound", err)
	}
}

// TestAgentProjectCredentialProductionGate proves the credential is
// production admission: no anon-register dev flag, no bootstrap header — a
// configured credential registers, a revoked (rotated) credential does not.
func TestAgentProjectCredentialProductionGate(t *testing.T) {
	ts := newTestStack(t)
	_, oClient, _ := ts.startServer(t,
		connect.WithInterceptors(auth.NewInterceptor(ts.store, false, auth.WithBootstrapToken("boot-secret"))),
	)
	ctx := context.Background()

	now := time.Now().UTC()
	// The credential is an admission gate, not an existence gate: the
	// project row itself must be created through normal means first.
	if err := ts.store.CreateProject(ctx, &store.Project{
		ID:               "gate-pool",
		MembershipPolicy: store.ProjectMembershipOpen,
		CreatedAt:        now,
	}); err != nil {
		t.Fatalf("create gate-pool project: %v", err)
	}
	if err := provision.ProvisionProjectAgentCredential(ctx, ts.store, "gate-pool", "tok-gate-1", now); err != nil {
		t.Fatalf("provision: %v", err)
	}

	gateID, _ := registerAgentCredential(t, ctx, oClient, "gate-pool", "tok-gate-1", "", false)
	if _, err := ts.store.GetOverseer(ctx, gateID); err != nil {
		t.Fatalf("registered agent row missing: %v", err)
	}

	// Rotate: the old token material is dead, presenting it again is
	// unauthenticated (the stack runs the bootstrap gate, and the old
	// credential presents no bootstrap header).
	if err := provision.ProvisionProjectAgentCredential(ctx, ts.store, "gate-pool", "tok-gate-2", now); err != nil {
		t.Fatalf("rotate: %v", err)
	}
	stale := connect.NewRequest(&pb.RegisterRequest{Name: "stale-runner"})
	stale.Header().Set("Authorization", "Bearer tok-gate-1")
	if _, err := oClient.Register(ctx, stale); connect.CodeOf(err) != connect.CodeUnauthenticated {
		t.Fatalf("stale credential register = %v, want Unauthenticated", err)
	}
	second := connect.NewRequest(&pb.RegisterRequest{Name: "gate-pool-runner-2"})
	second.Header().Set("Authorization", "Bearer tok-gate-2")
	if _, err := oClient.Register(ctx, second); err != nil {
		t.Fatalf("rotated credential register: %v", err)
	}

	// The operator row itself keys the credential: no overseer row named
	// operator-gate-pool was minted from registration — only agent rows.
	if _, err := ts.store.GetOverseer(ctx, "operator-gate-pool"); err == nil {
		t.Fatal("operator id must not double as an overseer row")
	}
}