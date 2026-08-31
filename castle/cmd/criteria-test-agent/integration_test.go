package main

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"connectrpc.com/connect"
	"golang.org/x/net/http2"
	"golang.org/x/net/http2/h2c"

	"github.com/brokenbots/castle/castle/internal/auth"
	"github.com/brokenbots/castle/castle/internal/hub"
	"github.com/brokenbots/castle/castle/internal/rpc"
	"github.com/brokenbots/castle/castle/internal/store/sqlite"
	criteria "github.com/brokenbots/criteria/sdk"
	pb "github.com/brokenbots/criteria/sdk/pb/criteria/v1"
	"github.com/brokenbots/criteria/sdk/pb/criteria/v1/criteriav1connect"
)

// startTestCastle starts an in-process Castle server with anonymous registration
// enabled so the test agent can register without a bootstrap token. It returns
// the server URL and a Criteria token that can be used for ServerService calls
// that require an authenticated caller.
func startTestCastle(t *testing.T) (string, string) {
	t.Helper()

	st, err := sqlite.Open(filepath.Join(t.TempDir(), "castle.db"))
	if err != nil {
		t.Fatalf("open sqlite store: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })

	// Serialize DB access for this single-process test to avoid SQLITE_BUSY
	// races under the race detector.
	st.SetMaxOpenConns(1)

	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	h := hub.New()
	controls := rpc.NewControlRegistry()
	criteriaSrv := rpc.NewCriteriaServer(st, h, log, controls)
	serverSrv := rpc.NewServerServer(st, h, log, controls)

	authInterceptor := auth.NewInterceptor(st, true, auth.WithAnonRegister())
	opts := []connect.HandlerOption{connect.WithInterceptors(authInterceptor)}

	mux := http.NewServeMux()
	criPath, criHandler := criteriav1connect.NewCriteriaServiceHandler(criteriaSrv, opts...)
	srvPath, srvHandler := criteriav1connect.NewServerServiceHandler(serverSrv, opts...)
	mux.Handle(criPath, criHandler)
	mux.Handle(srvPath, srvHandler)

	ts := httptest.NewUnstartedServer(h2c.NewHandler(mux, &http2.Server{}))
	ts.Start()
	t.Cleanup(ts.Close)

	client := h2cClient()
	criClient := criteriav1connect.NewCriteriaServiceClient(client, ts.URL)
	regReq := connect.NewRequest(&pb.RegisterRequest{Name: "test-owner"})
	regResp, err := criClient.Register(context.Background(), regReq)
	if err != nil {
		t.Fatalf("register owner: %v", err)
	}

	return ts.URL, regResp.Msg.Token
}

func TestAgentEndToEndAssignment(t *testing.T) {
	baseURL, ownerToken := startTestCastle(t)

	dir := t.TempDir()
	a := &agent{
		log:       slog.New(slog.NewTextHandler(io.Discard, nil)),
		state:     agentState{Runs: map[string]*runState{}},
		statePath: filepath.Join(dir, stateFileName),
		resumeCh:  map[string]chan string{},
		runCtx:    map[string]context.CancelFunc{},
		stopCh:    make(chan struct{}),
		cfg: config{
			castleAddr: baseURL,
			name:       "test-agent",
			labels:     map[string]string{"pool": "test"},
			homeDir:    dir,
		},
		client:     criteriav1connect.NewCriteriaServiceClient(h2cClient(), baseURL),
		srvClient:  criteriav1connect.NewServerServiceClient(h2cClient(), baseURL),
		httpClient: h2cClient(),
	}

	if err := a.loadState(); err != nil {
		t.Fatalf("load state: %v", err)
	}
	if err := a.ensureRegistered(context.Background()); err != nil {
		t.Fatalf("register agent: %v", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	go a.heartbeatLoop(ctx)
	go a.controlLoop(ctx)

	srvClient := criteriav1connect.NewServerServiceClient(h2cClient(), baseURL)

	// Wait for the agent to come online.
	waitCtx, waitCancel := context.WithTimeout(ctx, 10*time.Second)
	defer waitCancel()
	if err := waitForOnline(waitCtx, srvClient, ownerToken, 1); err != nil {
		t.Fatalf("agent not online: %v", err)
	}

	// Submit a workflow assignment targeted at the agent's label.
	submitReq := connect.NewRequest(&pb.SubmitWorkflowAssignmentRequest{
		WorkflowName:   "hello",
		WorkflowSource: "hello workflow",
		Labels:         map[string]string{"pool": "test"},
		IdempotencyKey: "hello-key",
	})
	submitReq.Header().Set("Authorization", "Bearer "+ownerToken)
	submitResp, err := srvClient.SubmitWorkflowAssignment(ctx, submitReq)
	if err != nil {
		t.Fatalf("submit assignment: %v", err)
	}
	runID := submitResp.Msg.RunId

	// Watch the run to successful completion.
	watchReq := connect.NewRequest(&pb.WatchRunRequest{RunId: runID, SubscriberId: "test"})
	stream, err := srvClient.WatchRun(ctx, watchReq)
	if err != nil {
		t.Fatalf("watch run: %v", err)
	}
	defer stream.Close()

	terminal := make(chan string, 1)
	go func() {
		for stream.Receive() {
			if criteria.IsTerminal(stream.Msg()) {
				switch stream.Msg().Payload.(type) {
				case *pb.Envelope_RunCompleted:
					terminal <- "succeeded"
				case *pb.Envelope_RunFailed:
					terminal <- "failed"
				}
				return
			}
		}
		if err := stream.Err(); err != nil {
			terminal <- err.Error()
		}
	}()

	select {
	case got := <-terminal:
		if got != "succeeded" {
			t.Fatalf("run finished with %q, want succeeded", got)
		}
	case <-time.After(15 * time.Second):
		t.Fatal("timeout waiting for run to finish")
	}

	// Verify the run is owned by our test agent.
	getReq := connect.NewRequest(&pb.GetRunRequest{RunId: runID})
	getResp, err := srvClient.GetRun(ctx, getReq)
	if err != nil {
		t.Fatalf("get run: %v", err)
	}
	if getResp.Msg.CriteriaId != a.criteriaID() {
		t.Fatalf("run owned by %q, want %q", getResp.Msg.CriteriaId, a.criteriaID())
	}

	// Exactly-once: no duplicate correlation IDs in the event history.
	listReq := connect.NewRequest(&pb.ListRunEventsRequest{RunId: runID, Limit: 500})
	listResp, err := srvClient.ListRunEvents(ctx, listReq)
	if err != nil {
		t.Fatalf("list run events: %v", err)
	}
	seen := map[string]bool{}
	for _, ev := range listResp.Msg.Events {
		if seen[ev.CorrelationId] {
			t.Fatalf("duplicate correlation id %q", ev.CorrelationId)
		}
		seen[ev.CorrelationId] = true
	}

	cancel()
}

func waitForOnline(ctx context.Context, client criteriav1connect.ServerServiceClient, token string, want int) error {
	for {
		req := connect.NewRequest(&pb.ListAgentsRequest{Limit: 10})
		req.Header().Set("Authorization", "Bearer "+token)
		resp, err := client.ListAgents(ctx, req)
		if err != nil {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(200 * time.Millisecond):
				continue
			}
		}
		online := 0
		for _, a := range resp.Msg.Agents {
			if a.Status == "online" {
				online++
			}
		}
		if online >= want {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(200 * time.Millisecond):
		}
	}
}
