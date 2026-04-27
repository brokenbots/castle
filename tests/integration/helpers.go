//go:build integration

package integration

import (
	"bufio"
	"context"
	"crypto/tls"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"connectrpc.com/connect"
	"golang.org/x/net/http2"

	pb "github.com/brokenbots/overlord/shared/pb/overlord/v1"
	"github.com/brokenbots/overlord/shared/pb/overlord/v1/overlordv1connect"
)

// repoRoot returns the absolute path to the repository root by walking up two
// directories from the location of helpers.go (tests/integration → tests → repo
// root).
func repoRoot() string {
	_, file, _, ok := runtime.Caller(0)
	if !ok {
		panic("runtime.Caller failed")
	}
	// file = .../tests/integration/helpers.go → up 2 dirs
	return filepath.Dir(filepath.Dir(filepath.Dir(file)))
}

// freePort temporarily binds to :0 to obtain an unused port and returns it.
func freePort(t *testing.T) int {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("freePort: listen: %v", err)
	}
	port := ln.Addr().(*net.TCPAddr).Port
	_ = ln.Close()
	return port
}

// portFromAddr parses the port number from a URL of the form "http://host:port".
func portFromAddr(rawURL string) int {
	host := strings.TrimPrefix(rawURL, "http://")
	_, portStr, err := net.SplitHostPort(host)
	if err != nil {
		panic("portFromAddr: " + err.Error())
	}
	port, _ := strconv.Atoi(portStr)
	return port
}

// ─── Castle ──────────────────────────────────────────────────────────────────

// CastleHandle holds a running Castle process and its connection details.
type CastleHandle struct {
	URL     string
	DBPath  string
	LogPath string // path of the on-disk process log written for CI artifact upload

	cmd      *exec.Cmd
	logLines []string
	logMu    sync.Mutex
}

// StartCastle starts a fresh Castle instance in a temp directory.
func StartCastle(t *testing.T) *CastleHandle {
	t.Helper()
	dbPath := filepath.Join(t.TempDir(), "castle.db")
	return startCastleOnDB(t, dbPath, 0)
}

// startCastleOnDB starts Castle against the given DB file. If port == 0 a free
// port is chosen automatically.
func startCastleOnDB(t *testing.T, dbPath string, port int) *CastleHandle {
	t.Helper()
	if port == 0 {
		port = freePort(t)
	}
	addr := fmt.Sprintf("127.0.0.1:%d", port)
	url := "http://" + addr

	logFile, err := os.CreateTemp(os.TempDir(), "castle-*.log")
	if err != nil {
		t.Fatalf("startCastleOnDB: create log file: %v", err)
	}

	bin := filepath.Join(repoRoot(), "bin", "castle")
	cmd := exec.Command(bin,
		"--addr", addr,
		"--db", dbPath,
		"--allow-anon-reads",
	)

	h := &CastleHandle{
		URL:     url,
		DBPath:  dbPath,
		LogPath: logFile.Name(),
		cmd:     cmd,
	}

	// Stream stderr for log capture.
	stderr, err := cmd.StderrPipe()
	if err != nil {
		t.Fatalf("startCastleOnDB: StderrPipe: %v", err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatalf("startCastleOnDB: Start: %v", err)
	}

	done := make(chan struct{})
	go func() {
		defer close(done)
		defer logFile.Close()
		sc := bufio.NewScanner(stderr)
		for sc.Scan() {
			line := sc.Text()
			h.logMu.Lock()
			h.logLines = append(h.logLines, line)
			h.logMu.Unlock()
			fmt.Fprintln(logFile, line)
		}
	}()

	t.Cleanup(func() {
		if cmd.Process != nil {
			_ = cmd.Process.Kill()
			_ = cmd.Wait()
		}
		<-done // ensure goroutine has drained all log lines before reading them
		if t.Failed() {
			t.Logf("=== castle logs (path: %s) ===", h.LogPath)
			h.logMu.Lock()
			for _, l := range h.logLines {
				t.Log(l)
			}
			h.logMu.Unlock()
		}
	})

	waitForCastleReady(t, url)
	return h
}

// RestartCastle kills the current Castle process (SIGKILL) and starts a new
// one on the same port and DB path.
func RestartCastle(t *testing.T, h *CastleHandle) *CastleHandle {
	t.Helper()
	if h.cmd.Process != nil {
		_ = h.cmd.Process.Kill()
		_ = h.cmd.Wait()
	}
	return startCastleOnDB(t, h.DBPath, portFromAddr(h.URL))
}

// waitForCastleReady polls the gRPC health endpoint until Castle responds with
// SERVING_STATUS_SERVING or the 30s timeout elapses.
func waitForCastleReady(t *testing.T, url string) {
	t.Helper()
	client := h2cHTTPClient()
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		req, _ := http.NewRequest(http.MethodPost, url+"/grpc.health.v1.Health/Check", strings.NewReader("{}"))
		req.Header.Set("Content-Type", "application/json")
		resp, err := client.Do(req)
		if err == nil {
			body, _ := io.ReadAll(resp.Body)
			_ = resp.Body.Close()
			if strings.Contains(string(body), "SERVING_STATUS_SERVING") {
				return
			}
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatalf("waitForCastleReady: Castle at %s did not become ready within 30s", url)
}

// ─── Overseer ────────────────────────────────────────────────────────────────

// OverseerHandle holds a running Overseer process.
type OverseerHandle struct {
	StateDir string
	LogPath  string // path of the on-disk process log written for CI artifact upload

	cmd      *exec.Cmd
	logLines []string
	logMu    sync.Mutex
}

// StartOverseer starts the overseer process against the given Castle URL and
// workflow file. It creates a temporary state directory and sets OVERSEER_STATE_DIR.
func StartOverseer(t *testing.T, castleURL, workflowPath string) *OverseerHandle {
	t.Helper()
	stateDir := t.TempDir()

	logFile, err := os.CreateTemp(os.TempDir(), "overseer-*.log")
	if err != nil {
		t.Fatalf("StartOverseer: create log file: %v", err)
	}

	bin := filepath.Join(repoRoot(), "bin", "overseer")
	cmd := exec.Command(bin, "apply", "--castle", castleURL, workflowPath)
	cmd.Env = append(os.Environ(), "OVERSEER_STATE_DIR="+stateDir)

	h := &OverseerHandle{
		StateDir: stateDir,
		LogPath:  logFile.Name(),
		cmd:      cmd,
	}

	stderr, err := cmd.StderrPipe()
	if err != nil {
		t.Fatalf("StartOverseer: StderrPipe: %v", err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatalf("StartOverseer: Start: %v", err)
	}

	done := make(chan struct{})
	go func() {
		defer close(done)
		defer logFile.Close()
		sc := bufio.NewScanner(stderr)
		for sc.Scan() {
			line := sc.Text()
			h.logMu.Lock()
			h.logLines = append(h.logLines, line)
			h.logMu.Unlock()
			fmt.Fprintln(logFile, line)
		}
	}()

	t.Cleanup(func() {
		if cmd.Process != nil {
			_ = cmd.Process.Kill()
			_ = cmd.Wait()
		}
		<-done // ensure goroutine has drained all log lines before reading them
		if t.Failed() {
			t.Logf("=== overseer logs (path: %s) ===", h.LogPath)
			h.logMu.Lock()
			for _, l := range h.logLines {
				t.Log(l)
			}
			h.logMu.Unlock()
		}
	})

	return h
}

// ─── Checkpoint ──────────────────────────────────────────────────────────────

// StepCheckpoint mirrors the JSON file written by the Overseer before each step.
type StepCheckpoint struct {
	RunID        string `json:"run_id"`
	Workflow     string `json:"workflow"`
	WorkflowPath string `json:"workflow_path"`
	CurrentStep  string `json:"current_step"`
	Attempt      int    `json:"attempt"`
	CastleURL    string `json:"castle_url"`
	OverseerID   string `json:"overseer_id"`
	Token        string `json:"token"`
}

// WaitForCheckpoint polls the state directory until the checkpoint file for the
// given run ID exists and is non-empty, or the timeout elapses.
func WaitForCheckpoint(t *testing.T, stateDir, runID string, timeout time.Duration) *StepCheckpoint {
	t.Helper()
	cpPath := filepath.Join(stateDir, "runs", runID+".json")
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		b, err := os.ReadFile(cpPath)
		if err == nil && len(b) > 0 {
			var cp StepCheckpoint
			if jsonErr := json.Unmarshal(b, &cp); jsonErr == nil && cp.Token != "" {
				return &cp
			}
		}
		time.Sleep(200 * time.Millisecond)
	}
	t.Fatalf("WaitForCheckpoint: no checkpoint for run %s within %s", runID, timeout)
	return nil
}

// ─── HTTP client ─────────────────────────────────────────────────────────────

// h2cHTTPClient returns an *http.Client configured for cleartext HTTP/2 (h2c).
func h2cHTTPClient() *http.Client {
	return &http.Client{
		Transport: &http2.Transport{
			AllowHTTP: true,
			DialTLSContext: func(ctx context.Context, network, addr string, _ *tls.Config) (net.Conn, error) {
				return (&net.Dialer{}).DialContext(ctx, network, addr)
			},
		},
	}
}

// ─── Connect clients ─────────────────────────────────────────────────────────

// NewCastleClient builds a CastleServiceClient for the given base URL.
func NewCastleClient(baseURL string) overlordv1connect.CastleServiceClient {
	return overlordv1connect.NewCastleServiceClient(h2cHTTPClient(), baseURL)
}

// NewOverseerClient builds an OverseerServiceClient that injects the bearer
// token on every request.
func NewOverseerClient(baseURL, token string) overlordv1connect.OverseerServiceClient {
	return overlordv1connect.NewOverseerServiceClient(
		h2cHTTPClient(),
		baseURL,
		connect.WithInterceptors(&tokenInterceptor{token: token}),
	)
}

// tokenInterceptor injects a Bearer token into every outgoing unary request.
type tokenInterceptor struct{ token string }

func (ti *tokenInterceptor) WrapUnary(next connect.UnaryFunc) connect.UnaryFunc {
	return func(ctx context.Context, req connect.AnyRequest) (connect.AnyResponse, error) {
		req.Header().Set("Authorization", "Bearer "+ti.token)
		return next(ctx, req)
	}
}

func (ti *tokenInterceptor) WrapStreamingClient(next connect.StreamingClientFunc) connect.StreamingClientFunc {
	return next
}

func (ti *tokenInterceptor) WrapStreamingHandler(next connect.StreamingHandlerFunc) connect.StreamingHandlerFunc {
	return next
}

// ─── Polling helpers ─────────────────────────────────────────────────────────

// WaitForRun polls ListRuns until at least one run appears, then returns its ID.
func WaitForRun(t *testing.T, client overlordv1connect.CastleServiceClient, timeout time.Duration) string {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		resp, err := client.ListRuns(context.Background(), connect.NewRequest(&pb.ListRunsRequest{}))
		if err == nil && len(resp.Msg.Runs) > 0 {
			return resp.Msg.Runs[0].RunId
		}
		time.Sleep(200 * time.Millisecond)
	}
	t.Fatalf("WaitForRun: no run appeared within %s", timeout)
	return ""
}

// WaitForRunStatus polls GetRun until the run reaches wantStatus or timeout.
func WaitForRunStatus(t *testing.T, client overlordv1connect.CastleServiceClient, runID, wantStatus string, timeout time.Duration) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		resp, err := client.GetRun(context.Background(), connect.NewRequest(&pb.GetRunRequest{RunId: runID}))
		if err == nil && resp.Msg.Status == wantStatus {
			return
		}
		time.Sleep(300 * time.Millisecond)
	}
	t.Fatalf("WaitForRunStatus: run %s did not reach status %q within %s", runID, wantStatus, timeout)
}

// ListAllEvents pages through ListRunEvents (page size 20) and returns all
// events for the given run.
func ListAllEvents(t *testing.T, client overlordv1connect.CastleServiceClient, runID string) []*pb.Envelope {
	t.Helper()
	var all []*pb.Envelope
	var sinceSeq uint64
	for {
		resp, err := client.ListRunEvents(context.Background(), connect.NewRequest(&pb.ListRunEventsRequest{
			RunId:    runID,
			SinceSeq: sinceSeq,
			Limit:    20,
		}))
		if err != nil {
			t.Fatalf("ListAllEvents: ListRunEvents: %v", err)
		}
		all = append(all, resp.Msg.Events...)
		if len(resp.Msg.Events) < 20 {
			break
		}
		sinceSeq = resp.Msg.NextSinceSeq
		if sinceSeq == 0 {
			break
		}
	}
	return all
}

// EventTypeString returns a human-readable type label for an Envelope payload.
func EventTypeString(env *pb.Envelope) string {
	switch env.Payload.(type) {
	case *pb.Envelope_RunStarted:
		return "run.started"
	case *pb.Envelope_RunCompleted:
		return "run.completed"
	case *pb.Envelope_RunFailed:
		return "run.failed"
	case *pb.Envelope_StepEntered:
		return "step.entered"
	case *pb.Envelope_StepOutcome:
		return "step.outcome"
	case *pb.Envelope_StepTransition:
		return "step.transition"
	case *pb.Envelope_StepLog:
		return "step.log"
	case *pb.Envelope_AdapterEvent:
		return "adapter.event"
	case *pb.Envelope_OverseerHeartbeat:
		return "overseer.heartbeat"
	case *pb.Envelope_OverseerDisconnected:
		return "overseer.disconnected"
	case *pb.Envelope_StepResumed:
		return "step.resumed"
	case *pb.Envelope_VariableSet:
		return "variable.set"
	case *pb.Envelope_StepOutputCaptured:
		return "step.output_captured"
	case *pb.Envelope_WaitEntered:
		return "wait.entered"
	case *pb.Envelope_WaitResumed:
		return "wait.resumed"
	case *pb.Envelope_ApprovalRequested:
		return "approval.requested"
	case *pb.Envelope_ApprovalDecision:
		return "approval.decision"
	case *pb.Envelope_BranchEvaluated:
		return "branch.evaluated"
	case *pb.Envelope_ForEachEntered:
		return "for_each.entered"
	case *pb.Envelope_ForEachIteration:
		return "for_each.iteration"
	case *pb.Envelope_ForEachOutcome:
		return "for_each.outcome"
	case *pb.Envelope_ScopeIterCursorSet:
		return "scope.iter_cursor_set"
	case *pb.Envelope_WatchReady:
		return "watch.ready"
	default:
		return "<unknown>"
	}
}
