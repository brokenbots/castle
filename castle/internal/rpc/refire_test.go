package rpc

import (
	"bytes"
	"context"
	"log/slog"
	"strings"
	"testing"
	"time"

	"connectrpc.com/connect"

	"github.com/brokenbots/castle/castle/internal/auth"
	"github.com/brokenbots/castle/castle/internal/store"
	pb "github.com/brokenbots/criteria/sdk/pb/criteria/v1"
)

// stallLikeAgent stamps the assignment's run failed the heartbeat reaper
// shape with an ended_at far enough in the past that the default refire
// backoff has elapsed (RefireFailedRunsNow evaluates at time.Now).
func stallLikeAgent(t *testing.T, ctx context.Context, st store.Store, runID string) {
	t.Helper()
	run, err := st.GetRun(ctx, runID)
	if err != nil {
		t.Fatalf("get run: %v", err)
	}
	started := time.Now().UTC().Add(-time.Minute)
	ended := time.Now().UTC().Add(-31 * time.Second)
	run.Status = "failed"
	run.CurrentStep = "s1"
	run.StartedAt = &started
	run.EndedAt = &ended
	run.FailureReason = "agent heartbeat lost"
	if err := st.UpdateRun(ctx, run); err != nil {
		t.Fatalf("update run: %v", err)
	}
	if err := st.MarkWorkflowAssignmentTerminal(ctx, runID, store.ReapReasonAgentHeartbeatLost); err != nil {
		t.Fatalf("mark terminal: %v", err)
	}
}

// parkLikeAgent stamps the assignment's run as the human-gate completion the
// refire policy classifies as a designed park (KB-321 / KB-270).
func parkLikeAgent(t *testing.T, ctx context.Context, st store.Store, runID string) {
	t.Helper()
	run, err := st.GetRun(ctx, runID)
	if err != nil {
		t.Fatalf("get run: %v", err)
	}
	ended := time.Now().UTC().Add(-31 * time.Second)
	run.Status = "failed"
	run.StartedAt = &ended
	run.EndedAt = &ended
	run.FailureReason = "workflow ended without success"
	if err := st.UpdateRun(ctx, run); err != nil {
		t.Fatalf("update run: %v", err)
	}
	if err := st.MarkWorkflowAssignmentTerminal(ctx, runID, "run completed"); err != nil {
		t.Fatalf("mark terminal: %v", err)
	}
	ev := store.Event{
		SchemaVersion: int32(store.EventSchemaVersion),
		RunID:         runID,
		Type:          "run.completed",
		Ts:            time.Now().UTC(),
		CorrelationID: "corr-refire-test",
		Payload:       []byte(`{"final_state":"need_assistance"}`),
	}
	if _, inserted, err := st.AppendEvent(ctx, &ev); err != nil || !inserted {
		t.Fatalf("append event: %v/%v", inserted, err)
	}
}

// TestRefireFailedRunsNow_RequeuesAndDispatchesStalledRun is the refire
// contract at the dispatch boundary: a stall-failed assignment is refired
// under the built-in policy and the minted successor assignment flows to a
// connected eligible agent through the normal Control pipe, with the
// assignment's labels and family lineage carried along.
func TestRefireFailedRunsNow_RequeuesAndDispatchesStalledRun(t *testing.T) {
	ts := newTestStack(t)
	ctx := context.Background()

	criteriaID, _ := registerAgent(t, ts, "agent-1", map[string]string{"env": "prod"})
	ch, err := ts.controls.Register(criteriaID)
	if err != nil {
		t.Fatalf("register control channel: %v", err)
	}
	defer ts.controls.Unregister(criteriaID, ch)

	subCtx := auth.WithCallerCriteriaID(ctx, "owner-1")
	resp, err := ts.server.SubmitWorkflowAssignment(subCtx, connect.NewRequest(&pb.SubmitWorkflowAssignmentRequest{WorkflowName: "wf", WorkflowSource: "hcl", IdempotencyKey: "kb-11-stall", Labels: map[string]string{"env": "prod"}}))
	if err != nil {
		t.Fatalf("submit: %v", err)
	}

	stallLikeAgent(t, ctx, ts.store, resp.Msg.RunId)

	outcomes, err := ts.server.RefireFailedRunsNow(ctx)
	if err != nil {
		t.Fatalf("RefireFailedRunsNow: %v", err)
	}
	if got := refireDispositionCount(outcomes, store.RefireDispositionRefire); got != 1 {
		t.Fatalf("outcomes = %v, want exactly one refire", outcomes)
	}
	successor, err := ts.store.GetWorkflowAssignment(ctx, outcomes[0].RefireAssignmentID)
	if err != nil {
		t.Fatalf("get successor: %v", err)
	}
	if successor.RunID == resp.Msg.RunId {
		t.Fatalf("successor must be a new run, got %s", successor.RunID)
	}
	if successor.IdempotencyKey != "kb-11-stall#attempt-2" {
		t.Fatalf("successor key = %q", successor.IdempotencyKey)
	}
	if successor.RefireRootRunID != resp.Msg.RunId || successor.RefireAttempt != 2 {
		t.Fatalf("successor lineage = %q/%d", successor.RefireRootRunID, successor.RefireAttempt)
	}

	// The minted successor is dispatched to the connected agent through the
	// normal assignment pipeline.
	select {
	case msg := <-ch:
		wa := msg.GetWorkflowAssignment()
		if wa == nil {
			t.Fatalf("expected WorkflowAssignment control message, got %v", msg)
		}
		if wa.RunId != successor.RunID {
			t.Fatalf("control message run id = %s, want successor %s", wa.RunId, successor.RunID)
		}
		if wa.GetLabels()["env"] != "prod" {
			t.Fatalf("control message labels = %v, want carried env=prod", wa.GetLabels())
		}
	case <-time.After(2 * time.Second):
		t.Fatalf("timed out waiting for refire dispatch")
	}

	leased, err := ts.store.GetWorkflowAssignment(ctx, successor.ID)
	if err != nil {
		t.Fatalf("reload successor: %v", err)
	}
	if leased.State != store.WorkflowAssignmentStateLeased || leased.LeasedCriteriaID != criteriaID {
		t.Fatalf("successor state = %s leased to %q", leased.State, leased.LeasedCriteriaID)
	}

	// The parked vocabulary is not involved: no awaiting_human stamps.
	origRun, err := ts.store.GetRun(ctx, resp.Msg.RunId)
	if err != nil {
		t.Fatalf("get original run: %v", err)
	}
	if origRun.Status != "failed" {
		t.Fatalf("original run status moved to %q", origRun.Status)
	}
}

// TestRefireFailedRunsNow_ParkedRunNeverDispatched is the human-gate half of
// the contract: a designed park (need_assistance completion) is reclassified
// to the parked run shape and never dispatched, with or without a connected
// agent.
func TestRefireFailedRunsNow_ParkedRunNeverDispatched(t *testing.T) {
	ts := newTestStack(t)
	ctx := context.Background()

	criteriaID, _ := registerAgent(t, ts, "agent-1", nil)
	ch, err := ts.controls.Register(criteriaID)
	if err != nil {
		t.Fatalf("register control channel: %v", err)
	}
	defer ts.controls.Unregister(criteriaID, ch)

	subCtx := auth.WithCallerCriteriaID(ctx, "owner-1")
	resp, err := ts.server.SubmitWorkflowAssignment(subCtx, connect.NewRequest(&pb.SubmitWorkflowAssignmentRequest{WorkflowName: "wf", WorkflowSource: "hcl", IdempotencyKey: "kb-11-park"}))
	if err != nil {
		t.Fatalf("submit: %v", err)
	}
	parkLikeAgent(t, ctx, ts.store, resp.Msg.RunId)

	outcomes, err := ts.server.RefireFailedRunsNow(ctx)
	if err != nil {
		t.Fatalf("RefireFailedRunsNow: %v", err)
	}
	if got := refireDispositionCount(outcomes, store.RefireDispositionPark); got != 1 {
		t.Fatalf("outcomes = %v, want one park: %v", outcomes, refireDispositionSummaries(outcomes))
	}

	run, err := ts.store.GetRun(ctx, resp.Msg.RunId)
	if err != nil {
		t.Fatalf("get run: %v", err)
	}
	if run.Status != store.RunStatusAwaitingHuman {
		t.Fatalf("parked run status = %q, want %s", run.Status, store.RunStatusAwaitingHuman)
	}
	if run.EndedAt != nil || run.FailureReason != "" {
		t.Fatalf("parked run still carries failure shape: ended_at=%v failure_reason=%q", run.EndedAt, run.FailureReason)
	}

	select {
	case msg := <-ch:
		t.Fatalf("parked run must never be dispatched, got %v", msg)
	case <-time.After(200 * time.Millisecond):
	}
}

// TestRunRefirePass_LogsRefireAndPark pins the passive-pass translation
// layer: the goroutine pass evaluates the queue, dispatches refires, and
// logs the verdict vocabulary so operators see requeue and park lines.
func TestRunRefirePass_LogsRefireAndPark(t *testing.T) {
	var buf bytes.Buffer
	ts := newTestStackWithLog(t, slog.New(slog.NewTextHandler(&buf, nil)))
	ctx := context.Background()
	subCtx := auth.WithCallerCriteriaID(ctx, "owner-1")

	resp, err := ts.server.SubmitWorkflowAssignment(subCtx, connect.NewRequest(&pb.SubmitWorkflowAssignmentRequest{WorkflowName: "wf", WorkflowSource: "hcl", IdempotencyKey: "kb-11-pass-log", Labels: map[string]string{"env": "prod"}}))
	if err != nil {
		t.Fatalf("submit stall: %v", err)
	}
	stallLikeAgent(t, ctx, ts.store, resp.Msg.RunId)
	parkResp, err := ts.server.SubmitWorkflowAssignment(subCtx, connect.NewRequest(&pb.SubmitWorkflowAssignmentRequest{WorkflowName: "wf", WorkflowSource: "hcl", IdempotencyKey: "kb-11-pass-park"}))
	if err != nil {
		t.Fatalf("submit park: %v", err)
	}
	parkLikeAgent(t, ctx, ts.store, parkResp.Msg.RunId)

	ts.server.RunRefirePass(ctx)

	logged := buf.String()
	if !strings.Contains(logged, "refire: requeued failed run under policy") {
		t.Fatalf("expected refire log line, got %q", logged)
	}
	if !strings.Contains(logged, "refire: parked failed run at human gate") {
		t.Fatalf("expected park log line, got %q", logged)
	}
}

func refireDispositionCount(outcomes []store.RefireOutcome, disposition string) int {
	n := 0
	for _, o := range outcomes {
		if o.Disposition == disposition {
			n++
		}
	}
	return n
}

func refireDispositionSummaries(outcomes []store.RefireOutcome) string {
	var parts []string
	for _, o := range outcomes {
		parts = append(parts, o.Disposition+": "+o.Reason)
	}
	return strings.Join(parts, "; ")
}
