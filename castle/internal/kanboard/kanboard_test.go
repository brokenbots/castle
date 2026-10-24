// Client envelope tests: HTTP-level failures, in-band JSON-RPC errors and
// the success decode path. The kanboardtrigger fakes mock at the fake
// handler level; these tests exercise the real Client.call over httptest.
package kanboard

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func newTestClient(t *testing.T, handler http.Handler) (*Client, *httptest.Server) {
	t.Helper()
	srv := httptest.NewServer(handler)
	t.Cleanup(srv.Close)
	return New(srv.URL+"/jsonrpc.php", "test-token"), srv
}

// TestCallNon200NamesStatus: a non-200 HTTP response surfaces the status
// code (and body) instead of being silently misparsed as JSON-RPC.
func TestCallNon200NamesStatus(t *testing.T) {
	c, _ := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "auth failed", http.StatusUnauthorized)
	}))
	var result struct{ Title string }
	err := c.call(context.Background(), "getTask", map[string]int{"task_id": 1}, &result)
	if err == nil {
		t.Fatal("call to 401 server should return an error")
	}
	if !strings.Contains(err.Error(), "HTTP 401") {
		t.Errorf("error %q does not name the HTTP status 401", err)
	}
}

// TestCallInBandRPCError: a 200 response carrying a JSON-RPC error member
// returns an error carrying the RPC code and message.
func TestCallInBandRPCError(t *testing.T) {
	c, _ := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(MarshalRPC(1, nil, &RPCError{Code: 1, Message: "task not found"})))
	}))
	var result struct{ Title string }
	err := c.call(context.Background(), "getTask", map[string]int{"task_id": 99}, &result)
	if err == nil {
		t.Fatal("in-band JSON-RPC error should surface as an error")
	}
	var rpcErr *RPCError
	if !errors.As(err, &rpcErr) {
		t.Fatalf("error %T (%v) is not *RPCError", err, err)
	}
	if rpcErr.Code != 1 || rpcErr.Message != "task not found" {
		t.Errorf("rpc error = %d/%q, want 1/task not found", rpcErr.Code, rpcErr.Message)
	}
}

// TestCallDecodesResult: the success path decodes the result member into
// the target.
func TestCallDecodesResult(t *testing.T) {
	c, _ := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(MarshalRPC(1, map[string]any{"title": "KB-320 trigger adapter"}, nil)))
	}))
	var result struct{ Title string }
	if err := c.call(context.Background(), "getTask", map[string]int{"task_id": 40}, &result); err != nil {
		t.Fatalf("call: %v", err)
	}
	if result.Title != "KB-320 trigger adapter" {
		t.Errorf("decoded title = %q", result.Title)
	}
}
