package auth

import (
	"context"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"connectrpc.com/connect"
	"golang.org/x/net/http2"
	"golang.org/x/net/http2/h2c"

	"github.com/brokenbots/castle/castle/internal/store"
	"github.com/brokenbots/castle/castle/internal/store/sqlite"
	pb "github.com/brokenbots/criteria/sdk/pb/criteria/v1"                // import-lint:allow castle service bindings (W08: move to castle-proto)
	"github.com/brokenbots/criteria/sdk/pb/criteria/v1/criteriav1connect" // import-lint:allow castle service bindings (W08: move to castle-proto)
)

func newAuthTestStore(t *testing.T) *sqlite.Store {
	t.Helper()
	db, err := sqlite.Open(filepath.Join(t.TempDir(), "auth-orch.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	now := time.Now().UTC()
	if err := db.CreateOverseer(context.Background(), &store.Overseer{
		ID: "agent-1", Name: "agent", TokenHash: HashToken("agent-token-1"), Status: "online", CreatedAt: now, LastSeenAt: now,
	}); err != nil {
		t.Fatal(err)
	}
	if err := db.UpsertOrchestrator(context.Background(), &store.Orchestrator{
		ID: "orchestrator-operator", Name: "operator", TokenHash: HashToken("orchestrator-token-1"), CreatedAt: now,
	}); err != nil {
		t.Fatal(err)
	}
	return db
}

// newUnaryProbe mounts an authenticated handler for procedure that captures
// the injected identity and echoes success. It returns the probe function to
// invoke the procedure with a given Authorization header value.
func newUnaryProbe(t *testing.T, db *sqlite.Store, procedure string, allowAnonReads bool, capture func(context.Context)) func(string) error {
	t.Helper()
	h := connect.NewUnaryHandler(
		procedure,
		func(ctx context.Context, _ *connect.Request[pb.GetRunRequest]) (*connect.Response[pb.Run], error) {
			if capture != nil {
				capture(ctx)
			}
			return connect.NewResponse(&pb.Run{RunId: "r1"}), nil
		},
		connect.WithInterceptors(NewInterceptor(db, allowAnonReads)),
	)
	mux := http.NewServeMux()
	mux.Handle(procedure, h)
	tsrv := httptest.NewUnstartedServer(h2c.NewHandler(mux, &http2.Server{}))
	tsrv.Start()
	t.Cleanup(tsrv.Close)

	client := connect.NewClient[pb.GetRunRequest, pb.Run](httpClient(), tsrv.URL+procedure)
	return func(token string) error {
		req := connect.NewRequest(&pb.GetRunRequest{RunId: "r1"})
		if token != "" {
			req.Header().Set("Authorization", "Bearer "+token)
		}
		_, err := client.CallUnary(context.Background(), req)
		return err
	}
}

func TestOrchestratorTokenAuthenticatesWithOrchestratorIdentity(t *testing.T) {
	db := newAuthTestStore(t)
	var criteriaID, orchID string
	call := newUnaryProbe(t, db, criteriav1connect.ServerServiceListRunsProcedure, false, func(ctx context.Context) {
		criteriaID, orchID = CallerCriteriaID(ctx), CallerOrchestratorID(ctx)
	})
	if err := call("orchestrator-token-1"); err != nil {
		t.Fatalf("orchestrator token rejected on read procedure: %v", err)
	}
	if orchID != "orchestrator-operator" {
		t.Errorf("expected CallerOrchestratorID=orchestrator-operator, got %q", orchID)
	}
	if criteriaID != "" {
		t.Errorf("orchestrator must not be treated as an agent; CallerCriteriaID=%q", criteriaID)
	}
}

func TestOrchestratorDeniedOnAgentOwnedWrites(t *testing.T) {
	db := newAuthTestStore(t)
	for _, procedure := range []string{
		criteriav1connect.CriteriaServiceCreateRunProcedure,
		criteriav1connect.CriteriaServiceSubmitEventsProcedure,
		criteriav1connect.CriteriaServiceControlProcedure,
		criteriav1connect.CriteriaServiceReattachRunProcedure,
		criteriav1connect.CriteriaServiceHeartbeatProcedure,
		criteriav1connect.CriteriaServiceResumeProcedure,
		criteriav1connect.ServerServiceStopRunProcedure,
		criteriav1connect.ServerServicePauseRunProcedure,
		criteriav1connect.ServerServiceResumeRunProcedure,
		criteriav1connect.ServerServiceSendPromptProcedure,
		criteriav1connect.ServerServiceSubmitWorkflowAssignmentProcedure,
		criteriav1connect.ServerServiceGetAssignmentDispositionProcedure,
	} {
		call := newUnaryProbe(t, db, procedure, false, nil)
		err := call("orchestrator-token-1")
		if connect.CodeOf(err) != connect.CodePermissionDenied {
			t.Errorf("%s: expected permission_denied for orchestrator, got %v", procedure, err)
		}
	}
}

func TestOrchestratorAllowedOnReadOnlySurface(t *testing.T) {
	db := newAuthTestStore(t)
	for _, procedure := range []string{
		criteriav1connect.OrchestratorServiceSubscribeRunEventsProcedure,
		criteriav1connect.OrchestratorServiceListActiveRunsProcedure,
		criteriav1connect.ServerServiceListRunsProcedure,
		criteriav1connect.ServerServiceGetRunProcedure,
		criteriav1connect.ServerServiceListRunEventsProcedure,
		criteriav1connect.ServerServiceListAgentsProcedure,
		criteriav1connect.ServerServiceGetAgentProcedure,
	} {
		call := newUnaryProbe(t, db, procedure, false, nil)
		if err := call("orchestrator-token-1"); err != nil {
			t.Errorf("%s: expected orchestrator read to be allowed, got %v", procedure, err)
		}
	}
}

func TestAgentTokenStillWritesAndInvalidTokenRejected(t *testing.T) {
	db := newAuthTestStore(t)
	// Agent tokens keep full access: the boundary is orchestrator-cannot-write.
	call := newUnaryProbe(t, db, criteriav1connect.CriteriaServiceCreateRunProcedure, false, nil)
	if err := call("agent-token-1"); err != nil {
		t.Errorf("agent token must not be restricted by the orchestrator boundary: %v", err)
	}

	// An agent-owned write with an unknown token stays unauthenticated.
	if err := call("not-a-token"); connect.CodeOf(err) != connect.CodeUnauthenticated {
		t.Errorf("expected unauthenticated for unknown token, got %v", err)
	}
}

func TestResolveOrchestratorToken(t *testing.T) {
	db := newAuthTestStore(t)
	ctx := context.Background()

	o, err := ResolveOrchestratorToken(ctx, db, "orchestrator-token-1")
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if o == nil || o.ID != "orchestrator-operator" {
		t.Fatalf("expected orchestrator-operator, got %+v", o)
	}

	if o, err := ResolveOrchestratorToken(ctx, db, "agent-token-1"); err != nil || o != nil {
		t.Fatalf("agent token must not resolve as an orchestrator: o=%+v err=%v", o, err)
	}
	if o, err := ResolveOrchestratorToken(ctx, db, "nope"); err != nil || o != nil {
		t.Fatalf("unknown token must not resolve: o=%+v err=%v", o, err)
	}
}
