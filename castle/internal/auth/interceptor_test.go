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

	"github.com/brokenbots/overlord/castle/internal/store"
	"github.com/brokenbots/overlord/castle/internal/store/sqlite"
	pb "github.com/brokenbots/overlord/shared/pb/overlord/v1"
	"github.com/brokenbots/overlord/shared/pb/overlord/v1/overlordv1connect"
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
		connect.WithInterceptors(NewInterceptor(nil, true)),
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

func httpClient() *http.Client {
	return &http.Client{Transport: &http2.Transport{
		AllowHTTP: true,
		DialTLSContext: func(ctx context.Context, network, addr string, _ *tls.Config) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, network, addr)
		},
	}}
}
