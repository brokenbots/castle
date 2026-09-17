package auth

import (
	"context"
	"testing"
	"time"

	"connectrpc.com/connect"

	"github.com/brokenbots/castle/castle/internal/store"
	"github.com/brokenbots/castle/castle/internal/store/sqlite"
	"github.com/brokenbots/criteria/sdk/pb/criteria/v1/criteriav1connect" // import-lint:allow castle service bindings (W08: move to castle-proto)
)

// seedConsoleUser stores a console user + live session for auth tests. The
// returned token is the plaintext session token a client would present.
func seedConsoleUser(t *testing.T, db *sqlite.Store) string {
	t.Helper()
	ctx := context.Background()
	hash, err := HashPassword("op-password")
	if err != nil {
		t.Fatalf("hash password: %v", err)
	}
	now := time.Now().UTC()
	if err := db.UpsertConsoleUser(ctx, &store.ConsoleUser{
		ID: "console-default", Username: "operator", PasswordHash: hash, CreatedAt: now, UpdatedAt: now,
	}); err != nil {
		t.Fatalf("seed console user: %v", err)
	}
	token, err := NewSessionToken()
	if err != nil {
		t.Fatalf("new session token: %v", err)
	}
	if err := db.CreateConsoleSession(ctx, &store.ConsoleSession{
		ID: "sess-1", UserID: "console-default", TokenHash: HashToken(token), CreatedAt: now,
	}); err != nil {
		t.Fatalf("seed console session: %v", err)
	}
	return token
}

func TestHashPasswordRoundTrip(t *testing.T) {
	hash, err := HashPassword("s3cret-operator-password")
	if err != nil {
		t.Fatalf("hash: %v", err)
	}
	// Stored form must be a bcrypt hash, never the plaintext.
	if hash == "s3cret-operator-password" || len(hash) < 59 || hash[:4] != "$2a$" {
		t.Fatalf("expected bcrypt hash at rest, got %q", hash)
	}
	if !VerifyPassword(hash, "s3cret-operator-password") {
		t.Error("correct password must verify")
	}
	if VerifyPassword(hash, "wrong-password") {
		t.Error("wrong password must not verify")
	}
	if VerifyPassword("not-a-hash", "s3cret-operator-password") {
		t.Error("invalid hash must not verify")
	}
}

func TestNewSessionToken(t *testing.T) {
	seen := map[string]bool{}
	for i := 0; i < 100; i++ {
		tok, err := NewSessionToken()
		if err != nil {
			t.Fatalf("generate: %v", err)
		}
		if len(tok) != 43 { // 32 bytes → base64.RawURLEncoding
			t.Fatalf("unexpected token length %d", len(tok))
		}
		if seen[tok] {
			t.Fatal("session tokens must be unique")
		}
		seen[tok] = true
	}
}

func TestResolveConsoleSession(t *testing.T) {
	db := newAuthTestStore(t)
	token := seedConsoleUser(t, db)
	ctx := context.Background()

	sess, err := ResolveConsoleSession(ctx, db, token)
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if sess == nil || sess.UserID != "console-default" {
		t.Fatalf("expected console-default session, got %+v", sess)
	}

	// Agent and orchestrator token material must not resolve as console
	// sessions.
	for _, tok := range []string{"agent-token-1", "orchestrator-token-1", "nope"} {
		sess, err := ResolveConsoleSession(ctx, db, tok)
		if err != nil || sess != nil {
			t.Errorf("token %q must not resolve as a console session (sess=%v, err=%v)", tok, sess, err)
		}
	}

	// Revoking sessions stops resolution (fail-closed).
	if err := db.DeleteConsoleSessions(ctx); err != nil {
		t.Fatalf("revoke sessions: %v", err)
	}
	if sess, err := ResolveConsoleSession(ctx, db, token); sess != nil || err != nil {
		t.Errorf("revoked session must not resolve (sess=%v, err=%v)", sess, err)
	}
}

func TestConsoleTokenResolvesAsConsoleIdentity(t *testing.T) {
	db := newAuthTestStore(t)
	token := seedConsoleUser(t, db)
	var consoleID, criteriaID, orchID string
	call := newUnaryProbe(t, db, criteriav1connect.ServerServiceListRunsProcedure, false, func(ctx context.Context) {
		consoleID, criteriaID, orchID = CallerConsoleUserID(ctx), CallerCriteriaID(ctx), CallerOrchestratorID(ctx)
	})
	if err := call(token); err != nil {
		t.Fatalf("console token rejected on read procedure: %v", err)
	}
	if consoleID != "console-default" {
		t.Errorf("expected CallerConsoleUserID=console-default, got %q", consoleID)
	}
	if criteriaID != "" || orchID != "" {
		t.Errorf("console caller must not be treated as agent/orchestrator (criteria=%q orch=%q)", criteriaID, orchID)
	}
}

// TestConsoleDeniedOnWrites pins the deterministic console allowlist (CRI-195):
// every write — CriteriaService, ServerService writes, OrchestratorService
// writes — is permission_denied for a console identity.
func TestConsoleDeniedOnWrites(t *testing.T) {
	db := newAuthTestStore(t)
	token := seedConsoleUser(t, db)
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
		criteriav1connect.OrchestratorServiceCancelRunProcedure,
	} {
		call := newUnaryProbe(t, db, procedure, false, nil)
		err := call(token)
		if connect.CodeOf(err) != connect.CodePermissionDenied {
			t.Errorf("%s: expected permission_denied for console identity, got %v", procedure, err)
		}
	}
}

// TestConsoleAllowedOnReadOnlySurface pins that the console allowlist is
// exactly the read-only ServerService observation surface.
func TestConsoleAllowedOnReadOnlySurface(t *testing.T) {
	db := newAuthTestStore(t)
	token := seedConsoleUser(t, db)
	for _, procedure := range []string{
		criteriav1connect.ServerServiceListAgentsProcedure,
		criteriav1connect.ServerServiceGetAgentProcedure,
		criteriav1connect.ServerServiceListRunsProcedure,
		criteriav1connect.ServerServiceGetRunProcedure,
		criteriav1connect.ServerServiceListRunEventsProcedure,
		criteriav1connect.ServerServiceInspectRunProcedure,
	} {
		call := newUnaryProbe(t, db, procedure, false, nil)
		if err := call(token); err != nil {
			t.Errorf("%s: expected console read to be allowed, got %v", procedure, err)
		}
	}
}

// TestConsoleDeniedOnAgentTokensWithoutSession pins that agent and
// orchestrator identities are unaffected by the console boundary, and that a
// random token is still unauthenticated.
func TestConsoleBoundaryLeavesOtherIdentitiesIntact(t *testing.T) {
	db := newAuthTestStore(t)
	seedConsoleUser(t, db)

	write := newUnaryProbe(t, db, criteriav1connect.CriteriaServiceCreateRunProcedure, false, nil)
	if err := write("agent-token-1"); err != nil {
		t.Errorf("agent token must not be restricted by the console boundary: %v", err)
	}
	orchRead := newUnaryProbe(t, db, criteriav1connect.OrchestratorServiceListActiveRunsProcedure, false, nil)
	if err := orchRead("orchestrator-token-1"); err != nil {
		t.Errorf("orchestrator token must not be restricted by the console boundary: %v", err)
	}
	if err := write("nope"); connect.CodeOf(err) != connect.CodeUnauthenticated {
		t.Errorf("unknown token must stay unauthenticated, got %v", err)
	}
}

// TestConsoleDeniedOnOrchestratorSurfaceUnderAnonReads covers the anon-read
// interaction (CRI-195): with --allow-anon-reads the OrchestratorService
// subscription APIs are anonymously readable, but a PRESENTED console token
// must not gain access to them — the console boundary is re-checked after
// identity injection on the anon-read surface.
func TestConsoleDeniedOnOrchestratorSurfaceUnderAnonReads(t *testing.T) {
	db := newAuthTestStore(t)
	token := seedConsoleUser(t, db)
	call := newUnaryProbe(t, db, criteriav1connect.OrchestratorServiceListActiveRunsProcedure, true, nil)
	err := call(token)
	if connect.CodeOf(err) != connect.CodePermissionDenied {
		t.Fatalf("console token on orchestrator surface must be permission_denied, got %v", err)
	}
}

// TestConsoleAllowedOnReadSurfaceUnderAnonReads ensures the console read
// surface keeps working under --allow-anon-reads with identity injected (so
// InspectRun view-all keeps functioning).
func TestConsoleAllowedOnReadSurfaceUnderAnonReads(t *testing.T) {
	db := newAuthTestStore(t)
	token := seedConsoleUser(t, db)
	var consoleID string
	call := newUnaryProbe(t, db, criteriav1connect.ServerServiceInspectRunProcedure, true, func(ctx context.Context) {
		consoleID = CallerConsoleUserID(ctx)
	})
	if err := call(token); err != nil {
		t.Fatalf("console token on InspectRun must be allowed, got %v", err)
	}
	if consoleID != "console-default" {
		t.Errorf("expected console identity injected under anon-reads, got %q", consoleID)
	}
}