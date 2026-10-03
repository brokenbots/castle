package rpc

import (
	"context"
	"testing"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/brokenbots/castle/castle/internal/auth"
	criteria "github.com/brokenbots/criteria/sdk"
	pb "github.com/brokenbots/criteria/sdk/pb/criteria/v1"
	criteriav1connect "github.com/brokenbots/criteria/sdk/pb/criteria/v1/criteriav1connect"
)

// TestK8sRunLifecyclePublishing is the CRI-131 contract test for the
// criteria-k8s operator flow against the released criteria/sdk: CreateRun
// carries no run-context fields, ticket/repo/pr arrive via run.metadata
// envelopes and are promoted onto castle's local run row (non-empty-only),
// and every metadata envelope stays visible on the read path
// (ListRunEvents/WatchRun) for the Parapet UI.
func TestK8sRunLifecyclePublishing(t *testing.T) {
	ts := newTestStack(t)
	ctx := context.Background()
	_, oClient, cClient := ts.startServer(t,
		connect.WithInterceptors(auth.NewInterceptor(ts.store, true, auth.WithAnonRegister())),
	)

	overseerID, token := mustRegisterNamed(t, oClient, "criteria-k8s-operator")

	// Operator-side item 1: CreateRun mints the run id that the operator
	// persists in the CR. The released wire contract has no first-class
	// ticket/repo fields, so run context is published afterwards via
	// run.metadata envelopes (CRI-131 dave ruling: never wire fields).
	createReq := connect.NewRequest(&pb.CreateRunRequest{
		CriteriaId:   overseerID,
		WorkflowName: "cri-131-flow",
	})
	createReq.Header().Set("Authorization", "Bearer "+token)
	runResp, err := oClient.CreateRun(ctx, createReq)
	if err != nil {
		t.Fatal(err)
	}
	runID := runResp.Msg.RunId
	if runID == "" {
		t.Fatal("expected castle to mint a run id")
	}
	row, err := ts.store.GetRun(ctx, runID)
	if err != nil {
		t.Fatal(err)
	}
	if row.Ticket != "" || row.RepoURL != "" || row.PRURL != "" {
		t.Fatalf("fresh run row should carry no run context, got %q/%q/%q", row.Ticket, row.RepoURL, row.PRURL)
	}

	// Subscribe before publishing so the run.metadata events are observed live.
	watch, err := cClient.WatchRun(ctx, connect.NewRequest(&pb.WatchRunRequest{RunId: runID, SinceSeq: 0}))
	if err != nil {
		t.Fatal(err)
	}
	if !watch.Receive() {
		t.Fatalf("expected WatchReady, err=%v", watch.Err())
	}
	if _, ok := watch.Msg().Payload.(*pb.Envelope_WatchReady); !ok {
		t.Fatalf("expected WatchReady, got %T", watch.Msg().Payload)
	}

	// submitMeta publishes one run.metadata envelope through SubmitEvents.
	submitMeta := func(t *testing.T, corr string, meta *pb.RunMetadata) {
		t.Helper()
		stream := oClient.SubmitEvents(ctx)
		stream.RequestHeader().Set("Authorization", "Bearer "+token)
		err := stream.Send(&pb.Envelope{
			SchemaVersion: int32(criteria.SchemaVersion),
			RunId:         runID,
			CorrelationId: corr,
			Ts:            timestamppb.Now(),
			Payload:       &pb.Envelope_RunMetadata{RunMetadata: meta},
		})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := stream.Receive(); err != nil {
			t.Fatal(err)
		}
		if err := stream.CloseRequest(); err != nil {
			t.Fatal(err)
		}
	}

	expectRow := func(t *testing.T, ticket, repo, pr string) {
		t.Helper()
		row, err := ts.store.GetRun(ctx, runID)
		if err != nil {
			t.Fatal(err)
		}
		if row.Ticket != ticket || row.RepoURL != repo || row.PRURL != pr {
			t.Fatalf("run row=%q/%q/%q want %q/%q/%q", row.Ticket, row.RepoURL, row.PRURL, ticket, repo, pr)
		}
	}

	// Operator-side item 2: ticket + repo are published as soon as the
	// operator reconciles the run; the PR URL is only known once the run
	// reaches a terminal phase, so it arrives in a later envelope.
	submitMeta(t, "cri131-meta-1", &pb.RunMetadata{
		Ticket:  "CRI-131",
		RepoUrl: "brokenbots/castle",
	})
	if !watch.Receive() {
		t.Fatalf("expected run.metadata event, err=%v", watch.Err())
	}
	if watch.Msg().GetRunMetadata() == nil {
		t.Fatalf("expected run.metadata payload, got %T", watch.Msg().Payload)
	}
	expectRow(t, "CRI-131", "brokenbots/castle", "")

	submitMeta(t, "cri131-meta-2", &pb.RunMetadata{
		PrUrl: "https://github.com/brokenbots/castle/pull/42",
	})
	expectRow(t, "CRI-131", "brokenbots/castle", "https://github.com/brokenbots/castle/pull/42")

	// Non-empty-only promotion: an all-empty metadata envelope must not clear
	// anything already recorded (proto3 absence is indistinguishable from "").
	submitMeta(t, "cri131-meta-3", &pb.RunMetadata{})
	expectRow(t, "CRI-131", "brokenbots/castle", "https://github.com/brokenbots/castle/pull/42")

	// The metadata envelopes are queryable for the run detail event log.
	events, err := cClient.ListRunEvents(ctx, connect.NewRequest(&pb.ListRunEventsRequest{RunId: runID, SinceSeq: 0, Limit: 100}))
	if err != nil {
		t.Fatal(err)
	}
	var metaEvents []*pb.RunMetadata
	for _, ev := range events.Msg.Events {
		if m := ev.GetRunMetadata(); m != nil {
			metaEvents = append(metaEvents, m)
		}
	}
	if len(metaEvents) != 3 {
		t.Fatalf("expected 3 run.metadata events in ListRunEvents, got %d", len(metaEvents))
	}
	// Envelopes are stored exactly as submitted (non-empty-only promotion is
	// derived store state, never a rewrite of the event log).
	if metaEvents[0].Ticket != "CRI-131" || metaEvents[0].RepoUrl != "brokenbots/castle" {
		t.Fatalf("run.metadata[0]=%q/%q want ticket+repo", metaEvents[0].Ticket, metaEvents[0].RepoUrl)
	}
	const wantPR = "https://github.com/brokenbots/castle/pull/42"
	if metaEvents[1].PrUrl != wantPR {
		t.Fatalf("run.metadata pr_url=%q want payload preserved", metaEvents[1].PrUrl)
	}
	if metaEvents[2].Ticket != "" || metaEvents[2].RepoUrl != "" || metaEvents[2].PrUrl != "" {
		t.Fatalf("run.metadata[2] should be the empty promotion guard event, got %+v", metaEvents[2])
	}

	// Phase transitions follow the existing vocabulary: Running → Succeeded.
	phaseStream := oClient.SubmitEvents(ctx)
	phaseStream.RequestHeader().Set("Authorization", "Bearer "+token)
	err = phaseStream.Send(&pb.Envelope{
		SchemaVersion: int32(criteria.SchemaVersion),
		RunId:         runID,
		CorrelationId: "cri131-running-1",
		Ts:            timestamppb.Now(),
		Payload:       &pb.Envelope_RunStarted{RunStarted: &pb.RunStarted{InitialStep: "publish"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := phaseStream.Receive(); err != nil {
		t.Fatal(err)
	}
	err = phaseStream.Send(&pb.Envelope{
		SchemaVersion: int32(criteria.SchemaVersion),
		RunId:         runID,
		CorrelationId: "cri131-succeeded-1",
		Ts:            timestamppb.Now(),
		Payload:       &pb.Envelope_RunCompleted{RunCompleted: &pb.RunCompleted{Success: true}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := phaseStream.Receive(); err != nil {
		t.Fatal(err)
	}
	if err := phaseStream.CloseRequest(); err != nil {
		t.Fatal(err)
	}

	phaseRun, err := cClient.GetRun(ctx, connect.NewRequest(&pb.GetRunRequest{RunId: runID}))
	if err != nil {
		t.Fatal(err)
	}
	if got := phaseRun.Msg.Status; got != "succeeded" {
		t.Fatalf("run status=%q want succeeded after RunCompleted", got)
	}
	// Run lifecycle must not disturb promoted run context.
	expectRow(t, "CRI-131", "brokenbots/castle", "https://github.com/brokenbots/castle/pull/42")
}

func mustRegisterNamed(t *testing.T, client criteriav1connect.CriteriaServiceClient, name string) (string, string) {
	t.Helper()
	resp, err := client.Register(context.Background(), connect.NewRequest(&pb.RegisterRequest{Name: name}))
	if err != nil {
		t.Fatal(err)
	}
	return resp.Msg.CriteriaId, resp.Msg.Token
}