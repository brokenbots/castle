package rpc

import (
	"context"
	"io"
	"log/slog"
	"net/http/httptest"
	"sort"
	"sync"
	"testing"
	"time"

	"connectrpc.com/connect"

	"github.com/brokenbots/castle/castle/internal/auth"
	"github.com/brokenbots/castle/castle/internal/store"
	criteria "github.com/brokenbots/criteria/sdk"
	pb "github.com/brokenbots/criteria/sdk/pb/criteria/v1"                // import-lint:allow castle service bindings (W08: move to castle-proto)
	"github.com/brokenbots/criteria/sdk/pb/criteria/v1/criteriav1connect" // import-lint:allow castle service bindings (W08: move to castle-proto)
)

// orchestratorHarness wires a full castle stack (all three services) behind
// an auth interceptor and provisions both an agent and an orchestrator
// identity. It is the base for the orchestrator read-path tests (CRI-133).
type orchestratorHarness struct {
	ts                *testStack
	tsrv              *httptest.Server
	oClient           criteriav1connect.CriteriaServiceClient
	cClient           criteriav1connect.ServerServiceClient
	orchClient        criteriav1connect.OrchestratorServiceClient
	agentID           string
	agentToken        string
	orchestratorToken string
}

func newOrchestratorHarness(t *testing.T) *orchestratorHarness {
	t.Helper()
	ts := newTestStack(t)
	// allowAnonReads=false: every RPC must present a token, so the boundary
	// tests below exercise the real production posture.
	tsrv, oClient, cClient := ts.startServer(t, connect.WithInterceptors(
		auth.NewInterceptor(ts.store, false, auth.WithAnonRegister()),
	))
	orchClient := orchestratorClient(tsrv)

	const orchToken = "orchestrator-token-orch"
	reg, err := oClient.Register(context.Background(), connect.NewRequest(&pb.RegisterRequest{Name: "agent-orch"}))
	if err != nil {
		t.Fatalf("register agent: %v", err)
	}
	provisionOrchestratorIdentity(t, ts.store, "orchestrator-operator", orchToken)

	return &orchestratorHarness{
		ts:                ts,
		tsrv:              tsrv,
		oClient:           oClient,
		cClient:           cClient,
		orchClient:        orchClient,
		agentID:           reg.Msg.CriteriaId,
		agentToken:        reg.Msg.Token,
		orchestratorToken: orchToken,
	}
}

// newAgentRun creates a run owned by the harness agent and returns its ID.
func (h *orchestratorHarness) newAgentRun(t *testing.T, workflow string) string {
	t.Helper()
	req := connect.NewRequest(&pb.CreateRunRequest{CriteriaId: h.agentID, WorkflowName: workflow})
	req.Header().Set("Authorization", "Bearer "+h.agentToken)
	resp, err := h.oClient.CreateRun(context.Background(), req)
	if err != nil {
		t.Fatalf("CreateRun: %v", err)
	}
	return resp.Msg.RunId
}

// submitEvents opens an authenticated SubmitEvents stream for the harness
// agent and sends each envelope, collecting acks.
func (h *orchestratorHarness) submitEvents(t *testing.T, envs []*pb.Envelope) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	stream := h.oClient.SubmitEvents(ctx)
	stream.RequestHeader().Set("Authorization", "Bearer "+h.agentToken)
	for _, env := range envs {
		if err := stream.Send(env); err != nil {
			t.Fatalf("SubmitEvents send: %v", err)
		}
		if _, err := stream.Receive(); err != nil {
			t.Fatalf("SubmitEvents ack: %v", err)
		}
	}
	if err := stream.CloseRequest(); err != nil {
		t.Fatalf("SubmitEvents close: %v", err)
	}
	for {
		if _, err := stream.Receive(); err != nil {
			break
		}
	}
}

// pollAll drains the run's events from the orchestrator read path using the
// operator reconcile contract: repeated SubscribeRunEvents calls anchored at
// a persisted cursor, following next_since_seq on full pages.
func pollRunEvents(t *testing.T, h *orchestratorHarness, runID string, sinceSeq uint64) []*pb.Envelope {
	t.Helper()
	var out []*pb.Envelope
	for {
		req := connect.NewRequest(&pb.SubscribeRunEventsRequest{RunId: runID, SinceSeq: sinceSeq})
		req.Header().Set("Authorization", "Bearer "+h.orchestratorToken)
		resp, err := h.orchClient.SubscribeRunEvents(context.Background(), req)
		if err != nil {
			t.Fatalf("SubscribeRunEvents: %v", err)
		}
		for _, env := range resp.Msg.Events {
			if len(out) > 0 && env.Seq <= out[len(out)-1].Seq {
				t.Fatalf("non-ascending seq across pages: %d after %d", env.Seq, out[len(out)-1].Seq)
			}
			out = append(out, env)
		}
		if resp.Msg.NextSinceSeq == 0 || len(resp.Msg.Events) == 0 {
			return out
		}
		sinceSeq = resp.Msg.NextSinceSeq
	}
}

// --- SubscribeRunEvents: replay + cursor semantics ---

func TestOrchestratorSubscribeReplayAndCursor(t *testing.T) {
	h := newOrchestratorHarness(t)
	runID := h.newAgentRun(t, "wf-replay")

	started := criteria.NewEnvelope(runID, &pb.RunStarted{WorkflowName: "wf-replay", InitialStep: "s1"})
	started.CorrelationId = "replay-started"
	log1 := criteria.NewEnvelope(runID, &pb.StepLog{Step: "s1", Chunk: "hello"})
	log1.CorrelationId = "replay-log-1"
	h.submitEvents(t, []*pb.Envelope{started, log1})

	// Full replay from since_seq=0.
	events := pollRunEvents(t, h, runID, 0)
	if len(events) != 2 {
		t.Fatalf("want 2 replayed events, got %d", len(events))
	}
	if got := events[0].Payload.(*pb.Envelope_RunStarted).RunStarted.WorkflowName; got != "wf-replay" {
		t.Errorf("replayed payload mismatch: %q", got)
	}

	// Cursor at last_seq yields an empty page.
	since := events[len(events)-1].Seq
	req := connect.NewRequest(&pb.SubscribeRunEventsRequest{RunId: runID, SinceSeq: since})
	req.Header().Set("Authorization", "Bearer "+h.orchestratorToken)
	resp, err := h.orchClient.SubscribeRunEvents(context.Background(), req)
	if err != nil {
		t.Fatalf("SubscribeRunEvents(cursor): %v", err)
	}
	if len(resp.Msg.Events) != 0 || resp.Msg.LastSeq != 0 || resp.Msg.NextSinceSeq != 0 {
		t.Fatalf("expected empty page at cursor, got %d events last_seq=%d next=%d",
			len(resp.Msg.Events), resp.Msg.LastSeq, resp.Msg.NextSinceSeq)
	}

	// New events arrive after the cursor; only the delta is returned.
	log2 := criteria.NewEnvelope(runID, &pb.StepLog{Step: "s1", Chunk: "again"})
	log2.CorrelationId = "replay-log-2"
	completed := criteria.NewEnvelope(runID, &pb.RunCompleted{})
	completed.CorrelationId = "replay-completed"
	h.submitEvents(t, []*pb.Envelope{log2, completed})

	events = pollRunEvents(t, h, runID, since)
	if len(events) != 2 {
		t.Fatalf("want 2 delta events after cursor, got %d", len(events))
	}
	if _, ok := events[1].Payload.(*pb.Envelope_RunCompleted); !ok {
		t.Fatalf("expected second event to be run.completed, got %T", events[1].Payload)
	}
}

func TestOrchestratorSubscribePagingAndLimits(t *testing.T) {
	h := newOrchestratorHarness(t)
	runID := h.newAgentRun(t, "wf-paging")

	var envs []*pb.Envelope
	for i := 0; i < 5; i++ {
		env := criteria.NewEnvelope(runID, &pb.StepLog{Step: "s1", Chunk: "line"})
		env.CorrelationId = "paging-" + itoa(i)
		envs = append(envs, env)
	}
	h.submitEvents(t, envs)

	// Page of 2: first page sets next_since_seq; subsequent pages drain the rest.
	req := connect.NewRequest(&pb.SubscribeRunEventsRequest{RunId: runID, SinceSeq: 0, Limit: 2})
	req.Header().Set("Authorization", "Bearer "+h.orchestratorToken)
	resp, err := h.orchClient.SubscribeRunEvents(context.Background(), req)
	if err != nil {
		t.Fatalf("page 1: %v", err)
	}
	if len(resp.Msg.Events) != 2 {
		t.Fatalf("want 2 events on page 1, got %d", len(resp.Msg.Events))
	}
	if resp.Msg.NextSinceSeq != resp.Msg.LastSeq || resp.Msg.LastSeq == 0 {
		t.Fatalf("expected next_since_seq=last_seq=%d on full page, got next=%d last=%d",
			resp.Msg.LastSeq, resp.Msg.NextSinceSeq, resp.Msg.LastSeq)
	}

	rest := pollRunEvents(t, h, runID, resp.Msg.NextSinceSeq)
	if len(rest) != 3 {
		t.Fatalf("want 3 remaining events, got %d", len(rest))
	}

	// limit above the server maximum is rejected.
	big := connect.NewRequest(&pb.SubscribeRunEventsRequest{RunId: runID, SinceSeq: 0, Limit: 100000})
	big.Header().Set("Authorization", "Bearer "+h.orchestratorToken)
	if _, err := h.orchClient.SubscribeRunEvents(context.Background(), big); connect.CodeOf(err) != connect.CodeInvalidArgument {
		t.Fatalf("expected invalid_argument for oversized limit, got %v", err)
	}

	// missing run_id is rejected.
	empty := connect.NewRequest(&pb.SubscribeRunEventsRequest{})
	empty.Header().Set("Authorization", "Bearer "+h.orchestratorToken)
	if _, err := h.orchClient.SubscribeRunEvents(context.Background(), empty); connect.CodeOf(err) != connect.CodeInvalidArgument {
		t.Fatalf("expected invalid_argument for empty run_id, got %v", err)
	}
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var buf [20]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	return string(buf[i:])
}

// --- ListActiveRuns: reconcile-loop discovery ---

func TestOrchestratorListActiveRuns(t *testing.T) {
	h := newOrchestratorHarness(t)
	ctx := context.Background()

	// Seed runs across the full status range directly in the store so status
	// coverage is deterministic.
	statuses := []string{"pending", "running", "paused", "succeeded", "failed", "cancelled"}
	ids := map[string]string{}
	for i, status := range statuses {
		id := "run-active-" + status
		ids[status] = id
		if err := h.ts.store.CreateRun(ctx, &store.Run{
			ID: id, OverseerID: h.agentID, WorkflowName: "wf", WorkflowHCL: "hcl", Status: status,
			CurrentStep: "s1", CreatedAt: time.Now().UTC().Add(time.Duration(i) * time.Second),
		}); err != nil {
			t.Fatalf("seed run %s: %v", status, err)
		}
	}

	req := connect.NewRequest(&pb.ListActiveRunsRequest{})
	req.Header().Set("Authorization", "Bearer "+h.orchestratorToken)
	resp, err := h.orchClient.ListActiveRuns(context.Background(), req)
	if err != nil {
		t.Fatalf("ListActiveRuns: %v", err)
	}
	got := map[string]bool{}
	for _, r := range resp.Msg.Runs {
		got[r.RunId] = true
	}
	for _, status := range []string{"pending", "running", "paused"} {
		if !got[ids[status]] {
			t.Errorf("expected active run %s (%s) to be listed", ids[status], status)
		}
	}
	for _, status := range []string{"succeeded", "failed", "cancelled"} {
		if got[ids[status]] {
			t.Errorf("terminal run %s (%s) must not be listed as active", ids[status], status)
		}
	}
	if len(resp.Msg.Runs) != 3 {
		t.Errorf("expected exactly 3 active runs, got %d", len(resp.Msg.Runs))
	}
}

// --- Run + adapter lifecycle observation (CRI-115 / CRI-133) ---

func TestOrchestratorObserveRunAndAdapterLifecycle(t *testing.T) {
	h := newOrchestratorHarness(t)
	runID := h.newAgentRun(t, "wf-lifecycle")

	const scopeInstanceID = "step-1/scope-0"
	const shimAddr = "127.0.0.1:52001"
	const tokenRef = "criteria/adapter/step-1"

	wanted := criteria.NewEnvelope(runID, &pb.AdapterLifecycleProvisionWanted{
		ScopeInstanceId:   scopeInstanceID,
		ShimListenAddress: shimAddr,
		TokenRef:          tokenRef,
	})
	wanted.CorrelationId = "lc-provision-wanted"
	started := criteria.NewEnvelope(runID, &pb.RunStarted{WorkflowName: "wf-lifecycle", InitialStep: "step-1"})
	started.CorrelationId = "lc-started"
	released := criteria.NewEnvelope(runID, &pb.AdapterLifecycleReleased{
		ScopeInstanceId:   scopeInstanceID,
		ShimListenAddress: shimAddr,
		TokenRef:          tokenRef,
	})
	released.CorrelationId = "lc-released"
	completed := criteria.NewEnvelope(runID, &pb.RunCompleted{Success: true})
	completed.CorrelationId = "lc-completed"
	h.submitEvents(t, []*pb.Envelope{wanted, started, released, completed})

	// The operator reconcile loop: ListActiveRuns then poll each run. The run
	// went terminal via run.completed, so it is excluded from discovery —
	// but its full event history remains pollable (needed to stamp status
	// and reconcile adapter pods when catching up after a restart).
	activeReq := connect.NewRequest(&pb.ListActiveRunsRequest{})
	activeReq.Header().Set("Authorization", "Bearer "+h.orchestratorToken)
	activeResp, err := h.orchClient.ListActiveRuns(context.Background(), activeReq)
	if err != nil {
		t.Fatalf("ListActiveRuns: %v", err)
	}
	if len(activeResp.Msg.Runs) != 0 {
		t.Fatalf("terminal run must not appear in ListActiveRuns, got %d", len(activeResp.Msg.Runs))
	}

	events := pollRunEvents(t, h, runID, 0)
	if len(events) != 4 {
		t.Fatalf("want 4 lifecycle events, got %d", len(events))
	}

	// Adapter lifecycle payloads must survive the round trip with all three
	// CRI-115 fields intact.
	provisionWanted, gotWanted := events[0].Payload.(*pb.Envelope_AdapterLifecycleProvisionWanted)
	if !gotWanted {
		t.Fatalf("expected adapter.lifecycle.provision_wanted first, got %T", events[0].Payload)
	}
	if provisionWanted.AdapterLifecycleProvisionWanted.ScopeInstanceId != scopeInstanceID ||
		provisionWanted.AdapterLifecycleProvisionWanted.ShimListenAddress != shimAddr ||
		provisionWanted.AdapterLifecycleProvisionWanted.TokenRef != tokenRef {
		t.Errorf("provision_wanted fields not preserved: %+v", provisionWanted.AdapterLifecycleProvisionWanted)
	}
	if _, ok := events[1].Payload.(*pb.Envelope_RunStarted); !ok {
		t.Errorf("expected run.started second, got %T", events[1].Payload)
	}
	releasedGot, gotReleased := events[2].Payload.(*pb.Envelope_AdapterLifecycleReleased)
	if !gotReleased {
		t.Fatalf("expected adapter.lifecycle.released third, got %T", events[2].Payload)
	}
	if releasedGot.AdapterLifecycleReleased.ScopeInstanceId != scopeInstanceID ||
		releasedGot.AdapterLifecycleReleased.ShimListenAddress != shimAddr ||
		releasedGot.AdapterLifecycleReleased.TokenRef != tokenRef {
		t.Errorf("released fields not preserved: %+v", releasedGot.AdapterLifecycleReleased)
	}
	if _, ok := events[3].Payload.(*pb.Envelope_RunCompleted); !ok {
		t.Errorf("expected run.completed fourth, got %T", events[3].Payload)
	}

	// The run record itself must carry the terminal status so ListActiveRuns
	// discovery and GetRun agree (operator can stamp CriteriaRun status from
	// castle alone).
	runReq := connect.NewRequest(&pb.GetRunRequest{RunId: runID})
	runReq.Header().Set("Authorization", "Bearer "+h.orchestratorToken)
	runResp, err := h.cClient.GetRun(context.Background(), runReq)
	if err != nil {
		t.Fatalf("GetRun: %v", err)
	}
	if runResp.Msg.Status != "succeeded" {
		t.Errorf("run status after run.completed = %q, want succeeded", runResp.Msg.Status)
	}
}

// --- Replay correctness under interleaved writers ---

// TestOrchestratorReplayUnderInterleavedWriters is the CRI-133 replay
// correctness check: several agents submit concurrently while the orchestrator
// polls. The replayed stream must contain every submitted event exactly once
// with gapless per-run sequence numbers 1..N, in ascending order per page.
func TestOrchestratorReplayUnderInterleavedWriters(t *testing.T) {
	h := newOrchestratorHarness(t)
	runID := h.newAgentRun(t, "wf-interleaved")

	const (
		writers             = 6
		eventsEach          = 10
		total               = writers * eventsEach
		reconcileIntervalMS = 5
	)

	var wg sync.WaitGroup
	errCh := make(chan error, writers)
	for g := 0; g < writers; g++ {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			stream := h.oClient.SubmitEvents(ctx)
			stream.RequestHeader().Set("Authorization", "Bearer "+h.agentToken)
			for i := 0; i < eventsEach; i++ {
				env := criteria.NewEnvelope(runID, &pb.StepLog{Step: "s1", Chunk: "w"})
				env.CorrelationId = "iw-" + itoa(g) + "-" + itoa(i)
				if err := stream.Send(env); err != nil {
					errCh <- err
					return
				}
				if _, err := stream.Receive(); err != nil {
					errCh <- err
					return
				}
			}
			_ = stream.CloseRequest()
			for {
				if _, err := stream.Receive(); err != nil {
					break
				}
			}
		}(g)
	}
	wg.Wait()
	select {
	case err := <-errCh:
		t.Fatalf("interleaved writer failed: %v", err)
	default:
	}

	// Operator reconcile: poll from a persisted cursor every reconcile
	// interval until every event has been consumed.
	seen := map[uint64]string{}
	correlations := map[string]bool{}
	var sinceSeq uint64
	for len(seen) < total {
		req := connect.NewRequest(&pb.SubscribeRunEventsRequest{RunId: runID, SinceSeq: sinceSeq, Limit: 4})
		req.Header().Set("Authorization", "Bearer "+h.orchestratorToken)
		resp, err := h.orchClient.SubscribeRunEvents(context.Background(), req)
		if err != nil {
			t.Fatalf("SubscribeRunEvents: %v", err)
		}
		if len(resp.Msg.Events) == 0 {
			time.Sleep(time.Duration(reconcileIntervalMS) * time.Millisecond)
			continue
		}
		for _, env := range resp.Msg.Events {
			if env.Seq <= sinceSeq {
				t.Fatalf("event seq %d not above cursor %d", env.Seq, sinceSeq)
			}
			if prev, dup := seen[env.Seq]; dup {
				t.Fatalf("duplicate seq %d (corr %q and %q)", env.Seq, prev, env.CorrelationId)
			}
			if _, dup := correlations[env.CorrelationId]; dup {
				t.Fatalf("duplicate correlation_id %q at seq %d", env.CorrelationId, env.Seq)
			}
			seen[env.Seq] = env.CorrelationId
			correlations[env.CorrelationId] = true
		}
		sinceSeq = resp.Msg.Events[len(resp.Msg.Events)-1].Seq
	}

	if len(correlations) != total {
		t.Fatalf("want %d distinct events, saw %d", total, len(correlations))
	}
	// Gapless replay: seqs must be exactly 1..total.
	seqs := make([]int, 0, total)
	for seq := range seen {
		seqs = append(seqs, int(seq))
	}
	sort.Ints(seqs)
	for i, seq := range seqs {
		if seq != i+1 {
			t.Fatalf("gapless replay violated: seqs[%d]=%d, want %d", i, seq, i+1)
		}
	}
}

// --- Auth boundary (CRI-133 exit criterion) ---

// TestOrchestratorAuthBoundary verifies over the wire that the orchestrator
// identity can observe but never write agent-owned data.
func TestOrchestratorAuthBoundary(t *testing.T) {
	h := newOrchestratorHarness(t)
	runID := h.newAgentRun(t, "wf-boundary")
	ctx := context.Background()

	// Reads are allowed: discovery, replay, run listing, per-run watch.
	activeReq := connect.NewRequest(&pb.ListActiveRunsRequest{})
	activeReq.Header().Set("Authorization", "Bearer "+h.orchestratorToken)
	if _, err := h.orchClient.ListActiveRuns(ctx, activeReq); err != nil {
		t.Errorf("ListActiveRuns denied for orchestrator: %v", err)
	}
	subReq := connect.NewRequest(&pb.SubscribeRunEventsRequest{RunId: runID, SinceSeq: 0})
	subReq.Header().Set("Authorization", "Bearer "+h.orchestratorToken)
	if _, err := h.orchClient.SubscribeRunEvents(ctx, subReq); err != nil {
		t.Errorf("SubscribeRunEvents denied for orchestrator: %v", err)
	}
	lreReq := connect.NewRequest(&pb.ListRunEventsRequest{RunId: runID, SinceSeq: 0})
	lreReq.Header().Set("Authorization", "Bearer "+h.orchestratorToken)
	if _, err := h.cClient.ListRunEvents(ctx, lreReq); err != nil {
		t.Errorf("ListRunEvents denied for orchestrator: %v", err)
	}
	watchReq := connect.NewRequest(&pb.WatchRunRequest{RunId: runID})
	watchReq.Header().Set("Authorization", "Bearer "+h.orchestratorToken)
	watch, err := h.cClient.WatchRun(ctx, watchReq)
	if err != nil {
		t.Errorf("WatchRun denied for orchestrator: %v", err)
	} else {
		_ = watch.Close()
	}

	// Agent-owned writes are denied. CreateRun is addressed to the legitimate
	// agent identity, so the rejection is the boundary, not ownership.
	createReq := connect.NewRequest(&pb.CreateRunRequest{CriteriaId: h.agentID, WorkflowName: "wf"})
	createReq.Header().Set("Authorization", "Bearer "+h.orchestratorToken)
	if _, err := h.oClient.CreateRun(ctx, createReq); connect.CodeOf(err) != connect.CodePermissionDenied {
		t.Errorf("orchestrator CreateRun: want permission_denied, got %v", err)
	}

	// SubmitEvents is a streaming agent-owned write: the boundary applies to
	// the streaming handler too.
	stream := h.oClient.SubmitEvents(ctx)
	stream.RequestHeader().Set("Authorization", "Bearer "+h.orchestratorToken)
	env := criteria.NewEnvelope(runID, &pb.StepLog{Step: "s1", Chunk: "should be rejected"})
	env.CorrelationId = "boundary-reject"
	if err := stream.Send(env); err == nil {
		if _, err := stream.Receive(); connect.CodeOf(err) != connect.CodePermissionDenied {
			t.Errorf("orchestrator SubmitEvents: want permission_denied, got %v", err)
		}
	} else if connect.CodeOf(err) != connect.CodePermissionDenied {
		t.Errorf("orchestrator SubmitEvents send: want permission_denied, got %v", err)
	}
	_ = stream.CloseRequest()

	stopReq := connect.NewRequest(&pb.StopRunRequest{RunId: runID})
	stopReq.Header().Set("Authorization", "Bearer "+h.orchestratorToken)
	if _, err := h.cClient.StopRun(ctx, stopReq); connect.CodeOf(err) != connect.CodePermissionDenied {
		t.Errorf("orchestrator StopRun: want permission_denied, got %v", err)
	}

	pauseReq := connect.NewRequest(&pb.PauseRunRequest{RunId: runID})
	pauseReq.Header().Set("Authorization", "Bearer "+h.orchestratorToken)
	if _, err := h.cClient.PauseRun(ctx, pauseReq); connect.CodeOf(err) != connect.CodePermissionDenied {
		t.Errorf("orchestrator PauseRun: want permission_denied, got %v", err)
	}

	resumeRunReq := connect.NewRequest(&pb.ResumeRunRequest{RunId: runID})
	resumeRunReq.Header().Set("Authorization", "Bearer "+h.orchestratorToken)
	if _, err := h.cClient.ResumeRun(ctx, resumeRunReq); connect.CodeOf(err) != connect.CodePermissionDenied {
		t.Errorf("orchestrator ResumeRun: want permission_denied, got %v", err)
	}

	promptReq := connect.NewRequest(&pb.SendPromptRequest{RunId: runID, Step: "s1", Prompt: "nope"})
	promptReq.Header().Set("Authorization", "Bearer "+h.orchestratorToken)
	if _, err := h.cClient.SendPrompt(ctx, promptReq); connect.CodeOf(err) != connect.CodePermissionDenied {
		t.Errorf("orchestrator SendPrompt: want permission_denied, got %v", err)
	}

	dispReq := connect.NewRequest(&pb.GetAssignmentDispositionRequest{RunId: runID})
	dispReq.Header().Set("Authorization", "Bearer "+h.orchestratorToken)
	if _, err := h.cClient.GetAssignmentDisposition(ctx, dispReq); connect.CodeOf(err) != connect.CodePermissionDenied {
		t.Errorf("orchestrator GetAssignmentDisposition: want permission_denied, got %v", err)
	}

	hbReq := connect.NewRequest(&pb.HeartbeatRequest{CriteriaId: h.agentID})
	hbReq.Header().Set("Authorization", "Bearer "+h.orchestratorToken)
	if _, err := h.oClient.Heartbeat(ctx, hbReq); connect.CodeOf(err) != connect.CodePermissionDenied {
		t.Errorf("orchestrator Heartbeat: want permission_denied, got %v", err)
	}

	// Nothing the orchestrator attempted may have mutated agent state: the
	// run still belongs to the agent, no boundary-reject event exists, and
	// the agent can still write normally.
	events := pollRunEvents(t, h, runID, 0)
	for _, ev := range events {
		if ev.CorrelationId == "boundary-reject" {
			t.Fatal("orchestrator-submitted event was persisted; boundary violated")
		}
	}
	// The agent can still write normally: the boundary only restricts
	// orchestrator identities.
	agentHb := connect.NewRequest(&pb.HeartbeatRequest{CriteriaId: h.agentID})
	agentHb.Header().Set("Authorization", "Bearer "+h.agentToken)
	if _, err := h.oClient.Heartbeat(ctx, agentHb); err != nil {
		t.Errorf("agent Heartbeat after boundary checks failed: %v", err)
	}
}

// TestAgentMayReadOrchestratorService documents the complementary boundary:
// OrchestratorService is a read-only surface, so agent identities may call it.
func TestAgentMayReadOrchestratorService(t *testing.T) {
	h := newOrchestratorHarness(t)
	runID := h.newAgentRun(t, "wf-agent-read")

	req := connect.NewRequest(&pb.SubscribeRunEventsRequest{RunId: runID, SinceSeq: 0})
	req.Header().Set("Authorization", "Bearer "+h.agentToken)
	if _, err := h.orchClient.SubscribeRunEvents(context.Background(), req); err != nil {
		t.Errorf("agent SubscribeRunEvents must be allowed (read-only): %v", err)
	}
	activeReq := connect.NewRequest(&pb.ListActiveRunsRequest{})
	activeReq.Header().Set("Authorization", "Bearer "+h.agentToken)
	if _, err := h.orchClient.ListActiveRuns(context.Background(), activeReq); err != nil {
		t.Errorf("agent ListActiveRuns must be allowed (read-only): %v", err)
	}
}

// --- Dev-mode anon reads ---

func TestOrchestratorAnonReadsInDevMode(t *testing.T) {
	ts := newTestStackWithLog(t, slog.New(slog.NewTextHandler(io.Discard, nil)))
	tsrv, _, _ := ts.startServer(t, connect.WithInterceptors(
		auth.NewInterceptor(ts.store, true, auth.WithAnonRegister()),
	))
	orch := orchestratorClient(tsrv)

	// With --allow-anon-reads (dev default), the orchestrator read path is
	// anonymously reachable for local tooling.
	req := connect.NewRequest(&pb.ListActiveRunsRequest{})
	if _, err := orch.ListActiveRuns(context.Background(), req); err != nil {
		t.Errorf("anon ListActiveRuns must be allowed in dev mode: %v", err)
	}
	subReq := connect.NewRequest(&pb.SubscribeRunEventsRequest{RunId: "any", SinceSeq: 0})
	if _, err := orch.SubscribeRunEvents(context.Background(), subReq); err != nil {
		t.Errorf("anon SubscribeRunEvents must be allowed in dev mode: %v", err)
	}
}
