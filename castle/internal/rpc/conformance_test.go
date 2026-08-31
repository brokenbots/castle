//go:build conformance

package rpc

import (
	"context"
	"net/http"
	"sync"
	"testing"
	"time"

	"connectrpc.com/connect"

	"github.com/brokenbots/castle/castle/internal/auth"
	"github.com/brokenbots/castle/castle/internal/store"
	criteria "github.com/brokenbots/criteria/sdk"
	"github.com/brokenbots/criteria/sdk/conformance"
	pb "github.com/brokenbots/criteria/sdk/pb/criteria/v1"                // import-lint:allow castle service bindings (W08: move to castle-proto)
	"github.com/brokenbots/criteria/sdk/pb/criteria/v1/criteriav1connect" // import-lint:allow castle service bindings (W08: move to castle-proto)
)

// TestCastleConformance runs the full SDK conformance suite against Castle.
// The suite is gated behind the "conformance" build tag so it stays out of
// the default `make test` lane. Use `make test-conformance` to run it.
func TestCastleConformance(t *testing.T) {
	conformance.Run(t, &castleSubject{})
}

// castleSubject implements conformance.Subject backed by a real Castle server
// (fully wired: SQLite store, event hub, auth interceptor, control registry).
// It is analogous to the per-test stack used in auth_negative_test.go and
// other Castle tests.
type castleSubject struct {
	mu sync.Mutex
	ts *testStack
}

// SetUp starts a fresh isolated Castle server with the standard auth interceptor
// (no anon-register). RegisterOverseer uses the direct-store path to bypass the
// wire-level bootstrap requirement, so anon-register is not needed. Standard
// auth ensures testOwnership_RegisterBootstrapGate observes the real bootstrap
// gate. The returned HTTP client is h2c-aware so bidi streams work over plain
// HTTP/2 (as in other Castle test helpers).
func (s *castleSubject) SetUp(t *testing.T) (string, *http.Client, func()) {
	t.Helper()
	ts := newTestStack(t)
	srv, _, _ := ts.startServer(t, connect.WithInterceptors(
		auth.NewInterceptor(ts.store, false),
	))
	s.mu.Lock()
	s.ts = ts
	s.mu.Unlock()
	// srv.Close is already registered as t.Cleanup inside startServer.
	return srv.URL, h2cClient(), func() {}
}

// RegisterAgent inserts an overseer record directly into the SQLite store,
// bypassing the Register RPC and its bootstrap requirement. This is the
// standard test-setup path: it does not exercise the Register wire contract
// (that is testOwnership_RegisterBootstrapGate's job).
func (s *castleSubject) RegisterAgent(t *testing.T, name, token string) string {
	t.Helper()
	s.mu.Lock()
	ts := s.ts
	s.mu.Unlock()
	if ts == nil {
		t.Fatal("castleSubject.RegisterOverseer called before SetUp")
	}
	id := "overseer-" + name
	now := time.Now().UTC()
	err := ts.store.CreateOverseer(context.Background(), &store.Overseer{
		ID:         id,
		Name:       name,
		TokenHash:  auth.HashToken(token),
		Status:     "online",
		CreatedAt:  now,
		LastSeenAt: now,
	})
	if err != nil {
		t.Fatalf("RegisterOverseer(%s): CreateOverseer: %v", name, err)
	}
	return id
}

// ListRunEvents retrieves events for runID via the ServerService ListRunEvents
// RPC. criteriaToken authenticates the caller. The conformance suite uses this
// to assert persistence without the conformance package importing ServerService.
func (s *castleSubject) ListRunEvents(t *testing.T, baseURL string, client *http.Client, overseerToken, runID string, sinceSeq uint64) []*criteria.Envelope {
	t.Helper()
	cClient := criteriav1connect.NewServerServiceClient(client, baseURL)
	req := connect.NewRequest(&pb.ListRunEventsRequest{
		RunId:    runID,
		SinceSeq: sinceSeq,
		Limit:    1000,
	})
	req.Header().Set("Authorization", "Bearer "+overseerToken)
	resp, err := cClient.ListRunEvents(context.Background(), req)
	if err != nil {
		t.Fatalf("ListRunEvents(run=%s): %v", runID, err)
	}
	return resp.Msg.Events
}

// StopRun sends a StopRun request via the ServerService on behalf of the run
// owner. Returns the RPC error so conformance tests can inspect the error code.
func (s *castleSubject) StopRun(t *testing.T, baseURL string, client *http.Client, ownerToken, runID string) error {
	t.Helper()
	cClient := criteriav1connect.NewServerServiceClient(client, baseURL)
	req := connect.NewRequest(&pb.StopRunRequest{RunId: runID})
	req.Header().Set("Authorization", "Bearer "+ownerToken)
	_, err := cClient.StopRun(context.Background(), req)
	return err
}
