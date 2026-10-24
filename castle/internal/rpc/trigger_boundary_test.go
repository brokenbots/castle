package rpc

import (
	"context"
	"net/http/httptest"
	"testing"
	"time"

	"connectrpc.com/connect"

	castlev1 "github.com/brokenbots/castle/castle/gen/castle/v1"
	"github.com/brokenbots/castle/castle/internal/auth"
	"github.com/brokenbots/castle/castle/internal/store"
	pb "github.com/brokenbots/criteria/sdk/pb/criteria/v1"
	criteriav1connect "github.com/brokenbots/criteria/sdk/pb/criteria/v1/criteriav1connect"
)

// newTriggerHarness provisions a trigger identity (KB-320) against a test
// server with the auth interceptor wired and the anon-register surface on.
func newTriggerHarness(t *testing.T) (*testStack, *httptest.Server, criteriav1connect.CriteriaServiceClient, criteriav1connect.ServerServiceClient) {
	t.Helper()
	ts := newTestStack(t)
	tsrv, oClient, cClient := ts.startServer(t, connect.WithInterceptors(
		auth.NewInterceptor(ts.store, true, auth.WithAnonRegister()),
	))
	if err := ts.store.UpsertTrigger(context.Background(), &store.Trigger{
		ID:        "trigger-kanboard",
		TokenHash: auth.HashToken("trigger-test-token"),
		CreatedAt: time.Now().UTC(),
	}); err != nil {
		t.Fatalf("provision trigger identity: %v", err)
	}
	return ts, tsrv, oClient, cClient
}

// triggerBearer returns a request carrying the kanboard trigger token.
func triggerBearer[T any](msg *T) *connect.Request[T] {
	return bearer(msg, "trigger-test-token")
}

func isPermissionDenied(err error) bool {
	return err != nil && connect.CodeOf(err) == connect.CodePermissionDenied
}

// TestTriggerBoundarySubmitOnlyAndOwned pins the trigger auth boundary
// (KB-320): the submit-only allowlist passes with the trigger's own token
// (assignments owned by the trigger identity), while agent procedures,
// registration, and project administration are PermissionDenied.
func TestTriggerBoundarySubmitOnlyAndOwned(t *testing.T) {
	ts, tsrv, oClient, cClient := newTriggerHarness(t)
	ctx := context.Background()
	pClient := projectClient(tsrv)

	// The project to submit into: admin is agent/console territory, so the
	// target project is created by a regular operator identity.
	opReg, err := oClient.Register(ctx, connect.NewRequest(&pb.RegisterRequest{Name: "op"}))
	if err != nil {
		t.Fatalf("register operator: %v", err)
	}
	createProject(t, ctx, pClient, opReg.Msg.Token, "kb", castlev1.ProjectMembershipPolicy_PROJECT_MEMBERSHIP_POLICY_OPEN)

	// Allowed: the project submit surface (the adapter's fire path).
	resp, err := pClient.SubmitProjectAssignment(ctx, triggerBearer(&castlev1.SubmitProjectAssignmentRequest{
		ProjectId:      "kb",
		WorkflowName:   "kb-develop",
		WorkflowSource: "hermes",
		IdempotencyKey: "kanboard:KB board:KB-5:1",
		InputValues:    map[string]string{"ticket_id": "KB-5", "repo_url": "own/name"},
	}))
	if err != nil {
		t.Fatalf("trigger submit: %v", err)
	}
	if resp.Msg.GetState() != pb.WorkflowAssignmentState_WORKFLOW_ASSIGNMENT_STATE_QUEUED {
		t.Fatalf("state = %v, want QUEUED", resp.Msg.GetState())
	}
	a, err := ts.store.GetWorkflowAssignmentByRunID(ctx, resp.Msg.GetRunId())
	if err != nil {
		t.Fatalf("get assignment: %v", err)
	}
	if a.OwnerCriteriaID != "trigger-kanboard" {
		t.Fatalf("assignment owner = %q, want the trigger identity", a.OwnerCriteriaID)
	}

	// Allowed: mirror surfaces (disposition + event stream). The batched
	// event read stays denied (asserted below): the adapter mirrors through
	// WatchRun only.
	disp, err := cClient.GetAssignmentDisposition(ctx, triggerBearer(&pb.GetAssignmentDispositionRequest{
		RunId: resp.Msg.GetRunId(),
	}))
	if err != nil || disp.Msg.GetState() != pb.WorkflowAssignmentState_WORKFLOW_ASSIGNMENT_STATE_QUEUED {
		t.Fatalf("disposition read as trigger: %v state %v", err, disp.Msg.GetState())
	}
	wReq := connect.NewRequest(&pb.WatchRunRequest{RunId: resp.Msg.GetRunId(), SubscriberId: "kanboard-trigger-adapter:test"})
	wReq.Header().Set("Authorization", "Bearer trigger-test-token")
	stream, err := cClient.WatchRun(ctx, wReq)
	if err != nil {
		t.Fatalf("watch run as trigger: %v", err)
	}
	stream.Close()

	// Denied: agent procedures, agent registration, and project admin.
	if _, err := cClient.ListRunEvents(ctx, triggerBearer(&pb.ListRunEventsRequest{RunId: resp.Msg.GetRunId()})); !isPermissionDenied(err) {
		t.Errorf("ListRunEvents as trigger: err = %v, want PermissionDenied (the adapter mirrors through WatchRun only)", err)
	}
	if _, err := oClient.CreateRun(ctx, triggerBearer(&pb.CreateRunRequest{CriteriaId: "trigger-kanboard", WorkflowName: "wf"})); !isPermissionDenied(err) {
		t.Errorf("CreateRun as trigger: err = %v, want PermissionDenied", err)
	}
	if _, err := oClient.Register(ctx, triggerBearer(&pb.RegisterRequest{Name: "rogue"})); !isPermissionDenied(err) {
		t.Errorf("Register as trigger: err = %v, want PermissionDenied", err)
	}
	if _, err := pClient.CreateProject(ctx, triggerBearer(&castlev1.CreateProjectRequest{
		Project: &castlev1.Project{Id: "rogue"},
	})); !isPermissionDenied(err) {
		t.Errorf("CreateProject as trigger: err = %v, want PermissionDenied", err)
	}
}
