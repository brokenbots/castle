package rpc

import (
	"context"
	"crypto/tls"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
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

	castlev1connect "github.com/brokenbots/castle/castle/gen/castle/v1/castlev1connect"
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
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "castle.db")
	s, err := sqlite.Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	ts := newTestStackOnStore(t, s, log)
	t.Cleanup(func() {
		// Ordered teardown (drain → close → sidecar-leak check → sweep) as
		// one cleanup so the ordering is deterministic; Go's own TempDir
		// cleanup runs after this and finds an already-swept directory.
		if err := teardownStack(ts, s, dbPath); err != nil {
			t.Errorf("test stack teardown: %v", err)
		}
	})
	return ts
}

// teardownStack performs the ordered teardown every store-backed test stack
// must run before the surrounding t.TempDir is removed:
//  1. drain debounced persistence writers (stop the variable-scope
//     coalescer's pending timers and flush what is queued, so no write can
//     race the store close — the cursor writers started by WatchRun are
//     stopped by their owning handlers, which the httptest.Server.Close
//     waits for before returning);
//  2. close the store;
//  3. detect leaks: after a clean close the SQLite driver deletes the
//     castle.db-wal/-shm sidecars, so one still present means a straggler
//     connection outlived the close — reported rather than swept silently;
//  4. sweep the store directory with bounded retries so no sidecar can fail
//     the TempDir removal with "directory not empty".
//
// The returned error is for direct callers that want to assert teardown
// cleanliness; the cleanup wrappers use t.Errorf on it.
func teardownStack(ts *testStack, s *sqlite.Store, dbPath string) error {
	closeErr := teardownStackStop(ts, s)
	dir := filepath.Dir(dbPath)
	if leaked := leakedStoreSidecars(dir, filepath.Base(dbPath)); len(leaked) > 0 {
		return fmt.Errorf("leaked WAL sidecars after store close: %v", leaked)
	}
	if sweepErr := sweepStoreDir(dir, 5, 20*time.Millisecond); sweepErr != nil {
		if closeErr != nil {
			return closeErr
		}
		return sweepErr
	}
	return closeErr
}

// teardownStackStop is teardownStack without any directory side effects:
// drains debounced writers, then closes the store (idempotent). Callers that
// still want to read the store after teardown reopen it on the same path.
func teardownStackStop(ts *testStack, s *sqlite.Store) error {
	ts.criteria.DrainBackgroundWriters()
	return s.Close()
}

// leakedStoreSidecars lists SQLite sidecar files ("<db>-wal", "<db>-shm")
// that still exist next to dbName in root: they are absent after a clean
// close (or when the DB was never written), so any hit is a leaked writer.
func leakedStoreSidecars(root, dbName string) []string {
	suffixes := []string{"-wal", "-shm"}
	var leaked []string
	for _, suf := range suffixes {
		if _, err := os.Stat(filepath.Join(root, dbName+suf)); err == nil {
			leaked = append(leaked, dbName+suf)
		}
	}
	return leaked
}

// sweepStoreDir removes dir with bounded retries, tolerating the lazy
// recreation of SQLite WAL sidecars by straggler connections.
func sweepStoreDir(dir string, attempts int, backoff time.Duration) error {
	var err error
	for i := 0; i < attempts; i++ {
		if err = os.RemoveAll(dir); err == nil {
			return nil
		}
		if i < attempts-1 {
			time.Sleep(backoff)
		}
	}
	return err
}

// newTestStackOnStore builds the wire-level server set on a caller-owned
// store. Used by tests that reopen persistence (restart simulations) or wrap
// the store to control visibility. The caller owns teardown: it must combine
// draining the stack's debounced writers with the store close and a
// sweepStoreDir of the store directory under one t.Cleanup (see
// newTestStackWithStore and newCursorSpyStack).
func newTestStackOnStore(t *testing.T, s *sqlite.Store, log *slog.Logger) *testStack {
	t.Helper()
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
	consolePath, consoleHandler := castlev1connect.NewConsoleServiceHandler(s.server, opts...)
	mux.Handle(oPath, oHandler)
	mux.Handle(cPath, cHandler)
	mux.Handle(orchPath, orchHandler)
	mux.Handle(consolePath, consoleHandler)

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

// consoleClient builds a castle-owned ConsoleServiceClient against the test
// server returned by startServer (CRI-195 login surface).
func consoleClient(tsrv *httptest.Server) castlev1connect.ConsoleServiceClient {
	return castlev1connect.NewConsoleServiceClient(h2cClient(), tsrv.URL)
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

// criteriaRegisterAgent mints a fresh agent identity through the real
// Register flow and returns (criteriaID, token). Service-scoped wire tests
// that cannot use the shared all-services mount use this to exercise the
// same podstate-feed token path.
func criteriaRegisterAgent(t *testing.T, srv *httptest.Server, name string) (string, string) {
	t.Helper()
	client := criteriav1connect.NewCriteriaServiceClient(h2cClient(), srv.URL)
	resp, err := client.Register(context.Background(), connect.NewRequest(&pb.RegisterRequest{Name: name}))
	if err != nil {
		t.Fatalf("register agent %q: %v", name, err)
	}
	return resp.Msg.GetCriteriaId(), resp.Msg.GetToken()
}

// mountCriteriaHandler mounts the criteria service handler on a custom mux,
// for service-scoped wire servers that need the Register flow mounted.
func mountCriteriaHandler(mux *http.ServeMux, ts *testStack, opts ...connect.HandlerOption) {
	path, handler := criteriav1connect.NewCriteriaServiceHandler(ts.criteria, opts...)
	mux.Handle(path, handler)
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
