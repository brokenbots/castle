package auth

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"connectrpc.com/connect"
	"golang.org/x/net/http2"
	"golang.org/x/net/http2/h2c"

	"github.com/brokenbots/castle/castle/internal/store/sqlite"
	pb "github.com/brokenbots/criteria/sdk/pb/criteria/v1" // import-lint:allow castle service bindings (W08: move to castle-proto)
	"github.com/brokenbots/criteria/sdk/pb/criteria/v1/criteriav1connect"
)

// capturedCaller records the credential class the handler observed through
// the interceptor — the identity (if any) and the anonymous-read marker.
type capturedCaller struct {
	criteria  string
	anonymous bool
}

// newMarkerUnaryServer serves procedure behind an auth interceptor built from
// the given allowAnonReads flag and captures the caller identity/mark the
// handler observes. Wire-shaped like the interceptor tests so the marker
// contract is exercised end to end (not just the helper functions).
func newMarkerUnaryServer(t *testing.T, st *sqlite.Store, allowAnonReads bool, procedure string) (string, *capturedCaller) {
	t.Helper()
	captured := &capturedCaller{}
	handler := func(ctx context.Context, _ *connect.Request[pb.GetRunRequest]) (*connect.Response[pb.Run], error) {
		captured.criteria = CallerCriteriaID(ctx)
		captured.anonymous = CallerIsAnonymousRead(ctx)
		return connect.NewResponse(&pb.Run{RunId: "r1"}), nil
	}
	h := connect.NewUnaryHandler(procedure, handler, connect.WithInterceptors(NewInterceptor(st, allowAnonReads)))
	mux := http.NewServeMux()
	mux.Handle(procedure, h)
	tsrv := httptest.NewUnstartedServer(h2c.NewHandler(mux, &http2.Server{}))
	tsrv.Start()
	t.Cleanup(tsrv.Close)
	return tsrv.URL + procedure, captured
}

// markerClient calls the procedure URL with (optionally) a bearer token.
func markerClientCall(url string, token string) (*connect.Response[pb.Run], error) {
	client := connect.NewClient[pb.GetRunRequest, pb.Run](httpClient(), url)
	req := connect.NewRequest(&pb.GetRunRequest{RunId: "r1"})
	if token != "" {
		req.Header().Set("Authorization", "Bearer "+token)
	}
	return client.CallUnary(context.Background(), req)
}

// TestAnonReadRunReadPassesThroughMarked pins the KB-326 S3 interceptor
// contract on the per-run read surface: with the flag OFF a tokenless GetRun
// is no longer refused by the interceptor itself — it is passed through
// MARKED anonymous so the handler owns the per-run grant — while a presented
// valid token is still fully authenticated (identity injected, never marked)
// and an invalid token stays rejected.
func TestAnonReadRunReadPassesThroughMarked(t *testing.T) {
	st := newVisibilityStore(t)
	seedOverseer(t, st, "agent-1", "tok-agent-1")

	url, captured := newMarkerUnaryServer(t, st, false, criteriav1connect.ServerServiceGetRunProcedure)

	t.Run("tokenless passes through marked anonymous", func(t *testing.T) {
		resp, err := markerClientCall(url, "")
		if err != nil {
			t.Fatalf("tokenless GetRun must reach the handler for the grant check: %v", err)
		}
		if resp.Msg.RunId != "r1" {
			t.Fatalf("unexpected run id %q", resp.Msg.RunId)
		}
		if !captured.anonymous {
			t.Error("tokenless caller was not marked anonymous")
		}
		if captured.criteria != "" {
			t.Errorf("tokenless caller carries identity %q", captured.criteria)
		}
	})

	t.Run("valid token authenticates unmarked", func(t *testing.T) {
		if _, err := markerClientCall(url, "tok-agent-1"); err != nil {
			t.Fatalf("valid token: %v", err)
		}
		if captured.anonymous {
			t.Error("an authenticated caller must not be marked anonymous")
		}
		if captured.criteria != "agent-1" {
			t.Errorf("identity = %q, want agent-1", captured.criteria)
		}
	})

	t.Run("invalid token stays rejected", func(t *testing.T) {
		if _, err := markerClientCall(url, "tok-never-issued"); err == nil {
			t.Fatal("an invalid token must not pass the anonymous surface")
		} else if connect.CodeOf(err) != connect.CodeUnauthenticated {
			t.Fatalf("expected Unauthenticated, got %v", connect.CodeOf(err))
		}
	})
}

// TestAnonReadFlagGatedObservationListAgents pins that only the PER-RUN read
// surfaces are flag-independent: ListAgents remains refused for a tokenless
// caller when the flag is off, and anonymously reachable (still marked) when
// the flag is on.
func TestAnonReadFlagGatedObservationListAgents(t *testing.T) {
	st := newVisibilityStore(t)

	t.Run("flag off stays credential-gated", func(t *testing.T) {
		url, captured := newMarkerUnaryServer(t, st, false, criteriav1connect.ServerServiceListAgentsProcedure)
		if _, err := markerClientCall(url, ""); err == nil {
			t.Fatal("tokenless ListAgents at flag off must be refused")
		} else if connect.CodeOf(err) != connect.CodeUnauthenticated {
			t.Fatalf("expected Unauthenticated, got %v", connect.CodeOf(err))
		}
		if captured.anonymous || captured.criteria != "" {
			t.Error("the rejected call should never have reached the handler")
		}
	})

	t.Run("flag on passes through marked anonymous", func(t *testing.T) {
		url, captured := newMarkerUnaryServer(t, st, true, criteriav1connect.ServerServiceListAgentsProcedure)
		if _, err := markerClientCall(url, ""); err != nil {
			t.Fatalf("tokenless ListAgents at flag on: %v", err)
		}
		if !captured.anonymous {
			t.Error("flag-on tokenless ListAgents must be marked anonymous")
		}
	})
}
