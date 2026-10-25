package auth

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"connectrpc.com/connect"
	"golang.org/x/net/http2"
	"golang.org/x/net/http2/h2c"

	pb "github.com/brokenbots/criteria/sdk/pb/criteria/v1"                // import-lint:allow castle service bindings (W08: move to castle-proto)
	"github.com/brokenbots/criteria/sdk/pb/criteria/v1/criteriav1connect" // import-lint:allow castle service bindings (W08: move to castle-proto)

	castlev1connect "github.com/brokenbots/castle/castle/gen/castle/v1/castlev1connect"
	"github.com/brokenbots/castle/castle/internal/store"
	"github.com/brokenbots/castle/castle/internal/store/sqlite"
)

// seedOperatorToken provisions an operator with the given role and one
// operator token (KB-330): the credential space for admin-issued bearer
// tokens. Returns the raw token.
func seedOperatorToken(t *testing.T, db *sqlite.Store, id, role string) string {
	t.Helper()
	ctx := context.Background()
	now := time.Now().UTC()
	if err := db.UpsertOperator(ctx, &store.Operator{
		ID:          id,
		Name:        id,
		DisplayName: id,
		Attributes:  map[string]string{store.OperatorAttributeRole: role},
		CreatedAt:   now,
		UpdatedAt:   now,
	}); err != nil {
		t.Fatalf("upsert operator %s: %v", id, err)
	}
	token := "operator-token-" + id
	if err := db.UpsertOperatorToken(ctx, &store.OperatorToken{
		ID:         id + "-default",
		OperatorID: id,
		TokenHash:  HashToken(token),
		CreatedAt:  now,
	}); err != nil {
		t.Fatalf("upsert operator token: %v", err)
	}
	return token
}

// TestAdminOperatorTokenTakesConsolePosture pins the KB-330 privilege model:
// an admin-role operator token is the console privilege class — the
// read-everything surface, run control, and project administration — while
// agent-owned writes and the orchestrator subscription surface stay denied,
// and the operator carries NO agent kind identity.
func TestAdminOperatorTokenTakesConsolePosture(t *testing.T) {
	db := newAuthTestStore(t)
	token := seedOperatorToken(t, db, "op-admin", store.OperatorRoleAdmin)

	var criteriaID, consoleID, role string
	call := newUnaryProbe(t, db, criteriav1connect.ServerServiceListRunsProcedure, false, func(ctx context.Context) {
		criteriaID, consoleID = CallerCriteriaID(ctx), CallerConsoleUserID(ctx)
		role = CallerOperatorRole(ctx)
	})
	if err := call(token); err != nil {
		t.Fatalf("admin operator token rejected on read surface: %v", err)
	}
	if role != store.OperatorRoleAdmin {
		t.Errorf("expected admin role attribute injected, got %q", role)
	}
	if criteriaID != "" {
		t.Errorf("admin operator must carry no agent identity, got %q", criteriaID)
	}
	if consoleID != "" {
		t.Errorf("operator tokens are not console sessions, CallerConsoleUserID=%q", consoleID)
	}

	for _, procedure := range []string{
		criteriav1connect.ServerServiceStopRunProcedure,
		castlev1connect.ConsoleServiceResolveResumeProcedure,
		castlev1connect.ProjectServiceSetProjectSettingsProcedure,
	} {
		call := newUnaryProbe(t, db, procedure, false, nil)
		if err := call(token); err != nil {
			t.Errorf("%s: admin operator token must reach the console surface, got %v", procedure, err)
		}
	}
	for _, procedure := range []string{
		criteriav1connect.CriteriaServiceCreateRunProcedure,
		criteriav1connect.ServerServiceSubmitWorkflowAssignmentProcedure,
		criteriav1connect.OrchestratorServiceCancelRunProcedure,
	} {
		call := newUnaryProbe(t, db, procedure, false, nil)
		if err := call(token); connect.CodeOf(err) != connect.CodePermissionDenied {
			t.Errorf("%s: admin operator token must stay off agent/orchestrator writes, got %v", procedure, err)
		}
	}
}

// TestAdminOperatorTokenCannotRegisterAgents pins the register gate: an
// admin-role operator (the console privilege class) can never bootstrap
// agent credentials, even in dev mode with anonymous register allowed.
func TestAdminOperatorTokenCannotRegisterAgents(t *testing.T) {
	db := newAuthTestStore(t)
	token := seedOperatorToken(t, db, "op-admin", store.OperatorRoleAdmin)
	h := connect.NewUnaryHandler(
		criteriav1connect.CriteriaServiceRegisterProcedure,
		func(context.Context, *connect.Request[pb.RegisterRequest]) (*connect.Response[pb.RegisterResponse], error) {
			return connect.NewResponse(&pb.RegisterResponse{CriteriaId: "x"}), nil
		},
		connect.WithInterceptors(NewInterceptor(db, false, WithAnonRegister())),
	)
	mux := http.NewServeMux()
	mux.Handle(criteriav1connect.CriteriaServiceRegisterProcedure, h)
	tsrv := httptest.NewUnstartedServer(h2c.NewHandler(mux, &http2.Server{}))
	tsrv.Start()
	t.Cleanup(tsrv.Close)
	client := connect.NewClient[pb.RegisterRequest, pb.RegisterResponse](httpClient(), tsrv.URL+criteriav1connect.CriteriaServiceRegisterProcedure)

	req := connect.NewRequest(&pb.RegisterRequest{Name: "sneaky"})
	req.Header().Set("Authorization", "Bearer "+token)
	_, err := client.CallUnary(context.Background(), req)
	if connect.CodeOf(err) != connect.CodePermissionDenied {
		t.Fatalf("admin operator token must not register agents, got %v", err)
	}
}

// TestAgentRoleOperatorTokenAuthenticatesAsAgent pins that an agent-role
// operator token is a full agent credential: it carries the agent kind
// identity (so ownership checks work) and reaches agent-owned writes.
func TestAgentRoleOperatorTokenAuthenticatesAsAgent(t *testing.T) {
	db := newAuthTestStore(t)
	token := seedOperatorToken(t, db, "op-agent", store.OperatorRoleAgent)

	var criteriaID, role string
	call := newUnaryProbe(t, db, criteriav1connect.CriteriaServiceCreateRunProcedure, false, func(ctx context.Context) {
		criteriaID, role = CallerCriteriaID(ctx), CallerOperatorRole(ctx)
	})
	if err := call(token); err != nil {
		t.Fatalf("agent-role operator token denied on agent write: %v", err)
	}
	if criteriaID != "op-agent" || role != store.OperatorRoleAgent {
		t.Errorf("expected agent identity op-agent with agent role, got id=%q role=%q", criteriaID, role)
	}
}

// TestTriggerRoleOperatorTokenKeepsTriggerPosture pins that a trigger-role
// operator token keeps the submit-only board-adapter boundary (KB-320 via
// KB-330): the submit surface and the WatchRun mirror stay reachable,
// agent-owned writes and register stay denied.
func TestTriggerRoleOperatorTokenKeepsTriggerPosture(t *testing.T) {
	db := newAuthTestStore(t)
	token := seedOperatorToken(t, db, "op-trg", store.OperatorRoleTrigger)

	for _, procedure := range []string{
		criteriav1connect.ServerServiceSubmitWorkflowAssignmentProcedure,
		criteriav1connect.ServerServiceWatchRunProcedure,
	} {
		call := newUnaryProbe(t, db, procedure, false, nil)
		if err := call(token); err != nil {
			t.Errorf("%s: trigger-role operator token must reach the submit-only surface, got %v", procedure, err)
		}
	}
	call := newUnaryProbe(t, db, criteriav1connect.CriteriaServiceCreateRunProcedure, false, nil)
	if err := call(token); connect.CodeOf(err) != connect.CodePermissionDenied {
		t.Errorf("trigger-role operator token must not reach agent writes, got %v", err)
	}

	h := connect.NewUnaryHandler(
		criteriav1connect.CriteriaServiceRegisterProcedure,
		func(context.Context, *connect.Request[pb.RegisterRequest]) (*connect.Response[pb.RegisterResponse], error) {
			return connect.NewResponse(&pb.RegisterResponse{CriteriaId: "x"}), nil
		},
		connect.WithInterceptors(NewInterceptor(db, false, WithAnonRegister())),
	)
	mux := http.NewServeMux()
	mux.Handle(criteriav1connect.CriteriaServiceRegisterProcedure, h)
	tsrv := httptest.NewUnstartedServer(h2c.NewHandler(mux, &http2.Server{}))
	tsrv.Start()
	t.Cleanup(tsrv.Close)
	client := connect.NewClient[pb.RegisterRequest, pb.RegisterResponse](httpClient(), tsrv.URL+criteriav1connect.CriteriaServiceRegisterProcedure)
	req := connect.NewRequest(&pb.RegisterRequest{Name: "sneaky"})
	req.Header().Set("Authorization", "Bearer "+token)
	if _, err := client.CallUnary(context.Background(), req); connect.CodeOf(err) != connect.CodePermissionDenied {
		t.Fatalf("trigger-role operator token must not register agents, got %v", err)
	}
}

// TestLegacyTokensCarryOperatorOverlay pins the compatibility contract
// (KB-330 deliverable 4): legacy class tokens keep authenticating AND the
// same operator model now backs them — each resolves to CallerOperator with
// the role its credential class always implied, without changing the kind
// keys handlers key ownership on.
func TestLegacyTokensCarryOperatorOverlay(t *testing.T) {
	db := newAuthTestStore(t)

	var agentRole string
	agentProbe := newUnaryProbe(t, db, criteriav1connect.CriteriaServiceCreateRunProcedure, false, func(ctx context.Context) {
		agentRole = CallerOperatorRole(ctx)
	})
	if err := agentProbe("agent-token-1"); err != nil {
		t.Fatalf("agent token must keep working: %v", err)
	}
	if agentRole != store.OperatorRoleAgent {
		t.Errorf("agent token must resolve onto an agent-role operator, got %q", agentRole)
	}

	var orchRole string
	orchProbe := newUnaryProbe(t, db, criteriav1connect.OrchestratorServiceListActiveRunsProcedure, false, func(ctx context.Context) {
		orchRole = CallerOperatorRole(ctx)
	})
	if err := orchProbe("orchestrator-token-1"); err != nil {
		t.Fatalf("orchestrator token must keep working: %v", err)
	}
	if orchRole != store.OperatorRoleOrchestrator {
		t.Errorf("orchestrator token must resolve onto an orchestrator-role operator, got %q", orchRole)
	}

	var consoleRole string
	consoleToken := seedConsoleUser(t, db)
	consoleProbe := newUnaryProbe(t, db, criteriav1connect.ServerServiceGetRunProcedure, false, func(ctx context.Context) {
		consoleRole = CallerOperatorRole(ctx)
	})
	if err := consoleProbe(consoleToken); err != nil {
		t.Fatalf("console session must keep working: %v", err)
	}
	if consoleRole != store.OperatorRoleAdmin {
		t.Errorf("console session must resolve onto an admin-role operator, got %q", consoleRole)
	}
}

// TestOperatorTokenRotationStopsOldMaterial pins that an operator token
// rotation invalidates the previous token immediately.
func TestOperatorTokenRotationStopsOldMaterial(t *testing.T) {
	db := newAuthTestStore(t)
	old := seedOperatorToken(t, db, "op-admin", store.OperatorRoleAdmin)
	now := time.Now().UTC()
	if err := db.UpsertOperatorToken(context.Background(), &store.OperatorToken{
		ID:         "op-admin-default",
		OperatorID: "op-admin",
		TokenHash:  HashToken("rotated-operator-token"),
		CreatedAt:  now,
	}); err != nil {
		t.Fatalf("rotate token: %v", err)
	}
	call := newUnaryProbe(t, db, criteriav1connect.ServerServiceListRunsProcedure, false, nil)
	if err := call(old); connect.CodeOf(err) != connect.CodeUnauthenticated {
		t.Errorf("rotated-out operator token must stop authenticating, got %v", err)
	}
	if err := call("rotated-operator-token"); err != nil {
		t.Errorf("new operator token must authenticate: %v", err)
	}
}

// surface used above.
