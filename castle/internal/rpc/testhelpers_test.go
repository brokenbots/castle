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
	"time"

	"connectrpc.com/connect"
	"connectrpc.com/grpcreflect"
	"golang.org/x/net/http2"
	"golang.org/x/net/http2/h2c"

	"github.com/brokenbots/castle/castle/internal/auth"
	"github.com/brokenbots/castle/castle/internal/hub"
	"github.com/brokenbots/castle/castle/internal/store"
	"github.com/brokenbots/castle/castle/internal/store/sqlite"
	criteria "github.com/brokenbots/criteria/sdk"
	pb "github.com/brokenbots/criteria/sdk/pb/criteria/v1"                // import-lint:allow castle service bindings (W08: move to castle-proto)
	"github.com/brokenbots/criteria/sdk/pb/criteria/v1/criteriav1connect" // import-lint:allow castle service bindings (W08: move to castle-proto)
)

type testStack struct {
	store        store.Store
	hub          *hub.Hub
	controls     *ControlRegistry
	criteria     *CriteriaServer
	server       *ServerServer
	orchestrator *OrchestratorServer
}

func newTestStack(t *testing.T) *testStack {
	t.Helper()
	return newTestStackWithLog(t, slog.New(slog.NewTextHandler(io.Discard, nil)))
}

func newTestStackWithLog(t *testing.T, log *slog.Logger) *testStack {
	t.Helper()
	s, err := sqlite.Open(filepath.Join(t.TempDir(), "castle.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	h := hub.New()
	controls := NewControlRegistry()
	return &testStack{
		store:        s,
		hub:          h,
		controls:     controls,
		criteria:     NewCriteriaServer(s, h, log, controls),
		server:       NewServerServer(s, h, log, controls),
		orchestrator: NewOrchestratorServer(s, log),
	}
}

func (s *testStack) startServer(t *testing.T, opts ...connect.HandlerOption) (*httptest.Server, criteriav1connect.CriteriaServiceClient, criteriav1connect.ServerServiceClient) {
	t.Helper()
	mux := http.NewServeMux()
	oPath, oHandler := criteriav1connect.NewCriteriaServiceHandler(s.criteria, opts...)
	cPath, cHandler := criteriav1connect.NewServerServiceHandler(s.server, opts...)
	orchPath, orchHandler := criteriav1connect.NewOrchestratorServiceHandler(s.orchestrator, opts...)
	mux.Handle(oPath, oHandler)
	mux.Handle(cPath, cHandler)
	mux.Handle(orchPath, orchHandler)

	// Mount reflection so e2e tests can assert the endpoint is reachable
	// and exempt from auth.
	reflector := grpcreflect.NewStaticReflector(
		criteriav1connect.CriteriaServiceName,
		criteriav1connect.ServerServiceName,
		criteriav1connect.OrchestratorServiceName,
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

// orchestratorClient builds an OrchestratorServiceClient against the test
// server returned by startServer.
func orchestratorClient(tsrv *httptest.Server) criteriav1connect.OrchestratorServiceClient {
	return criteriav1connect.NewOrchestratorServiceClient(h2cClient(), tsrv.URL)
}

// provisionOrchestratorIdentity stores an orchestrator accept-token identity
// for wire-level tests. The returned token is what the client presents in the
// Authorization header.
func provisionOrchestratorIdentity(t *testing.T, st store.Store, id, token string) {
	t.Helper()
	if err := st.UpsertOrchestrator(context.Background(), &store.Orchestrator{
		ID:        id,
		Name:      "operator",
		TokenHash: auth.HashToken(token),
		CreatedAt: time.Now().UTC(),
	}); err != nil {
		t.Fatalf("provision orchestrator identity: %v", err)
	}
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
