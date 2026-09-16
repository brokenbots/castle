package rpc

import (
	"context"
	"fmt"
	"testing"
	"time"

	"connectrpc.com/connect"

	"github.com/brokenbots/castle/castle/internal/auth"
	"github.com/brokenbots/castle/castle/internal/store"
	pb "github.com/brokenbots/criteria/sdk/pb/criteria/v1"
)

// TestListRunsRPCPaging is the wire-level contract test for CRI-187 run-list
// paging: ListRuns honours limit and page_token, returns next_page_token only
// when a further page exists, and rejects malformed tokens with
// CodeInvalidArgument instead of leaking a store error.
func TestListRunsRPCPaging(t *testing.T) {
	ts := newTestStack(t)
	ctx := context.Background()
	_, _, cClient := ts.startServer(t,
		connect.WithInterceptors(auth.NewInterceptor(ts.store, true, auth.WithAnonRegister())),
	)

	now := time.Now().UTC()
	if err := ts.store.CreateOverseer(ctx, &store.Overseer{ID: "ov-page", Name: "paging", TokenHash: "x", Status: "online", CreatedAt: now, LastSeenAt: now}); err != nil {
		t.Fatalf("create overseer: %v", err)
	}
	for i := 0; i < 5; i++ {
		r := &store.Run{
			ID:           fmt.Sprintf("run-%d", i),
			OverseerID:   "ov-page",
			WorkflowName: "wf",
			Status:       "succeeded",
			CreatedAt:    now.Add(time.Duration(i) * time.Minute),
		}
		if err := ts.store.CreateRun(ctx, r); err != nil {
			t.Fatalf("create run-%d: %v", i, err)
		}
	}

	first, err := cClient.ListRuns(ctx, connect.NewRequest(&pb.ListRunsRequest{Limit: 2}))
	if err != nil {
		t.Fatal(err)
	}
	if len(first.Msg.Runs) != 2 {
		t.Fatalf("first page len=%d, want 2", len(first.Msg.Runs))
	}
	if first.Msg.NextPageToken == "" {
		t.Fatal("first page returned no next_page_token despite a further page")
	}

	second, err := cClient.ListRuns(ctx, connect.NewRequest(&pb.ListRunsRequest{Limit: 2, PageToken: first.Msg.NextPageToken}))
	if err != nil {
		t.Fatal(err)
	}
	if len(second.Msg.Runs) != 2 {
		t.Fatalf("second page len=%d, want 2", len(second.Msg.Runs))
	}
	if second.Msg.Runs[0].RunId == first.Msg.Runs[0].RunId || second.Msg.Runs[0].RunId == first.Msg.Runs[1].RunId {
		t.Fatalf("second page repeats first-page rows: %v then %v", runIDs(first.Msg.Runs), runIDs(second.Msg.Runs))
	}

	third, err := cClient.ListRuns(ctx, connect.NewRequest(&pb.ListRunsRequest{Limit: 2, PageToken: second.Msg.NextPageToken}))
	if err != nil {
		t.Fatal(err)
	}
	if len(third.Msg.Runs) != 1 {
		t.Fatalf("third page len=%d, want the single remaining run", len(third.Msg.Runs))
	}
	if third.Msg.NextPageToken != "" {
		t.Fatalf("third page token=%q, want empty once the remainder fits in one page", third.Msg.NextPageToken)
	}

	_, err = cClient.ListRuns(ctx, connect.NewRequest(&pb.ListRunsRequest{Limit: 2, PageToken: "not-a-cursor"}))
	if err == nil {
		t.Fatal("malformed page token accepted, want an error")
	}
	if connect.CodeOf(err) != connect.CodeInvalidArgument {
		t.Fatalf("malformed token code=%v, want invalid_argument", connect.CodeOf(err))
	}
}

func runIDs(runs []*pb.Run) []string {
	out := make([]string, 0, len(runs))
	for _, r := range runs {
		out = append(out, r.RunId)
	}
	return out
}