package main

import (
	"context"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"connectrpc.com/connect"
	"connectrpc.com/grpchealth"
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
	healthPath, healthHandler := grpchealth.NewHandler(grpchealth.NewStaticChecker(
		criteriav1connect.CriteriaServiceName,
		criteriav1connect.ServerServiceName,
	))
	mux.Handle(criPath, criHandler)
	mux.Handle(srvPath, srvHandler)
	mux.Handle(healthPath, healthHandler)

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

type testLogWriter struct{ t *testing.T }

func (w *testLogWriter) Write(p []byte) (n int, err error) {
	w.t.Log(strings.TrimSpace(string(p)))
	return len(p), nil
}

func newIntegrationAgent(t *testing.T, baseURL, dir, name string, labels map[string]string) *agent {
	t.Helper()
	a := &agent{
		log:       slog.New(slog.NewTextHandler(&testLogWriter{t: t}, &slog.HandlerOptions{Level: slog.LevelDebug})),
		state:     agentState{Runs: map[string]*runState{}},
		statePath: filepath.Join(dir, stateFileName),
		resumeCh:  map[string]chan string{},
		runCtx:    map[string]*runHandle{},
		cfg: config{
			castleAddr:  baseURL,
			name:        name,
			labels:      labels,
			homeDir:     dir,
			longStepDur: 100 * time.Millisecond,
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
	return a
}

func waitForRunStatus(ctx context.Context, t *testing.T, srvClient criteriav1connect.ServerServiceClient, token, runID, want string) {
	t.Helper()
	for {
		req := connect.NewRequest(&pb.GetRunRequest{RunId: runID})
		req.Header().Set("Authorization", "Bearer "+token)
		resp, err := srvClient.GetRun(ctx, req)
		if err != nil {
			select {
			case <-ctx.Done():
				t.Fatalf("timeout waiting for run %s status %q: %v", runID, want, ctx.Err())
			case <-time.After(200 * time.Millisecond):
				continue
			}
		}
		if resp.Msg.Status == want {
			return
		}
		select {
		case <-ctx.Done():
			t.Fatalf("timeout waiting for run %s status %q (last %q): %v", runID, want, resp.Msg.Status, ctx.Err())
		case <-time.After(200 * time.Millisecond):
		}
	}
}

func watchRunTerminal(t *testing.T, srvClient criteriav1connect.ServerServiceClient, token, runID, want string, timeout time.Duration) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	watchReq := connect.NewRequest(&pb.WatchRunRequest{RunId: runID, SubscriberId: "test-" + runID})
	watchReq.Header().Set("Authorization", "Bearer "+token)
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
		if got != want {
			t.Fatalf("run %s finished with %q, want %q", runID, got, want)
		}
	case <-ctx.Done():
		t.Fatalf("timeout waiting for run %s to finish", runID)
	}
}

func assertNoDuplicateEvents(t *testing.T, srvClient criteriav1connect.ServerServiceClient, token, runID string) {
	t.Helper()
	listReq := connect.NewRequest(&pb.ListRunEventsRequest{RunId: runID, Limit: 500})
	listReq.Header().Set("Authorization", "Bearer "+token)
	listResp, err := srvClient.ListRunEvents(context.Background(), listReq)
	if err != nil {
		t.Fatalf("list run events: %v", err)
	}
	seen := map[string]bool{}
	for _, ev := range listResp.Msg.Events {
		if seen[ev.CorrelationId] {
			t.Fatalf("run %s duplicate correlation id %q", runID, ev.CorrelationId)
		}
		seen[ev.CorrelationId] = true
	}
}

func TestAgentEndToEndAssignment(t *testing.T) {
	baseURL, ownerToken := startTestCastle(t)
	srvClient := criteriav1connect.NewServerServiceClient(h2cClient(), baseURL)

	dir := t.TempDir()
	a := newIntegrationAgent(t, baseURL, dir, "test-agent", map[string]string{"pool": "test"})

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go a.heartbeatLoop(ctx)
	go a.controlLoop(ctx)

	waitCtx, waitCancel := context.WithTimeout(ctx, 10*time.Second)
	defer waitCancel()
	if err := waitForOnline(waitCtx, srvClient, ownerToken, 1); err != nil {
		t.Fatalf("agent not online: %v", err)
	}

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

	watchRunTerminal(t, srvClient, ownerToken, runID, "succeeded", 15*time.Second)

	getReq := connect.NewRequest(&pb.GetRunRequest{RunId: runID})
	getReq.Header().Set("Authorization", "Bearer "+ownerToken)
	getResp, err := srvClient.GetRun(ctx, getReq)
	if err != nil {
		t.Fatalf("get run: %v", err)
	}
	if getResp.Msg.CriteriaId != a.criteriaID() {
		t.Fatalf("run owned by %q, want %q", getResp.Msg.CriteriaId, a.criteriaID())
	}

	assertNoDuplicateEvents(t, srvClient, ownerToken, runID)
}

func TestAgentTokenCanControlOwnedRun(t *testing.T) {
	baseURL, ownerToken := startTestCastle(t)
	srvClient := criteriav1connect.NewServerServiceClient(h2cClient(), baseURL)

	dir := t.TempDir()
	a := newIntegrationAgent(t, baseURL, dir, "test-agent", map[string]string{"pool": "test"})
	a.cfg.longStepDur = 3 * time.Second // give the stop phase time to observe "running"

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go a.heartbeatLoop(ctx)
	go a.controlLoop(ctx)

	waitCtx, waitCancel := context.WithTimeout(ctx, 10*time.Second)
	defer waitCancel()
	if err := waitForOnline(waitCtx, srvClient, ownerToken, 1); err != nil {
		t.Fatalf("agent not online: %v", err)
	}

	// Pause/resume: the agent emits WaitEntered and waits for a resume signal.
	pauseReq := connect.NewRequest(&pb.SubmitWorkflowAssignmentRequest{
		WorkflowName:   "pause-test",
		WorkflowSource: "# pause fixture\nvalid\npause",
		Labels:         map[string]string{"pool": "test"},
		IdempotencyKey: "pause-key",
	})
	pauseReq.Header().Set("Authorization", "Bearer "+ownerToken)
	pauseResp, err := srvClient.SubmitWorkflowAssignment(ctx, pauseReq)
	if err != nil {
		t.Fatalf("submit pause assignment: %v", err)
	}
	pauseRunID := pauseResp.Msg.RunId

	waitCtx2, waitCancel2 := context.WithTimeout(ctx, 15*time.Second)
	defer waitCancel2()
	waitForRunStatus(waitCtx2, t, srvClient, ownerToken, pauseRunID, "paused")

	// Resume using the owning agent's token. This is how the separate
	// control-client container authenticates in the Compose smoke test.
	resumeReq := connect.NewRequest(&pb.ResumeRunRequest{RunId: pauseRunID})
	resumeReq.Header().Set("Authorization", "Bearer "+a.token())
	if _, err := srvClient.ResumeRun(context.Background(), resumeReq); err != nil {
		t.Fatalf("resume run as agent: %v", err)
	}
	watchRunTerminal(t, srvClient, ownerToken, pauseRunID, "succeeded", 15*time.Second)

	// Stop: submit a long-running run and cancel it using the agent's token.
	stopReq := connect.NewRequest(&pb.SubmitWorkflowAssignmentRequest{
		WorkflowName:   "stop-test",
		WorkflowSource: "# stop fixture\nvalid\nlong",
		Labels:         map[string]string{"pool": "test"},
		IdempotencyKey: "stop-key",
	})
	stopReq.Header().Set("Authorization", "Bearer "+ownerToken)
	stopResp, err := srvClient.SubmitWorkflowAssignment(ctx, stopReq)
	if err != nil {
		t.Fatalf("submit stop assignment: %v", err)
	}
	stopRunID := stopResp.Msg.RunId

	waitCtx3, waitCancel3 := context.WithTimeout(ctx, 15*time.Second)
	defer waitCancel3()
	waitForRunStatus(waitCtx3, t, srvClient, ownerToken, stopRunID, "running")

	cancelReq := connect.NewRequest(&pb.StopRunRequest{RunId: stopRunID, Reason: "test control client"})
	cancelReq.Header().Set("Authorization", "Bearer "+a.token())
	if _, err := srvClient.StopRun(context.Background(), cancelReq); err != nil {
		t.Fatalf("stop run as agent: %v", err)
	}
	watchRunTerminal(t, srvClient, ownerToken, stopRunID, "failed", 15*time.Second)
}

func TestAgentReattachAfterRestart(t *testing.T) {
	baseURL, ownerToken := startTestCastle(t)
	srvClient := criteriav1connect.NewServerServiceClient(h2cClient(), baseURL)

	dir := t.TempDir()
	a1 := newIntegrationAgent(t, baseURL, dir, "test-agent", map[string]string{"pool": "test"})
	a1.cfg.longStepDur = 3 * time.Second // long enough to restart mid-execution

	ctx1, cancel1 := context.WithCancel(context.Background())
	go a1.heartbeatLoop(ctx1)
	go a1.controlLoop(ctx1)

	waitCtx, waitCancel := context.WithTimeout(ctx1, 10*time.Second)
	defer waitCancel()
	if err := waitForOnline(waitCtx, srvClient, ownerToken, 1); err != nil {
		t.Fatalf("agent not online: %v", err)
	}

	// Start a long-running run so we can restart the agent mid-execution.
	submitReq := connect.NewRequest(&pb.SubmitWorkflowAssignmentRequest{
		WorkflowName:   "restart-test",
		WorkflowSource: "# restart fixture\nvalid\nlong",
		Labels:         map[string]string{"pool": "test"},
		IdempotencyKey: "restart-key",
	})
	submitReq.Header().Set("Authorization", "Bearer "+ownerToken)
	submitResp, err := srvClient.SubmitWorkflowAssignment(ctx1, submitReq)
	if err != nil {
		t.Fatalf("submit assignment: %v", err)
	}
	runID := submitResp.Msg.RunId

	waitCtx2, waitCancel2 := context.WithTimeout(ctx1, 15*time.Second)
	defer waitCancel2()
	waitForRunStatus(waitCtx2, t, srvClient, ownerToken, runID, "running")

	// Simulate agent restart: stop the first agent process and start a new one
	// that loads the same persistent state.
	cancel1()
	waitForNoActiveRuns(t, a1, 5*time.Second)

	a2 := newIntegrationAgent(t, baseURL, dir, "test-agent", map[string]string{"pool": "test"})
	a2.cfg.longStepDur = 3 * time.Second
	ctx2, cancel2 := context.WithCancel(context.Background())
	defer cancel2()

	// The new agent should reattach in-flight runs (matching main.go behavior).
	a2.reattachRuns(ctx2)
	go a2.heartbeatLoop(ctx2)
	go a2.controlLoop(ctx2)

	// The run should resume and complete; replay must be exactly-once.
	watchRunTerminal(t, srvClient, ownerToken, runID, "succeeded", 15*time.Second)
	assertNoDuplicateEvents(t, srvClient, ownerToken, runID)
}

func waitForNoActiveRuns(t *testing.T, a *agent, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		a.runMu.Lock()
		count := len(a.runCtx)
		a.runMu.Unlock()
		if count == 0 {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	a.runMu.Lock()
	count := len(a.runCtx)
	a.runMu.Unlock()
	t.Fatalf("agent still has %d active runs after %v", count, timeout)
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

// trackedListener wraps a net.Listener so we can forcibly close all accepted
// connections. httptest.Server.Close does not close hijacked HTTP/2 (h2c)
// connections, so we must track and close them ourselves to simulate a
// transient Castle outage that breaks existing agent streams.
type trackedListener struct {
	net.Listener
	mu    sync.Mutex
	conns []net.Conn
}

func (l *trackedListener) Accept() (net.Conn, error) {
	c, err := l.Listener.Accept()
	if err != nil {
		return nil, err
	}
	tc := &trackedConn{Conn: c, l: l}
	l.mu.Lock()
	l.conns = append(l.conns, tc)
	l.mu.Unlock()
	return tc, nil
}

func (l *trackedListener) activeConns() int {
	l.mu.Lock()
	defer l.mu.Unlock()
	return len(l.conns)
}

func (l *trackedListener) closeConns() {
	l.mu.Lock()
	for _, c := range l.conns {
		_ = c.(*trackedConn).Conn.Close()
	}
	l.conns = nil
	l.mu.Unlock()
}

func (l *trackedListener) remove(c net.Conn) {
	l.mu.Lock()
	for i, cc := range l.conns {
		if cc == c {
			l.conns = append(l.conns[:i], l.conns[i+1:]...)
			break
		}
	}
	l.mu.Unlock()
}

type trackedConn struct {
	net.Conn
	l *trackedListener
}

func (c *trackedConn) Close() error {
	c.l.remove(c)
	return c.Conn.Close()
}

// restartableCastle runs an in-process Castle server on a listener that can be
// closed and rebound, simulating a transient Castle restart while preserving
// the same SQLite store.
type restartableCastle struct {
	t        *testing.T
	server   *httptest.Server
	listener net.Listener
	store    *sqlite.Store
	log      *slog.Logger
	token    string
}

func newRestartableCastle(t *testing.T) *restartableCastle {
	t.Helper()

	st, err := sqlite.Open(filepath.Join(t.TempDir(), "castle.db"))
	if err != nil {
		t.Fatalf("open sqlite store: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	st.SetMaxOpenConns(1)

	log := slog.New(slog.NewTextHandler(&testLogWriter{t: t}, &slog.HandlerOptions{Level: slog.LevelDebug}))
	cf := &restartableCastle{t: t, store: st, log: log}
	cf.startServer()

	client := h2cClient()
	criClient := criteriav1connect.NewCriteriaServiceClient(client, cf.server.URL)
	regReq := connect.NewRequest(&pb.RegisterRequest{Name: "test-owner"})
	regResp, err := criClient.Register(context.Background(), regReq)
	if err != nil {
		t.Fatalf("register owner: %v", err)
	}
	cf.token = regResp.Msg.Token

	return cf
}

func (cf *restartableCastle) startServer() {
	cf.t.Helper()

	if cf.server != nil {
		cf.server.Close()
	}
	if cf.listener != nil {
		_ = cf.listener.Close()
	}

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		cf.t.Fatalf("listen: %v", err)
	}
	cf.listener = &trackedListener{Listener: ln}

	h := hub.New()
	controls := rpc.NewControlRegistry()
	criteriaSrv := rpc.NewCriteriaServer(cf.store, h, cf.log, controls)
	serverSrv := rpc.NewServerServer(cf.store, h, cf.log, controls)

	authInterceptor := auth.NewInterceptor(cf.store, true, auth.WithAnonRegister())
	opts := []connect.HandlerOption{connect.WithInterceptors(authInterceptor)}

	mux := http.NewServeMux()
	criPath, criHandler := criteriav1connect.NewCriteriaServiceHandler(criteriaSrv, opts...)
	srvPath, srvHandler := criteriav1connect.NewServerServiceHandler(serverSrv, opts...)
	healthPath, healthHandler := grpchealth.NewHandler(grpchealth.NewStaticChecker(
		criteriav1connect.CriteriaServiceName,
		criteriav1connect.ServerServiceName,
	))
	mux.Handle(criPath, criHandler)
	mux.Handle(srvPath, srvHandler)
	mux.Handle(healthPath, healthHandler)

	cf.server = httptest.NewUnstartedServer(h2c.NewHandler(mux, &http2.Server{}))
	cf.server.Listener = cf.listener
	cf.server.Start()
}

// Restart closes the current HTTP server, rebinds the same listener address,
// and starts a fresh Castle server backed by the same store.
func (cf *restartableCastle) Restart() {
	cf.t.Helper()

	addr := cf.listener.Addr().String()
	cf.server.Close()
	cf.listener.(*trackedListener).closeConns()

	ln, err := net.Listen("tcp", addr)
	if err != nil {
		cf.t.Fatalf("rebind listener on %s: %v", addr, err)
	}
	cf.listener = &trackedListener{Listener: ln}

	h := hub.New()
	controls := rpc.NewControlRegistry()
	criteriaSrv := rpc.NewCriteriaServer(cf.store, h, cf.log, controls)
	serverSrv := rpc.NewServerServer(cf.store, h, cf.log, controls)

	authInterceptor := auth.NewInterceptor(cf.store, true, auth.WithAnonRegister())
	opts := []connect.HandlerOption{connect.WithInterceptors(authInterceptor)}

	mux := http.NewServeMux()
	criPath, criHandler := criteriav1connect.NewCriteriaServiceHandler(criteriaSrv, opts...)
	srvPath, srvHandler := criteriav1connect.NewServerServiceHandler(serverSrv, opts...)
	healthPath, healthHandler := grpchealth.NewHandler(grpchealth.NewStaticChecker(
		criteriav1connect.CriteriaServiceName,
		criteriav1connect.ServerServiceName,
	))
	mux.Handle(criPath, criHandler)
	mux.Handle(srvPath, srvHandler)
	mux.Handle(healthPath, healthHandler)

	cf.server = httptest.NewUnstartedServer(h2c.NewHandler(mux, &http2.Server{}))
	cf.server.Listener = cf.listener
	cf.server.Start()
}

func TestAgentReattachAfterCastleRestart(t *testing.T) {
	cf := newRestartableCastle(t)
	srvClient := criteriav1connect.NewServerServiceClient(h2cClient(), cf.server.URL)

	dir := t.TempDir()
	a := newIntegrationAgent(t, cf.server.URL, dir, "test-agent", map[string]string{"pool": "test"})
	a.cfg.longStepDur = 10 * time.Second // long enough to restart Castle mid-execution

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go a.heartbeatLoop(ctx)
	go a.controlLoop(ctx)

	waitCtx, waitCancel := context.WithTimeout(ctx, 10*time.Second)
	defer waitCancel()
	if err := waitForOnline(waitCtx, srvClient, cf.token, 1); err != nil {
		t.Fatalf("agent not online: %v", err)
	}

	// Start a long-running run so we can restart Castle mid-execution.
	submitReq := connect.NewRequest(&pb.SubmitWorkflowAssignmentRequest{
		WorkflowName:   "castle-restart-test",
		WorkflowSource: "# restart fixture\nvalid\nlong",
		Labels:         map[string]string{"pool": "test"},
		IdempotencyKey: "castle-restart-key",
	})
	submitReq.Header().Set("Authorization", "Bearer "+cf.token)
	submitResp, err := srvClient.SubmitWorkflowAssignment(ctx, submitReq)
	if err != nil {
		t.Fatalf("submit assignment: %v", err)
	}
	runID := submitResp.Msg.RunId

	waitCtx2, waitCancel2 := context.WithTimeout(ctx, 15*time.Second)
	defer waitCancel2()
	waitForRunStatus(waitCtx2, t, srvClient, cf.token, runID, "running")

	// Simulate transient Castle restart: close the listener and rebind the
	// same port after a short delay.
	t.Logf("before restart; active conns=%d", cf.listener.(*trackedListener).activeConns())
	cf.Restart()
	t.Logf("after restart; active conns=%d new addr=%s", cf.listener.(*trackedListener).activeConns(), cf.server.URL)
	// Use a fresh test-side client for post-restart queries; the agent's
	// existing client will redial the same address once the old connections
	// are closed.
	srvClient = criteriav1connect.NewServerServiceClient(h2cClient(), cf.server.URL)

	// The agent should reconnect Control, reattach the in-flight run, and
	// resume event submission on a fresh SubmitEvents stream.
	watchRunTerminal(t, srvClient, cf.token, runID, "succeeded", 30*time.Second)

	cancel()
	waitForNoActiveRuns(t, a, 5*time.Second)
	assertNoDuplicateEvents(t, srvClient, cf.token, runID)
}

func TestTerminalRunSurvivesCastleRestart(t *testing.T) {
	cf := newRestartableCastle(t)
	srvClient := criteriav1connect.NewServerServiceClient(h2cClient(), cf.server.URL)

	dir := t.TempDir()
	a := newIntegrationAgent(t, cf.server.URL, dir, "test-agent", map[string]string{"pool": "test"})
	a.cfg.longStepDur = 5 * time.Second // long enough to restart Castle mid-execution

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go a.heartbeatLoop(ctx)
	go a.controlLoop(ctx)

	waitCtx, waitCancel := context.WithTimeout(ctx, 10*time.Second)
	defer waitCancel()
	if err := waitForOnline(waitCtx, srvClient, cf.token, 1); err != nil {
		t.Fatalf("agent not online: %v", err)
	}

	// Start a short run that will complete before Castle restarts.
	submitReq := connect.NewRequest(&pb.SubmitWorkflowAssignmentRequest{
		WorkflowName:   "terminal-survive-test",
		WorkflowSource: "# terminal fixture\nvalid",
		Labels:         map[string]string{"pool": "test"},
		IdempotencyKey: "terminal-survive-key",
	})
	submitReq.Header().Set("Authorization", "Bearer "+cf.token)
	submitResp, err := srvClient.SubmitWorkflowAssignment(ctx, submitReq)
	if err != nil {
		t.Fatalf("submit assignment: %v", err)
	}
	runID := submitResp.Msg.RunId

	watchRunTerminal(t, srvClient, cf.token, runID, "succeeded", 15*time.Second)

	// Restart Castle after the run is terminal. The agent must report the
	// terminal status without reopening a SubmitEvents stream.
	cf.Restart()
	srvClient = criteriav1connect.NewServerServiceClient(h2cClient(), cf.server.URL)

	getReq := connect.NewRequest(&pb.GetRunRequest{RunId: runID})
	getReq.Header().Set("Authorization", "Bearer "+cf.token)
	getResp, err := srvClient.GetRun(context.Background(), getReq)
	if err != nil {
		t.Fatalf("get run: %v", err)
	}
	if getResp.Msg.Status != "succeeded" {
		t.Fatalf("run status %q after restart, want succeeded", getResp.Msg.Status)
	}

	// Give the agent a moment to reconnect Control and ensure it does not emit
	// duplicate terminal events.
	time.Sleep(500 * time.Millisecond)
	cancel()
	waitForNoActiveRuns(t, a, 5*time.Second)
	assertNoDuplicateEvents(t, srvClient, cf.token, runID)
}
