package rpc

// KB-343 rpc-level failure-story contract: a dispatch fire that cannot lease
// produces an operator-visible Info record naming the declined rule
// ("dispatch for agent: assignment not leased") while an empty queue stays a
// quiet Debug line.
import (
	"context"
	"log/slog"
	"testing"

	"connectrpc.com/connect"
	pb "github.com/brokenbots/criteria/sdk/pb/criteria/v1" // import-lint:allow castle service bindings (W08: move to castle-proto)

	"github.com/brokenbots/castle/castle/internal/auth"
	"github.com/brokenbots/castle/castle/internal/store"
)

func dispatchNotLeasedRecords(handler *recordingSlogHandler) []recordedLog {
	var out []recordedLog
	for _, r := range handler.snapshot() {
		if r.Message == "dispatch for agent: assignment not leased" {
			out = append(out, r)
		}
	}
	return out
}

// TestDispatchNotLeasedLogsLabelsMismatch: a queued assignment demanding a
// label the online agent lacks produces one Info record naming labels_mismatch
// with the declined assignment, and the stamp lands on the row
// (lease_attempt counts the visit).
func TestDispatchNotLeasedLogsLabelsMismatch(t *testing.T) {
	handler := &recordingSlogHandler{}
	ts := newTestStackWithLog(t, slog.New(handler))
	ctx := context.Background()

	agentID, _ := registerAgent(t, ts, "agent-decision", map[string]string{"env": "prod"})
	ownerCtx := auth.WithCallerCriteriaID(ctx, "owner-1")
	submitResp, err := ts.server.SubmitWorkflowAssignment(ownerCtx, connect.NewRequest(&pb.SubmitWorkflowAssignmentRequest{
		WorkflowName:   "wf",
		WorkflowSource: "hcl",
		IdempotencyKey: "key-mismatch",
		Labels:         map[string]string{"env": "staging"},
	}))
	if err != nil {
		t.Fatalf("submit assignment: %v", err)
	}

	ts.criteria.dispatchForAgent(ctx, agentID)

	records := dispatchNotLeasedRecords(handler)
	if len(records) != 1 {
		t.Fatalf("not-leased Info records = %d (%+v), want 1", len(records), records)
	}
	if got := records[0].Attrs["reason"]; got != store.LeaseRejectionLabelsMismatch {
		t.Fatalf("reason = %v, want %v", got, store.LeaseRejectionLabelsMismatch)
	}
	if got, _ := records[0].Attrs["assignment_id"].(string); got == "" {
		t.Fatalf("assignment_id empty, want the declined assignment")
	}
	a, err := ts.store.GetWorkflowAssignmentByRunID(ctx, submitResp.Msg.RunId)
	if err != nil {
		t.Fatal(err)
	}
	if a.LastLeaseRejection != store.LeaseRejectionLabelsMismatch || a.LeaseAttempt == 0 {
		t.Fatalf("row disposition rejection=%q attempt=%d, want labels_mismatch with attempt ≥1",
			a.LastLeaseRejection, a.LeaseAttempt)
	}
}

// TestDispatchOfflineAgentLogsRule: an offline agent short-circuits dispatch
// with the agent_offline rule at Info instead of returning silently.
func TestDispatchOfflineAgentLogsRule(t *testing.T) {
	handler := &recordingSlogHandler{}
	ts := newTestStackWithLog(t, slog.New(handler))
	ctx := context.Background()

	agentID, _ := registerAgent(t, ts, "agent-offline", nil)
	if err := ts.store.UpdateOverseerStatus(ctx, agentID, "offline"); err != nil {
		t.Fatalf("UpdateOverseerStatus: %v", err)
	}
	ownerCtx := auth.WithCallerCriteriaID(ctx, "owner-1")
	if _, err := ts.server.SubmitWorkflowAssignment(ownerCtx, connect.NewRequest(&pb.SubmitWorkflowAssignmentRequest{
		WorkflowName:   "wf",
		WorkflowSource: "hcl",
		IdempotencyKey: "key-offline",
	})); err != nil {
		t.Fatalf("submit assignment: %v", err)
	}

	ts.criteria.dispatchForAgent(ctx, agentID)

	records := dispatchNotLeasedRecords(handler)
	if len(records) != 1 {
		t.Fatalf("not-leased Info records = %d (%+v), want 1", len(records), records)
	}
	if got := records[0].Attrs["reason"]; got != store.LeaseRejectionAgentOffline {
		t.Fatalf("reason = %v, want %v", got, store.LeaseRejectionAgentOffline)
	}
}

// TestDispatchEmptyQueueStaysQuiet: an online agent with nothing queued
// yields no not-leased Info record; the scan outcome is the Debug
// "no queued assignment" line only.
func TestDispatchEmptyQueueStaysQuiet(t *testing.T) {
	handler := &recordingSlogHandler{}
	ts := newTestStackWithLog(t, slog.New(handler))
	ctx := context.Background()

	agentID, _ := registerAgent(t, ts, "agent-quiet", nil)
	ts.criteria.dispatchForAgent(ctx, agentID)

	if records := dispatchNotLeasedRecords(handler); len(records) != 0 {
		t.Fatalf("empty queue produced not-leased Info records: %+v", records)
	}
	var quiet bool
	for _, r := range handler.snapshot() {
		if r.Message == "dispatch for agent: no queued assignment" {
			quiet = true
			break
		}
	}
	if !quiet {
		t.Fatal("empty queue produced no Debug no-queued-assignment record")
	}
}