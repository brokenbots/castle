// criteria-test-agent is a minimal long-lived Criteria agent used by the
// Castle system-test Compose profile. It implements enough of the agent
// contract to exercise label routing, restart recovery, durable queueing,
// and failure visibility against a real Castle server.
package main

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"connectrpc.com/connect"
	"golang.org/x/net/http2"
	"google.golang.org/protobuf/types/known/timestamppb"

	criteria "github.com/brokenbots/criteria/sdk"
	pb "github.com/brokenbots/criteria/sdk/pb/criteria/v1"
	"github.com/brokenbots/criteria/sdk/pb/criteria/v1/criteriav1connect"
)

const (
	stateFileName    = "agent-state.json"
	heartbeatInterval = 10 * time.Second
	watchTimeout      = 60 * time.Second
)

type agentState struct {
	CriteriaID string            `json:"criteria_id"`
	Token      string            `json:"token"`
	Runs       map[string]*runState `json:"runs"`
}

type runState struct {
	RunID          string `json:"run_id"`
	WorkflowName   string `json:"workflow_name"`
	WorkflowSource string `json:"workflow_source"`
	CurrentStep    string `json:"current_step"`
	Attempt        int32  `json:"attempt"`
	LastSeq        uint64 `json:"last_seq"`
	Status         string `json:"status"`
	Paused         bool   `json:"paused"`
	ResumeSignal   string `json:"resume_signal,omitempty"`
	FailureReason  string `json:"failure_reason,omitempty"`
}

type agent struct {
	log      *slog.Logger
	cfg      config
	client   criteriav1connect.CriteriaServiceClient
	srvClient criteriav1connect.ServerServiceClient
	httpClient *http.Client

	mu       sync.Mutex
	state    agentState
	statePath string

	resumeCh map[string]chan string // run_id -> resume signal channel
	runCtx   map[string]context.CancelFunc
	runMu    sync.Mutex

	stopCh chan struct{}
}

type config struct {
	castleAddr string
	name       string
	labels     map[string]string
	homeDir    string
}

func envOrDefault(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

func parseLabels(s string) map[string]string {
	out := map[string]string{}
	for _, part := range strings.Split(s, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		kv := strings.SplitN(part, "=", 2)
		if len(kv) == 2 {
			out[strings.TrimSpace(kv[0])] = strings.TrimSpace(kv[1])
		}
	}
	return out
}

func loadConfig() config {
	return config{
		castleAddr: envOrDefault("CASTLE_ADDR", "http://castle:8080"),
		name:       envOrDefault("AGENT_NAME", "criteria-test-agent"),
		labels:     parseLabels(envOrDefault("AGENT_LABELS", "")),
		homeDir:    envOrDefault("AGENT_HOME_DIR", "/var/lib/agent"),
	}
}

func h2cClient() *http.Client {
	return &http.Client{
		Transport: &http2.Transport{
			AllowHTTP: true,
			DialTLSContext: func(ctx context.Context, network, addr string, _ *tls.Config) (net.Conn, error) {
				return (&net.Dialer{}).DialContext(ctx, network, addr)
			},
		},
	}
}

func main() {
	cfg := loadConfig()
	log := slog.New(slog.NewJSONHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo}))

	if err := os.MkdirAll(cfg.homeDir, 0o755); err != nil {
		log.Error("create home dir", "err", err)
		os.Exit(1)
	}

	client := criteriav1connect.NewCriteriaServiceClient(h2cClient(), cfg.castleAddr)
	srvClient := criteriav1connect.NewServerServiceClient(h2cClient(), cfg.castleAddr)

	statePath := filepath.Join(cfg.homeDir, stateFileName)
	a := &agent{
		log:        log,
		cfg:        cfg,
		client:     client,
		srvClient:  srvClient,
		httpClient: h2cClient(),
		state: agentState{
			Runs: map[string]*runState{},
		},
		statePath: statePath,
		resumeCh:  map[string]chan string{},
		runCtx:    map[string]context.CancelFunc{},
		stopCh:    make(chan struct{}),
	}

	if err := a.loadState(); err != nil {
		log.Error("load state", "err", err)
		os.Exit(1)
	}

	if err := a.ensureRegistered(context.Background()); err != nil {
		log.Error("register", "err", err)
		os.Exit(1)
	}

	ctx, cancel := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer cancel()

	// Reattach any in-flight runs from a previous process lifecycle.
	a.reattachRuns(ctx)

	go a.heartbeatLoop(ctx)
	go a.controlLoop(ctx)

	log.Info("agent started", "name", cfg.name, "criteria_id", a.state.CriteriaID, "labels", cfg.labels)

	<-ctx.Done()
	log.Info("shutting down")
	if err := a.saveState(); err != nil {
		log.Error("save state", "err", err)
	}
}

func (a *agent) loadState() error {
	data, err := os.ReadFile(a.statePath)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return err
	}
	return json.Unmarshal(data, &a.state)
}

func (a *agent) saveState() error {
	a.mu.Lock()
	data, err := json.MarshalIndent(a.state, "", "  ")
	a.mu.Unlock()
	if err != nil {
		return err
	}
	return os.WriteFile(a.statePath, data, 0o600)
}

func (a *agent) ensureRegistered(ctx context.Context) error {
	if a.state.CriteriaID != "" && a.state.Token != "" {
		// Verify token is still valid with a heartbeat.
		req := connect.NewRequest(&pb.HeartbeatRequest{})
		req.Header().Set("Authorization", "Bearer "+a.state.Token)
		if _, err := a.client.Heartbeat(ctx, req); err == nil {
			return nil
		}
		a.log.Warn("stored token invalid, re-registering", "criteria_id", a.state.CriteriaID)
	}

	req := connect.NewRequest(&pb.RegisterRequest{
		Name:   a.cfg.name,
		Labels: a.cfg.labels,
	})
	resp, err := a.client.Register(ctx, req)
	if err != nil {
		return fmt.Errorf("register: %w", err)
	}

	a.mu.Lock()
	a.state.CriteriaID = resp.Msg.CriteriaId
	a.state.Token = resp.Msg.Token
	a.mu.Unlock()
	return a.saveState()
}

func (a *agent) heartbeatLoop(ctx context.Context) {
	t := time.NewTicker(heartbeatInterval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			req := connect.NewRequest(&pb.HeartbeatRequest{})
			req.Header().Set("Authorization", "Bearer "+a.token())
			_, err := a.client.Heartbeat(ctx, req)
			if err != nil {
				a.log.Warn("heartbeat failed", "err", err)
			}
		}
	}
}

func (a *agent) token() string {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.state.Token
}

func (a *agent) criteriaID() string {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.state.CriteriaID
}

func (a *agent) setRunState(rs *runState) {
	a.mu.Lock()
	a.state.Runs[rs.RunID] = rs
	a.mu.Unlock()
}

func (a *agent) deleteRunState(runID string) {
	a.mu.Lock()
	delete(a.state.Runs, runID)
	a.mu.Unlock()
}

func (a *agent) controlLoop(ctx context.Context) {
	backoff := 1 * time.Second
	for {
		select {
		case <-ctx.Done():
			return
		default:
		}

		err := a.runControlStream(ctx)
		if err != nil {
			a.log.Warn("control stream error", "err", err)
		}

		// The control stream dropped. Castle may have restarted, so reattach
		// any in-flight runs so they resume execution.
		a.reattachRuns(ctx)

		select {
		case <-ctx.Done():
			return
		case <-time.After(backoff):
			backoff *= 2
			if backoff > 30*time.Second {
				backoff = 30 * time.Second
			}
		}
	}
}

func (a *agent) runControlStream(ctx context.Context) error {
	req := connect.NewRequest(&pb.ControlSubscribeRequest{CriteriaId: a.criteriaID()})
	req.Header().Set("Authorization", "Bearer "+a.token())

	stream, err := a.client.Control(ctx, req)
	if err != nil {
		return err
	}
	defer stream.Close()

	// Consume ControlReady.
	if !stream.Receive() {
		return stream.Err()
	}
	if _, ok := stream.Msg().Command.(*pb.ControlMessage_ControlReady); !ok {
		return fmt.Errorf("expected ControlReady, got %T", stream.Msg().Command)
	}

	a.log.Info("control stream ready")

	for {
		if !stream.Receive() {
			err := stream.Err()
			if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
				return nil
			}
			return err
		}
		msg := stream.Msg()
		switch cmd := msg.Command.(type) {
		case *pb.ControlMessage_WorkflowAssignment:
			a.handleWorkflowAssignment(ctx, cmd.WorkflowAssignment)
		case *pb.ControlMessage_RunCancel:
			a.handleRunCancel(cmd.RunCancel)
		case *pb.ControlMessage_PauseRun:
			a.handlePauseRun(cmd.PauseRun)
		case *pb.ControlMessage_ResumeRun:
			a.handleResumeRun(cmd.ResumeRun)
		default:
			a.log.Debug("ignored control command", "type", fmt.Sprintf("%T", msg.Command))
		}
	}
}

func (a *agent) handleWorkflowAssignment(ctx context.Context, assignment *pb.WorkflowAssignment) {
	a.log.Info("received workflow assignment",
		"run_id", assignment.RunId,
		"workflow_name", assignment.WorkflowName,
		"labels", assignment.Labels)

	// Persist the assignment before accepting it.
	rs := &runState{
		RunID:          assignment.RunId,
		WorkflowName:   assignment.WorkflowName,
		WorkflowSource: assignment.WorkflowSource,
		Status:         "pending",
	}
	a.setRunState(rs)

	if !a.startRunGoroutine(ctx, rs) {
		a.log.Info("run already has an active executor, skipping duplicate start", "run_id", rs.RunID)
	}
}

// startRunGoroutine starts executeRun for rs unless one is already running.
// It returns true when a new goroutine was started.
func (a *agent) startRunGoroutine(ctx context.Context, rs *runState) bool {
	runCtx, cancel := context.WithCancel(ctx)
	a.runMu.Lock()
	defer a.runMu.Unlock()
	if _, ok := a.runCtx[rs.RunID]; ok {
		cancel()
		return false
	}
	a.runCtx[rs.RunID] = cancel
	go a.executeRun(runCtx, rs)
	return true
}

func (a *agent) handleRunCancel(cancel *pb.RunCancel) {
	a.log.Info("received run cancel", "run_id", cancel.RunId, "reason", cancel.Reason)
	a.runMu.Lock()
	cancelFn, ok := a.runCtx[cancel.RunId]
	a.runMu.Unlock()
	if ok {
		cancelFn()
	}
}

func (a *agent) handlePauseRun(pause *pb.PauseRun) {
	a.log.Info("received run pause", "run_id", pause.RunId)
	a.mu.Lock()
	rs, ok := a.state.Runs[pause.RunId]
	a.mu.Unlock()
	if ok {
		rs.Paused = true
		a.setRunState(rs)
	}
}

func (a *agent) handleResumeRun(resume *pb.ResumeRun) {
	a.log.Info("received run resume", "run_id", resume.RunId, "signal", resume.Signal)
	a.mu.Lock()
	ch, ok := a.resumeCh[resume.RunId]
	a.mu.Unlock()
	if ok {
		select {
		case ch <- resume.Signal:
		default:
		}
	}
}

func (a *agent) reattachRuns(ctx context.Context) {
	a.mu.Lock()
	runs := make(map[string]*runState, len(a.state.Runs))
	for id, rs := range a.state.Runs {
		runs[id] = rs
	}
	a.mu.Unlock()

	for _, rs := range runs {
		if isTerminal(rs.Status) {
			continue
		}
		a.log.Info("reattaching run", "run_id", rs.RunID, "status", rs.Status)
		req := connect.NewRequest(&pb.ReattachRunRequest{RunId: rs.RunID})
		req.Header().Set("Authorization", "Bearer "+a.token())
		resp, err := a.client.ReattachRun(ctx, req)
		if err != nil {
			a.log.Warn("reattach failed", "run_id", rs.RunID, "err", err)
			continue
		}
		if !resp.Msg.CanResume {
			a.log.Info("run not resumable", "run_id", rs.RunID, "status", resp.Msg.Status)
			rs.Status = resp.Msg.Status
			a.setRunState(rs)
			continue
		}

		rs.Status = resp.Msg.Status
		rs.CurrentStep = resp.Msg.CurrentStep
		rs.Attempt = resp.Msg.Attempt
		rs.LastSeq = resp.Msg.LastSeq
		a.setRunState(rs)

		if !a.startRunGoroutine(ctx, rs) {
			a.log.Info("run executor already active after reattach", "run_id", rs.RunID)
		}
	}
}

func (a *agent) executeRun(ctx context.Context, rs *runState) {
	a.log.Info("executing run", "run_id", rs.RunID, "workflow", rs.WorkflowName)

	// Open SubmitEvents stream for this run.
	stream := a.client.SubmitEvents(ctx)
	stream.RequestHeader().Set("Authorization", "Bearer "+a.token())

	defer func() {
		_ = stream.CloseRequest()
		_ = stream.CloseResponse()
		a.runMu.Lock()
		delete(a.runCtx, rs.RunID)
		a.runMu.Unlock()
	}()

	// If the run is already terminal, there is nothing to do.
	if isTerminal(rs.Status) {
		return
	}

	// Start the run if it is not already running.
	if rs.Status == "pending" {
		if err := a.sendEvent(stream, rs, criteria.NewEnvelope(rs.RunID, &pb.RunStarted{
			WorkflowName: rs.WorkflowName,
			InitialStep:  "compile",
		})); err != nil {
			a.logErrorAndFail(stream, rs, "failed to start run", err)
			return
		}
		rs.Status = "running"
		rs.CurrentStep = "compile"
		a.setRunState(rs)
	}

	// Simulate compilation.
	if err := a.sendEvent(stream, rs, criteria.NewEnvelope(rs.RunID, &pb.StepEntered{
		Step:    "compile",
		Attempt: 1,
	})); err != nil {
		a.logErrorAndFail(stream, rs, "failed to emit step entered", err)
		return
	}

	if strings.Contains(rs.WorkflowSource, "invalid") {
		a.failRun(stream, rs, "compilation failed: invalid workflow source")
		return
	}

	if err := a.sendEvent(stream, rs, criteria.NewEnvelope(rs.RunID, &pb.StepOutcome{
		Step:    "compile",
		Outcome: "success",
	})); err != nil {
		a.logErrorAndFail(stream, rs, "failed to emit step outcome", err)
		return
	}

	// Execute the main step.
	rs.CurrentStep = "main"
	a.setRunState(rs)
	if err := a.sendEvent(stream, rs, criteria.NewEnvelope(rs.RunID, &pb.StepEntered{
		Step:    "main",
		Attempt: 1,
	})); err != nil {
		a.logErrorAndFail(stream, rs, "failed to emit step entered", err)
		return
	}

	// Pause workflow: emit WaitEntered and block until resumed.
	if strings.Contains(rs.WorkflowSource, "pause") {
		if err := a.sendEvent(stream, rs, criteria.NewEnvelope(rs.RunID, &pb.WaitEntered{
			Node:   "approval",
			Signal: "resume-test",
		})); err != nil {
			a.logErrorAndFail(stream, rs, "failed to emit wait entered", err)
			return
		}

		rs.Paused = true
		rs.ResumeSignal = "resume-test"
		a.setRunState(rs)

		ch := make(chan string, 1)
		a.mu.Lock()
		a.resumeCh[rs.RunID] = ch
		a.mu.Unlock()

		select {
		case <-ctx.Done():
			a.failRun(stream, rs, "cancelled while paused")
			return
		case sig := <-ch:
			a.log.Info("resuming paused run", "run_id", rs.RunID, "signal", sig)
		}

		if err := a.sendEvent(stream, rs, criteria.NewEnvelope(rs.RunID, &pb.WaitResumed{
			Node:   "approval",
			Mode:   "signal",
			Signal: "resume-test",
		})); err != nil {
			a.logErrorAndFail(stream, rs, "failed to emit wait resumed", err)
			return
		}
		rs.Paused = false
		rs.ResumeSignal = ""
		a.setRunState(rs)
	}

	// Long-running workflow: allow time for external cancel.
	if strings.Contains(rs.WorkflowSource, "long") {
		select {
		case <-ctx.Done():
			a.failRun(stream, rs, "cancelled during long step")
			return
		case <-time.After(30 * time.Second):
		}
	}

	if err := a.sendEvent(stream, rs, criteria.NewEnvelope(rs.RunID, &pb.StepOutcome{
		Step:    "main",
		Outcome: "success",
	})); err != nil {
		a.logErrorAndFail(stream, rs, "failed to emit step outcome", err)
		return
	}

	rs.CurrentStep = "finish"
	a.setRunState(rs)
	if err := a.sendEvent(stream, rs, criteria.NewEnvelope(rs.RunID, &pb.StepEntered{
		Step:    "finish",
		Attempt: 1,
	})); err != nil {
		a.logErrorAndFail(stream, rs, "failed to emit step entered", err)
		return
	}
	if err := a.sendEvent(stream, rs, criteria.NewEnvelope(rs.RunID, &pb.StepOutcome{
		Step:    "finish",
		Outcome: "success",
	})); err != nil {
		a.logErrorAndFail(stream, rs, "failed to emit step outcome", err)
		return
	}

	if err := a.sendEvent(stream, rs, criteria.NewEnvelope(rs.RunID, &pb.RunCompleted{Success: true})); err != nil {
		a.logErrorAndFail(stream, rs, "failed to emit run completed", err)
		return
	}

	rs.Status = "succeeded"
	a.setRunState(rs)
	a.deleteRunState(rs.RunID)
	a.log.Info("run completed", "run_id", rs.RunID)
}

func (a *agent) sendEvent(stream *connect.BidiStreamForClient[criteria.Envelope, pb.Ack], rs *runState, env *criteria.Envelope) error {
	if env.CorrelationId == "" {
		env.CorrelationId = deterministicCorrelationID(rs.RunID, env)
	}
	if env.Ts == nil || env.Ts.AsTime().IsZero() {
		env.Ts = timestamppb.New(time.Now().UTC())
	}
	if err := stream.Send(env); err != nil {
		return err
	}
	// Wait for ack to ensure exactly-once persistence before proceeding.
	ack, err := stream.Receive()
	if err != nil {
		return err
	}
	if ack.RunId != env.RunId || ack.Seq == 0 {
		return fmt.Errorf("unexpected ack: run_id=%s seq=%d", ack.RunId, ack.Seq)
	}
	rs.LastSeq = ack.Seq
	a.setRunState(rs)
	return nil
}

func deterministicCorrelationID(runID string, env *criteria.Envelope) string {
	typ := criteria.TypeString(env)
	switch p := env.Payload.(type) {
	case *pb.Envelope_StepEntered:
		return fmt.Sprintf("%s-%s-%s", runID, typ, p.StepEntered.GetStep())
	case *pb.Envelope_StepOutcome:
		return fmt.Sprintf("%s-%s-%s", runID, typ, p.StepOutcome.GetStep())
	case *pb.Envelope_StepOutputCaptured:
		return fmt.Sprintf("%s-%s-%s", runID, typ, p.StepOutputCaptured.GetStep())
	case *pb.Envelope_WaitEntered:
		return fmt.Sprintf("%s-%s-%s", runID, typ, p.WaitEntered.GetSignal())
	case *pb.Envelope_WaitResumed:
		return fmt.Sprintf("%s-%s-%s", runID, typ, p.WaitResumed.GetSignal())
	case *pb.Envelope_ApprovalRequested:
		return fmt.Sprintf("%s-%s-%s", runID, typ, p.ApprovalRequested.GetNode())
	case *pb.Envelope_ApprovalDecision:
		return fmt.Sprintf("%s-%s-%s", runID, typ, p.ApprovalDecision.GetNode())
	default:
		return fmt.Sprintf("%s-%s", runID, typ)
	}
}

func (a *agent) failRun(stream *connect.BidiStreamForClient[criteria.Envelope, pb.Ack], rs *runState, reason string) {
	a.log.Info("failing run", "run_id", rs.RunID, "reason", reason)
	if err := a.sendEvent(stream, rs, criteria.NewEnvelope(rs.RunID, &pb.RunFailed{
		Reason: reason,
	})); err != nil {
		a.log.Error("failed to emit run failed", "run_id", rs.RunID, "err", err)
	}
	rs.Status = "failed"
	rs.FailureReason = reason
	a.setRunState(rs)
	a.deleteRunState(rs.RunID)
}

func (a *agent) logErrorAndFail(stream *connect.BidiStreamForClient[criteria.Envelope, pb.Ack], rs *runState, msg string, err error) {
	a.log.Error(msg, "run_id", rs.RunID, "err", err)
	a.failRun(stream, rs, fmt.Sprintf("%s: %v", msg, err))
}

func isTerminal(status string) bool {
	switch status {
	case "succeeded", "failed", "cancelled":
		return true
	default:
		return false
	}
}
