package rpc

import (
	"context"
	"fmt"
	"testing"
	"time"

	"connectrpc.com/connect"

	pb "github.com/brokenbots/criteria/sdk/pb/criteria/v1"
	criteriav1connect "github.com/brokenbots/criteria/sdk/pb/criteria/v1/criteriav1connect"

	castlev1 "github.com/brokenbots/castle/castle/gen/castle/v1"
	castlev1connect "github.com/brokenbots/castle/castle/gen/castle/v1/castlev1connect"
	"github.com/brokenbots/castle/castle/internal/auth"
	"github.com/brokenbots/castle/castle/internal/store"
)

// projectRunsFixture is the KB-340 project console runs-page contract
// fixture: a console admin session, two admitted AI members (owner and
// member) and one memberless outsider, two project-scoped assignment runs
// owned by the owner, and one unscoped run each for the member and the
// outsider.
type projectRunsFixture struct {
	ts            *testStack
	oClient       criteriav1connect.CriteriaServiceClient
	pClient       castlev1connect.ProjectServiceClient
	console       string
	ownerToken    string
	ownerID       string
	memberToken   string
	memberID      string
	outsiderToken string
	outsiderID    string
	orchToken     string
	agentOpTk     string
	scopedRuns    []string
	memberRun     string
	outsiderRun   string
}

func newProjectRunsFixture(t *testing.T) *projectRunsFixture {
	t.Helper()
	ctx := context.Background()
	ts := newTestStack(t)
	tsrv, oClient, _ := ts.startServer(t,
		connect.WithInterceptors(auth.NewInterceptor(ts.store, false, auth.WithAnonRegister())),
	)
	f := &projectRunsFixture{ts: ts, oClient: oClient, pClient: projectClient(tsrv)}

	reg := func(name string) (string, string) {
		t.Helper()
		r, err := oClient.Register(ctx, connect.NewRequest(&pb.RegisterRequest{Name: name}))
		if err != nil {
			t.Fatalf("register %s: %v", name, err)
		}
		return r.Msg.CriteriaId, r.Msg.Token
	}
	f.ownerID, f.ownerToken = reg("runs-owner")
	f.memberID, f.memberToken = reg("runs-member")
	f.outsiderID, f.outsiderToken = reg("runs-outsider")
	f.console = consoleOperatorSession(t, ts, tsrv, "operator", "op-password")

	issue := func(id, role string) string {
		t.Helper()
		if err := ts.store.UpsertOperator(ctx, &store.Operator{
			ID: id, Name: id, Attributes: map[string]string{store.OperatorAttributeRole: role},
			CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC(),
		}); err != nil {
			t.Fatalf("upsert operator %s: %v", id, err)
		}
		secret := "bearer-" + id
		if err := ts.store.UpsertOperatorToken(ctx, &store.OperatorToken{
			ID: id + "-token", OperatorID: id, TokenHash: auth.HashToken(secret),
			CreatedAt: time.Now().UTC(),
		}); err != nil {
			t.Fatalf("upsert operator token %s: %v", id, err)
		}
		return secret
	}
	f.orchToken = issue("runs-orch-op", store.OperatorRoleOrchestrator)
	f.agentOpTk = issue("runs-agent-op", store.OperatorRoleAgent)

	createProject(t, ctx, f.pClient, f.console, "squad", castlev1.ProjectMembershipPolicy_PROJECT_MEMBERSHIP_POLICY_OPEN)
	for _, id := range []string{f.ownerID, f.memberID} {
		if _, err := f.pClient.AddProjectMember(ctx, bearer(&castlev1.AddProjectMemberRequest{
			ProjectId: "squad", MemberId: id, Kind: castlev1.ProjectMemberKind_PROJECT_MEMBER_KIND_AI,
		}, f.console)); err != nil {
			t.Fatalf("console admits %s: %v", id, err)
		}
	}

	for i, key := range []string{"deploy-1", "deploy-2"} {
		sub, err := f.pClient.SubmitProjectAssignment(ctx, bearer(&castlev1.SubmitProjectAssignmentRequest{
			ProjectId:      "squad",
			WorkflowName:   fmt.Sprintf("ship-%d", i+1),
			WorkflowSource: "hcl-source",
			IdempotencyKey: key,
		}, f.ownerToken))
		if err != nil {
			t.Fatalf("submit project assignment %s: %v", key, err)
		}
		f.scopedRuns = append(f.scopedRuns, sub.Msg.RunId)
	}

	f.memberRun = f.createUnscopedRun(t, ctx, f.memberID, f.memberToken, "member-solo")
	f.outsiderRun = f.createUnscopedRun(t, ctx, f.outsiderID, f.outsiderToken, "outsider-solo")
	return f
}

func (f *projectRunsFixture) createUnscopedRun(t *testing.T, ctx context.Context, criteriaID, token, workflow string) string {
	t.Helper()
	resp, err := f.oClient.CreateRun(ctx, bearer(&pb.CreateRunRequest{
		CriteriaId: criteriaID, WorkflowName: workflow,
	}, token))
	if err != nil {
		t.Fatalf("create unscoped run %s: %v", workflow, err)
	}
	return resp.Msg.RunId
}

func (f *projectRunsFixture) listIDs(t *testing.T, ctx context.Context, token string, req *castlev1.ListProjectRunsRequest) (map[string]bool, string) {
	t.Helper()
	resp, err := f.pClient.ListProjectRuns(ctx, bearer(req, token))
	if err != nil {
		t.Fatalf("ListProjectRuns: %v", err)
	}
	ids := map[string]bool{}
	for _, r := range resp.Msg.GetRuns() {
		ids[r.GetRunId()] = true
	}
	return ids, resp.Msg.GetNextPageToken()
}

func (f *projectRunsFixture) assertContains(t *testing.T, got map[string]bool, want []string, label string) {
	t.Helper()
	for _, id := range want {
		if !got[id] {
			t.Errorf("%s missing run %q", label, id)
		}
	}
}

// TestProjectRunsWireContract pins the KB-340 project console runs page
// over the wire: the console session defaults to the project's own runs and
// may widen to the legacy unscoped arm, admitted agents read exactly their
// project's runs and never widen membership scope, a memberless agent is
// refused (the wire shape the UI needs to surface no-membership distinctly
// from no-data), anonymous callers get unauthenticated, and role gates hold
// (orchestrator refused; agent-role operator refused until it holds the
// membership it presents).
func TestProjectRunsWireContract(t *testing.T) {
	f := newProjectRunsFixture(t)
	ctx := context.Background()
	proj := "squad"

	// Console (admin role): the default page is the project's own runs only.
	ids, next := f.listIDs(t, ctx, f.console, &castlev1.ListProjectRunsRequest{ProjectId: proj})
	if next != "" {
		t.Fatalf("default console page claims a cursor %q with no limit", next)
	}
	f.assertContains(t, ids, f.scopedRuns, "console default")
	for _, stray := range []string{f.memberRun, f.outsiderRun} {
		if ids[stray] {
			t.Errorf("console default page leaked unscoped run %q", stray)
		}
	}

	// Console widen: include_unscoped adds the legacy unscoped runs of every
	// owner (admin posture mirrors ListRunsUnscoped).
	ids, _ = f.listIDs(t, ctx, f.console, &castlev1.ListProjectRunsRequest{
		ProjectId: proj, IncludeUnscoped: true,
	})
	f.assertContains(t, ids, f.scopedRuns, "console widened")
	f.assertContains(t, ids, []string{f.memberRun, f.outsiderRun}, "console widened")

	// Unknown but well-formed project id: the admin page is honestly empty,
	// not an error (empty-state honesty mirrors GetProject).
	ids, _ = f.listIDs(t, ctx, f.console, &castlev1.ListProjectRunsRequest{ProjectId: "ghost-project"})
	if len(ids) != 0 {
		t.Fatalf("unknown project listed %v runs, want none", ids)
	}

	// Admitted agent: exactly the project's runs; membership scope never widens.
	ids, _ = f.listIDs(t, ctx, f.ownerToken, &castlev1.ListProjectRunsRequest{ProjectId: proj})
	f.assertContains(t, ids, f.scopedRuns, "owner default")
	if len(ids) != len(f.scopedRuns) {
		t.Fatalf("owner page = %v runs, want exactly %v", ids, f.scopedRuns)
	}
	if _, err := f.pClient.ListProjectRuns(ctx, bearer(&castlev1.ListProjectRunsRequest{
		ProjectId: proj, IncludeUnscoped: true,
	}, f.ownerToken)); connect.CodeOf(err) != connect.CodePermissionDenied {
		t.Fatalf("owner include_unscoped: got %v, want PermissionDenied", err)
	}

	// Member: same membership arm as the owner.
	ids, _ = f.listIDs(t, ctx, f.memberToken, &castlev1.ListProjectRunsRequest{ProjectId: proj})
	f.assertContains(t, ids, f.scopedRuns, "member default")
	if len(ids) != len(f.scopedRuns) {
		t.Fatalf("member page = %v runs, want exactly %v", ids, f.scopedRuns)
	}

	// Memberless agent: refused on membership, before any run reads.
	if _, err := f.pClient.ListProjectRuns(ctx, bearer(&castlev1.ListProjectRunsRequest{
		ProjectId: proj,
	}, f.outsiderToken)); connect.CodeOf(err) != connect.CodePermissionDenied {
		t.Fatalf("outsider page: got %v, want PermissionDenied", err)
	}

	// Agent-role operator token (protocol credential, non-member): refused.
	if _, err := f.pClient.ListProjectRuns(ctx, bearer(&castlev1.ListProjectRunsRequest{
		ProjectId: proj,
	}, f.agentOpTk)); connect.CodeOf(err) != connect.CodePermissionDenied {
		t.Fatalf("agent-role operator non-member: got %v, want PermissionDenied", err)
	}

	// Orchestrator role: project-surface reads are not orchestrator scope.
	if _, err := f.pClient.ListProjectRuns(ctx, bearer(&castlev1.ListProjectRunsRequest{
		ProjectId: proj,
	}, f.orchToken)); connect.CodeOf(err) != connect.CodePermissionDenied {
		t.Fatalf("orchestrator role: got %v, want PermissionDenied", err)
	}

	// Anonymous (no token): the project runs page is never part of the
	// anonymously-readable surface (KB-326).
	if _, err := f.pClient.ListProjectRuns(ctx, &connect.Request[castlev1.ListProjectRunsRequest]{
		Msg: &castlev1.ListProjectRunsRequest{ProjectId: proj},
	}); connect.CodeOf(err) != connect.CodeUnauthenticated {
		t.Fatalf("anonymous page: got %v, want Unauthenticated", err)
	}
}

// TestProjectRunsWireArguments covers the argument and state arms the
// handler enforces above the store contract: bad or missing project ids,
// the negative limit, the status filter, cursor paging, and the unwired
// direct-call pass-through (no auth interceptor in the request context).
func TestProjectRunsWireArguments(t *testing.T) {
	f := newProjectRunsFixture(t)
	ctx := context.Background()
	proj := "squad"

	// Invalid project slug.
	if _, err := f.pClient.ListProjectRuns(ctx, bearer(&castlev1.ListProjectRunsRequest{
		ProjectId: "BAD/ID",
	}, f.console)); connect.CodeOf(err) != connect.CodeInvalidArgument {
		t.Fatalf("invalid project id: got %v, want InvalidArgument", err)
	}

	// Missing project id.
	if _, err := f.pClient.ListProjectRuns(ctx, bearer(&castlev1.ListProjectRunsRequest{}, f.console)); connect.CodeOf(err) != connect.CodeInvalidArgument {
		t.Fatalf("missing project id: got %v, want InvalidArgument", err)
	}

	// Negative limit.
	if _, err := f.pClient.ListProjectRuns(ctx, bearer(&castlev1.ListProjectRunsRequest{
		ProjectId: proj, Limit: -1,
	}, f.console)); connect.CodeOf(err) != connect.CodeInvalidArgument {
		t.Fatalf("negative limit: got %v, want InvalidArgument", err)
	}

	// Malformed cursor (the request comment promises malformed tokens are
	// rejected, not ignored).
	if _, err := f.pClient.ListProjectRuns(ctx, bearer(&castlev1.ListProjectRunsRequest{
		ProjectId: proj, PageToken: "not-a-cursor",
	}, f.console)); connect.CodeOf(err) != connect.CodeInvalidArgument {
		t.Fatalf("malformed cursor: got %v, want InvalidArgument", err)
	}

	// Status filter: mark one scoped run failed and filter by that status.
	markRunStatus(t, f.ts, f.scopedRuns[0], "failed")
	ids, _ := f.listIDs(t, ctx, f.console, &castlev1.ListProjectRunsRequest{ProjectId: proj, Status: "failed"})
	if len(ids) != 1 || !ids[f.scopedRuns[0]] {
		t.Fatalf("status filtered page = %v, want exactly {%s}", ids, f.scopedRuns[0])
	}
	markRunStatus(t, f.ts, f.scopedRuns[0], "running")

	// Cursor paging: a one-per-page drain from the console walks the scoped
	// arm exactly once, ending with an empty cursor.
	seen := map[string]bool{}
	token := ""
	for page := 0; page < 10; page++ {
		one, next := f.listIDs(t, ctx, f.console, &castlev1.ListProjectRunsRequest{
			ProjectId: proj, Limit: 1, PageToken: token,
		})
		for id := range one {
			if seen[id] {
				t.Fatalf("run %q delivered on two pages", id)
			}
			seen[id] = true
		}
		if next == "" {
			break
		}
		token = next
	}
	if len(seen) != len(f.scopedRuns) {
		t.Fatalf("paged drain saw %v runs, want %v", seen, f.scopedRuns)
	}

	// Unwired direct call (no auth interceptor in the request context):
	// requireProjectReader passes through and the page answers — the
	// handler's own gates are the boundary, not the interceptor alone.
	resp, err := f.ts.server.ListProjectRuns(ctx, &connect.Request[castlev1.ListProjectRunsRequest]{Msg: &castlev1.ListProjectRunsRequest{ProjectId: proj}})
	if err != nil {
		t.Fatalf("unwired direct ListProjectRuns: %v", err)
	}
	if len(resp.Msg.GetRuns()) != len(f.scopedRuns) {
		t.Fatalf("direct page runs = %d, want %d", len(resp.Msg.GetRuns()), len(f.scopedRuns))
	}
}
