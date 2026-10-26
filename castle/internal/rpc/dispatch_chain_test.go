package rpc

import (
	"context"
	"flag"
	"io"
	"log/slog"
	"testing"
	"time"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/brokenbots/castle/castle/internal/auth"
	"github.com/brokenbots/castle/castle/internal/store"
	"github.com/brokenbots/castle/castle/internal/store/pg/pgtest"
	pb "github.com/brokenbots/criteria/sdk/pb/criteria/v1" // import-lint:allow castle service bindings (W08: move to castle-proto)
)

// chainSkipPG skips the Postgres engine of the dispatch-chain suite (for
// sandboxed runs without the embedded-binary cache), mirroring
// -suite.skip-pg in internal/store/suite.
var chainSkipPG = flag.Bool("rpc.skip-pg", false, "skip the Postgres engine (for sandboxed runs without the embedded-binary cache)")

// chainEngine is one persistent-store engine the wire stack runs over.
type chainEngine struct {
	name  string
	build func(t *testing.T) (*testStack, store.Store)
}

// chainEngines enumerates both suite engines for the parity test: the
// standard sqlite wire stack, and the pg suite store wrapped in the same
// stack (pgtest owns the database lifecycle).
func chainEngines(t *testing.T) []chainEngine {
	eng := []chainEngine{
		{"sqlite", func(t *testing.T) (*testStack, store.Store) {
			// The standard wire stack: its cleanup owns sidecar sweeping.
			ts := newTestStack(t)
			return ts, ts.store
		}},
	}
	if *chainSkipPG {
		t.Logf("pg engine disabled (-rpc.skip-pg)")
		return eng
	}
	eng = append(eng, chainEngine{"pg", func(t *testing.T) (*testStack, store.Store) {
		st := pgtest.Store(t)
		ts := newTestStackOnStore(t, st, slog.New(slog.NewTextHandler(io.Discard, nil)))
		t.Cleanup(func() {
			// pgtest owns the database directory lifecycle; teardown stops
			// at drain+close (empty dbPath skips the sidecar sweep).
			if err := teardownStack(t, ts, st, ""); err != nil {
				t.Errorf("test stack teardown: %v", err)
			}
		})
		return ts, st
	}})
	return eng
}

// TestDispatchChainEngineParity runs the KB-342 chain as an in-repo
// integration test over BOTH store engines (KB-343 acceptance 4): an owner
// submits an assignment, the agent's Control stream connects, castle leases
// and pushes it, the agent accepts by pushing RunStarted, and the run row
// (the CriteriaRun perspective castle serves) is stamped running with
// StartedAt set. It also exercises the KB-343 lease-disposition surface: a
// granted lease lands on the row with a positive lease attempt and no
// rejection, queryable without reconstructing logs.
//
// The wire is real castle handlers (httptest + the production auth
// interceptor posture with anon register); only the agent's Control stream
// and event submission are driven by the test, the same way the deployed
// criteria agent consumes them.
func TestDispatchChainEngineParity(t *testing.T) {
	for _, engine := range chainEngines(t) {
		engine := engine
		t.Run(engine.name, func(t *testing.T) {
			ts, st := engine.build(t)
			ctx := context.Background()
			_, oClient, cClient := ts.startServer(t,
				connect.WithInterceptors(auth.NewInterceptor(ts.store, false, auth.WithAnonRegister())),
			)

			ownerReg, err := oClient.Register(ctx, connect.NewRequest(&pb.RegisterRequest{Name: "chain-owner"}))
			if err != nil {
				t.Fatalf("register owner: %v", err)
			}
			agentReg, err := oClient.Register(ctx, connect.NewRequest(&pb.RegisterRequest{
				Name:   "chain-agent",
				Labels: map[string]string{"gpu": "true"},
			}))
			if err != nil {
				t.Fatalf("register agent: %v", err)
			}
			agentID := agentReg.Msg.CriteriaId

			submitReq := connect.NewRequest(&pb.SubmitWorkflowAssignmentRequest{
				WorkflowName:   "chain-wf",
				WorkflowSource: "hcl",
				IdempotencyKey: "chain-key-1",
				Labels:         map[string]string{"gpu": "true"},
			})
			submitReq.Header().Set("Authorization", "Bearer "+ownerReg.Msg.Token)
			submitResp, err := cClient.SubmitWorkflowAssignment(ctx, submitReq)
			if err != nil {
				t.Fatalf("submit assignment: %v", err)
			}
			runID := submitResp.Msg.RunId

			// The agent connects its Control stream: ControlReady first, then
			// the leased WorkflowAssignment.
			ctrlReq := connect.NewRequest(&pb.ControlSubscribeRequest{CriteriaId: agentID})
			ctrlReq.Header().Set("Authorization", "Bearer "+agentReg.Msg.Token)
			ctrl, err := oClient.Control(ctx, ctrlReq)
			if err != nil {
				t.Fatalf("control: %v", err)
			}
			t.Cleanup(func() { _ = ctrl.Close() })

			msg := chainReceiveControl(t, ctrl)
			if _, ok := msg.Command.(*pb.ControlMessage_ControlReady); !ok {
				t.Fatalf("expected ControlReady, got %T", msg.Command)
			}
			assigned := chainReceiveControl(t, ctrl).GetWorkflowAssignment()
			if assigned == nil {
				t.Fatalf("expected WorkflowAssignment, got %T", msg.Command)
			}
			if assigned.RunId != runID {
				t.Fatalf("leased run id = %s, want %s", assigned.RunId, runID)
			}

			// The granted lease is visible on the row: leased to this agent,
			// visited by the lease scan, and carrying NO rejection (KB-343).
			a, err := st.GetWorkflowAssignmentByRunID(ctx, runID)
			if err != nil {
				t.Fatalf("get assignment after lease: %v", err)
			}
			if a.State != store.WorkflowAssignmentStateLeased {
				t.Fatalf("assignment state = %s, want leased", a.State)
			}
			if a.LeasedCriteriaID != agentID {
				t.Fatalf("leased_criteria_id = %q, want %q", a.LeasedCriteriaID, agentID)
			}
			if a.LeaseAttempt < 1 {
				t.Fatalf("lease_attempt = %d, want >= 1 (the scan must be visible on the row)", a.LeaseAttempt)
			}
			if a.LastLeaseRejection != "" {
				t.Fatalf("last_lease_rejection = %q, want empty on a granted lease", a.LastLeaseRejection)
			}

			// Accept: the agent pushes RunStarted through the event stream.
			submitEvents := oClient.SubmitEvents(ctx)
			submitEvents.RequestHeader().Set("Authorization", "Bearer "+agentReg.Msg.Token)
			if err := submitEvents.Send(&pb.Envelope{
				SchemaVersion: 1,
				RunId:         runID,
				CorrelationId: "chain-started",
				Ts:            timestamppb.Now(),
				Payload:       &pb.Envelope_RunStarted{RunStarted: &pb.RunStarted{WorkflowName: "chain-wf", InitialStep: "s1"}},
			}); err != nil {
				t.Fatalf("send RunStarted: %v", err)
			}
			ack, err := submitEvents.Receive()
			if err != nil {
				t.Fatalf("receive ack: %v", err)
			}
			if ack == nil || ack.RunId != runID {
				t.Fatalf("ack mismatch: %+v", ack)
			}

			// The CriteriaRun is stamped: the run row entered "running" with a
			// first-start StartedAt (CRI-187), the run belongs to the agent
			// that leased it, and the lease disposition survived the accept —
			// no rejection was ever recorded along the chain.
			deadline := time.Now().Add(5 * time.Second)
			var run *store.Run
			for {
				run, err = st.GetRun(ctx, runID)
				if err != nil {
					t.Fatalf("get run after accept: %v", err)
				}
				if run.Status == "running" && run.StartedAt != nil {
					break
				}
				if time.Now().After(deadline) {
					t.Fatalf("run not stamped: status %s, started_at %v", run.Status, run.StartedAt)
				}
				time.Sleep(20 * time.Millisecond)
			}
			if run.OverseerID != agentID {
				t.Fatalf("run overseer_id = %q, want the leasing agent %q", run.OverseerID, agentID)
			}
			a, err = st.GetWorkflowAssignmentByRunID(ctx, runID)
			if err != nil {
				t.Fatalf("get assignment after accept: %v", err)
			}
			if a.State != store.WorkflowAssignmentStateLeased {
				t.Fatalf("assignment state after accept = %s, want still leased until the run finishes", a.State)
			}
			if a.LastLeaseRejection != "" {
				t.Fatalf("last_lease_rejection after accept = %q, want empty", a.LastLeaseRejection)
			}

			// The pushed event is persisted: the chain is observable, not
			// broadcast-and-lost.
			events, err := st.ListEvents(ctx, runID, 0, 10)
			if err != nil {
				t.Fatalf("list events: %v", err)
			}
			var sawStarted bool
			for _, ev := range events {
				if ev.Type == "run.started" {
					sawStarted = true
				}
			}
			if !sawStarted {
				t.Fatalf("run.started never persisted; events = %v", chainEventNames(events))
			}

			// The same stamp is readable on the wire by the agent (the
			// CriteriaRun read surface the operator's view is built on).
			getReq := connect.NewRequest(&pb.GetRunRequest{RunId: runID})
			getReq.Header().Set("Authorization", "Bearer "+agentReg.Msg.Token)
			getResp, err := cClient.GetRun(ctx, getReq)
			if err != nil {
				t.Fatalf("GetRun: %v", err)
			}
			if got := getResp.Msg.GetStartedAt(); got == nil || got.AsTime().Before(run.StartedAt.Add(-time.Second)) {
				t.Fatalf("wire GetRun started_at = %v, want the stored stamp %v", got, run.StartedAt)
			}
		})
	}
}

// chainReceiveControl receives one Control message off the agent's stream,
// failing with a diagnostic when nothing arrives.
func chainReceiveControl(t *testing.T, ctrl *connect.ServerStreamForClient[pb.ControlMessage]) *pb.ControlMessage {
	t.Helper()
	done := make(chan struct{})
	var received bool
	go func() {
		received = ctrl.Receive()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatalf("timed out waiting for control message")
	}
	if !received {
		t.Fatalf("control receive failed, err=%v", ctrl.Err())
	}
	return ctrl.Msg()
}

// chainEventNames lists a page of events as discriminator strings for
// assertion messages.
func chainEventNames(events []*store.Event) []string {
	names := make([]string, 0, len(events))
	for _, ev := range events {
		names = append(names, ev.Type)
	}
	return names
}
