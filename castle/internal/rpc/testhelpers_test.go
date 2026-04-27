package rpc

import (
	"context"
	"crypto/tls"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"connectrpc.com/connect"
	"connectrpc.com/grpcreflect"
	"golang.org/x/net/http2"
	"golang.org/x/net/http2/h2c"

	"github.com/brokenbots/overlord/castle/internal/hub"
	"github.com/brokenbots/overlord/castle/internal/store"
	"github.com/brokenbots/overlord/castle/internal/store/sqlite"
	pb "github.com/brokenbots/overlord/shared/pb/overlord/v1" // import-lint:allow castle service bindings (W08: move to castle-proto)
	"github.com/brokenbots/overlord/shared/pb/overlord/v1/overlordv1connect" // import-lint:allow castle service bindings (W08: move to castle-proto)
	overseer "github.com/brokenbots/overlord/shared/sdk/overseer"
)

type testStack struct {
	store    store.Store
	hub      *hub.Hub
	controls *ControlRegistry
	overseer *OverseerServer
	castle   *CastleServer
}

func newTestStack(t *testing.T) *testStack {
	t.Helper()
	s, err := sqlite.Open(filepath.Join(t.TempDir(), "castle.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	h := hub.New()
	controls := NewControlRegistry()
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	return &testStack{
		store:    s,
		hub:      h,
		controls: controls,
		overseer: NewOverseerServer(s, h, log, controls),
		castle:   NewCastleServer(s, h, log, controls),
	}
}

func (s *testStack) startServer(t *testing.T, opts ...connect.HandlerOption) (*httptest.Server, overseer.ServiceClient, overlordv1connect.CastleServiceClient) {
	t.Helper()
	mux := http.NewServeMux()
	oPath, oHandler := overseer.NewServiceHandler(s.overseer, opts...)
	cPath, cHandler := overlordv1connect.NewCastleServiceHandler(s.castle, opts...)
	mux.Handle(oPath, oHandler)
	mux.Handle(cPath, cHandler)

	// Mount reflection so e2e tests can assert the endpoint is reachable
	// and exempt from auth.
	reflector := grpcreflect.NewStaticReflector(
		overseer.ServiceName,
		overlordv1connect.CastleServiceName,
	)
	rPath, rHandler := grpcreflect.NewHandlerV1(reflector)
	mux.Handle(rPath, rHandler)
	rAlphaPath, rAlphaHandler := grpcreflect.NewHandlerV1Alpha(reflector)
	mux.Handle(rAlphaPath, rAlphaHandler)

	tsrv := httptest.NewUnstartedServer(h2c.NewHandler(mux, &http2.Server{}))
	tsrv.Start()
	t.Cleanup(tsrv.Close)

	client := h2cClient()
	return tsrv,
		overseer.NewServiceClient(client, tsrv.URL),
		overlordv1connect.NewCastleServiceClient(client, tsrv.URL)
}

func h2cClient() *http.Client {
	return &http.Client{Transport: &http2.Transport{
		AllowHTTP: true,
		DialTLSContext: func(ctx context.Context, network, addr string, _ *tls.Config) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, network, addr)
		},
	}}
}

func mustRegister(t *testing.T, client overseer.ServiceClient) (string, string) {
	t.Helper()
	resp, err := client.Register(context.Background(), connect.NewRequest(&pb.RegisterRequest{Name: "test-overseer"}))
	if err != nil {
		t.Fatal(err)
	}
	return resp.Msg.OverseerId, resp.Msg.Token
}
