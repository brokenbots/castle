package rpc

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/brokenbots/castle/castle/internal/auth"
	"github.com/brokenbots/castle/castle/internal/store"
	"github.com/brokenbots/castle/castle/internal/store/sqlite"
	criteria "github.com/brokenbots/criteria/sdk"
	pb "github.com/brokenbots/criteria/sdk/pb/criteria/v1"
)

const (
	cursorFlushInterval        = 250 * time.Millisecond
	cursorFlushBatch           = 100
	cursorFinalPerWriteTimeout = 6 * time.Second
	cursorFinalMaxDuration     = 15 * time.Second
)

func (s *ServerServer) ListAgents(ctx context.Context, req *connect.Request[pb.ListAgentsRequest]) (*connect.Response[pb.ListAgentsResponse], error) {
	list, err := s.Store.ListOverseers(ctx)
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	out := make([]*pb.Agent, 0, len(list))
	for _, o := range list {
		out = append(out, mapAgent(o))
	}
	return connect.NewResponse(&pb.ListAgentsResponse{Agents: out}), nil
}

func (s *ServerServer) GetAgent(ctx context.Context, req *connect.Request[pb.GetAgentRequest]) (*connect.Response[pb.Agent], error) {
	o, err := s.Store.GetOverseer(ctx, req.Msg.CriteriaId)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return nil, connect.NewError(connect.CodeNotFound, err)
		}
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	return connect.NewResponse(mapAgent(o)), nil
}

func (s *ServerServer) ListRuns(ctx context.Context, req *connect.Request[pb.ListRunsRequest]) (*connect.Response[pb.ListRunsResponse], error) {
	list, err := s.Store.ListRuns(ctx, req.Msg.CriteriaId, req.Msg.Status)
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	out := make([]*pb.Run, 0, len(list))
	for _, r := range list {
		out = append(out, mapRun(r))
	}
	return connect.NewResponse(&pb.ListRunsResponse{Runs: out}), nil
}

func (s *ServerServer) GetRun(ctx context.Context, req *connect.Request[pb.GetRunRequest]) (*connect.Response[pb.Run], error) {
	r, err := s.Store.GetRun(ctx, req.Msg.RunId)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return nil, connect.NewError(connect.CodeNotFound, err)
		}
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	return connect.NewResponse(mapRun(r)), nil
}

func (s *ServerServer) ListRunEvents(ctx context.Context, req *connect.Request[pb.ListRunEventsRequest]) (*connect.Response[pb.ListRunEventsResponse], error) {
	limit := int(req.Msg.Limit)
	if limit <= 0 {
		limit = sqlite.ListEventsDefaultLimit
	}
	if limit > sqlite.ListEventsMaxLimit {
		return nil, connect.NewError(connect.CodeInvalidArgument, fmt.Errorf("limit %d exceeds maximum %d; page using since_seq", limit, sqlite.ListEventsMaxLimit))
	}

	all, err := s.Store.ListEvents(ctx, req.Msg.RunId, req.Msg.SinceSeq, limit)
	if err != nil {
		return nil, mapListEventsError(err)
	}

	events := make([]*criteria.Envelope, 0, len(all))
	for _, ev := range all {
		env, convErr := eventToEnvelope(ev)
		if convErr != nil {
			return nil, connect.NewError(connect.CodeInternal, convErr)
		}
		events = append(events, env)
	}

	resp := &pb.ListRunEventsResponse{Events: events}
	if len(events) > 0 {
		resp.LastSeq = events[len(events)-1].Seq
		if len(events) == limit {
			resp.NextSinceSeq = events[len(events)-1].Seq
		}
	}
	return connect.NewResponse(resp), nil
}

func (s *ServerServer) WatchRun(ctx context.Context, req *connect.Request[pb.WatchRunRequest], stream *connect.ServerStream[criteria.Envelope]) error {
	runID := req.Msg.RunId
	if runID == "" {
		return connect.NewError(connect.CodeInvalidArgument, errors.New("run_id required"))
	}
	subscriberID := req.Msg.SubscriberId
	effectiveSince := req.Msg.SinceSeq
	if subscriberID != "" && effectiveSince == 0 {
		if cursor, found, err := s.Store.GetSubscriberCursor(ctx, subscriberID, runID); err != nil {
			s.Log.Warn("watch cursor lookup failed", "run_id", runID, "subscriber_id", subscriberID, "error", err)
		} else if found {
			effectiveSince = cursor
		}
	}

	var (
		updateCursor func(uint64)
		writer       cursorWriter
	)
	updateCursor = func(uint64) {}
	if subscriberID != "" {
		writer = s.startCursorWriter(ctx, subscriberID, runID)
		updateCursor = writer.update
	}

	// Subscribe before replay so events published while replaying persisted rows
	// are queued for the subsequent buffer/live phases.
	sub := s.Hub.Subscribe(runID)
	defer s.Hub.Unsubscribe(sub)

	lastSent := effectiveSince
	terminalInReplay := false

	if subscriberID != "" {
		defer func() {
			// Ensure the highest sequence actually delivered to the client is
			// durably flushed before the writer stops. This closes the race
			// between the server's last updateCursor call and the client close.
			if lastSent > effectiveSince {
				writer.update(lastSent)
			}
			writer.flush()
			writer.stop()
		}()
	}

	_, err := forEachPersistedEventPage(ctx, s.Store, runID, effectiveSince, func(env *criteria.Envelope) error {
		if env.Seq <= lastSent {
			return nil
		}
		if err := stream.Send(env); err != nil {
			return err
		}
		lastSent = env.Seq
		updateCursor(env.Seq)
		if criteria.IsTerminal(env) {
			terminalInReplay = true
			return errStopEventPagination
		}
		return nil
	})
	if err != nil {
		return mapListEventsError(err)
	}
	if terminalInReplay {
		return nil
	}

	// WatchReady is sent once replay is complete so Connect flushes response
	// headers even when no live event has arrived yet. Clients must ignore it.
	if err := stream.Send(&criteria.Envelope{SchemaVersion: criteria.SchemaVersion, RunId: runID, Payload: &criteria.Envelope_WatchReady{WatchReady: &criteria.WatchReady{}}}); err != nil {
		return connect.NewError(connect.CodeUnknown, err)
	}

	// Drain the in-memory gap-closure tier after durable replay and before
	// tailing live fan-out.
	for _, env := range s.Hub.Since(runID, lastSent) {
		if env.Seq <= lastSent {
			continue
		}
		if err := stream.Send(env); err != nil {
			return connect.NewError(connect.CodeUnknown, err)
		}
		lastSent = env.Seq
		updateCursor(env.Seq)
		if criteria.IsTerminal(env) {
			return nil
		}
	}

	for {
		select {
		case <-ctx.Done():
			return nil
		case env, ok := <-sub.C:
			if !ok {
				return nil
			}
			if env.Seq <= lastSent {
				continue
			}
			if err := stream.Send(env); err != nil {
				return connect.NewError(connect.CodeUnknown, err)
			}
			lastSent = env.Seq
			updateCursor(env.Seq)
			if criteria.IsTerminal(env) {
				return nil
			}
		}
	}
}

type cursorWriter struct {
	update func(uint64)
	flush  func()
	stop   func()
}

// isCursorBusyError reports whether err is a transient SQLite busy error that
// should be retried by the cursor writer.
func isCursorBusyError(err error) bool {
	return err != nil && strings.Contains(err.Error(), "SQLITE_BUSY")
}

// upsertCursorWithRetry attempts to persist seq for (subscriberID, runID). It
// retries on SQLITE_BUSY errors with bounded backoff until the write succeeds,
// a non-retryable error occurs, the supplied context is canceled, or the retry
// budget is exhausted.
func upsertCursorWithRetry(
	ctx context.Context,
	st store.Store,
	subscriberID, runID string,
	seq uint64,
	perWriteTimeout, maxDuration time.Duration,
) error {
	deadline := time.Now().Add(maxDuration)
	delay := 5 * time.Millisecond
	var lastErr error
	for {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if time.Now().After(deadline) {
			break
		}

		// Bound the per-attempt timeout by the remaining overall budget so the
		// final attempt does not overshoot maxDuration.
		timeout := perWriteTimeout
		if remaining := time.Until(deadline); remaining < timeout {
			timeout = remaining
		}
		if timeout <= 0 {
			break
		}

		writeCtx, cancel := context.WithTimeout(context.Background(), timeout)
		lastErr = st.UpsertSubscriberCursor(writeCtx, subscriberID, runID, seq)
		cancel()
		if lastErr == nil {
			return nil
		}
		if !isCursorBusyError(lastErr) {
			return lastErr
		}

		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(delay):
		}
		delay *= 2
		if delay > 50*time.Millisecond {
			delay = 50 * time.Millisecond
		}
	}
	return lastErr
}

func (s *ServerServer) startCursorWriter(ctx context.Context, subscriberID, runID string) cursorWriter {
	var (
		mu           sync.Mutex
		hasPending   bool
		pendingSeq   uint64
		persistedSeq uint64
		pendingCount int
		once         sync.Once
	)

	// flushReq carries an optional completion channel. A nil channel is a
	// best-effort wake-up (used by batch-triggered updates); a non-nil channel
	// is closed after the requested flush attempt finishes.
	flushReq := make(chan chan struct{}, 1)
	stopCh := make(chan struct{})
	done := make(chan struct{})

	const (
		normalPerWriteTimeout = 2 * time.Second
		normalMaxDuration     = 2 * time.Second
	)

	// flush performs a single cursor write attempt with bounded retry. On
	// success it advances the persisted sequence and only clears pending state
	// when no higher sequence has arrived in the meantime. On a non-retryable
	// error it clears pending to avoid endless failure. On a retryable
	// SQLITE_BUSY error it leaves the pending sequence untouched so a later
	// flush can retry the same (or higher) value.
	flush := func(final bool, deadline time.Time) bool {
		mu.Lock()
		if !hasPending {
			mu.Unlock()
			return true
		}
		seq := pendingSeq
		mu.Unlock()

		maxDuration := normalMaxDuration
		perWrite := normalPerWriteTimeout
		if final {
			perWrite = cursorFinalPerWriteTimeout
			remaining := time.Until(deadline)
			if remaining <= 0 {
				s.Log.Warn("watch cursor final flush exhausted retry budget",
					"run_id", runID, "subscriber_id", subscriberID, "seq", seq)
				return false
			}
			maxDuration = remaining
		}

		// Cursor writes must outlive the request context: WatchRun's client
		// stream may be canceled before the final cursor flush completes, and
		// we still need to durably persist the highest delivered sequence.
		err := upsertCursorWithRetry(context.Background(), s.Store, subscriberID, runID, seq, perWrite, maxDuration)
		if err != nil {
			s.Log.Warn("watch cursor persist failed", "run_id", runID, "subscriber_id", subscriberID, "seq", seq, "error", err)
			if !isCursorBusyError(err) {
				mu.Lock()
				hasPending = false
				pendingCount = 0
				mu.Unlock()
			}
			return false
		}

		mu.Lock()
		persistedSeq = seq
		pendingCount = 0
		if pendingSeq <= persistedSeq {
			hasPending = false
		}
		mu.Unlock()
		return true
	}

	go func() {
		defer close(done)
		ticker := time.NewTicker(cursorFlushInterval)
		defer ticker.Stop()
		for {
			select {
			case doneCh := <-flushReq:
				flush(false, time.Time{})
				if doneCh != nil {
					close(doneCh)
				}
			case <-ticker.C:
				flush(false, time.Time{})
			case <-stopCh:
				finalDeadline := time.Now().Add(cursorFinalMaxDuration)
				for {
					flushed := flush(true, finalDeadline)
					mu.Lock()
					noPending := !hasPending
					mu.Unlock()
					if noPending || !flushed {
						return
					}
				}
			}
		}
	}()

	return cursorWriter{
		update: func(seq uint64) {
			if seq == 0 {
				return
			}
			mu.Lock()
			if seq <= persistedSeq {
				// Already durably persisted at or above seq.
				mu.Unlock()
				return
			}
			if seq > pendingSeq {
				pendingSeq = seq
			}
			if !hasPending {
				hasPending = true
				pendingCount = 0
			}
			pendingCount++
			shouldFlush := pendingCount >= cursorFlushBatch
			mu.Unlock()
			if shouldFlush {
				select {
				case flushReq <- nil:
				default:
				}
			}
		},
		flush: func() {
			doneCh := make(chan struct{})
			select {
			case flushReq <- doneCh:
				<-doneCh
			default:
				// A flush is already queued; wait for the next flush cycle to
				// complete by sending a fresh completion channel once there is
				// room.
				ticker := time.NewTicker(5 * time.Millisecond)
				defer ticker.Stop()
				for {
					select {
					case flushReq <- doneCh:
						<-doneCh
						return
					case <-ticker.C:
					}
				}
			}
		},
		stop: func() {
			once.Do(func() {
				close(stopCh)
			})
			<-done
		},
	}
}

func (s *ServerServer) StopRun(ctx context.Context, req *connect.Request[pb.StopRunRequest]) (*connect.Response[pb.StopRunResponse], error) {
	_, run, err := requireCallerOwnsRun(ctx, s.Store, req.Msg.RunId)
	if err != nil {
		return nil, err
	}
	if isTerminalRunStatus(run.Status) {
		return nil, connect.NewError(connect.CodeFailedPrecondition, errors.New("run is terminal"))
	}
	reason := req.Msg.Reason
	if reason == "" {
		reason = "requested by operator"
	}
	msg := &pb.ControlMessage{Command: &pb.ControlMessage_RunCancel{RunCancel: &pb.RunCancel{RunId: run.ID, Reason: reason}}}
	issuedAt, err := s.issueControlCommand(ctx, run, msg)
	if err != nil {
		return nil, err
	}
	return connect.NewResponse(&pb.StopRunResponse{IssuedAt: issuedAt}), nil
}

func (s *ServerServer) PauseRun(ctx context.Context, req *connect.Request[pb.PauseRunRequest]) (*connect.Response[pb.PauseRunResponse], error) {
	_, run, err := requireCallerOwnsRun(ctx, s.Store, req.Msg.RunId)
	if err != nil {
		return nil, err
	}
	if isTerminalRunStatus(run.Status) {
		return nil, connect.NewError(connect.CodeFailedPrecondition, errors.New("run is terminal"))
	}
	reason := "requested by operator"
	msg := &pb.ControlMessage{Command: &pb.ControlMessage_PauseRun{PauseRun: &pb.PauseRun{RunId: run.ID, Reason: reason}}}
	issuedAt, err := s.issueControlCommand(ctx, run, msg)
	if err != nil {
		return nil, err
	}
	return connect.NewResponse(&pb.PauseRunResponse{IssuedAt: issuedAt}), nil
}

func (s *ServerServer) ResumeRun(ctx context.Context, req *connect.Request[pb.ResumeRunRequest]) (*connect.Response[pb.ResumeRunResponse], error) {
	_, run, err := requireCallerOwnsRun(ctx, s.Store, req.Msg.RunId)
	if err != nil {
		return nil, err
	}
	if isTerminalRunStatus(run.Status) {
		return nil, connect.NewError(connect.CodeFailedPrecondition, errors.New("run is terminal"))
	}
	if run.Status != "paused" {
		return nil, connect.NewError(connect.CodeFailedPrecondition, errors.New("run is not paused"))
	}
	signal := run.PendingSignal
	if signal == "" {
		return nil, connect.NewError(connect.CodeFailedPrecondition, errors.New("run has no pending signal"))
	}
	msg := &pb.ControlMessage{Command: &pb.ControlMessage_ResumeRun{ResumeRun: &pb.ResumeRun{RunId: run.ID, Signal: signal}}}
	issuedAt, err := s.issueControlCommand(ctx, run, msg)
	if err != nil {
		return nil, err
	}
	return connect.NewResponse(&pb.ResumeRunResponse{IssuedAt: issuedAt}), nil
}

func (s *ServerServer) InspectRun(ctx context.Context, req *connect.Request[pb.InspectRunRequest]) (*connect.Response[pb.InspectRunResponse], error) {
	_, run, err := requireCallerOwnsRun(ctx, s.Store, req.Msg.RunId)
	if err != nil {
		return nil, err
	}

	resp := &pb.InspectRunResponse{
		RunId:       run.ID,
		SessionId:   req.Msg.SessionId,
		CurrentStep: run.CurrentStep,
		StateJson:   run.VariableScope,
	}

	// Report the most recent activity for the run.
	latest, err := s.Store.GetLatestEvent(ctx, run.ID)
	if err != nil && !errors.Is(err, store.ErrNotFound) {
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	if latest != nil && !latest.Ts.IsZero() {
		resp.LastActivityAt = timestamppb.New(latest.Ts)
	}

	// Report the most recent adapter the run entered.
	entered, err := s.Store.GetLatestStepEnteredEvent(ctx, run.ID)
	if err != nil && !errors.Is(err, store.ErrNotFound) {
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	if entered != nil {
		var payload pb.StepEntered
		if jsonErr := json.Unmarshal(entered.Payload, &payload); jsonErr == nil {
			resp.Adapter = payload.Adapter
		}
	}

	return connect.NewResponse(resp), nil
}

func (s *ServerServer) SubmitWorkflowAssignment(ctx context.Context, req *connect.Request[pb.SubmitWorkflowAssignmentRequest]) (*connect.Response[pb.SubmitWorkflowAssignmentResponse], error) {
	callerID := auth.CallerCriteriaID(ctx)
	if callerID == "" {
		return nil, connect.NewError(connect.CodeUnauthenticated, errors.New("caller required"))
	}
	if req.Msg.WorkflowName == "" || req.Msg.WorkflowSource == "" {
		return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("workflow_name and workflow_source required"))
	}
	if req.Msg.IdempotencyKey == "" {
		return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("idempotency_key required"))
	}

	a := &store.WorkflowAssignment{
		OwnerCriteriaID: callerID,
		WorkflowName:    req.Msg.WorkflowName,
		WorkflowSource:  req.Msg.WorkflowSource,
		LockfileSource:  req.Msg.LockfileSource,
		IdempotencyKey:  req.Msg.IdempotencyKey,
		Labels:          req.Msg.Labels,
	}
	existing, created, err := s.Store.CreateWorkflowAssignment(ctx, a)
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}

	resp := &pb.SubmitWorkflowAssignmentResponse{
		RunId:     existing.RunID,
		State:     mapAssignmentState(existing.State),
		CreatedAt: timestamppb.New(existing.CreatedAt),
	}
	if existing.LeasedCriteriaID != "" {
		resp.LeasedCriteriaId = existing.LeasedCriteriaID
	}

	// If a new assignment was created, attempt to dispatch it immediately to any
	// connected eligible agent.
	if created {
		// Fire-and-forget dispatch; errors are logged, not returned to caller.
		// Detach from the request context so dispatch survives RPC completion.
		go s.dispatchAssignment(context.WithoutCancel(ctx), existing)
	}

	return connect.NewResponse(resp), nil
}

func (s *ServerServer) GetAssignmentDisposition(ctx context.Context, req *connect.Request[pb.GetAssignmentDispositionRequest]) (*connect.Response[pb.GetAssignmentDispositionResponse], error) {
	callerID := auth.CallerCriteriaID(ctx)
	if callerID == "" {
		return nil, connect.NewError(connect.CodeUnauthenticated, errors.New("caller required"))
	}
	if req.Msg.RunId == "" {
		return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("run_id required"))
	}
	a, err := s.Store.GetWorkflowAssignmentByRunID(ctx, req.Msg.RunId)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return nil, connect.NewError(connect.CodeNotFound, err)
		}
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	if a.OwnerCriteriaID != callerID {
		return nil, connect.NewError(connect.CodePermissionDenied, errors.New("caller does not own this assignment"))
	}
	resp := &pb.GetAssignmentDispositionResponse{
		RunId:     a.RunID,
		State:     mapAssignmentState(a.State),
		CreatedAt: timestamppb.New(a.CreatedAt),
		UpdatedAt: timestamppb.New(a.UpdatedAt),
	}
	if a.State == store.WorkflowAssignmentStateRejected {
		resp.RejectionReason = a.TerminalReason
	}
	if a.LeasedCriteriaID != "" {
		resp.LeasedCriteriaId = a.LeasedCriteriaID
	}
	return connect.NewResponse(resp), nil
}

// dispatchAssignment attempts to lease the assignment to a connected
// eligible agent and push it via the agent's Control stream. If
// LeaseWorkflowAssignment returns a different queued assignment (because it
// always leases the oldest matching work), that assignment is still dispatched
// to the agent so it is never stranded in the leased state without a
// corresponding control message.
func (s *ServerServer) dispatchAssignment(ctx context.Context, a *store.WorkflowAssignment) {
	if a == nil || a.State != store.WorkflowAssignmentStateQueued {
		return
	}
	// Iterate over connected agents and try to lease to the first eligible one.
	// Serialize the actual lease attempt so two concurrent dispatchers cannot
	// grant the same agent two unstarted leases.
	for _, criteriaID := range s.controls.Registered() {
		o, err := s.Store.GetOverseer(ctx, criteriaID)
		if err != nil {
			s.Log.Debug("dispatch assignment: cannot load agent", "criteria_id", criteriaID, "err", err)
			continue
		}
		if o.Status != "online" {
			continue
		}

		s.controls.LeaseLock()
		now := time.Now().UTC()
		leased, err := s.Store.LeaseWorkflowAssignment(ctx, criteriaID, o.Labels, now, s.leaseDuration())
		s.controls.LeaseUnlock()
		if err != nil {
			if !errors.Is(err, store.ErrNotFound) {
				s.Log.Debug("dispatch assignment: lease failed", "criteria_id", criteriaID, "err", err)
			}
			continue
		}
		// Always dispatch the assignment that actually won the atomic lease,
		// even if it is not the one that triggered this dispatch. Otherwise the
		// leased assignment would sit undelivered until its lease expires.
		if err := enqueueWorkflowAssignment(s.Store, s.controls, criteriaID, leased, s.Log); err != nil {
			s.Log.Warn("dispatch assignment: control enqueue failed", "criteria_id", criteriaID, "run_id", leased.RunID, "err", err)
		}
		if leased.ID == a.ID {
			return
		}
		// Leased a different assignment; dispatch it above and keep looking
		// for the originally submitted assignment with the next agent.
	}
}

// ExpireLeasesNow expires any unstarted leases that are past their deadline
// and redispatches the requeued assignments to connected eligible agents.
func (s *ServerServer) ExpireLeasesNow(ctx context.Context) error {
	now := time.Now().UTC()
	ids, err := s.Store.ExpireWorkflowAssignmentLeases(ctx, now)
	if err != nil {
		return err
	}
	for _, id := range ids {
		a, err := s.Store.GetWorkflowAssignment(ctx, id)
		if err != nil {
			s.Log.Debug("expire leases: cannot load assignment", "assignment_id", id, "err", err)
			continue
		}
		s.dispatchAssignment(ctx, a)
	}
	return nil
}

const defaultAssignmentLeaseDuration = 5 * time.Minute

func mapAssignmentState(state string) pb.WorkflowAssignmentState {
	switch state {
	case store.WorkflowAssignmentStateQueued:
		return pb.WorkflowAssignmentState_WORKFLOW_ASSIGNMENT_STATE_QUEUED
	case store.WorkflowAssignmentStateLeased:
		return pb.WorkflowAssignmentState_WORKFLOW_ASSIGNMENT_STATE_LEASED
	case store.WorkflowAssignmentStateTerminal:
		return pb.WorkflowAssignmentState_WORKFLOW_ASSIGNMENT_STATE_TERMINAL
	case store.WorkflowAssignmentStateRejected:
		return pb.WorkflowAssignmentState_WORKFLOW_ASSIGNMENT_STATE_REJECTED
	default:
		return pb.WorkflowAssignmentState_WORKFLOW_ASSIGNMENT_STATE_UNSPECIFIED
	}
}

func (s *ServerServer) SendPrompt(ctx context.Context, req *connect.Request[pb.SendPromptRequest]) (*connect.Response[pb.SendPromptResponse], error) {
	_, run, err := requireCallerOwnsRun(ctx, s.Store, req.Msg.RunId)
	if err != nil {
		return nil, err
	}
	if isTerminalRunStatus(run.Status) {
		return nil, connect.NewError(connect.CodeFailedPrecondition, errors.New("run is terminal"))
	}
	if req.Msg.Step == "" {
		return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("step required"))
	}
	msg := &pb.ControlMessage{Command: &pb.ControlMessage_AgentPrompt{AgentPrompt: &pb.AgentPrompt{RunId: run.ID, Step: req.Msg.Step, Prompt: req.Msg.Prompt}}}
	issuedAt, err := s.issueControlCommand(ctx, run, msg)
	if err != nil {
		return nil, err
	}
	return connect.NewResponse(&pb.SendPromptResponse{IssuedAt: issuedAt}), nil
}

// issueControlCommand enqueues a control message to the criteria agent that
// owns the run. It rejects terminal runs with FailedPrecondition and maps
// control-registry errors to explicit Connect codes. The returned timestamp
// is the server time the command was issued.
func (s *ServerServer) issueControlCommand(ctx context.Context, run *store.Run, msg *pb.ControlMessage) (*timestamppb.Timestamp, error) {
	err := s.controls.Enqueue(run.OverseerID, msg)
	switch {
	case errors.Is(err, ErrAgentNotConnected):
		return nil, connect.NewError(connect.CodeFailedPrecondition, err)
	case errors.Is(err, ErrControlBacklogFull):
		s.Log.Warn("control backlog full; command dropped", "criteria_id", run.OverseerID, "run_id", run.ID)
		return nil, connect.NewError(connect.CodeFailedPrecondition, err)
	case err != nil:
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	return timestamppb.New(time.Now().UTC()), nil
}

// isTerminalRunStatus reports whether a run status represents a finished run.
func isTerminalRunStatus(status string) bool {
	switch status {
	case "succeeded", "failed", "cancelled":
		return true
	default:
		return false
	}
}
