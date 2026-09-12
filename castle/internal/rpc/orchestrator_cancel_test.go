package rpc

import (
	"context"
	"io"
	"log/slog"
	"testing"
	"time"

	"connectrpc.com/connect"

	"github.com/brokenbots/castle/castle/internal/auth"
	"github.com/brokenbots/castle/castle/internal/store"
	criteria "github.com/brokenbots/criteria/sdk"
	pb "github.com/brokenbots/criteria/sdk/pb/criteria/v1" // import-lint:allow castle service bindings (W08: move to castle-proto)
)

// --- CancelRun: operator terminal stamping (CRI-142) ---

func (h *orchestratorHarness) cancelRun(t *testing.T, token, runID, reason string) (*connect.Response[pb.CancelRunResponse], error) {
	t.Helper()
	req := connect.NewRequest(&pb.CancelRunRequest{RunId: runID, Reason: reason})
	req.Header().Set("Authorization", "Bearer "+token)
	return h.orchClient.CancelRun(context.Background(), req)
}

func TestOrchestratorCancelRun_ByOrchestrator(t *testing.T) {
	h := newOrchestratorHarness(t)
	runID := h.newAgentRun(t, "wf-cancel")

	resp, err := h.cancelRun(t, h.orchestratorToken, runID, "criteriarun deleted")
	if err != nil {
		t.Fatalf("CancelRun: %v", err)
	}
	run := resp.Msg.Run
	if run == nil || run.RunId != runID {
		t.Fatalf("response run mismatch: %+v", run)
	}
	if run.Status != "cancelled" {
		t.Errorf("status = %q, want cancelled", run.Status)
	}
	if run.FailureReason != "criteriarun deleted" {
		t.Errorf("failure_reason = %q, want operator reason", run.FailureReason)
	}
	if run.EndedAt == nil || run.EndedAt.AsTime().IsZero() {
		t.Errorf("ended_at not stamped: %v", run.GetEndedAt())
	}

	// Persisted state agrees with the wire response.
	got, err := h.ts.store.GetRun(context.Background(), runID)
	if err != nil {
		t.Fatalf("store get: %v", err)
	}
	if got.Status != "cancelled" || got.FailureReason != "criteriarun deleted" || got.EndedAt == nil {
		t.Fatalf("persisted cancel: status=%q reason=%q ended=%v", got.Status, got.FailureReason, got.EndedAt)
	}

	// The cancelled run leaves the active set (no permanent pendings).
	activeReq := connect.NewRequest(&pb.ListActiveRunsRequest{})
	activeReq.Header().Set("Authorization", "Bearer "+h.orchestratorToken)
	active, err := h.orchClient.ListActiveRuns(context.Background(), activeReq)
	if err != nil {
		t.Fatalf("ListActiveRuns: %v", err)
	}
	for _, r := range active.Msg.Runs {
		if r.RunId == runID {
			t.Errorf("cancelled run still listed as active")
		}
	}
}

func TestOrchestratorCancelRun_DefaultReason(t *testing.T) {
	h := newOrchestratorHarness(t)
	runID := h.newAgentRun(t, "wf-cancel-default")

	resp, err := h.cancelRun(t, h.orchestratorToken, runID, "")
	if err != nil {
		t.Fatalf("CancelRun: %v", err)
	}
	if resp.Msg.Run.GetFailureReason() != "cancelled by operator" {
		t.Errorf("failure_reason = %q, want default", resp.Msg.Run.GetFailureReason())
	}
	if resp.Msg.Run.GetEndedAt() == nil || resp.Msg.Run.GetEndedAt().AsTime().IsZero() {
		t.Errorf("ended_at not stamped: %v", resp.Msg.Run.GetEndedAt())
	}
}

func TestOrchestratorCancelRun_EmptyRunId(t *testing.T) {
	h := newOrchestratorHarness(t)
	if _, err := h.cancelRun(t, h.orchestratorToken, "", ""); err == nil {
		t.Fatal("cancel with empty run_id must fail")
	} else if connect.CodeOf(err) != connect.CodeInvalidArgument {
		t.Fatalf("want %v, got %v", connect.CodeInvalidArgument, connect.CodeOf(err))
	}
}

func TestOrchestratorCancelRun_ByOwningAgent(t *testing.T) {
	h := newOrchestratorHarness(t)
	runID := h.newAgentRun(t, "wf-cancel-agent")

	if _, err := h.cancelRun(t, h.agentToken, runID, "agent self-cleanup"); err != nil {
		t.Fatalf("owning agent CancelRun: %v", err)
	}
	got, err := h.ts.store.GetRun(context.Background(), runID)
	if err != nil {
		t.Fatalf("store get: %v", err)
	}
	if got.Status != "cancelled" || got.FailureReason != "agent self-cleanup" {
		t.Fatalf("cancelled run: status=%q reason=%q", got.Status, got.FailureReason)
	}
	if got.EndedAt == nil {
		t.Fatalf("cancelled run: ended_at not stamped")
	}
}

func TestOrchestratorCancelRun_ForeignAgentDenied(t *testing.T) {
	h := newOrchestratorHarness(t)
	runID := h.newAgentRun(t, "wf-cancel-foreign")

	reg, err := h.oClient.Register(context.Background(), connect.NewRequest(&pb.RegisterRequest{Name: "agent-other"}))
	if err != nil {
		t.Fatalf("register foreign agent: %v", err)
	}
	if _, err := h.cancelRun(t, reg.Msg.Token, runID, "not mine"); err == nil {
		t.Fatal("foreign agent cancel must be denied")
	} else if connect.CodeOf(err) != connect.CodePermissionDenied {
		t.Fatalf("want %v, got %v", connect.CodePermissionDenied, connect.CodeOf(err))
	}
	got, err := h.ts.store.GetRun(context.Background(), runID)
	if err != nil {
		t.Fatalf("store get: %v", err)
	}
	if got.Status != "pending" || got.FailureReason != "" {
		t.Fatalf("denied cancel mutated run: status=%q reason=%q", got.Status, got.FailureReason)
	}
}

func TestOrchestratorCancelRun_TerminalRunPrecondition(t *testing.T) {
	h := newOrchestratorHarness(t)
	runID := h.newAgentRun(t, "wf-cancel-terminal")

	h.submitEvents(t, []*pb.Envelope{criteria.NewEnvelope(runID, &pb.RunCompleted{Success: true})})

	if _, err := h.cancelRun(t, h.orchestratorToken, runID, "late"); err == nil {
		t.Fatal("cancelling a terminal run must fail")
	} else if connect.CodeOf(err) != connect.CodeFailedPrecondition {
		t.Fatalf("want %v, got %v", connect.CodeFailedPrecondition, connect.CodeOf(err))
	}
	got, err := h.ts.store.GetRun(context.Background(), runID)
	if err != nil {
		t.Fatalf("store get: %v", err)
	}
	if got.Status != "succeeded" || got.FailureReason != "" {
		t.Fatalf("terminal run mutated: status=%q reason=%q", got.Status, got.FailureReason)
	}
}

func TestOrchestratorCancelRun_NotFound(t *testing.T) {
	h := newOrchestratorHarness(t)
	if _, err := h.cancelRun(t, h.orchestratorToken, "missing-run", ""); err == nil {
		t.Fatal("cancel unknown run must fail")
	} else if connect.CodeOf(err) != connect.CodeNotFound {
		t.Fatalf("want %v, got %v", connect.CodeNotFound, connect.CodeOf(err))
	}
}

// TestOrchestratorCancelRun_MarksAssignmentTerminal verifies the handler
// terminates queued workflow-assignment work for the cancelled run so the
// operator's delete is deterministic cleanup (CRI-142).
func TestOrchestratorCancelRun_MarksAssignmentTerminal(t *testing.T) {
	h := newOrchestratorHarness(t)
	now := time.Now().UTC()
	a, created, err := h.ts.store.CreateWorkflowAssignment(context.Background(), &store.WorkflowAssignment{
		OwnerCriteriaID: h.agentID,
		WorkflowName:    "wf-cancel-asg",
		WorkflowSource:  "workflow main {}",
		IdempotencyKey:  "cancel-asg-1",
		State:           store.WorkflowAssignmentStateQueued,
		CreatedAt:       now,
		UpdatedAt:       now,
	})
	if err != nil || !created {
		t.Fatalf("create assignment: %v created=%v", err, created)
	}

	if _, err := h.cancelRun(t, h.orchestratorToken, a.RunID, "criteriarun deleted"); err != nil {
		t.Fatalf("CancelRun: %v", err)
	}

	got, err := h.ts.store.GetWorkflowAssignment(context.Background(), a.ID)
	if err != nil {
		t.Fatalf("get assignment: %v", err)
	}
	if got.State != store.WorkflowAssignmentStateTerminal || got.TerminalReason != "criteriarun deleted" {
		t.Fatalf("assignment: state=%q reason=%q", got.State, got.TerminalReason)
	}
}

// TestOrchestratorCancelRun_DurableAgainstLateAgentEvents pins cancel
// durability (CRI-142): a still-live agent that submits terminal events after
// the operator cancelled must not flip the run out of "cancelled". The late
// events themselves stay pollable on the event log.
func TestOrchestratorCancelRun_DurableAgainstLateAgentEvents(t *testing.T) {
	h := newOrchestratorHarness(t)
	runID := h.newAgentRun(t, "wf-cancel-late")

	if _, err := h.cancelRun(t, h.orchestratorToken, runID, "criteriarun deleted"); err != nil {
		t.Fatalf("CancelRun: %v", err)
	}

	h.submitEvents(t, []*pb.Envelope{
		criteria.NewEnvelope(runID, &pb.RunCompleted{Success: true}),
		criteria.NewEnvelope(runID, &pb.RunFailed{Reason: "agent gave up"}),
	})

	got, err := h.ts.store.GetRun(context.Background(), runID)
	if err != nil {
		t.Fatalf("store get: %v", err)
	}
	if got.Status != "cancelled" || got.FailureReason != "criteriarun deleted" {
		t.Fatalf("cancel not durable: status=%q reason=%q", got.Status, got.FailureReason)
	}

	events := pollRunEvents(t, h, runID, 0)
	var sawCompleted, sawFailed bool
	for _, env := range events {
		switch env.Payload.(type) {
		case *pb.Envelope_RunCompleted:
			sawCompleted = true
		case *pb.Envelope_RunFailed:
			sawFailed = true
		}
	}
	if !sawCompleted || !sawFailed {
		t.Fatalf("late agent events must remain pollable: completed=%v failed=%v (%d events)", sawCompleted, sawFailed, len(events))
	}
}

// TestOrchestratorCancelRunAnonDenied pins the security boundary: CancelRun
// is a write and is never covered by dev-mode anonymous reads — only the
// read-only orchestrator procedures are.
func TestOrchestratorCancelRunAnonDenied(t *testing.T) {
	ts := newTestStackWithLog(t, slog.New(slog.NewTextHandler(io.Discard, nil)))
	tsrv, _, _ := ts.startServer(t, connect.WithInterceptors(
		auth.NewInterceptor(ts.store, true, auth.WithAnonRegister()),
	))
	orch := orchestratorClient(tsrv)

	req := connect.NewRequest(&pb.CancelRunRequest{RunId: "any", Reason: ""})
	if _, err := orch.CancelRun(context.Background(), req); err == nil {
		t.Fatal("anonymous CancelRun must be denied even in dev mode")
	} else if connect.CodeOf(err) != connect.CodeUnauthenticated {
		t.Fatalf("want %v, got %v", connect.CodeUnauthenticated, connect.CodeOf(err))
	}
}
