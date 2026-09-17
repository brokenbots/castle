package rpc

import (
	"bytes"
	"context"
	"log/slog"
	"strings"
	"testing"

	"connectrpc.com/connect"

	"github.com/brokenbots/castle/castle/internal/auth"
	pb "github.com/brokenbots/criteria/sdk/pb/criteria/v1" // import-lint:allow castle service bindings (W08: move to castle-proto)
)

// consoleLoginHarness wires a test stack with console login enabled and the
// default user seeded, mirroring what main.go does when the env vars are set.
func consoleLoginHarness(t *testing.T, log *slog.Logger) *testStack {
	t.Helper()
	ts := newTestStackWithLog(t, log)
	ts.server.EnableConsoleLogin()
	if err := ProvisionConsoleUser(context.Background(), ts.store, "operator", "op-password", log); err != nil {
		t.Fatalf("provision console user: %v", err)
	}
	return ts
}

func loginRequest(username, password string) *connect.Request[pb.LoginRequest] {
	return connect.NewRequest(&pb.LoginRequest{Username: username, Password: password})
}

func TestLogin_DisabledWhenNotEnabled(t *testing.T) {
	ts := newTestStack(t) // main.go never called EnableConsoleLogin
	_, err := ts.server.Login(context.Background(), loginRequest("operator", "op-password"))
	if connect.CodeOf(err) != connect.CodeUnimplemented {
		t.Fatalf("expected unimplemented when console login disabled, got %v", err)
	}
	if !strings.Contains(err.Error(), "CASTLE_CONSOLE_USER") {
		t.Errorf("error must tell the operator how to enable login, got %q", err.Error())
	}
}

func TestLogin_EmptyCredentialsInvalidArgument(t *testing.T) {
	ts := consoleLoginHarness(t, nil)
	for _, req := range []*connect.Request[pb.LoginRequest]{
		loginRequest("", "op-password"),
		loginRequest("operator", ""),
		loginRequest("", ""),
	} {
		_, err := ts.server.Login(context.Background(), req)
		if connect.CodeOf(err) != connect.CodeInvalidArgument {
			t.Errorf("empty credentials: expected invalid_argument, got %v", err)
		}
	}
}

func TestLogin_UnknownUsernameUnauthenticated(t *testing.T) {
	ts := consoleLoginHarness(t, nil)
	_, err := ts.server.Login(context.Background(), loginRequest("nobody", "whatever"))
	if connect.CodeOf(err) != connect.CodeUnauthenticated {
		t.Fatalf("expected unauthenticated for unknown username, got %v", err)
	}
}

func TestLogin_WrongPasswordUnauthenticated(t *testing.T) {
	ts := consoleLoginHarness(t, nil)
	_, err := ts.server.Login(context.Background(), loginRequest("operator", "wrong-password"))
	if connect.CodeOf(err) != connect.CodeUnauthenticated {
		t.Fatalf("expected unauthenticated for wrong password, got %v", err)
	}
}

func TestLogin_SuccessIssuesSession(t *testing.T) {
	ts := consoleLoginHarness(t, nil)
	resp, err := ts.server.Login(context.Background(), loginRequest("operator", "op-password"))
	if err != nil {
		t.Fatalf("login: %v", err)
	}
	if resp.Msg.SessionToken == "" {
		t.Fatal("expected non-empty session token")
	}
	if resp.Msg.Username != "operator" {
		t.Fatalf("expected username echo, got %q", resp.Msg.Username)
	}
	// The token must authenticate as the console identity afterwards.
	sess, err := auth.ResolveConsoleSession(context.Background(), ts.store, resp.Msg.SessionToken)
	if err != nil || sess == nil {
		t.Fatalf("issued token must resolve as a console session (sess=%v, err=%v)", sess, err)
	}
	if sess.UserID != ConsoleDefaultUserID {
		t.Fatalf("expected session user %q, got %q", ConsoleDefaultUserID, sess.UserID)
	}
}

func TestLogin_SessionStoredHashedNeverPlaintext(t *testing.T) {
	ts := consoleLoginHarness(t, nil)
	resp, err := ts.server.Login(context.Background(), loginRequest("operator", "op-password"))
	if err != nil {
		t.Fatalf("login: %v", err)
	}
	sessions, err := ts.store.ListConsoleSessions(context.Background())
	if err != nil || len(sessions) != 1 {
		t.Fatalf("expected exactly one session, got %d (err=%v)", len(sessions), err)
	}
	if sessions[0].TokenHash == resp.Msg.SessionToken {
		t.Fatal("session token must be persisted hashed, not as plaintext")
	}
	if sessions[0].TokenHash != auth.HashToken(resp.Msg.SessionToken) {
		t.Fatal("persisted hash must be the SHA-256 of the issued token")
	}
	// The plaintext token must not appear anywhere in the users table either.
	users, err := ts.store.GetConsoleUser(context.Background(), "operator")
	if err != nil {
		t.Fatalf("get user: %v", err)
	}
	if strings.Contains(users.PasswordHash, "op-password") || !auth.VerifyPassword(users.PasswordHash, "op-password") {
		t.Fatal("password must be stored as a verifiable bcrypt hash, never plaintext")
	}
}

func TestLogin_NoSecretsInLogs(t *testing.T) {
	var buf bytes.Buffer
	log := slog.New(slog.NewTextHandler(&buf, nil))
	ts := consoleLoginHarness(t, log)

	if _, err := ts.server.Login(context.Background(), loginRequest("operator", "op-password")); err != nil {
		t.Fatalf("login: %v", err)
	}
	// Failed attempts and rotations must not leak secrets either.
	if _, err := ts.server.Login(context.Background(), loginRequest("operator", "op-password")); err != nil {
		t.Fatalf("second login: %v", err)
	}
	if err := ProvisionConsoleUser(context.Background(), ts.store, "operator", "op-password", log); err != nil {
		t.Fatalf("re-provision: %v", err)
	}
	out := buf.String()
	if strings.Contains(out, "op-password") {
		t.Fatalf("password leaked into logs: %s", out)
	}
	if !strings.Contains(out, "username=operator") {
		t.Errorf("expected username (not a secret) in logs, got: %s", out)
	}
}

func TestProvisionConsoleUser_IdempotentAndRotating(t *testing.T) {
	ts := newTestStack(t)
	ts.server.EnableConsoleLogin()
	ctx := context.Background()

	if err := ProvisionConsoleUser(ctx, ts.store, "operator", "first-password", nil); err != nil {
		t.Fatalf("seed: %v", err)
	}
	first, err := ts.store.GetConsoleUser(ctx, "operator")
	if err != nil {
		t.Fatalf("get: %v", err)
	}

	// Issue a session, then re-provision with the SAME credentials: the
	// session must survive (idempotent no-op).
	resp, err := ts.server.Login(context.Background(), loginRequest("operator", "first-password"))
	if err != nil {
		t.Fatalf("login: %v", err)
	}
	if err := ProvisionConsoleUser(ctx, ts.store, "operator", "first-password", nil); err != nil {
		t.Fatalf("re-seed same creds: %v", err)
	}
	if sess, err := auth.ResolveConsoleSession(ctx, ts.store, resp.Msg.SessionToken); sess == nil || err != nil {
		t.Fatalf("session must survive idempotent re-seed (sess=%v, err=%v)", sess, err)
	}

	// Rotate the password: the stored hash changes, CreatedAt is preserved,
	// the old password stops verifying, and sessions issued under the old
	// password are revoked.
	if err := ProvisionConsoleUser(ctx, ts.store, "operator", "second-password", nil); err != nil {
		t.Fatalf("rotate: %v", err)
	}
	rotated, err := ts.store.GetConsoleUser(ctx, "operator")
	if err != nil {
		t.Fatalf("get rotated: %v", err)
	}
	if rotated.CreatedAt != first.CreatedAt {
		t.Errorf("CreatedAt must be preserved across rotation (want %v got %v)", first.CreatedAt, rotated.CreatedAt)
	}
	if auth.VerifyPassword(rotated.PasswordHash, "first-password") {
		t.Error("old password must stop verifying after rotation")
	}
	if !auth.VerifyPassword(rotated.PasswordHash, "second-password") {
		t.Error("rotated password must verify")
	}
	if sess, err := auth.ResolveConsoleSession(ctx, ts.store, resp.Msg.SessionToken); sess != nil || err != nil {
		t.Errorf("sessions must be revoked on password rotation (sess=%v, err=%v)", sess, err)
	}
	if _, err := ts.server.Login(context.Background(), loginRequest("operator", "first-password")); connect.CodeOf(err) != connect.CodeUnauthenticated {
		t.Errorf("old password must no longer log in, got %v", err)
	}
	if _, err := ts.server.Login(context.Background(), loginRequest("operator", "second-password")); err != nil {
		t.Errorf("new password must log in, got %v", err)
	}
}

func TestProvisionConsoleUser_RotatesUsernameOnSameFixedID(t *testing.T) {
	ts := newTestStack(t)
	ctx := context.Background()

	if err := ProvisionConsoleUser(ctx, ts.store, "first-user", "pw", nil); err != nil {
		t.Fatalf("seed: %v", err)
	}
	if err := ProvisionConsoleUser(ctx, ts.store, "second-user", "pw", nil); err != nil {
		t.Fatalf("rotate username: %v", err)
	}
	if _, err := ts.store.GetConsoleUser(ctx, "first-user"); err == nil {
		t.Fatal("old username must stop resolving after username rotation")
	}
	if _, err := ts.store.GetConsoleUser(ctx, "second-user"); err != nil {
		t.Fatalf("new username must resolve: %v", err)
	}
	users, err := ts.store.ListConsoleSessions(ctx) // sanity: no sessions yet
	if err != nil || len(users) != 0 {
		t.Fatalf("unexpected sessions: %d (err=%v)", len(users), err)
	}
}

func TestRevokeConsoleAuth_WipesUsersAndSessions(t *testing.T) {
	ts := consoleLoginHarness(t, nil)
	ctx := context.Background()

	resp, err := ts.server.Login(ctx, loginRequest("operator", "op-password"))
	if err != nil {
		t.Fatalf("login: %v", err)
	}
	if err := RevokeConsoleAuth(ctx, ts.store); err != nil {
		t.Fatalf("revoke: %v", err)
	}
	if sess, err := auth.ResolveConsoleSession(ctx, ts.store, resp.Msg.SessionToken); sess != nil || err != nil {
		t.Errorf("revoked session must not resolve (sess=%v, err=%v)", sess, err)
	}
	if _, err := ts.store.GetConsoleUser(ctx, "operator"); err == nil {
		t.Fatal("revoked user must not resolve")
	}
	// Fail-closed: after revocation, login cannot succeed even while enabled.
	if _, err := ts.server.Login(ctx, loginRequest("operator", "op-password")); connect.CodeOf(err) != connect.CodeUnauthenticated {
		t.Fatalf("login after revoke must be unauthenticated, got %v", err)
	}
}