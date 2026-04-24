package rpc

import (
	"context"
	"encoding/json"
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
		if msg.Ts == nil || msg.Ts.AsTime().IsZero() {
			msg.Ts = timestamppb.New(time.Now().UTC())
		}

		if replayRequested && !replayed[msg.RunId] {
			prior, listErr := s.Store.ListEvents(ctx, msg.RunId, sinceSeq, 5000)
			if listErr != nil {
				return connect.NewError(connect.CodeInternal, listErr)
			}
			for _, priorEvent := range prior {
				if err := stream.Send(&pb.Ack{RunId: priorEvent.RunID, Seq: priorEvent.Seq, CorrelationId: priorEvent.CorrelationID}); err != nil {
					return connect.NewError(connect.CodeUnknown, err)
				}
			}
			replayed[msg.RunId] = true
		}

		env, convErr := toStoreEnvelope(msg)
		if convErr != nil {
			return connect.NewError(connect.CodeInvalidArgument, convErr)
		}
		seq, appendErr := s.Store.AppendEvent(ctx, env)
		if appendErr != nil {
			return connect.NewError(connect.CodeInternal, appendErr)
		}
		env.Seq = seq
		s.applyRunStatus(ctx, env)

		// Publish before ack to preserve real-time observer ordering guarantees.
		s.Hub.Publish(env)

		if err := stream.Send(&pb.Ack{RunId: env.RunID, Seq: env.Seq, CorrelationId: env.CorrelationID}); err != nil {
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

func (s *OverseerServer) applyRunStatus(ctx context.Context, env events.Envelope) {
	switch env.Type {
	case events.TypeRunStarted:
		run, err := s.Store.GetRun(ctx, env.RunID)
		if err != nil {
			return
		}
		run.Status = "running"
		var p events.RunStarted
		if json.Unmarshal(env.Payload, &p) == nil {
			run.CurrentStep = p.InitialStep
		}
		_ = s.Store.UpdateRun(ctx, run)
	case events.TypeStepEntered:
		run, err := s.Store.GetRun(ctx, env.RunID)
		if err != nil {
			return
		}
		var p events.StepEntered
		if json.Unmarshal(env.Payload, &p) == nil {
			run.CurrentStep = p.Step
			_ = s.Store.UpdateRun(ctx, run)
		}
	case events.TypeRunCompleted, events.TypeRunFailed:
		run, err := s.Store.GetRun(ctx, env.RunID)
		if err != nil {
			return
		}
		now := time.Now().UTC()
		run.EndedAt = &now
		if env.Type == events.TypeRunCompleted {
			var p events.RunCompleted
			if json.Unmarshal(env.Payload, &p) == nil && p.Success {
				run.Status = "succeeded"
			} else {
				run.Status = "failed"
			}
		} else {
			run.Status = "failed"
		}
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
