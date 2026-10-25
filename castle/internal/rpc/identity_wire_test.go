// KB-333 wire acceptance: the CastleService.GetCallerIdentity surface. Every
// authenticated identity reads exactly its OWN identity — unified operator
// attributes (role/display) and the membership projection of the shared
// membership set — regardless of the credential class it arrived through:
// a console login session, an operator token, and the legacy protocol-token
// classes all answer with the same shape. The membership scope mirrors the
// run read scope exactly (membershipScopedCaller), and anonymous callers
// stay denied.
package rpc

import (
	"context"
	"slices"
	"testing"

	"connectrpc.com/connect"

	castlev1 "github.com/brokenbots/castle/castle/gen/castle/v1"
	"github.com/brokenbots/castle/castle/internal/auth"
	"github.com/brokenbots/castle/castle/internal/store"
)

// TestIdentityAdminLoginSession verifies the console-login leg: the seeded
// admin operator's session answers role=admin, the login's display
// attribution, and membership_scoped=false — the admin UI gates everything
// off the ROLE, never off the credential class.
func TestIdentityAdminLoginSession(t *testing.T) {
	f := newRunScopeFixture(t)

	resp, err := identityClient(f.tsrv).GetCallerIdentity(context.Background(),
		bearer(&castlev1.GetCallerIdentityRequest{}, f.consoleToken))
	if err != nil {
		t.Fatalf("admin login GetCallerIdentity: %v", err)
	}
	id := resp.Msg
	if id.GetRole() != store.OperatorRoleAdmin {
		t.Fatalf("admin login identity role = %q, want %q", id.GetRole(), store.OperatorRoleAdmin)
	}
	if id.GetMembershipScoped() {
		t.Fatalf("admin login identity membership_scoped = true, want false")
	}
	attrs := id.GetAttributes()
	if attrs[store.OperatorAttributeRole] != store.OperatorRoleAdmin {
		t.Fatalf("admin login identity attributes role = %q, want %q", attrs[store.OperatorAttributeRole], store.OperatorRoleAdmin)
	}
	if attrs[store.OperatorAttributeDisplay] != "operator" {
		t.Fatalf("admin login identity display = %q, want the login's display attribution", attrs[store.OperatorAttributeDisplay])
	}
	if id.GetOperatorId() != "console-default" {
		t.Fatalf("admin login identity operator_id = %q, want the console operator row id", id.GetOperatorId())
	}
	if id.GetName() != "operator" {
		t.Fatalf("admin login identity name = %q, want the login username", id.GetName())
	}
}

// TestIdentityAdminOperatorTokenSameIdentity pins the second admin session
// path from the workstream: an admin-role operator token (the KB-330
// operator_tokens space) answers the SAME attributed identity through the
// token credential — role=admin, membership_scoped=false — so a UI session
// driven by an admin token keys off attributes, not the credential class
// (KB-331 retired the provisioning flag; the token space itself stays).
func TestIdentityAdminOperatorTokenSameIdentity(t *testing.T) {
	f := newRunScopeFixture(t)
	ctx := context.Background()

	admin := &store.Operator{
		ID:          "admin-op",
		Name:        "ops-admin",
		DisplayName: "Ops Admin",
		Attributes: map[string]string{
			store.OperatorAttributeRole:    store.OperatorRoleAdmin,
			store.OperatorAttributeDisplay: "Ops Admin",
		},
	}
	if err := f.ts.store.UpsertOperator(ctx, admin); err != nil {
		t.Fatalf("seed admin operator: %v", err)
	}
	adminToken := "admin-op-token"
	if err := f.ts.store.UpsertOperatorToken(ctx, &store.OperatorToken{
		ID: "admin-op-tok", OperatorID: admin.ID, TokenHash: auth.HashToken(adminToken),
	}); err != nil {
		t.Fatalf("seed admin operator token: %v", err)
	}

	resp, err := identityClient(f.tsrv).GetCallerIdentity(ctx, bearer(&castlev1.GetCallerIdentityRequest{}, adminToken))
	if err != nil {
		t.Fatalf("admin operator token GetCallerIdentity: %v", err)
	}
	id := resp.Msg
	if id.GetRole() != store.OperatorRoleAdmin || id.GetMembershipScoped() {
		t.Fatalf("admin token identity = (role %q, scoped %v), want (admin, unscoped)", id.GetRole(), id.GetMembershipScoped())
	}
	if id.GetDisplayName() != "Ops Admin" || id.GetName() != "ops-admin" {
		t.Fatalf("admin token identity display/name = (%q, %q), want the seeded row", id.GetDisplayName(), id.GetName())
	}
	// Same surface reach the login has: the project-admin gates key on the
	// role attribute for this credential too — the membership write shape
	// only an admin gate admits, then the matching eviction.
	if _, err := f.pClient.AddProjectMember(ctx, bearer(&castlev1.AddProjectMemberRequest{
		ProjectId: "squad", MemberId: f.outsiderID, Kind: castlev1.ProjectMemberKind_PROJECT_MEMBER_KIND_AI,
	}, adminToken)); err != nil {
		t.Fatalf("admin token AddProjectMember: %v", err)
	}
	if _, err := f.pClient.RemoveProjectMember(ctx, bearer(&castlev1.RemoveProjectMemberRequest{
		ProjectId: "squad", MemberId: f.outsiderID,
	}, adminToken)); err != nil {
		t.Fatalf("admin token RemoveProjectMember: %v", err)
	}
}

// TestIdentityScopedAgentAttributes verifies the agent leg: role=agent,
// membership_scoped=true, and the membership list exactly the shared
// membership set (the member agent of the fixture project lists "squad";
// the non-member outsider lists nothing). The scoped run view stays
// unchanged — the identity read adds no visibility.
func TestIdentityScopedAgentAttributes(t *testing.T) {
	f := newRunScopeFixture(t)

	member, err := identityClient(f.tsrv).GetCallerIdentity(context.Background(),
		bearer(&castlev1.GetCallerIdentityRequest{}, f.memberToken))
	if err != nil {
		t.Fatalf("member agent GetCallerIdentity: %v", err)
	}
	if got := member.Msg.GetRole(); got != store.OperatorRoleAgent {
		t.Fatalf("member agent identity role = %q, want %q", got, store.OperatorRoleAgent)
	}
	if !member.Msg.GetMembershipScoped() {
		t.Fatalf("member agent identity membership_scoped = false, want true")
	}
	if got := member.Msg.GetMembership(); len(got) != 1 || got[0] != "squad" {
		t.Fatalf("member agent identity membership = %v, want [squad]", got)
	}
	if got := member.Msg.GetAttributes()[store.OperatorAttributeRole]; got != store.OperatorRoleAgent {
		t.Fatalf("member agent attributes role = %q, want %q", got, store.OperatorRoleAgent)
	}

	outsider, err := identityClient(f.tsrv).GetCallerIdentity(context.Background(),
		bearer(&castlev1.GetCallerIdentityRequest{}, f.outsiderToken))
	if err != nil {
		t.Fatalf("outsider agent GetCallerIdentity: %v", err)
	}
	if got := outsider.Msg.GetMembership(); len(got) != 0 {
		t.Fatalf("outsider identity membership = %v, want no membership rows", got)
	}
	if !outsider.Msg.GetMembershipScoped() {
		t.Fatalf("outsider identity membership_scoped = false, want true")
	}
	// The membership read stays a projection of the read scope: the
	// outsider still sees only its own run.
	if seen := listRunIDs(t, f.sClient, f.outsiderToken); len(seen) != 1 || !seen[f.standaloneRun] {
		t.Fatalf("outsider ListRuns = %v, want exactly its standalone run", seen)
	}
}

// TestIdentityUnscopedRolesNotScoped verifies the orchestrator leg (role
// reads its whole space, no membership scoping) alongside the shared-owner
// membership detail: membership is display metadata for unscoped roles.
func TestIdentityUnscopedOrchestratorAttributes(t *testing.T) {
	f := newRunScopeFixture(t)
	ctx := context.Background()

	orchToken := "orch-token-for-identity"
	provisionOrchestratorIdentity(t, f.ts.store, "orch-identity", orchToken)
	resp, err := identityClient(f.tsrv).GetCallerIdentity(ctx, bearer(&castlev1.GetCallerIdentityRequest{}, orchToken))
	if err != nil {
		t.Fatalf("orchestrator GetCallerIdentity: %v", err)
	}
	id := resp.Msg
	if id.GetRole() != store.OperatorRoleOrchestrator {
		t.Fatalf("orchestrator identity role = %q, want %q", id.GetRole(), store.OperatorRoleOrchestrator)
	}
	if id.GetMembershipScoped() || len(id.GetMembership()) != 0 {
		t.Fatalf("orchestrator identity scope = (%v, membership %v), want unscoped with no rows", id.GetMembershipScoped(), id.GetMembership())
	}
}

// TestIdentityLegacyDirectCallSynthesis pins the pre-merge direct-call path:
// a context injecting only the legacy kind keys (no operator identity)
// synthesizes the same attributed shape — role from the kind key, identity
// fields from the kind id — so unwired handlers keep their exact
// pre-operators effective identity (the CallerOperatorRole counterpart).
func TestIdentityLegacyDirectCallSynthesis(t *testing.T) {
	f := newRunScopeFixture(t)

	resp, err := f.ts.server.GetCallerIdentity(auth.WithCallerCriteriaID(context.Background(), f.ownerID),
		connect.NewRequest(&castlev1.GetCallerIdentityRequest{}))
	if err != nil {
		t.Fatalf("legacy direct-call GetCallerIdentity: %v", err)
	}
	id := resp.Msg
	if id.GetOperatorId() != f.ownerID || id.GetRole() != store.OperatorRoleAgent {
		t.Fatalf("legacy identity = (id %q, role %q), want (%q, agent)", id.GetOperatorId(), id.GetRole(), f.ownerID)
	}
	if !id.GetMembershipScoped() || !slices.Equal(id.GetMembership(), []string{"squad"}) {
		t.Fatalf("legacy identity scope = (%v, membership %v), want scoped [squad]", id.GetMembershipScoped(), id.GetMembership())
	}

	// The bare context (no operator, no kind key) stays fail-closed.
	if _, err := f.ts.server.GetCallerIdentity(context.Background(), connect.NewRequest(&castlev1.GetCallerIdentityRequest{})); connect.CodeOf(err) != connect.CodeUnauthenticated {
		t.Fatalf("bare direct-call GetCallerIdentity err = %v, want Unauthenticated", err)
	}
}

// TestIdentityAnonymousDenied denies what the workstream forbids: the identity
// read carries no way to read ANOTHER operator's identity (no request field
// exists), and an ANONYMOUS caller gets UNAUTHENTICATED rather than an
// empty-shape identity response. A revoked/expired credential similarly
// stops resolving (covered by the interceptor's token validation).
func TestIdentityAnonymousDenied(t *testing.T) {
	f := newRunScopeFixture(t)

	_, err := identityClient(f.tsrv).GetCallerIdentity(context.Background(), connect.NewRequest(&castlev1.GetCallerIdentityRequest{}))
	if connect.CodeOf(err) != connect.CodeUnauthenticated {
		t.Fatalf("anonymous GetCallerIdentity err = %v, want Unauthenticated", err)
	}

	// A caller may fetch its own identity through the membership it holds;
	// the response never contains another operator's membership. Pin the
	// projection against the store's own membership listing so the UI's
	// scoped-empty diagnostic can never disagree with the server.
	ctx := context.Background()
	storeRows, err := f.ts.store.ListMemberProjects(ctx, f.memberID)
	if err != nil {
		t.Fatalf("store ListMemberProjects: %v", err)
	}
	resp, err := identityClient(f.tsrv).GetCallerIdentity(ctx, bearer(&castlev1.GetCallerIdentityRequest{}, f.memberToken))
	if err != nil {
		t.Fatalf("member GetCallerIdentity re-read: %v", err)
	}
	if !slices.Equal(resp.Msg.GetMembership(), storeRows) {
		t.Fatalf("identity membership %v diverges from the shared membership set %v", resp.Msg.GetMembership(), storeRows)
	}
}
