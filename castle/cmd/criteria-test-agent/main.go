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
	"sync/atomic"
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
	stateFileName      = "agent-state.json"
	heartbeatInterval  = 10 * time.Second
	watchTimeout       = 60 * time.Second
	defaultLongStepDur = 30 * time.Second
)

type agentState struct {
	CriteriaID string               `json:"criteria_id"`
	Token      string               `json:"token"`
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

type runHandle struct {
	cancel context.CancelFunc
	gen    int64
}

type agent struct {
	log        *slog.Logger
	cfg        config
	client     criteriav1connect.CriteriaServiceClient
	srvClient  criteriav1connect.ServerServiceClient
	httpClient *http.Client

	mu        sync.Mutex
	state     agentState
	statePath string

	resumeCh map[string]chan string // run_id -> resume signal channel
	runCtx   map[string]*runHandle
	runMu    sync.Mutex
	runGen   atomic.Int64
}

type config struct {
	castleAddr  string
	name        string
	labels      map[string]string
	homeDir     string
	longStepDur time.Duration
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
	longDur := defaultLongStepDur
	if v := os.Getenv("AGENT_LONG_STEP_DURATION"); v != "" {
		if d, err := time.ParseDuration(v); err == nil {
			longDur = d
		}
	}
	return config{
		castleAddr:  envOrDefault("CASTLE_ADDR", "http://castle:8080"),
		name:        envOrDefault("AGENT_NAME", "criteria-test-agent"),
		labels:      parseLabels(envOrDefault("AGENT_LABELS", "")),
		homeDir:     envOrDefault("AGENT_HOME_DIR", "/var/lib/agent"),
		longStepDur: longDur,
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
		runCtx:    map[string]*runHandle{},
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
	// Store a private copy so the persisted state is never aliased with a
	// running executor goroutine. This lets reattachRuns read the map under
	// lock without racing the executor's unlocked field updates.
	cpy := *rs
	a.state.Runs[rs.RunID] = &cpy
	a.mu.Unlock()
	if err := a.saveState(); err != nil {
		a.log.Error("persist run state", "run_id", rs.RunID, "err", err)
	}
}

func (a *agent) deleteRunState(runID string) {
	a.mu.Lock()
	delete(a.state.Runs, runID)
	a.mu.Unlock()
	if err := a.saveState(); err != nil {
		a.log.Error("persist run state", "run_id", runID, "err", err)
	}
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

	// The control stream is live. Reattach any in-flight runs so a Castle
	// restart or transient outage does not leave them stranded on a broken
	// SubmitEvents stream.
	a.reattachRuns(ctx)

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
	gen := a.runGen.Add(1)
	a.runCtx[rs.RunID] = &runHandle{cancel: cancel, gen: gen}
	go a.executeRun(runCtx, rs, gen)
	return true
}

// restartRunGoroutine cancels any active executor for rs and starts a fresh
// one. It is used during reattachment so the recovered run gets a working
// SubmitEvents stream instead of continuing on a stale one.
func (a *agent) restartRunGoroutine(ctx context.Context, rs *runState) {
	runCtx, cancel := context.WithCancel(ctx)
	a.runMu.Lock()
	if h, ok := a.runCtx[rs.RunID]; ok {
		h.cancel()
	}
	gen := a.runGen.Add(1)
	a.runCtx[rs.RunID] = &runHandle{cancel: cancel, gen: gen}
	a.runMu.Unlock()
	go a.executeRun(runCtx, rs, gen)
}

// isCurrentExecutor reports whether the executor identified by gen is still the
// active executor for the run.
func (a *agent) isCurrentExecutor(runID string, gen int64) bool {
	a.runMu.Lock()
	defer a.runMu.Unlock()
	h, ok := a.runCtx[runID]
	return ok && h.gen == gen
}

func (a *agent) handleRunCancel(cancel *pb.RunCancel) {
	a.log.Info("received run cancel", "run_id", cancel.RunId, "reason", cancel.Reason)

	// Mark the run as failed immediately using a background stream; do not
	// rely on the run goroutine noticing the cancellation, because the
	// goroutine's event stream may already be closed.
	a.mu.Lock()
	rs, ok := a.state.Runs[cancel.RunId]
	a.mu.Unlock()
	if ok && !isTerminal(rs.Status) {
		a.failRunWithBackgroundStream(rs, "cancelled: "+cancel.Reason)
	}

	a.runMu.Lock()
	h, ok := a.runCtx[cancel.RunId]
	a.runMu.Unlock()
	if ok {
		h.cancel()
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
		// Work on a private copy; a previous executor may still reference the
		// persisted pointer and must not observe mutations intended for the
		// fresh executor.
		cpy := *rs
		runs[id] = &cpy
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

		// Replace any active executor with a fresh one so event submission uses
		// a new SubmitEvents stream after a Castle outage.
		a.restartRunGoroutine(ctx, rs)
	}
}

func (a *agent) executeRun(ctx context.Context, rs *runState, gen int64) {
	a.log.Info("executing run", "run_id", rs.RunID, "workflow", rs.WorkflowName, "gen", gen)

	// Open SubmitEvents stream for this run.
	stream := a.client.SubmitEvents(ctx)
	stream.RequestHeader().Set("Authorization", "Bearer "+a.token())

	defer func() {
		_ = stream.CloseRequest()
		_ = stream.CloseResponse()
		a.runMu.Lock()
		if h, ok := a.runCtx[rs.RunID]; ok && h.gen == gen {
			delete(a.runCtx, rs.RunID)
		}
		a.runMu.Unlock()
	}()

	// If the run is already terminal, there is nothing to do.
	if isTerminal(rs.Status) {
		return
	}

	// abort is a helper that returns true when this executor has been
	// superseded or its context is done and should not mutate run state.
	abort := func() bool {
		return ctx.Err() != nil || !a.isCurrentExecutor(rs.RunID, gen)
	}

	// send is a wrapper around sendEvent that treats stream errors as fatal
	// only while this executor remains current. A superseded executor (e.g.
	// one whose stream broke during a Castle restart) must not fail the run.
	send := func(env *criteria.Envelope) bool {
		if abort() {
			return false
		}
		if err := a.sendEvent(stream, rs, env); err != nil {
			if abort() {
				return false
			}
			a.logErrorAndFail(ctx, stream, rs, gen, "send event", err)
			return false
		}
		return true
	}

	// Start the run if it is not already running.
	if rs.Status == "pending" {
		if !send(criteria.NewEnvelope(rs.RunID, &pb.RunStarted{
			WorkflowName: rs.WorkflowName,
			InitialStep:  "compile",
		})) {
			return
		}
		rs.Status = "running"
		rs.CurrentStep = "compile"
		a.setRunState(rs)
	}

	// Simulate compilation.
	if !send(criteria.NewEnvelope(rs.RunID, &pb.StepEntered{
		Step:    "compile",
		Attempt: 1,
	})) {
		return
	}

	if strings.Contains(rs.WorkflowSource, "invalid") {
		if abort() {
			return
		}
		a.failRun(ctx, stream, rs, gen, "compilation failed: invalid workflow source")
		return
	}

	if !send(criteria.NewEnvelope(rs.RunID, &pb.StepOutcome{
		Step:    "compile",
		Outcome: "success",
	})) {
		return
	}

	// Execute the main step.
	rs.CurrentStep = "main"
	a.setRunState(rs)
	if !send(criteria.NewEnvelope(rs.RunID, &pb.StepEntered{
		Step:    "main",
		Attempt: 1,
	})) {
		return
	}

	// Pause workflow: emit WaitEntered and block until resumed.
	if strings.Contains(rs.WorkflowSource, "pause") {
		if !send(criteria.NewEnvelope(rs.RunID, &pb.WaitEntered{
			Node:   "approval",
			Signal: "resume-test",
		})) {
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
			// Agent is shutting down; leave the run in its persisted state so a
			// restarted process can reattach. Do not emit a terminal event.
			a.log.Info("pausing run execution on shutdown", "run_id", rs.RunID)
			return
		case sig := <-ch:
			a.log.Info("resuming paused run", "run_id", rs.RunID, "signal", sig)
		}

		if !send(criteria.NewEnvelope(rs.RunID, &pb.WaitResumed{
			Node:   "approval",
			Mode:   "signal",
			Signal: "resume-test",
		})) {
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
			// Agent is shutting down; leave the run running for reattach.
			a.log.Info("stopping run execution on shutdown", "run_id", rs.RunID)
			return
		case <-time.After(a.cfg.longStepDur):
		}
	}

	if !send(criteria.NewEnvelope(rs.RunID, &pb.StepOutcome{
		Step:    "main",
		Outcome: "success",
	})) {
		return
	}

	rs.CurrentStep = "finish"
	a.setRunState(rs)
	if !send(criteria.NewEnvelope(rs.RunID, &pb.StepEntered{
		Step:    "finish",
		Attempt: 1,
	})) {
		return
	}
	if !send(criteria.NewEnvelope(rs.RunID, &pb.StepOutcome{
		Step:    "finish",
		Outcome: "success",
	})) {
		return
	}

	if !send(criteria.NewEnvelope(rs.RunID, &pb.RunCompleted{Success: true})) {
		return
	}

	if abort() {
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

// failRunWithBackgroundStream sends a terminal RunFailed event using a fresh
// SubmitEvents stream. It is used when the run's own event stream has already
// been closed (e.g. the run was cancelled via StopRun).
func (a *agent) failRunWithBackgroundStream(rs *runState, reason string) {
	a.log.Info("failing run on background stream", "run_id", rs.RunID, "reason", reason)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	stream := a.client.SubmitEvents(ctx)
	stream.RequestHeader().Set("Authorization", "Bearer "+a.token())
	defer func() {
		_ = stream.CloseRequest()
		_ = stream.CloseResponse()
	}()
	if err := a.sendEvent(stream, rs, criteria.NewEnvelope(rs.RunID, &pb.RunFailed{
		Reason: reason,
	})); err != nil {
		a.log.Error("failed to emit run failed on background stream", "run_id", rs.RunID, "err", err)
	}
	rs.Status = "failed"
	rs.FailureReason = reason
	a.setRunState(rs)
	a.deleteRunState(rs.RunID)
}

func (a *agent) failRun(ctx context.Context, stream *connect.BidiStreamForClient[criteria.Envelope, pb.Ack], rs *runState, gen int64, reason string) {
	if ctx.Err() != nil || !a.isCurrentExecutor(rs.RunID, gen) {
		a.log.Info("aborting failRun for superseded executor", "run_id", rs.RunID, "reason", reason)
		return
	}
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

func (a *agent) logErrorAndFail(ctx context.Context, stream *connect.BidiStreamForClient[criteria.Envelope, pb.Ack], rs *runState, gen int64, msg string, err error) {
	a.log.Error(msg, "run_id", rs.RunID, "err", err)
	if ctx.Err() != nil || !a.isCurrentExecutor(rs.RunID, gen) {
		a.log.Info("aborting logErrorAndFail for superseded executor", "run_id", rs.RunID)
		return
	}
	a.failRun(ctx, stream, rs, gen, fmt.Sprintf("%s: %v", msg, err))
}

func isTerminal(status string) bool {
	switch status {
	case "succeeded", "failed", "cancelled":
		return true
	default:
		return false
	}
}
