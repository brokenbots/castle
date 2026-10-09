package rpc

import (
	"context"
	"io"
	"log/slog"
	"path/filepath"
	"testing"
	"time"

	"connectrpc.com/connect"

	"github.com/brokenbots/castle/castle/internal/auth"
	"github.com/brokenbots/castle/castle/internal/store"
	"github.com/brokenbots/castle/castle/internal/store/sqlite"
	pb "github.com/brokenbots/criteria/sdk/pb/criteria/v1" // import-lint:allow castle service bindings (W08: move to castle-proto)
)

// TestRegisterThenCreateRunAfterCastleRestart is the KB-223 regression: after
// a castle restart that leaves existing runs behind, an engine that registers
// must be able to use the fresh token on CreateRun immediately. Before the
// durable-before-ack fix a registered-but-invisible token surfaced as
// "unauthenticated: invalid token" 1-2ms after a successful Register, burning
// the engine's restart budget in a crash loop while castle stayed up.
func TestRegisterThenCreateRunAfterCastleRestart(t *testing.T) {
	ctx := context.Background()
	dbPath := filepath.Join(t.TempDir(), "castle.db")
	now := time.Now().UTC()

	// First life: castle had a veteran agent and an existing run before the
	// bounce (the incident shape: restart with existing runs).
	first, err := sqlite.Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := first.CreateOverseer(ctx, &store.Overseer{
		ID: "veteran", Name: "veteran", TokenHash: auth.HashToken("veteran-token"),
		Status: "online", CreatedAt: now.Add(-time.Hour), LastSeenAt: now.Add(-time.Hour),
	}); err != nil {
		t.Fatal(err)
	}
	if err := first.CreateRun(ctx, &store.Run{
		ID: "run-veteran", OverseerID: "veteran", WorkflowName: "wf-veteran",
		Status: "succeeded", CreatedAt: now.Add(-30 * time.Minute),
	}); err != nil {
		t.Fatal(err)
	}
	if err := first.Close(); err != nil {
		t.Fatal(err)
	}

	// Second life: reopen the same persistence file and wire the full HTTP
	// stack with the production auth shape (bootstrap-gated Register).
	reopened, err := sqlite.Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	ts := newTestStackOnStore(t, reopened, slog.New(slog.NewTextHandler(io.Discard, nil)))
	opts := []connect.HandlerOption{connect.WithInterceptors(auth.NewInterceptor(reopened, false, auth.WithBootstrapToken("bootstrap-secret")))}
	_, oClient, _ := ts.startServer(t, opts...)

	regReq := connect.NewRequest(&pb.RegisterRequest{Name: "post-restart-engine"})
	regReq.Header().Set("X-Server-Bootstrap", "bootstrap-secret")
	reg, err := oClient.Register(ctx, regReq)
	if err != nil {
		t.Fatalf("register after restart: %v", err)
	}

	// The regression: CreateRun with the fresh token immediately after a
	// successful Register must authenticate. The token is acked only once the
	// read-back confirmed it is resolvable, so a rejection here is a
	// store-level failure, not a visibility window.
	createReq := connect.NewRequest(&pb.CreateRunRequest{CriteriaId: reg.Msg.CriteriaId, WorkflowName: "wf"})
	createReq.Header().Set("Authorization", "Bearer "+reg.Msg.Token)
	run, err := oClient.CreateRun(ctx, createReq)
	if err != nil {
		t.Fatalf("create run with freshly registered token after restart: %v", err)
	}
	if run.Msg.RunId == "" {
		t.Fatal("create run returned an empty run id")
	}

	// The restart preserved existing data, and the fresh token resolves.
	vet, err := reopened.GetRun(ctx, "run-veteran")
	if err != nil || vet == nil {
		t.Fatalf("veteran run did not survive the restart: %v %v", vet, err)
	}
	resolved, err := auth.ResolveToken(ctx, reopened, reg.Msg.Token)
	if err != nil {
		t.Fatalf("resolve fresh token after restart: %v", err)
	}
	if resolved == nil || resolved.ID != reg.Msg.CriteriaId {
		t.Fatalf("fresh token resolved to %v, want %s", resolved, reg.Msg.CriteriaId)
	}
}