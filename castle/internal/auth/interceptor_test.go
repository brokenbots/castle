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
	pb "github.com/brokenbots/castle/shared/pb/overlord/v1"                // import-lint:allow castle service bindings (W08: move to castle-proto)
	"github.com/brokenbots/castle/shared/pb/overlord/v1/overlordv1connect" // import-lint:allow castle service bindings (W08: move to castle-proto)
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
		overlordv1connect.CastleServiceGetRunProcedure,
		func(context.Context, *connect.Request[pb.GetRunRequest]) (*connect.Response[pb.Run], error) {
			return connect.NewResponse(&pb.Run{RunId: "r1"}), nil
		},
		connect.WithInterceptors(NewInterceptor(db, false)),
	)
	mux := http.NewServeMux()
	mux.Handle(overlordv1connect.CastleServiceGetRunProcedure, h)
	tsrv := httptest.NewUnstartedServer(h2c.NewHandler(mux, &http2.Server{}))
	tsrv.Start()
	t.Cleanup(tsrv.Close)

	client := connect.NewClient[pb.GetRunRequest, pb.Run](httpClient(), tsrv.URL+overlordv1connect.CastleServiceGetRunProcedure)

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
		overlordv1connect.OverseerServiceRegisterProcedure,
		func(context.Context, *connect.Request[pb.RegisterRequest]) (*connect.Response[pb.RegisterResponse], error) {
			return connect.NewResponse(&pb.RegisterResponse{OverseerId: "o1"}), nil
		},
		// WithAnonRegister is required: Register now requires a bootstrap token unless
		// anonymous registration is explicitly enabled (dev/test mode).
		connect.WithInterceptors(NewInterceptor(nil, true, WithAnonRegister())),
	)
	mux := http.NewServeMux()
	mux.Handle(overlordv1connect.OverseerServiceRegisterProcedure, h)
	tsrv := httptest.NewUnstartedServer(h2c.NewHandler(mux, &http2.Server{}))
	tsrv.Start()
	t.Cleanup(tsrv.Close)

	client := connect.NewClient[pb.RegisterRequest, pb.RegisterResponse](httpClient(), tsrv.URL+overlordv1connect.OverseerServiceRegisterProcedure)
	_, err := client.CallUnary(context.Background(), connect.NewRequest(&pb.RegisterRequest{Name: "test"}))
	if err != nil {
		t.Fatal(err)
	}
}

// TestAuthInterceptor_CallerIDInjected verifies that AuthInterceptor resolves
// the caller's overseer ID and injects it into the handler context via
// CallerOverseerID. This locks in the context-injection contract so future
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
		overlordv1connect.CastleServiceGetRunProcedure,
		func(ctx context.Context, _ *connect.Request[pb.GetRunRequest]) (*connect.Response[pb.Run], error) {
			capturedID = CallerOverseerID(ctx)
			return connect.NewResponse(&pb.Run{RunId: "r1"}), nil
		},
		connect.WithInterceptors(NewInterceptor(db, false)),
	)
	mux := http.NewServeMux()
	mux.Handle(overlordv1connect.CastleServiceGetRunProcedure, h)
	tsrv := httptest.NewUnstartedServer(h2c.NewHandler(mux, &http2.Server{}))
	tsrv.Start()
	t.Cleanup(tsrv.Close)

	client := connect.NewClient[pb.GetRunRequest, pb.Run](httpClient(), tsrv.URL+overlordv1connect.CastleServiceGetRunProcedure)
	req := connect.NewRequest(&pb.GetRunRequest{RunId: "r1"})
	req.Header().Set("Authorization", "Bearer tok-inject")
	if _, err := client.CallUnary(context.Background(), req); err != nil {
		t.Fatal(err)
	}

	if capturedID != "overseer-xyz" {
		t.Errorf("expected CallerOverseerID='overseer-xyz', got %q", capturedID)
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
