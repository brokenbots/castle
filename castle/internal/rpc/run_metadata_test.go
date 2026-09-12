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
// criteria-k8s operator flow: CreateRun with ticket + repo URL, RunMetadata
// events promoting the PR URL, and full read-path visibility
// (ListRuns/GetRun/ListRunEvents/WatchRun) for the Parapet UI.
func TestK8sRunLifecyclePublishing(t *testing.T) {
	ts := newTestStack(t)
	ctx := context.Background()
	_, oClient, cClient := ts.startServer(t,
		connect.WithInterceptors(auth.NewInterceptor(ts.store, true, auth.WithAnonRegister())),
	)

	overseerID, token := mustRegisterNamed(t, oClient, "criteria-k8s-operator")

	// Operator-side item 1: CreateRun carries the run identifier, ticket label
	// and repo URL. Castle mints the run id; the operator persists it in the CR.
	createReq := connect.NewRequest(&pb.CreateRunRequest{
		CriteriaId:   overseerID,
		WorkflowName: "cri-131-flow",
		Ticket:       "CRI-131",
		RepoUrl:      "brokenbots/castle",
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
	if got := runResp.Msg.Ticket; got != "CRI-131" {
		t.Fatalf("created run ticket=%q want CRI-131", got)
	}
	if got := runResp.Msg.RepoUrl; got != "brokenbots/castle" {
		t.Fatalf("created run repo_url=%q want brokenbots/castle", got)
	}

	// Parapet read path: ListRuns and GetRun expose the k8s-native fields.
	listResp, err := cClient.ListRuns(ctx, connect.NewRequest(&pb.ListRunsRequest{}))
	if err != nil {
		t.Fatal(err)
	}
	var listed *pb.Run
	for _, r := range listResp.Msg.Runs {
		if r.RunId == runID {
			listed = r
		}
	}
	if listed == nil {
		t.Fatal("created run missing from ListRuns")
	}
	if listed.Ticket != "CRI-131" || listed.RepoUrl != "brokenbots/castle" {
		t.Fatalf("ListRun fields=%q/%q want ticket/repo persisted", listed.Ticket, listed.RepoUrl)
	}

	getResp, err := cClient.GetRun(ctx, connect.NewRequest(&pb.GetRunRequest{RunId: runID}))
	if err != nil {
		t.Fatal(err)
	}
	if getResp.Msg.Ticket != "CRI-131" || getResp.Msg.RepoUrl != "brokenbots/castle" {
		t.Fatalf("GetRun fields=%q/%q want ticket/repo persisted", getResp.Msg.Ticket, getResp.Msg.RepoUrl)
	}

	// Subscribe before publishing so the run.metadata event is observed live.
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

	// Operator-side item 2: a phase/metadata transition is submitted through
	// SubmitEvents; the PR URL is only known once the run reaches a terminal
	// phase, so it is published via a run.metadata envelope. The store must
	// promote it onto the run row without clearing ticket/repo_url.
	stream := oClient.SubmitEvents(ctx)
	stream.RequestHeader().Set("Authorization", "Bearer "+token)
	err = stream.Send(&pb.Envelope{
		SchemaVersion: int32(criteria.SchemaVersion),
		RunId:         runID,
		CorrelationId: "cri131-meta-1",
		Ts:            timestamppb.Now(),
		Payload: &pb.Envelope_RunMetadata{RunMetadata: &pb.RunMetadata{
			PrUrl: "https://github.com/brokenbots/castle/pull/42",
		}},
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

	if !watch.Receive() {
		t.Fatalf("expected run.metadata event, err=%v", watch.Err())
	}
	if watch.Msg().GetRunMetadata() == nil {
		t.Fatalf("expected run.metadata payload, got %T", watch.Msg().Payload)
	}

	afterMeta, err := cClient.GetRun(ctx, connect.NewRequest(&pb.GetRunRequest{RunId: runID}))
	if err != nil {
		t.Fatal(err)
	}
	if got := afterMeta.Msg.PrUrl; got != "https://github.com/brokenbots/castle/pull/42" {
		t.Fatalf("run pr_url=%q want promoted value", got)
	}
	// Non-empty-only promotion: empty ticket in the metadata event must not
	// clear the ticket recorded at create time.
	if got := afterMeta.Msg.Ticket; got != "CRI-131" {
		t.Fatalf("run ticket=%q want unchanged", got)
	}

	// The metadata event is also queryable for the run detail event log.
	events, err := cClient.ListRunEvents(ctx, connect.NewRequest(&pb.ListRunEventsRequest{RunId: runID, SinceSeq: 0, Limit: 100}))
	if err != nil {
		t.Fatal(err)
	}
	var metaEvent *pb.Envelope
	for _, ev := range events.Msg.Events {
		if ev.GetRunMetadata() != nil {
			metaEvent = ev
		}
	}
	if metaEvent == nil {
		t.Fatalf("expected a run.metadata event in ListRunEvents, got %d events", len(events.Msg.Events))
	}
	if got := metaEvent.GetRunMetadata().GetPrUrl(); got != "https://github.com/brokenbots/castle/pull/42" {
		t.Fatalf("run.metadata pr_url=%q want payload preserved", got)
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

	// The agent-initiated shape stays intact: no ticket, repo or PR.
	legacyReq := connect.NewRequest(&pb.CreateRunRequest{CriteriaId: overseerID, WorkflowName: "legacy-agent-run"})
	legacyReq.Header().Set("Authorization", "Bearer "+token)
	legacyResp, err := oClient.CreateRun(ctx, legacyReq)
	if err != nil {
		t.Fatal(err)
	}
	if legacyResp.Msg.Ticket != "" || legacyResp.Msg.RepoUrl != "" || legacyResp.Msg.PrUrl != "" {
		t.Fatalf("legacy run metadata should be empty, got %q/%q/%q", legacyResp.Msg.Ticket, legacyResp.Msg.RepoUrl, legacyResp.Msg.PrUrl)
	}
}

func mustRegisterNamed(t *testing.T, client criteriav1connect.CriteriaServiceClient, name string) (string, string) {
	t.Helper()
	resp, err := client.Register(context.Background(), connect.NewRequest(&pb.RegisterRequest{Name: name}))
	if err != nil {
		t.Fatal(err)
	}
	return resp.Msg.CriteriaId, resp.Msg.Token
}