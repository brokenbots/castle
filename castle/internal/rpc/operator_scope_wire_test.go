package rpc

import (
	"context"
	"testing"
	"time"

	"github.com/brokenbots/castle/castle/internal/auth"
	"github.com/brokenbots/castle/castle/internal/store"

	pb "github.com/brokenbots/criteria/sdk/pb/criteria/v1"
	"github.com/brokenbots/criteria/sdk/pb/criteria/v1/criteriav1connect"
)

// operatorScopeFixture provisions operator bearer tokens over the KB-324
// read-visibility matrix: an admin-role operator, an agent-role operator
// with no memberships, and an orchestrator-role operator, each with an
// operator token issued through the unified credential space.
type operatorScopeFixture struct {
	*runScopeFixture
	adminToken string
	agentToken string
	orchToken  string
}

func newOperatorScopeFixture(t *testing.T) *operatorScopeFixture {
	t.Helper()
	ctx := context.Background()
	f := &operatorScopeFixture{runScopeFixture: newRunScopeFixture(t)}

	issue := func(id, role string) string {
		t.Helper()
		if err := f.ts.store.UpsertOperator(ctx, &store.Operator{
			ID: id, Name: id + "-name", DisplayName: "Operator " + id,
			Attributes: map[string]string{store.OperatorAttributeRole: role},
			CreatedAt:  time.Now().UTC(), UpdatedAt: time.Now().UTC(),
		}); err != nil {
			t.Fatalf("upsert operator %s: %v", id, err)
		}
		secret := "operator-bearer-" + id
		if err := f.ts.store.UpsertOperatorToken(ctx, &store.OperatorToken{
			ID: id + "-token", OperatorID: id, TokenHash: auth.HashToken(secret),
			CreatedAt: time.Now().UTC(),
		}); err != nil {
			t.Fatalf("upsert operator token %s: %v", id, err)
		}
		return secret
	}

	f.adminToken = issue("operator-admin-1", store.OperatorRoleAdmin)
	f.agentToken = issue("operator-agent-1", store.OperatorRoleAgent)
	f.orchToken = issue("operator-orch-1", store.OperatorRoleOrchestrator)
	return f
}

// TestOperatorAttributesDriveRunReadScope pins the KB-330 privilege rule on
// the wire: read scope derives from the operator's role attribute, not from
// a credential class. The admin-role operator token observes the whole
// space like the console session, the agent-role operator token stays
// membership-scoped, and the orchestrator-role token's whole-space read
// matches the legacy orchestrator posture.
func TestOperatorAttributesDriveRunReadScope(t *testing.T) {
	f := newOperatorScopeFixture(t)
	ctx := context.Background()

	// Admin-role operator token: see-all across memberships, like the
	// console session carrying the same role.
	for _, runID := range []string{f.scopedRun, f.standaloneRun} {
		if _, err := f.sClient.GetRun(ctx, bearer(&pb.GetRunRequest{RunId: runID}, f.adminToken)); err != nil {
			t.Fatalf("admin operator token GetRun %s: %v", runID, err)
		}
	}
	adminIDs := listRunIDs(t, f.sClient, f.adminToken)
	for _, want := range []string{f.scopedRun, f.standaloneRun} {
		if !adminIDs[want] {
			t.Errorf("admin operator token ListRuns missing %q", want)
		}
	}

	// Agent-role operator token with no memberships: every run is
	// NotFound / absent from listings, exactly like a scoped agent.
	if _, err := f.sClient.GetRun(ctx, bearer(&pb.GetRunRequest{RunId: f.scopedRun}, f.agentToken)); err == nil {
		t.Error("memberless agent operator token must not read the project run")
	}
	if _, err := f.sClient.GetRun(ctx, bearer(&pb.GetRunRequest{RunId: f.standaloneRun}, f.agentToken)); err == nil {
		t.Error("memberless agent operator token must not read the outsider run")
	}
	agentIDs := listRunIDs(t, f.sClient, f.agentToken)
	if len(agentIDs) != 0 {
		t.Errorf("agent operator token ListRuns = %v, want empty", agentIDs)
	}

	// Orchestrator-role operator token: whole-space reads, matching the
	// legacy orchestrator credential posture the unification must keep.
	for _, runID := range []string{f.scopedRun, f.standaloneRun} {
		if _, err := f.sClient.GetRun(ctx, bearer(&pb.GetRunRequest{RunId: runID}, f.orchToken)); err != nil {
			t.Fatalf("orchestrator operator token GetRun %s: %v", runID, err)
		}
	}
}

func listRunIDs(t *testing.T, client criteriav1connect.ServerServiceClient, token string) map[string]bool {
	t.Helper()
	resp, err := client.ListRuns(context.Background(), bearer(&pb.ListRunsRequest{}, token))
	if err != nil {
		t.Fatalf("ListRuns: %v", err)
	}
	seen := map[string]bool{}
	for _, r := range resp.Msg.Runs {
		seen[r.RunId] = true
	}
	return seen
}
