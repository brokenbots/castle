package rpc

import (
	"context"
	"errors"
	"io"
	"strconv"
	"time"

	"connectrpc.com/connect"
	"github.com/google/uuid"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/brokenbots/overlord/castle/internal/auth"
	"github.com/brokenbots/overlord/castle/internal/store"
	"github.com/brokenbots/overlord/shared/events"
	pb "github.com/brokenbots/overlord/shared/pb/overlord/v1"
)

func (s *OverseerServer) Register(ctx context.Context, req *connect.Request[pb.RegisterRequest]) (*connect.Response[pb.RegisterResponse], error) {
	now := time.Now().UTC()
	overseerID := uuid.NewString()
	token := uuid.NewString()
	o := &store.Overseer{
		ID:         overseerID,
		Name:       req.Msg.Name,
		Hostname:   req.Msg.Labels["hostname"],
		Version:    req.Msg.Labels["version"],
		TokenHash:  auth.HashToken(token),
		Status:     "online",
		CreatedAt:  now,
		LastSeenAt: now,
	}
	if err := s.Store.CreateOverseer(ctx, o); err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	return connect.NewResponse(&pb.RegisterResponse{OverseerId: overseerID, Token: token}), nil
}

func (s *OverseerServer) Heartbeat(ctx context.Context, req *connect.Request[pb.HeartbeatRequest]) (*connect.Response[pb.HeartbeatResponse], error) {
	if req.Msg.OverseerId == "" {
		return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("overseer_id required"))
	}
	now := time.Now().UTC()
	if err := s.Store.UpdateOverseerSeen(ctx, req.Msg.OverseerId, now); err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	return connect.NewResponse(&pb.HeartbeatResponse{ServerTime: timestamppb.New(now)}), nil
}

func (s *OverseerServer) CreateRun(ctx context.Context, req *connect.Request[pb.CreateRunRequest]) (*connect.Response[pb.Run], error) {
	if req.Msg.OverseerId == "" || req.Msg.WorkflowName == "" {
		return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("overseer_id and workflow_name required"))
	}
	now := time.Now().UTC()
	r := &store.Run{
		ID:           uuid.NewString(),
		OverseerID:   req.Msg.OverseerId,
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

func (s *OverseerServer) SubmitEvents(ctx context.Context, stream *connect.BidiStream[pb.Envelope, pb.Ack]) error {
	sinceSeq, replayRequested := parseSinceSeq(stream.RequestHeader().Get("since_seq"), stream.RequestHeader().Get("since-seq"))
	replayed := make(map[string]bool)

	for {
		msg, err := stream.Receive()
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return connect.NewError(connect.CodeUnknown, err)
		}
		if msg.SchemaVersion != int32(events.SchemaVersion) {
			return connect.NewError(connect.CodeFailedPrecondition, errors.New("schema_version mismatch"))
		}
		// Reject server-synthesised payloads that must never be ingested
		// from an Overseer. WatchReady is emitted by Castle on WatchRun
		// stream-open to flush response headers; allowing it through
		// SubmitEvents would persist an un-decodable row (see
		// sqlite.unmarshalPayload which has no watch.ready case).
		if _, ok := msg.Payload.(*pb.Envelope_WatchReady); ok {
			return connect.NewError(connect.CodeInvalidArgument, errors.New("watch.ready is server-only and cannot be submitted"))
		}
		if msg.Ts == nil || msg.Ts.AsTime().IsZero() {
			msg.Ts = timestamppb.New(time.Now().UTC())
		}

		if replayRequested && !replayed[msg.RunId] {
			_, listErr := forEachPersistedEventPage(ctx, s.Store, msg.RunId, sinceSeq, func(priorEvent *pb.Envelope) error {
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

		seq, inserted, appendErr := s.Store.AppendEvent(ctx, msg)
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

func (s *OverseerServer) Control(ctx context.Context, req *connect.Request[pb.ControlSubscribeRequest], stream *connect.ServerStream[pb.ControlMessage]) error {
	overseerID := req.Msg.OverseerId
	if overseerID == "" {
		return connect.NewError(connect.CodeInvalidArgument, errors.New("overseer_id required"))
	}
	ch, err := s.controls.Register(overseerID)
	if err != nil {
		return connect.NewError(connect.CodeInvalidArgument, err)
	}
	defer s.controls.Unregister(overseerID, ch)

	// Send ControlReady immediately so the Connect server flushes response
	// headers. Without this, the Connect client's Control(...) call blocks in
	// http2 roundTrip until the first body byte arrives, which would prevent
	// any subsequent StopRun (driven via this same stream) from ever being
	// observed by the client. See workstream 02 reviewer notes.
	if err := stream.Send(&pb.ControlMessage{Command: &pb.ControlMessage_ControlReady{ControlReady: &pb.ControlReady{}}}); err != nil {
		return connect.NewError(connect.CodeUnknown, err)
	}

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

func (s *OverseerServer) ReattachRun(ctx context.Context, req *connect.Request[pb.ReattachRunRequest]) (*connect.Response[pb.ReattachRunResponse], error) {
	if req.Msg.RunId == "" || req.Msg.OverseerId == "" {
		return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("run_id and overseer_id required"))
	}
	run, err := s.Store.GetRun(ctx, req.Msg.RunId)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return nil, connect.NewError(connect.CodeNotFound, errors.New("run not found"))
		}
		return nil, connect.NewError(connect.CodeInternal, err)
	}

	// Cannot resume a terminal run or a run owned by a different Overseer.
	isTerminal := run.Status == "succeeded" || run.Status == "failed" || run.Status == "cancelled"
	if isTerminal || run.OverseerID != req.Msg.OverseerId {
		return connect.NewResponse(&pb.ReattachRunResponse{
			Status:    run.Status,
			CanResume: false,
		}), nil
	}

	resp := &pb.ReattachRunResponse{
		Status:      run.Status,
		CurrentStep: run.CurrentStep,
		LastSeq:     run.LastSeq,
		CanResume:   true,
	}
	if run.CurrentStep != "" {
		latest, latestErr := s.Store.GetLatestAttempt(ctx, run.ID, run.CurrentStep)
		if latestErr == nil && latest != nil {
			resp.Attempt = int32(latest.Attempt)
		}
	}
	return connect.NewResponse(resp), nil
}

func (s *OverseerServer) applyRunStatus(ctx context.Context, env *pb.Envelope) {
	switch p := env.Payload.(type) {
	case *pb.Envelope_RunStarted:
		run, err := s.Store.GetRun(ctx, env.RunId)
		if err != nil {
			return
		}
		run.Status = "running"
		if p.RunStarted != nil {
			run.CurrentStep = p.RunStarted.InitialStep
		}
		_ = s.Store.UpdateRun(ctx, run)
	case *pb.Envelope_StepEntered:
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
	case *pb.Envelope_StepOutcome:
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
	case *pb.Envelope_StepResumed:
		// Informational only; no run-status side-effect.
		if p.StepResumed != nil {
			s.Log.Info("step resumed after crash",
				"run_id", env.RunId,
				"step", p.StepResumed.Step,
				"attempt", p.StepResumed.Attempt,
				"reason", p.StepResumed.Reason)
		}
	case *pb.Envelope_RunCompleted:
		run, err := s.Store.GetRun(ctx, env.RunId)
		if err != nil {
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
	case *pb.Envelope_RunFailed:
		run, err := s.Store.GetRun(ctx, env.RunId)
		if err != nil {
			return
		}
		now := time.Now().UTC()
		run.EndedAt = &now
		run.Status = "failed"
		_ = s.Store.UpdateRun(ctx, run)
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
