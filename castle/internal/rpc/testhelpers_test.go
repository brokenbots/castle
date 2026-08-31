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

	"github.com/brokenbots/castle/castle/internal/hub"
	"github.com/brokenbots/castle/castle/internal/store"
	"github.com/brokenbots/castle/castle/internal/store/sqlite"
	pb "github.com/brokenbots/criteria/sdk/pb/criteria/v1"                // import-lint:allow castle service bindings (W08: move to castle-proto)
	"github.com/brokenbots/criteria/sdk/pb/criteria/v1/criteriav1connect" // import-lint:allow castle service bindings (W08: move to castle-proto)
	criteria "github.com/brokenbots/criteria/sdk"
)

type testStack struct {
	store    store.Store
	hub      *hub.Hub
	controls *ControlRegistry
	criteria *CriteriaServer
	server   *ServerServer
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
		criteria: NewCriteriaServer(s, h, log, controls),
		server:   NewServerServer(s, h, log, controls),
	}
}

func (s *testStack) startServer(t *testing.T, opts ...connect.HandlerOption) (*httptest.Server, criteriav1connect.CriteriaServiceClient, criteriav1connect.ServerServiceClient) {
	t.Helper()
	mux := http.NewServeMux()
	oPath, oHandler := criteriav1connect.NewCriteriaServiceHandler(s.criteria, opts...)
	cPath, cHandler := criteriav1connect.NewServerServiceHandler(s.server, opts...)
	mux.Handle(oPath, oHandler)
	mux.Handle(cPath, cHandler)

	// Mount reflection so e2e tests can assert the endpoint is reachable
	// and exempt from auth.
	reflector := grpcreflect.NewStaticReflector(
		criteriav1connect.CriteriaServiceName,
		criteriav1connect.ServerServiceName,
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
		criteriav1connect.NewCriteriaServiceClient(client, tsrv.URL),
		criteriav1connect.NewServerServiceClient(client, tsrv.URL)
}

func h2cClient() *http.Client {
	return &http.Client{Transport: &http2.Transport{
		AllowHTTP: true,
		DialTLSContext: func(ctx context.Context, network, addr string, _ *tls.Config) (net.Conn, error) {
			return (&net.Dialer{}).DialContext(ctx, network, addr)
		},
	}}
}

func mustRegister(t *testing.T, client criteriav1connect.CriteriaServiceClient) (string, string) {
	t.Helper()
	resp, err := client.Register(context.Background(), connect.NewRequest(&pb.RegisterRequest{Name: "test-overseer"}))
	if err != nil {
		t.Fatal(err)
	}
	return resp.Msg.CriteriaId, resp.Msg.Token
}

// mustStoreEvent converts a wire envelope into the storage-neutral Event used
// by the store layer. It fails the test on codec errors. Test code that seeds
// persisted events directly can use this rather than duplicating the RPC codec.
func mustStoreEvent(t *testing.T, env *criteria.Envelope) *store.Event {
	t.Helper()
	ev, err := envelopeToEvent(env)
	if err != nil {
		t.Fatalf("convert envelope to store event: %v", err)
	}
	return ev
}
