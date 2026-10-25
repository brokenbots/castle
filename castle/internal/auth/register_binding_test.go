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
	pb "github.com/brokenbots/criteria/sdk/pb/criteria/v1"                                  // import-lint:allow castle service bindings (W08: move to castle-proto)
	criteriav1connect "github.com/brokenbots/criteria/sdk/pb/criteria/v1/criteriav1connect" // import-lint:allow castle service bindings (W08: move to castle-proto)
)

// seedProjectOperator provisions a per-project agent credential's identity
// (KB-337) straight through the store: operator row operator-<project> with
// {role: agent, project: <project>} and one operator token. (The auth
// package cannot import provision — provision imports auth for HashToken.)
func seedProjectOperator(t *testing.T, st store.Store, project, rawToken string) {
	t.Helper()
	now := time.Now().UTC()
	opID := "operator-" + project
	if err := st.UpsertOperator(context.Background(), &store.Operator{
		ID:   opID,
		Name: opID,
		Attributes: map[string]string{
			store.OperatorAttributeRole:    store.OperatorRoleAgent,
			store.OperatorAttributeProject: project,
		},
		CreatedAt: now,
		UpdatedAt: now,
	}); err != nil {
		t.Fatalf("upsert %s: %v", opID, err)
	}
	if err := st.UpsertOperatorToken(context.Background(), &store.OperatorToken{
		ID:         "agent-project-" + project,
		OperatorID: opID,
		TokenHash:  HashToken(rawToken),
		CreatedAt:  now,
	}); err != nil {
		t.Fatalf("upsert token %s: %v", project, err)
	}
}

// registerCall drives one Register call through a wire-shaped interceptor
// stack and captures the handler-visible registration context. opts is the
// exact production interceptor configuration under test (anon flag,
// bootstrap) so the tests pin the admission behavior rather than a test
// convenience.
type registerCapture struct {
	err          error
	callerID     string
	claimProject string // project a per-project credential binds into
	jointProject string // header-parsed self-join project
	identity     *store.AgentIdentity
}

func registerCall(t *testing.T, st store.Store, headers map[string]string, opts ...InterceptorOption) registerCapture {
	t.Helper()
	cap := registerCapture{}
	h := connect.NewUnaryHandler(
		criteriav1connect.CriteriaServiceRegisterProcedure,
		func(ctx context.Context, _ *connect.Request[pb.RegisterRequest]) (*connect.Response[pb.RegisterResponse], error) {
			cap.callerID = CallerCriteriaID(ctx)
			cap.claimProject = ProjectClaimFromContext(ctx)
			cap.jointProject = RegisterProjectIDFromContext(ctx)
			cap.identity = AgentIdentityFromContext(ctx)
			return connect.NewResponse(&pb.RegisterResponse{CriteriaId: "id", Token: "tok"}), nil
		},
		connect.WithInterceptors(NewInterceptor(st, false, opts...)),
	)
	mux := http.NewServeMux()
	mux.Handle(criteriav1connect.CriteriaServiceRegisterProcedure, h)
	tsrv := httptest.NewUnstartedServer(h2c.NewHandler(mux, &http2.Server{}))
	tsrv.Start()
	t.Cleanup(tsrv.Close)

	client := connect.NewClient[pb.RegisterRequest, pb.RegisterResponse](httpClient(), tsrv.URL+criteriav1connect.CriteriaServiceRegisterProcedure)
	req := connect.NewRequest(&pb.RegisterRequest{Name: "worker"})
	for k, v := range headers {
		req.Header().Set(k, v)
	}
	_, cap.err = client.CallUnary(context.Background(), req)
	return cap
}

// TestRegisterProjectBinding is the KB-337 admission suite: the credential's
// operator claims exactly one project, the registration binds it (regardless
// of the join policy — provisioning was the admission), a header claiming
// any OTHER project is refused, and the shared gate keeps ruling otherwise
// unclaimed registrations.
func TestRegisterProjectBinding(t *testing.T) {
	newStore := func(t *testing.T) store.Store {
		t.Helper()
		st, err := sqlite.Open(filepath.Join(t.TempDir(), "binding.db"))
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = st.Close() })
		return st
	}
	credHeaders := func(token string, project string) map[string]string {
		h := map[string]string{"Authorization": "Bearer " + token}
		if project != "" {
			h["X-Criteria-Agent-Project"] = project
			h[HeaderAgentVersion] = "9.9.9"
		}
		return h
	}

	t.Run("credential alone binds its project", func(t *testing.T) {
		st := newStore(t)
		seedProjectOperator(t, st, "alpha", "cred-alpha")
		cap := registerCall(t, st, credHeaders("cred-alpha", ""), WithAnonRegister())
		if cap.err != nil {
			t.Fatalf("register with claim: %v", cap.err)
		}
		// The binding, not the header key, reaches the handler: the join is
		// policy-bypassing because provisioning WAS the admission.
		if cap.claimProject != "alpha" || cap.jointProject != "" {
			t.Fatalf("binding = claim %q header %q, want alpha/\"\"", cap.claimProject, cap.jointProject)
		}
		// Register mints the agent's own overseer identity (fresh uuid row
		// joined into the claim project); the operator id is the credential's
		// admission identity, not the agent's runtime identity, so no caller
		// id is injected here.
		if cap.callerID != "" {
			t.Fatalf("caller = %q, want empty (register mints its own agent identity)", cap.callerID)
		}
	})

	t.Run("matching header still binds the claim", func(t *testing.T) {
		st := newStore(t)
		seedProjectOperator(t, st, "alpha", "cred-alpha")
		cap := registerCall(t, st, credHeaders("cred-alpha", "alpha"), WithAnonRegister())
		if cap.err != nil {
			t.Fatalf("register with matching header: %v", cap.err)
		}
		if cap.claimProject != "alpha" || cap.jointProject != "" {
			t.Fatalf("binding = claim %q header %q, want alpha/\"\"", cap.claimProject, cap.jointProject)
		}
		if cap.identity == nil || cap.identity.Version != "9.9.9" {
			t.Fatalf("identity block lost on the claim path: %+v", cap.identity)
		}
	})

	t.Run("cross-project header refused", func(t *testing.T) {
		st := newStore(t)
		seedProjectOperator(t, st, "alpha", "cred-alpha")
		seedProjectOperator(t, st, "beta", "cred-beta")
		for _, tc := range []struct{ cred, header string }{
			{cred: "cred-alpha", header: "beta"},
			{cred: "cred-beta", header: "alpha"},
		} {
			cap := registerCall(t, st, credHeaders(tc.cred, tc.header), WithAnonRegister())
			if connect.CodeOf(cap.err) != connect.CodePermissionDenied {
				t.Fatalf("credential %s header %s: got %v, want PermissionDenied", tc.cred, tc.header, cap.err)
			}
		}
	})

	t.Run("no credential: header self-join unchanged", func(t *testing.T) {
		st := newStore(t)
		cap := registerCall(t, st, credHeaders("", "legacy-pool"), WithAnonRegister())
		if cap.err != nil {
			t.Fatalf("legacy register: %v", cap.err)
		}
		if cap.claimProject != "" || cap.jointProject != "legacy-pool" {
			t.Fatalf("binding = claim %q header %q, want legacy header only", cap.claimProject, cap.jointProject)
		}
	})

	t.Run("credential is the admission gate (no anon flag, no bootstrap)", func(t *testing.T) {
		st := newStore(t)
		seedProjectOperator(t, st, "alpha", "cred-alpha")
		cap := registerCall(t, st, credHeaders("cred-alpha", ""))
		if cap.err != nil {
			t.Fatalf("register with configured credential: %v", cap.err)
		}
		if cap.claimProject != "alpha" {
			t.Fatalf("claim = %q, want alpha", cap.claimProject)
		}
	})

	t.Run("project-less agent operator row stays behind the gate", func(t *testing.T) {
		st := newStore(t)
		// An agent-role operator row with NO project attribute is not a
		// per-project credential: it must not open registration on its own.
		now := time.Now().UTC()
		if err := st.UpsertOperator(context.Background(), &store.Operator{
			ID: "operator-noclm", Name: "operator-noclm",
			Attributes: map[string]string{store.OperatorAttributeRole: store.OperatorRoleAgent},
			CreatedAt:  now, UpdatedAt: now,
		}); err != nil {
			t.Fatalf("upsert: %v", err)
		}
		if err := st.UpsertOperatorToken(context.Background(), &store.OperatorToken{
			ID: "tok-noclm", OperatorID: "operator-noclm", TokenHash: HashToken("cred-noclm"), CreatedAt: now,
		}); err != nil {
			t.Fatalf("upsert token: %v", err)
		}
		cap := registerCall(t, st, credHeaders("cred-noclm", ""))
		if connect.CodeOf(cap.err) != connect.CodeUnimplemented {
			t.Fatalf("unclaimed agent operator register = %v, want gate refusal (Unimplemented without config)", cap.err)
		}
	})

	t.Run("revoked credential unauthenticated (bootstrap stack)", func(t *testing.T) {
		st := newStore(t)
		seedProjectOperator(t, st, "alpha", "cred-alpha")
		opts := []InterceptorOption{WithBootstrapToken("boot-secret")}
		if cap := registerCall(t, st, credHeaders("cred-alpha", ""), opts...); cap.err != nil {
			t.Fatalf("credential register on bootstrap stack: %v", cap.err)
		}
		// Revoke = delete the operator row (cascade kills the token).
		if err := st.DeleteOperator(context.Background(), "operator-alpha"); err != nil {
			t.Fatalf("revoke: %v", err)
		}
		cap := registerCall(t, st, credHeaders("cred-alpha", ""), opts...)
		if connect.CodeOf(cap.err) != connect.CodeUnauthenticated {
			t.Fatalf("revoked credential register = %v, want Unauthenticated", cap.err)
		}
	})
}
