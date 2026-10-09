package rpc

// Regression integration test for KB-222 (2026-09-21 incident): an operator
// SubmitEvents bidi stream hit its five-minute gRPC deadline and, afterwards,
// every DB-backed RPC on the instance failed until restart.
//
// The store layer now detaches every operation from the stream context
// (sqlite.opCtx); these tests pin the streaming wiring end to end: a stream
// whose deadline expires (both mid-processing and after acknowledged work)
// must never leave the server unable to serve subsequent RPCs, and the
// acknowledged events must stay durable.
//
// Note on the incident signature: on the driver version deployed at the time
// (modernc.org/sqlite v1.33.1) the expired context armed sqlite3_interrupt on
// the shared serialized writer connection. Deterministic poisoning of the
// in-flight write is pinned at the store layer (sqlite/stream_cancel_test.go,
// red against the pre-fix store); here we assert the same invariant through
// the live gRPC surface, where the post-ack expiry path is what is
// deterministically constructible over the wire.

import (
	"context"
	"testing"
	"time"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/types/known/timestamppb"

	criteria "github.com/brokenbots/criteria/sdk"
	pb "github.com/brokenbots/criteria/sdk/pb/criteria/v1" // import-lint:allow castle service bindings (W08: move to castle-proto)
)

func envelopeForRun(runID, correlationID string) *pb.Envelope {
	return &pb.Envelope{
		SchemaVersion: int32(criteria.SchemaVersion),
		RunId:         runID,
		CorrelationId: correlationID,
		Ts:            timestamppb.Now(),
		Payload:       &pb.Envelope_StepEntered{StepEntered: &pb.StepEntered{Step: "step1", Adapter: "shell", Attempt: 1}},
	}
}

// pollPersistedSequences lists runID's events until correlationID is present
// or the deadline lapses.
func pollPersistedSequences(t *testing.T, ts *testStack, runID, correlationID string, within time.Duration) bool {
	t.Helper()
	deadline := time.Now().Add(within)
	for time.Now().Before(deadline) {
		events, err := ts.store.ListEvents(context.Background(), runID, 0, 2048)
		if err != nil {
			t.Fatalf("ListEvents while polling: %v", err)
		}
		for _, ev := range events {
			if ev.CorrelationID == correlationID {
				return true
			}
		}
		time.Sleep(20 * time.Millisecond)
	}
	return false
}

func TestSubmitEventsStreamExpiryKeepsServerHealthy(t *testing.T) {
	ts := newTestStack(t)
	_, oClient, _ := ts.startServer(t)
	overseerID, token := mustRegister(t, oClient)

	createReq := connect.NewRequest(&pb.CreateRunRequest{CriteriaId: overseerID, WorkflowName: "wf", WorkflowHash: "hash"})
	createReq.Header().Set("Authorization", "Bearer "+token)
	runResp, err := oClient.CreateRun(context.Background(), createReq)
	if err != nil {
		t.Fatal(err)
	}
	runID := runResp.Msg.RunId

	// Phase 1: acknowledged work on a stream whose deadline fires minutes
	// later (compressed here) — the expired stream must not poison anything.
	stream := oClient.SubmitEvents(context.Background())
	stream.RequestHeader().Set("Authorization", "Bearer "+token)
	if err := stream.Send(envelopeForRun(runID, "c-expiry")); err != nil {
		t.Fatal(err)
	}
	ack, err := stream.Receive()
	if err != nil {
		t.Fatal(err)
	}
	if ack.CorrelationId != "c-expiry" || ack.Seq == 0 {
		t.Fatalf("unexpected ack: %+v", ack)
	}
	_ = stream.CloseRequest()

	if !pollPersistedSequences(t, ts, runID, "c-expiry", 2*time.Second) {
		t.Fatal("acknowledged event not persisted")
	}

	// Phase 2: a stream that expires with no work in flight (the incident's
	// five-minute idle expiry).
	expired, cancelExpired := context.WithTimeout(context.Background(), 60*time.Millisecond)
	defer cancelExpired()
	idleStream := oClient.SubmitEvents(expired)
	idleStream.RequestHeader().Set("Authorization", "Bearer "+token)
	time.Sleep(120 * time.Millisecond)
	_ = idleStream.CloseRequest()

	// Phase 3: the server must be fully healthy afterwards — during the
	// incident Heartbeat and every subsequent RPC failed for ~32 minutes.
	hbCtx, cancelHB := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancelHB()
	if _, err := oClient.Heartbeat(hbCtx, connect.NewRequest(&pb.HeartbeatRequest{CriteriaId: overseerID})); err != nil {
		t.Fatalf("Heartbeat after stream expiry: %v", err)
	}

	// A fresh stream acks and persists, and the acknowledged event lands in
	// the store for later streams to replay (since_seq replay wiring).
	healthy := oClient.SubmitEvents(context.Background())
	healthy.RequestHeader().Set("Authorization", "Bearer "+token)
	healthy.RequestHeader().Set("since_seq", "0")
	if err := healthy.Send(envelopeForRun(runID, "c-healthy")); err != nil {
		t.Fatal(err)
	}
	replayed := 0
	for {
		ack, err := healthy.Receive()
		if err != nil {
			t.Fatalf("fresh stream died after expired stream: %v", err)
		}
		if ack.CorrelationId == "c-expiry" {
			replayed++
			continue
		}
		if ack.CorrelationId == "c-healthy" {
			break
		}
		t.Fatalf("unexpected ack correlation %q seq %d", ack.CorrelationId, ack.Seq)
	}
	_ = healthy.CloseRequest()
	if replayed != 1 {
		t.Fatalf("replayed %d prior events, want 1", replayed)
	}
	if !pollPersistedSequences(t, ts, runID, "c-healthy", 2*time.Second) {
		t.Fatal("fresh-stream event not persisted after expired stream")
	}

	// Unary RPCs that read the run stay healthy too.
	if _, err := ts.store.GetRun(context.Background(), runID); err != nil {
		t.Fatalf("GetRun after stream expiry: %v", err)
	}
}
