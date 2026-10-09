package auth

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
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

// visibilityTestStore wraps a real store so tests can make a registered
// overseer invisible to token resolution while still reporting it through the
// OverseerTokenHashPresent capability the auth interceptor consults on a full
// token miss (KB-223).
type visibilityTestStore struct {
	store.Store
	hideOverseers bool
	presentFn     func(ctx context.Context, tokenHash string) (bool, error)
}

func (s *visibilityTestStore) ListOverseers(ctx context.Context) ([]*store.Overseer, error) {
	if s.hideOverseers {
		return nil, nil
	}
	return s.Store.ListOverseers(ctx)
}

func (s *visibilityTestStore) OverseerTokenHashPresent(ctx context.Context, tokenHash string) (bool, error) {
	if s.presentFn != nil {
		return s.presentFn(ctx, tokenHash)
	}
	return false, nil
}

// newVisibilityStore opens a file-backed store in a temp dir; the sqlite
// backend supplies every delegation path of visibilityTestStore.
func newVisibilityStore(t *testing.T) *sqlite.Store {
	t.Helper()
	st, err := sqlite.Open(filepath.Join(t.TempDir(), "visibility.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	return st
}

// newVisibilityUnaryServer serves GetRun behind an auth interceptor over the
// given store, the same wiring shape as the interceptor tests.
func newVisibilityUnaryServer(t *testing.T, st store.Store) string {
	t.Helper()
	h := connect.NewUnaryHandler(
		criteriav1connect.ServerServiceGetRunProcedure,
		func(context.Context, *connect.Request[pb.GetRunRequest]) (*connect.Response[pb.Run], error) {
			return connect.NewResponse(&pb.Run{RunId: "r1"}), nil
		},
		connect.WithInterceptors(NewInterceptor(st, false)),
	)
	mux := http.NewServeMux()
	mux.Handle(criteriav1connect.ServerServiceGetRunProcedure, h)
	tsrv := httptest.NewUnstartedServer(h2c.NewHandler(mux, &http2.Server{}))
	tsrv.Start()
	t.Cleanup(tsrv.Close)
	return tsrv.URL + criteriav1connect.ServerServiceGetRunProcedure
}

func callWithToken(t *testing.T, url, token string) error {
	t.Helper()
	client := connect.NewClient[pb.GetRunRequest, pb.Run](httpClient(), url)
	req := connect.NewRequest(&pb.GetRunRequest{RunId: "r1"})
	req.Header().Set("Authorization", "Bearer "+token)
	_, err := client.CallUnary(context.Background(), req)
	return err
}

func seedOverseer(t *testing.T, st *sqlite.Store, id, token string) {
	t.Helper()
	now := time.Now().UTC()
	if err := st.CreateOverseer(context.Background(), &store.Overseer{
		ID: id, Name: id, TokenHash: HashToken(token),
		Status: "online", CreatedAt: now, LastSeenAt: now,
	}); err != nil {
		t.Fatalf("seed overseer %s: %v", id, err)
	}
}

// TestInterceptorRegisteredButInvisibleTokenIsUnavailable pins the KB-223
// error distinction: when a presented token exists in the store's write view
// but token resolution could not observe it, the rejection must be the
// retryable "token not yet visible" (Unavailable) rather than the misleading
// generic "invalid token" (Unauthenticated) that sent engines into restart
// loops after a castle restart.
func TestInterceptorRegisteredButInvisibleTokenIsUnavailable(t *testing.T) {
	st := newVisibilityStore(t)
	seedOverseer(t, st, "victim", "victim-token")

	wrapped := &visibilityTestStore{
		Store:         st,
		hideOverseers: true,
		presentFn: func(context.Context, string) (bool, error) {
			return true, nil
		},
	}

	url := newVisibilityUnaryServer(t, wrapped)
	err := callWithToken(t, url, "victim-token")
	if err == nil {
		t.Fatal("a token that resolution cannot observe must not authenticate")
	}
	if connect.CodeOf(err) != connect.CodeUnavailable {
		t.Fatalf("expected Unavailable for a registered-but-invisible token, got %v: %v", connect.CodeOf(err), err)
	}
	if !strings.Contains(err.Error(), "token not yet visible") {
		t.Fatalf("expected the not-yet-visible message, got %q", err.Error())
	}
}

// TestInterceptorUnknownTokenStaysInvalid covers the negative distinctions:
// a token that exists nowhere (or whose presence the store cannot confirm)
// remains a plain invalid-token rejection even through the capability path.
func TestInterceptorUnknownTokenStaysInvalid(t *testing.T) {
	t.Run("probe confirms absence", func(t *testing.T) {
		st := newVisibilityStore(t)
		seedOverseer(t, st, "unrelated", "unrelated-token")
		wrapped := &visibilityTestStore{
			Store:         st,
			hideOverseers: true,
			presentFn: func(context.Context, string) (bool, error) {
				return false, nil
			},
		}
		url := newVisibilityUnaryServer(t, wrapped)
		err := callWithToken(t, url, "never-issued-token")
		if connect.CodeOf(err) != connect.CodeUnauthenticated {
			t.Fatalf("expected Unauthenticated, got %v: %v", connect.CodeOf(err), err)
		}
		if !strings.Contains(err.Error(), "invalid token") {
			t.Fatalf("expected the invalid-token message, got %q", err.Error())
		}
	})

	t.Run("steady-state store without a suspected gap", func(t *testing.T) {
		st := newVisibilityStore(t)
		seedOverseer(t, st, "victim", "victim-token")

		// The real store implements the capability, but with no suspected
		// gap the gate is closed: an unknown token is a plain miss.
		url := newVisibilityUnaryServer(t, st)
		err := callWithToken(t, url, "never-issued-token")
		if connect.CodeOf(err) != connect.CodeUnauthenticated {
			t.Fatalf("expected Unauthenticated, got %v: %v", connect.CodeOf(err), err)
		}
		if strings.Contains(err.Error(), "token not yet visible") {
			t.Fatalf("steady-state miss must not be reported as not-yet-visible: %q", err.Error())
		}
	})

	t.Run("probe failure surfaces internal", func(t *testing.T) {
		st := newVisibilityStore(t)
		wrapped := &visibilityTestStore{
			Store:         st,
			hideOverseers: true,
			presentFn: func(context.Context, string) (bool, error) {
				return false, errors.New("probe failed")
			},
		}
		url := newVisibilityUnaryServer(t, wrapped)
		err := callWithToken(t, url, "never-issued-token")
		if connect.CodeOf(err) != connect.CodeInternal {
			t.Fatalf("expected Internal when the presence probe fails, got %v: %v", connect.CodeOf(err), err)
		}
	})
}