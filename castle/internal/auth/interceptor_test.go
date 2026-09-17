package auth

import (
	"context"
	"crypto/tls"
	"net"
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

func TestAuthInterceptorUnary(t *testing.T) {
	db, err := sqlite.Open(filepath.Join(t.TempDir(), "auth-int.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })

	now := time.Now().UTC()
	if err := db.CreateOverseer(context.Background(), &store.Overseer{
		ID: "o1", Name: "name", TokenHash: HashToken("tok-1"), Status: "online", CreatedAt: now, LastSeenAt: now,
	}); err != nil {
		t.Fatal(err)
	}

	h := connect.NewUnaryHandler(
		criteriav1connect.ServerServiceGetRunProcedure,
		func(context.Context, *connect.Request[pb.GetRunRequest]) (*connect.Response[pb.Run], error) {
			return connect.NewResponse(&pb.Run{RunId: "r1"}), nil
		},
		connect.WithInterceptors(NewInterceptor(db, false)),
	)
	mux := http.NewServeMux()
	mux.Handle(criteriav1connect.ServerServiceGetRunProcedure, h)
	tsrv := httptest.NewUnstartedServer(h2c.NewHandler(mux, &http2.Server{}))
	tsrv.Start()
	t.Cleanup(tsrv.Close)

	client := connect.NewClient[pb.GetRunRequest, pb.Run](httpClient(), tsrv.URL+criteriav1connect.ServerServiceGetRunProcedure)

	_, err = client.CallUnary(context.Background(), connect.NewRequest(&pb.GetRunRequest{RunId: "r1"}))
	if connect.CodeOf(err) != connect.CodeUnauthenticated {
		t.Fatalf("expected unauthenticated, got %v", err)
	}

	req := connect.NewRequest(&pb.GetRunRequest{RunId: "r1"})
	req.Header().Set("Authorization", "Bearer tok-1")
	_, err = client.CallUnary(context.Background(), req)
	if err != nil {
		t.Fatal(err)
	}
}

func TestAuthInterceptorExemptions(t *testing.T) {
	h := connect.NewUnaryHandler(
		criteriav1connect.CriteriaServiceRegisterProcedure,
		func(context.Context, *connect.Request[pb.RegisterRequest]) (*connect.Response[pb.RegisterResponse], error) {
			return connect.NewResponse(&pb.RegisterResponse{CriteriaId: "o1"}), nil
		},
		// WithAnonRegister is required: Register now requires a bootstrap token unless
		// anonymous registration is explicitly enabled (dev/test mode).
		connect.WithInterceptors(NewInterceptor(nil, true, WithAnonRegister())),
	)
	mux := http.NewServeMux()
	mux.Handle(criteriav1connect.CriteriaServiceRegisterProcedure, h)
	tsrv := httptest.NewUnstartedServer(h2c.NewHandler(mux, &http2.Server{}))
	tsrv.Start()
	t.Cleanup(tsrv.Close)

	client := connect.NewClient[pb.RegisterRequest, pb.RegisterResponse](httpClient(), tsrv.URL+criteriav1connect.CriteriaServiceRegisterProcedure)
	_, err := client.CallUnary(context.Background(), connect.NewRequest(&pb.RegisterRequest{Name: "test"}))
	if err != nil {
		t.Fatal(err)
	}
}

// TestAuthInterceptor_CallerIDInjected verifies that AuthInterceptor resolves
// the caller's overseer ID and injects it into the handler context via
// CallerCriteriaID. This locks in the context-injection contract so future
// refactors of the interceptor don't silently break run-ownership checks.
func TestAuthInterceptor_CallerIDInjected(t *testing.T) {
	db, err := sqlite.Open(filepath.Join(t.TempDir(), "auth-id.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })

	now := time.Now().UTC()
	if err := db.CreateOverseer(context.Background(), &store.Overseer{
		ID: "overseer-xyz", Name: "injected", TokenHash: HashToken("tok-inject"), Status: "online", CreatedAt: now, LastSeenAt: now,
	}); err != nil {
		t.Fatal(err)
	}

	var capturedID string
	h := connect.NewUnaryHandler(
		criteriav1connect.ServerServiceGetRunProcedure,
		func(ctx context.Context, _ *connect.Request[pb.GetRunRequest]) (*connect.Response[pb.Run], error) {
			capturedID = CallerCriteriaID(ctx)
			return connect.NewResponse(&pb.Run{RunId: "r1"}), nil
		},
		connect.WithInterceptors(NewInterceptor(db, false)),
	)
	mux := http.NewServeMux()
	mux.Handle(criteriav1connect.ServerServiceGetRunProcedure, h)
	tsrv := httptest.NewUnstartedServer(h2c.NewHandler(mux, &http2.Server{}))
	tsrv.Start()
	t.Cleanup(tsrv.Close)

	client := connect.NewClient[pb.GetRunRequest, pb.Run](httpClient(), tsrv.URL+criteriav1connect.ServerServiceGetRunProcedure)
	req := connect.NewRequest(&pb.GetRunRequest{RunId: "r1"})
	req.Header().Set("Authorization", "Bearer tok-inject")
	if _, err := client.CallUnary(context.Background(), req); err != nil {
		t.Fatal(err)
	}

	if capturedID != "overseer-xyz" {
		t.Errorf("expected CallerCriteriaID='overseer-xyz', got %q", capturedID)
	}
}

// TestAnonReads_InspectRunAnonymousReadable verifies InspectRun is part of
// the dev-mode anonymous-read surface (CRI-194): with no token the call
// passes anonymously when --allow-anon-reads is enabled — matching every
// other read the Parapet run detail page makes — and requires
// authentication when the flag is off.
func TestAnonReads_InspectRunAnonymousReadable(t *testing.T) {
	newInspectClient := func(t *testing.T, allowAnonReads bool) *connect.Client[pb.InspectRunRequest, pb.InspectRunResponse] {
		t.Helper()
		h := connect.NewUnaryHandler(
			criteriav1connect.ServerServiceInspectRunProcedure,
			func(context.Context, *connect.Request[pb.InspectRunRequest]) (*connect.Response[pb.InspectRunResponse], error) {
				return connect.NewResponse(&pb.InspectRunResponse{RunId: "r1"}), nil
			},
			connect.WithInterceptors(NewInterceptor(nil, allowAnonReads)),
		)
		mux := http.NewServeMux()
		mux.Handle(criteriav1connect.ServerServiceInspectRunProcedure, h)
		tsrv := httptest.NewUnstartedServer(h2c.NewHandler(mux, &http2.Server{}))
		tsrv.Start()
		t.Cleanup(tsrv.Close)
		return connect.NewClient[pb.InspectRunRequest, pb.InspectRunResponse](httpClient(), tsrv.URL+criteriav1connect.ServerServiceInspectRunProcedure)
	}

	if _, err := newInspectClient(t, true).CallUnary(context.Background(), connect.NewRequest(&pb.InspectRunRequest{RunId: "r1"})); err != nil {
		t.Fatalf("anonymous InspectRun under --allow-anon-reads: %v", err)
	}

	_, err := newInspectClient(t, false).CallUnary(context.Background(), connect.NewRequest(&pb.InspectRunRequest{RunId: "r1"}))
	if connect.CodeOf(err) != connect.CodeUnauthenticated {
		t.Fatalf("expected unauthenticated with the flag off, got code=%v err=%v", connect.CodeOf(err), err)
	}
}

// TestAnonReads_PresentedTokenValidation locks the token semantics of the
// dev-mode anonymous-read surface (CRI-194): anonymous callers (no token)
// keep the anonymous pass-through, a presented token must be valid — a
// valid one is authenticated and its identity injected so per-caller
// authorization (InspectRun caller-owns-run) stays enforceable, and an
// invalid one is rejected as unauthenticated instead of silently
// succeeding. That rejection is what lets the Parapet login gate's
// ListAgents probe detect a bad token rather than accept it and trap the UI
// in a login loop on a deep-linked route.
func TestAnonReads_PresentedTokenValidation(t *testing.T) {
	db, err := sqlite.Open(filepath.Join(t.TempDir(), "auth-anon.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })

	now := time.Now().UTC()
	if err := db.CreateOverseer(context.Background(), &store.Overseer{
		ID: "o1", Name: "name", TokenHash: HashToken("tok-1"), Status: "online", CreatedAt: now, LastSeenAt: now,
	}); err != nil {
		t.Fatal(err)
	}

	var capturedID string
	h := connect.NewUnaryHandler(
		criteriav1connect.ServerServiceGetRunProcedure,
		func(ctx context.Context, _ *connect.Request[pb.GetRunRequest]) (*connect.Response[pb.Run], error) {
			capturedID = CallerCriteriaID(ctx)
			return connect.NewResponse(&pb.Run{RunId: "r1"}), nil
		},
		connect.WithInterceptors(NewInterceptor(db, true)),
	)
	mux := http.NewServeMux()
	mux.Handle(criteriav1connect.ServerServiceGetRunProcedure, h)
	tsrv := httptest.NewUnstartedServer(h2c.NewHandler(mux, &http2.Server{}))
	tsrv.Start()
	t.Cleanup(tsrv.Close)

	client := connect.NewClient[pb.GetRunRequest, pb.Run](httpClient(), tsrv.URL+criteriav1connect.ServerServiceGetRunProcedure)

	// No token: anonymous pass-through with no identity injected.
	if _, err := client.CallUnary(context.Background(), connect.NewRequest(&pb.GetRunRequest{RunId: "r1"})); err != nil {
		t.Fatalf("anonymous read without token: %v", err)
	}
	if capturedID != "" {
		t.Errorf("anonymous read must not carry an identity, got %q", capturedID)
	}

	// Invalid presented token: rejected instead of silently succeeding.
	req := connect.NewRequest(&pb.GetRunRequest{RunId: "r1"})
	req.Header().Set("Authorization", "Bearer not-a-registered-token")
	if _, err := client.CallUnary(context.Background(), req); connect.CodeOf(err) != connect.CodeUnauthenticated {
		t.Fatalf("expected unauthenticated for invalid token, got code=%v err=%v", connect.CodeOf(err), err)
	}

	// Valid presented token: authenticated, identity injected.
	req = connect.NewRequest(&pb.GetRunRequest{RunId: "r1"})
	req.Header().Set("Authorization", "Bearer tok-1")
	if _, err := client.CallUnary(context.Background(), req); err != nil {
		t.Fatalf("read with valid token: %v", err)
	}
	if capturedID != "o1" {
		t.Errorf("expected CallerCriteriaID='o1', got %q", capturedID)
	}
}

// TestAnonReads_WatchStreamPresentedTokenValidation covers the streaming
// half of the anonymous-read surface (CRI-194): an anonymous WatchRun keeps
// streaming under --allow-anon-reads, and an invalid presented token is
// rejected before the handler runs.
func TestAnonReads_WatchStreamPresentedTokenValidation(t *testing.T) {
	db, err := sqlite.Open(filepath.Join(t.TempDir(), "auth-anon-stream.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })

	now := time.Now().UTC()
	if err := db.CreateOverseer(context.Background(), &store.Overseer{
		ID: "o1", Name: "name", TokenHash: HashToken("tok-1"), Status: "online", CreatedAt: now, LastSeenAt: now,
	}); err != nil {
		t.Fatal(err)
	}

	var capturedID string
	h := connect.NewServerStreamHandler(
		criteriav1connect.ServerServiceWatchRunProcedure,
		func(ctx context.Context, _ *connect.Request[pb.WatchRunRequest], conn *connect.ServerStream[pb.Envelope]) error {
			capturedID = CallerCriteriaID(ctx)
			return conn.Send(&pb.Envelope{})
		},
		connect.WithInterceptors(NewInterceptor(db, true)),
	)
	mux := http.NewServeMux()
	mux.Handle(criteriav1connect.ServerServiceWatchRunProcedure, h)
	tsrv := httptest.NewUnstartedServer(h2c.NewHandler(mux, &http2.Server{}))
	tsrv.Start()
	t.Cleanup(tsrv.Close)

	client := connect.NewClient[pb.WatchRunRequest, pb.Envelope](httpClient(), tsrv.URL+criteriav1connect.ServerServiceWatchRunProcedure)

	// No token: anonymous pass-through.
	stream, _ := client.CallServerStream(context.Background(), connect.NewRequest(&pb.WatchRunRequest{RunId: "r1"}))
	if !stream.Receive() {
		t.Fatalf("anonymous watch without token: %v", stream.Err())
	}
	if capturedID != "" {
		t.Errorf("anonymous watch must not carry an identity, got %q", capturedID)
	}

	// Invalid presented token: rejected before the handler runs.
	req := connect.NewRequest(&pb.WatchRunRequest{RunId: "r1"})
	req.Header().Set("Authorization", "Bearer not-a-registered-token")
	stream, _ = client.CallServerStream(context.Background(), req)
	if stream.Receive() {
		t.Fatal("expected invalid token to be rejected, got a stream event")
	}
	if connect.CodeOf(stream.Err()) != connect.CodeUnauthenticated {
		t.Fatalf("expected unauthenticated for invalid token, got code=%v err=%v", connect.CodeOf(stream.Err()), stream.Err())
	}
}

func httpClient() *http.Client {
	return &http.Client{Transport: &http2.Transport{
		AllowHTTP: true,
		DialTLSContext: func(ctx context.Context, network, addr string, _ *tls.Config) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, network, addr)
		},
	}}
}
