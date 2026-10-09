package rpc

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"strconv"
	"time"

	"connectrpc.com/connect"
	"github.com/google/uuid"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/brokenbots/castle/castle/internal/auth"
	"github.com/brokenbots/castle/castle/internal/store"
	criteria "github.com/brokenbots/criteria/sdk"
	pb "github.com/brokenbots/criteria/sdk/pb/criteria/v1"
)

func (s *CriteriaServer) Register(ctx context.Context, req *connect.Request[pb.RegisterRequest]) (*connect.Response[pb.RegisterResponse], error) {
	now := time.Now().UTC()
	criteriaID := uuid.NewString()
	token := uuid.NewString()
	labels := make(map[string]string, len(req.Msg.Labels))
	for k, v := range req.Msg.Labels {
		labels[k] = v
	}
	o := &store.Overseer{
		ID:         criteriaID,
		Name:       req.Msg.Name,
		Hostname:   req.Msg.Labels["hostname"],
		Version:    req.Msg.Labels["version"],
		TokenHash:  auth.HashToken(token),
		Status:     "online",
		Labels:     labels,
		CreatedAt:  now,
		LastSeenAt: now,
	}
	if err := s.Store.CreateOverseer(ctx, o); err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	// Durable-before-ack (KB-223): do not hand back a token the CreateRun
	// auth path may not resolve yet. The 2026-09-21 incident had engines burn
	// their restart budget on "unauthenticated: invalid token" 1-2ms after a
	// successful Register while the row was invisible to token resolution.
	if err := s.awaitTokenVisible(ctx, o, token); err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	return connect.NewResponse(&pb.RegisterResponse{CriteriaId: criteriaID, Token: token}), nil
}

// awaitTokenVisible verifies, before Register acknowledges success, that
//   1. the overseer row and token hash are committed on the writer handle
//      (writer is synchronous=FULL in WAL mode, so this is fsync-durable), and
//   2. the fresh token resolves through the same reader-backed path the
//      CreateRun auth interceptor uses (auth.ResolveToken), and the resolved
//      row is the overseer just written.
//
// If the read-back cannot confirm within the bounded retry window, Register
// withholds the ack and returns an error, so a client either gets a token the
// subsequent CreateRun will accept or no token at all — never a token that
// deterministically fails authentication.
func (s *CriteriaServer) awaitTokenVisible(ctx context.Context, created *store.Overseer, token string) error {
	const maxAttempts = 10
	var reason string
	var attempts int
	for attempt := 0; attempt < maxAttempts; attempt++ {
		attempts = attempt + 1
		if attempt > 0 {
			wait := min(time.Duration(1<<uint(attempt))*2*time.Millisecond, 64*time.Millisecond)
			select {
			case <-ctx.Done():
				return fmt.Errorf("register ack withheld: overseer %s token not visible to read path; context canceled (%v)", created.ID, ctx.Err())
			case <-time.After(wait):
			}
		}
		var visible bool
		visible, reason = s.tokenVisible(ctx, created, token)
		if visible {
			return nil
		}
		if s.Log != nil {
			s.Log.Warn("register token not yet visible to read path; retrying before ack",
				"criteria_id", created.ID, "attempt", attempt+1, "reason", reason)
		}
	}
	return fmt.Errorf("register ack withheld: overseer %s token not visible to read path after %d attempts (%s)", created.ID, attempts, reason)
}

// tokenVisible runs the two-legged read-back: writer commit evidence and
// reader-backed token resolution. It reports a human-readable reason on miss.
func (s *CriteriaServer) tokenVisible(ctx context.Context, created *store.Overseer, token string) (bool, string) {
	committed, err := s.Store.GetOverseer(ctx, created.ID)
	if err != nil {
		return false, fmt.Sprintf("writer commit not confirmed: %v", err)
	}
	if committed.TokenHash != created.TokenHash {
		return false, "writer commit not confirmed: token hash mismatch on stored row"
	}
	resolved, err := auth.ResolveToken(ctx, s.Store, token)
	if err != nil {
		return false, fmt.Sprintf("read-path resolution failed: %v", err)
	}
	if resolved == nil {
		return false, "read-path resolution missed the freshly registered token"
	}
	if resolved.ID != created.ID {
		return false, "read-path resolution matched a different overseer row"
	}
	return true, ""
}

func (s *CriteriaServer) Heartbeat(ctx context.Context, req *connect.Request[pb.HeartbeatRequest]) (*connect.Response[pb.HeartbeatResponse], error) {
	// CallerCriteriaID wins; the request-supplied criteria_id is used only when
	// the interceptor is not wired (direct-call tests). In production the
	// caller's authenticated identity always determines which record is updated.
	criteriaID, err := requireCaller(ctx, req.Msg.CriteriaId)
	if err != nil {
		return nil, err
	}
	if criteriaID == "" {
		return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("criteria_id required"))
	}
	now := time.Now().UTC()
	if err := s.Store.UpdateOverseerSeen(ctx, criteriaID, now); err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	return connect.NewResponse(&pb.HeartbeatResponse{ServerTime: timestamppb.New(now)}), nil
}

func (s *CriteriaServer) CreateRun(ctx context.Context, req *connect.Request[pb.CreateRunRequest]) (*connect.Response[pb.Run], error) {
	// The run is always owned by the authenticated caller. The request-supplied
	// criteria_id is treated as informational only; CallerCriteriaID wins when
	// the interceptor is wired. When the interceptor is not present (direct-call
	// tests) the request field is used as a fallback.
	criteriaID, err := requireCaller(ctx, req.Msg.CriteriaId)
	if err != nil {
		return nil, err
	}
	if criteriaID == "" || req.Msg.WorkflowName == "" {
		return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("criteria_id and workflow_name required"))
	}
	now := time.Now().UTC()
	// Run context (ticket/repo/pr) no longer rides CreateRun (CRI-131: the
	// released criteria/sdk has no first-class fields for it); external
	// orchestrators publish it afterwards via run.metadata envelopes, which
	// castle promotes onto the run row in applyRunStatus.
	r := &store.Run{
		ID:           uuid.NewString(),
		OverseerID:   criteriaID,
		WorkflowName: req.Msg.WorkflowName,
		WorkflowHCL:  req.Msg.WorkflowHash,
		Status:       "pending",
		CreatedAt:    now,
	}
	if err := s.Store.CreateRun(ctx, r); err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	return connect.NewResponse(mapRun(r)), nil
}

func (s *CriteriaServer) SubmitEvents(ctx context.Context, stream *connect.BidiStream[criteria.Envelope, pb.Ack]) error {
	sinceSeq, replayRequested := parseSinceSeq(stream.RequestHeader().Get("since_seq"), stream.RequestHeader().Get("since-seq"))
	replayed := make(map[string]bool)

	// Per-envelope ownership: cache run→owner lookups so we hit the DB once per
	// run_id per stream, not once per envelope. Cache is stream-local (single
	// goroutine) so no synchronisation is needed.
	callerID := auth.CallerCriteriaID(ctx)
	runOwnerCache := make(map[string]string) // run_id → owner criteria_id

	for {
		msg, err := stream.Receive()
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return connect.NewError(connect.CodeUnknown, err)
		}
		if msg.SchemaVersion != int32(criteria.SchemaVersion) {
			return connect.NewError(connect.CodeFailedPrecondition, errors.New("schema_version mismatch"))
		}
		// Reject server-synthesised payloads that must never be ingested
		// from an Overseer. WatchReady is emitted by Castle on WatchRun
		// stream-open to flush response headers; allowing it through
		// SubmitEvents would persist an un-decodable row (see
		// sqlite.unmarshalPayload which has no watch.ready case).
		if _, ok := msg.Payload.(*criteria.Envelope_WatchReady); ok {
			return connect.NewError(connect.CodeInvalidArgument, errors.New("watch.ready is server-only and cannot be submitted"))
		}
		if msg.Ts == nil || msg.Ts.AsTime().IsZero() {
			msg.Ts = timestamppb.New(time.Now().UTC())
		}

		// Enforce run ownership when the interceptor is wired. Reject the
		// stream on the first envelope that belongs to a run the caller does
		// not own; do not silently drop offending envelopes.
		if callerID != "" {
			ownerID, cached := runOwnerCache[msg.RunId]
			if !cached {
				run, getErr := s.Store.GetRun(ctx, msg.RunId)
				if getErr != nil {
					if errors.Is(getErr, store.ErrNotFound) {
						return connect.NewError(connect.CodeNotFound, errors.New("run not found"))
					}
					return connect.NewError(connect.CodeInternal, getErr)
				}
				ownerID = run.OverseerID
				runOwnerCache[msg.RunId] = ownerID
			}
			if ownerID != callerID {
				return connect.NewError(connect.CodePermissionDenied, errors.New("caller does not own this run"))
			}
		}

		if replayRequested && !replayed[msg.RunId] {
			_, listErr := forEachPersistedEventPage(ctx, s.Store, msg.RunId, sinceSeq, func(priorEvent *criteria.Envelope) error {
				if err := stream.Send(&pb.Ack{RunId: priorEvent.RunId, Seq: priorEvent.Seq, CorrelationId: priorEvent.CorrelationId}); err != nil {
					return connect.NewError(connect.CodeUnknown, err)
				}
				return nil
			})
			if listErr != nil {
				return mapListEventsError(listErr)
			}
			replayed[msg.RunId] = true
		}

		ev, convErr := envelopeToEvent(msg)
		if convErr != nil {
			return connect.NewError(connect.CodeInvalidArgument, convErr)
		}
		seq, inserted, appendErr := s.Store.AppendEvent(ctx, ev)
		if appendErr != nil {
			return connect.NewError(connect.CodeInternal, appendErr)
		}
		msg.Seq = seq
		if inserted {
			s.applyRunStatus(ctx, msg)

			// Publish before ack to preserve real-time observer ordering guarantees.
			s.Hub.Publish(msg)
		} else {
			// Duplicate (run_id, correlation_id): Overseer replayed an
			// envelope whose prior ack we delivered on an earlier
			// stream. Ack again (idempotent) without re-running
			// side-effects. Logged at Debug so reconnect replays are
			// observable without adding noise.
			s.Log.Debug("duplicate event ignored", "run_id", msg.RunId, "correlation_id", msg.CorrelationId, "seq", seq)
		}

		if err := stream.Send(&pb.Ack{RunId: msg.RunId, Seq: msg.Seq, CorrelationId: msg.CorrelationId}); err != nil {
			return connect.NewError(connect.CodeUnknown, err)
		}
	}
}

func (s *CriteriaServer) Control(ctx context.Context, req *connect.Request[pb.ControlSubscribeRequest], stream *connect.ServerStream[pb.ControlMessage]) error {
	// CallerCriteriaID wins for the registry key. The request-supplied
	// criteria_id is used as fallback when the interceptor is not wired (tests).
	criteriaID, err := requireCaller(ctx, req.Msg.CriteriaId)
	if err != nil {
		return err
	}
	if criteriaID == "" {
		return connect.NewError(connect.CodeInvalidArgument, errors.New("criteria_id required"))
	}
	ch, err := s.controls.Register(criteriaID)
	if err != nil {
		return connect.NewError(connect.CodeInvalidArgument, err)
	}
	defer s.controls.Unregister(criteriaID, ch)

	// Send ControlReady immediately so the Connect server flushes response
	// headers. Without this, the Connect client's Control(...) call blocks in
	// http2 roundTrip until the first body byte arrives, which would prevent
	// any subsequent StopRun (driven via this same stream) from ever being
	// observed by the client. See workstream 02 reviewer notes.
	if err := stream.Send(&pb.ControlMessage{Command: &pb.ControlMessage_ControlReady{ControlReady: &pb.ControlReady{}}}); err != nil {
		return connect.NewError(connect.CodeUnknown, err)
	}

	// On connection, attempt to lease any queued assignment this agent is
	// eligible for and push it to the agent.
	go s.dispatchForAgent(ctx, criteriaID)

	for {
		select {
		case <-ctx.Done():
			return nil
		case msg, ok := <-ch:
			if !ok {
				return nil
			}
			if err := stream.Send(msg); err != nil {
				return connect.NewError(connect.CodeUnknown, err)
			}
		}
	}
}

func (s *CriteriaServer) ReattachRun(ctx context.Context, req *connect.Request[pb.ReattachRunRequest]) (*connect.Response[pb.ReattachRunResponse], error) {
	if req.Msg.RunId == "" {
		return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("run_id required"))
	}
	_, run, err := requireCallerOwnsRun(ctx, s.Store, req.Msg.RunId)
	if err != nil {
		return nil, err
	}

	// Cannot resume a terminal run; cannot reattach a stopped run either
	// (CRI-207): an operator-parked run resumes through the StopRun/ResumeRun
	// console path on the agent that owns it, never through crash-recovery
	// re-registration, which would create an ambiguous second executor for the
	// same run.
	isTerminal := run.Status == "succeeded" || run.Status == "failed" || run.Status == "cancelled"
	if isTerminal || run.Status == "stopped" {
		return connect.NewResponse(&pb.ReattachRunResponse{
			Status:    run.Status,
			CanResume: false,
		}), nil
	}

	// Flush any pending variable-scope mutations before reading run state so
	// the reattach response reflects the latest scope (W04/CRI-71).
	s.scope.FlushNow(ctx, run.ID)
	run, err = s.Store.GetRun(ctx, run.ID)
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}

	resp := &pb.ReattachRunResponse{
		Status:        run.Status,
		CurrentStep:   run.CurrentStep,
		LastSeq:       run.LastSeq,
		CanResume:     true,
		VariableScope: run.VariableScope,
		PendingSignal: run.PendingSignal,
	}
	if run.CurrentStep != "" {
		latest, latestErr := s.Store.GetLatestAttempt(ctx, run.ID, run.CurrentStep)
		if latestErr == nil && latest != nil {
			resp.Attempt = int32(latest.Attempt)
		}
	}
	return connect.NewResponse(resp), nil
}

func (s *CriteriaServer) applyRunStatus(ctx context.Context, env *criteria.Envelope) {
	switch p := env.Payload.(type) {
	case *criteria.Envelope_RunStarted:
		run, err := s.Store.GetRun(ctx, env.RunId)
		if err != nil {
			return
		}
		if isTerminalRunStatus(run.Status) {
			// Terminal states are never rewritten (CRI-142): a late agent
			// RunStarted after an operator CancelRun or heartbeat reaping
			// must not flip the run back to "running". The event itself
			// stays pollable via the event log.
			return
		}
		if run.Status == "stopped" {
			// A run an operator parked as stopped (CRI-207) stays parked out
			// of band of agent events: only an explicit ResumeRun returns it
			// to running, and RunStarted must not stamp started_at for a run
			// whose work castle did not dispatch. The event itself stays
			// pollable via the event log.
			s.Log.Debug("applyRunStatus: ignoring run started for stopped run", "run_id", env.RunId)
			return
		}
		run.Status = "running"
		if run.StartedAt == nil {
			// First transition to running stamps the start instant (CRI-187
			// run-list durations); a duplicate or late RunStarted never
			// rewrites it, and UpdateRun keeps the stored value anyway.
			now := time.Now().UTC()
			run.StartedAt = &now
		}
		if p.RunStarted != nil {
			run.CurrentStep = p.RunStarted.InitialStep
		}
		_ = s.Store.UpdateRun(ctx, run)
		// The agent accepted this lease; send the next queued assignment if any.
		if run.OverseerID != "" {
			go s.dispatchForAgent(context.Background(), run.OverseerID)
		}
	case *criteria.Envelope_RunMetadata:
		// External orchestrators (e.g. the criteria-k8s operator) publish run
		// metadata such as the PR URL after the run record is created (CRI-131).
		// Non-empty values are promoted onto the run row; the event itself is
		// still stored and fanned out like any other envelope.
		if p.RunMetadata == nil {
			return
		}
		_ = s.Store.SetRunMetadata(ctx, env.RunId, p.RunMetadata.Ticket, p.RunMetadata.RepoUrl, p.RunMetadata.PrUrl)
	case *criteria.Envelope_StepEntered:
		run, err := s.Store.GetRun(ctx, env.RunId)
		if err != nil {
			return
		}
		if p.StepEntered != nil {
			run.CurrentStep = p.StepEntered.Step
			_ = s.Store.UpdateRun(ctx, run)
			_ = s.Store.RecordAttemptStart(ctx, &store.RunAttempt{
				RunID:     env.RunId,
				Step:      p.StepEntered.Step,
				Attempt:   int(p.StepEntered.Attempt),
				StartedAt: time.Now().UTC(),
			})
		}
	case *criteria.Envelope_StepOutcome:
		if p.StepOutcome == nil {
			return
		}
		// Complete the latest attempt for this step.
		latest, err := s.Store.GetLatestAttempt(ctx, env.RunId, p.StepOutcome.Step)
		if err == nil && latest != nil {
			outcome := p.StepOutcome.Outcome
			if outcome == "" {
				outcome = "error"
			}
			_ = s.Store.RecordAttemptComplete(ctx, env.RunId, p.StepOutcome.Step, latest.Attempt, outcome)
		}
	case *criteria.Envelope_StepResumed:
		// Informational only; no run-status side-effect.
		if p.StepResumed != nil {
			s.Log.Info("step resumed after crash",
				"run_id", env.RunId,
				"step", p.StepResumed.Step,
				"attempt", p.StepResumed.Attempt,
				"reason", p.StepResumed.Reason)
		}
	case *criteria.Envelope_VariableSet:
		// Queue the scope mutation for coalesced persistence (W04).
		if p.VariableSet == nil {
			return
		}
		name, value := p.VariableSet.Name, p.VariableSet.Value
		s.scope.Enqueue(env.RunId, func(scope map[string]interface{}) {
			varMap, _ := scope["var"].(map[string]interface{})
			if varMap == nil {
				varMap = map[string]interface{}{}
			}
			varMap[name] = value
			scope["var"] = varMap
		})
	case *criteria.Envelope_StepOutputCaptured:
		// Queue step output merge for coalesced persistence (W04).
		if p.StepOutputCaptured == nil {
			return
		}
		step := p.StepOutputCaptured.Step
		outputs := make(map[string]string, len(p.StepOutputCaptured.Outputs))
		for k, v := range p.StepOutputCaptured.Outputs {
			outputs[k] = v
		}
		s.scope.Enqueue(env.RunId, func(scope map[string]interface{}) {
			stepsMap, _ := scope["steps"].(map[string]interface{})
			if stepsMap == nil {
				stepsMap = map[string]interface{}{}
			}
			outputMap := make(map[string]interface{}, len(outputs))
			for k, v := range outputs {
				outputMap[k] = v
			}
			stepsMap[step] = outputMap
			scope["steps"] = stepsMap
		})
	case *criteria.Envelope_WaitEntered:
		// Run entered a wait node; mark as paused only for signal-mode waits (W05).
		// Duration-mode waits do not pause the run; the engine sleeps and resumes
		// without a control-plane round-trip, so the DB status stays "running".
		if p.WaitEntered == nil || p.WaitEntered.Signal == "" {
			return
		}
		now := time.Now().UTC()
		_ = s.Store.SetRunPaused(ctx, env.RunId, p.WaitEntered.Signal, now)
	case *criteria.Envelope_WaitResumed:
		// Informational: the handler that accepted the resume (resume.go for
		// agent-initiated resumes, ServerService.ResumeRun for console ones)
		// already called ClearRunPaused before enqueuing the ResumeRun control
		// message. A second ClearRunPaused here would race against RunCompleted
		// and could set status="running" after the run has already succeeded.
		// No-op. (W05/F-04)
		if p.WaitResumed == nil {
			return
		}
	case *criteria.Envelope_ApprovalRequested:
		// Run entered an approval node; mark as paused with the node name as signal (W05).
		if p.ApprovalRequested == nil {
			return
		}
		now := time.Now().UTC()
		_ = s.Store.SetRunPaused(ctx, env.RunId, p.ApprovalRequested.Node, now)
	case *criteria.Envelope_ApprovalDecision:
		// Informational: same as WaitResumed — the accepting resume handler
		// (resume.go or ServerService.ResumeRun) already cleared the pause.
		// No-op here to avoid the double-clear race with RunCompleted. (W05/F-04)
		if p.ApprovalDecision == nil {
			return
		}
	case *criteria.Envelope_BranchEvaluated:
		// No run-status side-effect. BranchEvaluated is informational; Castle
		// stores and fans out the event without mutating run state (W06).
		if p.BranchEvaluated == nil {
			return
		}
	case *criteria.Envelope_ForEachEntered:
		// Informational event only. Cursor persistence is handled by
		// ScopeIterCursorSet so Castle does not need to know IterCursor's
		// schema (W07 split-readiness).
		if p.ForEachEntered == nil {
			return
		}
	case *criteria.Envelope_StepIterationStarted:
		// Informational event only. See ForEachEntered comment above.
		if p.StepIterationStarted == nil {
			return
		}
	case *criteria.Envelope_StepIterationCompleted:
		// Informational event only. See ForEachEntered comment above.
		if p.StepIterationCompleted == nil {
			return
		}
	case *criteria.Envelope_ScopeIterCursorSet:
		// Overseer serialises the full IterCursor as opaque JSON and emits this
		// event whenever the cursor is created, advanced, or cleared. Castle
		// stores cursor_json verbatim into scope["iter"] without interpreting
		// field names, preserving 1.6 split independence (W07).
		if p.ScopeIterCursorSet == nil {
			return
		}
		blob := p.ScopeIterCursorSet.CursorJson
		s.scope.Enqueue(env.RunId, func(scope map[string]interface{}) {
			if blob == "" {
				delete(scope, "iter")
				return
			}
			var iterMap map[string]interface{}
			if json.Unmarshal([]byte(blob), &iterMap) == nil {
				scope["iter"] = iterMap
			}
		})
	case *criteria.Envelope_RunCompleted:
		// Flush any pending scope mutations before marking the run terminal so
		// the final scope is available to any post-run readers.
		s.scope.FlushNow(ctx, env.RunId)
		run, err := s.Store.GetRun(ctx, env.RunId)
		if err != nil {
			return
		}
		if isTerminalRunStatus(run.Status) {
			// Terminal states are never rewritten (CRI-142): a late agent
			// event after an operator CancelRun or heartbeat reaping must not
			// flip the run out of its stamped terminal state. The event
			// itself stays pollable via the event log.
			return
		}
		if run.Status == "stopped" {
			// A run an operator parked as stopped stays parked (CRI-207):
			// stop tears the engine's sessions down and the agent may surface
			// teardown/completion events while parking, but the operator stop
			// is authoritative and only an explicit ResumeRun returns the run
			// to running. The event itself stays pollable via the event log.
			s.Log.Debug("applyRunStatus: ignoring completion event for stopped run", "run_id", env.RunId, "type", criteria.TypeString(env))
			return
		}
		now := time.Now().UTC()
		run.EndedAt = &now
		if p.RunCompleted != nil && p.RunCompleted.Success {
			run.Status = "succeeded"
		} else {
			run.Status = "failed"
		}
		_ = s.Store.UpdateRun(ctx, run)
		_ = s.Store.MarkWorkflowAssignmentTerminal(ctx, env.RunId, "run completed")
		if run.OverseerID != "" {
			go s.dispatchForAgent(context.Background(), run.OverseerID)
		}
	case *criteria.Envelope_RunFailed:
		// Flush pending scope before marking terminal.
		s.scope.FlushNow(ctx, env.RunId)
		run, err := s.Store.GetRun(ctx, env.RunId)
		if err != nil {
			return
		}
		if isTerminalRunStatus(run.Status) {
			// See RunCompleted above: terminal stamps are final (CRI-142).
			return
		}
		if run.Status == "stopped" {
			// See RunCompleted above: an operator-parked run stays parked
			// (CRI-207) even when the stopping agent reports a failure from
			// its own teardown; resume is operator-driven.
			s.Log.Debug("applyRunStatus: ignoring failure event for stopped run", "run_id", env.RunId, "type", criteria.TypeString(env))
			return
		}
		now := time.Now().UTC()
		run.EndedAt = &now
		run.Status = "failed"
		if p.RunFailed != nil && p.RunFailed.Reason != "" {
			run.FailureReason = p.RunFailed.Reason
		}
		_ = s.Store.UpdateRun(ctx, run)
		_ = s.Store.MarkWorkflowAssignmentTerminal(ctx, env.RunId, "run failed")
		if run.OverseerID != "" {
			go s.dispatchForAgent(context.Background(), run.OverseerID)
		}
	default:
		// Log unknown payload types so any drift from the expected set is visible.
		// This closes TD-14 (silent default in applyRunStatus).
		s.Log.Debug("applyRunStatus: unhandled payload type", "run_id", env.RunId, "type", criteria.TypeString(env))
	}
}

// dispatchForAgent redelivers any active leases the agent already holds (e.g.
// after a Castle restart) and then attempts to lease one new queued assignment
// the agent is eligible for. It is safe to run in a goroutine; errors are logged.
func (s *CriteriaServer) dispatchForAgent(ctx context.Context, criteriaID string) {
	// First, redeliver any in-flight leases that were active before this Control
	// stream opened. This restores assignment dispatch after a Castle restart
	// without re-leasing the work.
	active, err := s.Store.ListLeasedPendingAssignmentsByCriteriaID(ctx, criteriaID)
	if err != nil {
		s.Log.Debug("dispatch for agent: cannot list active leases", "criteria_id", criteriaID, "err", err)
	} else {
		for _, a := range active {
			if err := enqueueWorkflowAssignment(s.Store, s.controls, criteriaID, a, s.Log); err != nil {
				s.Log.Warn("dispatch for agent: redeliver active lease failed", "criteria_id", criteriaID, "run_id", a.RunID, "err", err)
			}
		}
	}

	o, err := s.Store.GetOverseer(ctx, criteriaID)
	if err != nil {
		s.Log.Debug("dispatch for agent: cannot load agent", "criteria_id", criteriaID, "err", err)
		return
	}
	if o.Status != "online" {
		return
	}

	// Serialize lease attempts to prevent concurrent dispatchers from granting
	// this agent a second unstarted lease before it accepts the first one.
	leaseAndDispatchNext(ctx, s.Store, s.controls, s.Log, criteriaID, o.Labels, s.leaseDuration())
}

// leaseAndDispatchNext leases one queued assignment for the agent under the
// registry lease lock and pushes it to the agent's control channel. It is the
// CriteriaServer dispatch loop's lease trigger (CRI-73): an agent that has
// accepted its running work becomes eligible for the next queued assignment,
// and a reconnecting agent re-leases work it no longer holds. Safe to run in
// a goroutine; errors are logged.
func leaseAndDispatchNext(ctx context.Context, st store.Store, controls *ControlRegistry, log *slog.Logger, criteriaID string, agentLabels map[string]string, leaseDuration time.Duration) {
	controls.LeaseLock()
	defer controls.LeaseUnlock()

	now := time.Now().UTC()
	leased, err := st.LeaseWorkflowAssignment(ctx, criteriaID, agentLabels, now, leaseDuration)
	if err != nil {
		if !errors.Is(err, store.ErrNotFound) {
			log.Debug("dispatch for agent: lease failed", "criteria_id", criteriaID, "err", err)
		}
		return
	}
	if err := enqueueWorkflowAssignment(st, controls, criteriaID, leased, log); err != nil {
		log.Warn("dispatch for agent: control enqueue failed", "criteria_id", criteriaID, "run_id", leased.RunID, "err", err)
	}
}

// parseSinceSeq returns the parsed uint64 sinceSeq and a flag indicating
// whether the caller explicitly requested replay. The flag is true when any
// of the provided header values parses as a valid unsigned integer
// (including zero, which means "replay from the beginning"). It is false
// only when no header is present or all values are malformed.
func parseSinceSeq(values ...string) (uint64, bool) {
	for _, v := range values {
		if v == "" {
			continue
		}
		out, err := strconv.ParseUint(v, 10, 64)
		if err == nil {
			return out, true
		}
	}
	return 0, false
}
