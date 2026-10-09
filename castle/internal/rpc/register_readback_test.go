package rpc

import (
	"context"
	"strings"
	"sync"
	"testing"

	"connectrpc.com/connect"

	"github.com/brokenbots/castle/castle/internal/auth"
	"github.com/brokenbots/castle/castle/internal/store"
	pb "github.com/brokenbots/criteria/sdk/pb/criteria/v1" // import-lint:allow castle service bindings (W08: move to castle-proto)
)

// blindSpotStore wraps a store to simulate the KB-223 visibility window: an
// overseer row the writer committed but the read path cannot see yet. The
// first listBlind ListOverseers calls hide the newly registered row and the
// first getBlind GetOverseer calls report it as not found, mimicking a
// reader/WAL snapshot that lags behind the writer just after CreateOverseer.
// The Register read-back must retry across this window before acknowledging.
type blindSpotStore struct {
	store.Store

	mu        sync.Mutex
	captured  *store.Overseer
	listBlind int
	getBlind  int
	listCalls int
}

func (s *blindSpotStore) CreateOverseer(ctx context.Context, o *store.Overseer) error {
	if err := s.Store.CreateOverseer(ctx, o); err != nil {
		return err
	}
	s.mu.Lock()
	c := *o
	s.captured = &c
	s.mu.Unlock()
	return nil
}

func (s *blindSpotStore) GetOverseer(ctx context.Context, id string) (*store.Overseer, error) {
	s.mu.Lock()
	blind := s.getBlind > 0
	if blind {
		s.getBlind--
	}
	target := s.captured
	s.mu.Unlock()
	if blind && target != nil && id == target.ID {
		return nil, store.ErrNotFound
	}
	return s.Store.GetOverseer(ctx, id)
}

func (s *blindSpotStore) ListOverseers(ctx context.Context) ([]*store.Overseer, error) {
	s.mu.Lock()
	blind := s.listBlind > 0
	if blind {
		s.listBlind--
	}
	target := s.captured
	s.listCalls++
	s.mu.Unlock()
	outs, err := s.Store.ListOverseers(ctx)
	if err != nil {
		return nil, err
	}
	if blind && target != nil {
		filtered := make([]*store.Overseer, 0, len(outs))
		for _, o := range outs {
			if o.ID == target.ID {
				continue
			}
			filtered = append(filtered, o)
		}
		return filtered, nil
	}
	return outs, nil
}

// TestRegisterAcksOnlyAfterTokenVisible is the KB-223 durable-before-ack
// contract: a fresh token returned by Register must be resolvable through the
// exact read path CreateRun authentication uses. A lagging read path makes
// Register retry — and hold back the ack — instead of returning a token that
// deterministically fails CreateRun with unauthenticated.
func TestRegisterAcksOnlyAfterTokenVisible(t *testing.T) {
	ctx := context.Background()
	ts := newTestStack(t)
	// Two blind ListOverseers calls (the first two read-back resolution
	// attempts) and one blind GetOverseer (the first commit confirmation).
	wrapped := &blindSpotStore{Store: ts.store, listBlind: 2, getBlind: 1}
	srv := NewCriteriaServer(wrapped, ts.hub, ts.criteria.Log, ts.controls)

	resp, err := srv.Register(ctx, connect.NewRequest(&pb.RegisterRequest{Name: "post-restart-engine"}))
	if err != nil {
		t.Fatalf("register: %v", err)
	}
	if resp.Msg.CriteriaId == "" || resp.Msg.Token == "" {
		t.Fatalf("register returned an incomplete ack: %+v", resp.Msg)
	}
	if got := wrapped.listCalls; got < 3 {
		t.Fatalf("read-back resolved in %d ListOverseers calls, want >= 3 (two blind attempts plus the confirming one)", got)
	}
	// The acked token resolves against the real underlying store, through
	// the same ListOverseers-backed path the CreateRun interceptor uses.
	resolved, err := auth.ResolveToken(ctx, ts.store, resp.Msg.Token)
	if err != nil {
		t.Fatalf("resolve acked token: %v", err)
	}
	if resolved == nil || resolved.ID != resp.Msg.CriteriaId {
		t.Fatalf("acked token resolved to %v, want the register-returned identity %s", resolved, resp.Msg.CriteriaId)
	}
}

// TestRegisterWithholdsAckWhenTokenNeverVisible proves the ack is withheld
// rather than issued blind: if the read path never resolves the fresh token,
// Register fails without handing out a token, and the identity row remains
// durable in the store for the engine to re-register against.
func TestRegisterWithholdsAckWhenTokenNeverVisible(t *testing.T) {
	ctx := context.Background()
	ts := newTestStack(t)
	// Never reveal the new row to ListOverseers; GetOverseer is honest so the
	// failure isolates the resolution path, exactly like a commit that is
	// durable but invisible to the token read.
	wrapped := &blindSpotStore{Store: ts.store, listBlind: 1000}
	srv := NewCriteriaServer(wrapped, ts.hub, ts.criteria.Log, ts.controls)

	resp, err := srv.Register(ctx, connect.NewRequest(&pb.RegisterRequest{Name: "post-restart-engine"}))
	if err == nil {
		t.Fatal("register acked although the token would never resolve")
	}
	if resp != nil {
		t.Fatalf("failed register must withhold the token; got %+v", resp.Msg)
	}
	if connect.CodeOf(err) != connect.CodeInternal {
		t.Fatalf("expected Internal for a withheld ack, got %v: %v", connect.CodeOf(err), err)
	}
	if !strings.Contains(err.Error(), "not visible to read path") {
		t.Fatalf("expected the withheld-ack reason, got %q", err.Error())
	}
	wrapped.mu.Lock()
	captured := wrapped.captured
	wrapped.mu.Unlock()
	if captured == nil {
		t.Fatal("CreateOverseer never reached the underlying store")
	}
	o, err := ts.store.GetOverseer(ctx, captured.ID)
	if err != nil || o == nil {
		t.Fatalf("identity row not durable after withheld ack: %v %v", o, err)
	}
	if o.TokenHash != captured.TokenHash {
		t.Fatal("persisted token hash does not match the identity Register attempted to issue")
	}
}