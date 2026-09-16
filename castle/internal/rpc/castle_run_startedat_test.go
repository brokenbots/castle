package rpc

import (
	"context"
	"testing"
	"time"

	"connectrpc.com/connect"

	"github.com/brokenbots/castle/castle/internal/auth"
	"github.com/brokenbots/castle/castle/internal/store"
	pb "github.com/brokenbots/criteria/sdk/pb/criteria/v1"
)

// TestRunStartedAtWireContract is the wire-level started_at contract for
// CRI-187 run-list durations: a started run exposes StartedAt in both
// ListRuns and GetRun responses, a never-started run omits it, and a
// negative limit is rejected instead of treated as unbounded.
func TestRunStartedAtWireContract(t *testing.T) {
	ts := newTestStack(t)
	ctx := context.Background()
	_, _, cClient := ts.startServer(t,
		connect.WithInterceptors(auth.NewInterceptor(ts.store, true, auth.WithAnonRegister())),
	)

	now := time.Now().UTC()
	if err := ts.store.CreateOverseer(ctx, &store.Overseer{ID: "ov-start", Name: "startedat", TokenHash: "x", Status: "online", CreatedAt: now, LastSeenAt: now}); err != nil {
		t.Fatalf("create overseer: %v", err)
	}
	created := now.Add(-5 * time.Minute)
	started := created.Add(2 * time.Minute)
	if err := ts.store.CreateRun(ctx, &store.Run{ID: "run-started", OverseerID: "ov-start", WorkflowName: "wf", Status: "running", CreatedAt: created, StartedAt: &started}); err != nil {
		t.Fatalf("create run-started: %v", err)
	}
	if err := ts.store.CreateRun(ctx, &store.Run{ID: "run-pending", OverseerID: "ov-start", WorkflowName: "wf", Status: "pending", CreatedAt: created}); err != nil {
		t.Fatalf("create run-pending: %v", err)
	}

	list, err := cClient.ListRuns(ctx, connect.NewRequest(&pb.ListRunsRequest{}))
	if err != nil {
		t.Fatal(err)
	}
	byID := map[string]*pb.Run{}
	for _, r := range list.Msg.Runs {
		byID[r.RunId] = r
	}
	got := byID["run-started"].GetStartedAt()
	if got == nil || !got.AsTime().Equal(started) {
		t.Fatalf("ListRuns started run startedAt = %v, want %v", got, started)
	}
	if byID["run-pending"].GetStartedAt() != nil {
		t.Fatalf("ListRuns pending run startedAt = %v, want unset", byID["run-pending"].GetStartedAt())
	}

	getResp, err := cClient.GetRun(ctx, connect.NewRequest(&pb.GetRunRequest{RunId: "run-started"}))
	if err != nil {
		t.Fatal(err)
	}
	gotGet := getResp.Msg.GetStartedAt()
	if gotGet == nil || !gotGet.AsTime().Equal(started) {
		t.Fatalf("GetRun startedAt = %v, want %v", gotGet, started)
	}

	_, err = cClient.ListRuns(ctx, connect.NewRequest(&pb.ListRunsRequest{Limit: -1}))
	if err == nil {
		t.Fatal("negative limit accepted, want an error")
	}
	if connect.CodeOf(err) != connect.CodeInvalidArgument {
		t.Fatalf("negative limit code=%v, want invalid_argument", connect.CodeOf(err))
	}
}