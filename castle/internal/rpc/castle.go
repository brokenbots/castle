package rpc

import (
	"context"
	"errors"
	"time"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/brokenbots/overlord/castle/internal/store"
	"github.com/brokenbots/overlord/shared/events"
	pb "github.com/brokenbots/overlord/shared/pb/overlord/v1"
)

func (s *CastleServer) ListOverseers(ctx context.Context, _ *connect.Request[pb.ListOverseersRequest]) (*connect.Response[pb.ListOverseersResponse], error) {
	list, err := s.Store.ListOverseers(ctx)
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	out := make([]*pb.Overseer, 0, len(list))
	for _, o := range list {
		out = append(out, mapOverseer(o))
	}
	return connect.NewResponse(&pb.ListOverseersResponse{Overseers: out}), nil
}

func (s *CastleServer) GetOverseer(ctx context.Context, req *connect.Request[pb.GetOverseerRequest]) (*connect.Response[pb.Overseer], error) {
	o, err := s.Store.GetOverseer(ctx, req.Msg.OverseerId)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return nil, connect.NewError(connect.CodeNotFound, err)
		}
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	return connect.NewResponse(mapOverseer(o)), nil
}

func (s *CastleServer) ListRuns(ctx context.Context, req *connect.Request[pb.ListRunsRequest]) (*connect.Response[pb.ListRunsResponse], error) {
	list, err := s.Store.ListRuns(ctx, req.Msg.OverseerId, req.Msg.Status)
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	out := make([]*pb.Run, 0, len(list))
	for _, r := range list {
		out = append(out, mapRun(r))
	}
	return connect.NewResponse(&pb.ListRunsResponse{Runs: out}), nil
}

func (s *CastleServer) GetRun(ctx context.Context, req *connect.Request[pb.GetRunRequest]) (*connect.Response[pb.Run], error) {
	r, err := s.Store.GetRun(ctx, req.Msg.RunId)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return nil, connect.NewError(connect.CodeNotFound, err)
		}
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	return connect.NewResponse(mapRun(r)), nil
}

func (s *CastleServer) ListRunEvents(ctx context.Context, req *connect.Request[pb.ListRunEventsRequest]) (*connect.Response[pb.ListRunEventsResponse], error) {
	limit := int(req.Msg.Limit)
	if limit <= 0 {
		limit = 500
	}
	if limit > 5000 {
		limit = 5000
	}

	remaining := limit
	since := req.Msg.SinceSeq
	all := make([]*pb.Envelope, 0, limit)
	for remaining > 0 {
		chunk := remaining
		if chunk > 1000 {
			chunk = 1000
		}
		list, err := s.Store.ListEvents(ctx, req.Msg.RunId, since, chunk)
		if err != nil {
			return nil, connect.NewError(connect.CodeInternal, err)
		}
		if len(list) == 0 {
			break
		}
		all = append(all, list...)
		since = list[len(list)-1].Seq
		remaining -= len(list)
		if len(list) < chunk {
			break
		}
	}

	resp := &pb.ListRunEventsResponse{Events: all}
	if len(all) > 0 {
		resp.LastSeq = all[len(all)-1].Seq
	}
	return connect.NewResponse(resp), nil
}

func (s *CastleServer) WatchRun(ctx context.Context, req *connect.Request[pb.WatchRunRequest], stream *connect.ServerStream[pb.Envelope]) error {
	runID := req.Msg.RunId
	if runID == "" {
		return connect.NewError(connect.CodeInvalidArgument, errors.New("run_id required"))
	}

	// Subscribe to the hub BEFORE replay so that any event published while
	// we're draining persisted rows is still delivered (de-duplicated below
	// by seq). Unsubscribe on return.
	sub := s.Hub.Subscribe(runID)
	defer s.Hub.Unsubscribe(sub)

	// Flush response headers immediately so the Connect client's WatchRun
	// call can return to the caller. Without this sentinel, net/http2 would
	// not flush until the first body byte, which would deadlock any client
	// subscribing to a run that has not yet emitted an event. Clients must
	// ignore WatchReady payloads.
	if err := stream.Send(&pb.Envelope{SchemaVersion: 1, RunId: runID, Payload: &pb.Envelope_WatchReady{WatchReady: &pb.WatchReady{}}}); err != nil {
		return connect.NewError(connect.CodeUnknown, err)
	}

	seen := make(map[uint64]struct{})
	since := req.Msg.SinceSeq

	persisted, err := s.Store.ListEvents(ctx, runID, since, 5000)
	if err != nil {
		return connect.NewError(connect.CodeInternal, err)
	}
	for _, env := range persisted {
		if _, ok := seen[env.Seq]; ok {
			continue
		}
		seen[env.Seq] = struct{}{}
		if err := stream.Send(env); err != nil {
			return connect.NewError(connect.CodeUnknown, err)
		}
		if events.IsTerminal(env) {
			return nil
		}
		since = env.Seq
	}

	for {
		select {
		case <-ctx.Done():
			return nil
		case env, ok := <-sub.C:
			if !ok {
				return nil
			}
			if env.Seq <= since {
				continue
			}
			if _, exists := seen[env.Seq]; exists {
				continue
			}
			seen[env.Seq] = struct{}{}
			if err := stream.Send(env); err != nil {
				return connect.NewError(connect.CodeUnknown, err)
			}
			if events.IsTerminal(env) {
				return nil
			}
			since = env.Seq
		}
	}
}

func (s *CastleServer) StopRun(ctx context.Context, req *connect.Request[pb.StopRunRequest]) (*connect.Response[pb.StopRunResponse], error) {
	run, err := s.Store.GetRun(ctx, req.Msg.RunId)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return nil, connect.NewError(connect.CodeNotFound, err)
		}
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	reason := req.Msg.Reason
	if reason == "" {
		reason = "requested by operator"
	}
	err = s.controls.Enqueue(run.OverseerID, &pb.ControlMessage{Command: &pb.ControlMessage_RunCancel{RunCancel: &pb.RunCancel{RunId: run.ID, Reason: reason}}})
	switch {
	case errors.Is(err, ErrOverseerNotConnected):
		return nil, connect.NewError(connect.CodeFailedPrecondition, err)
	case errors.Is(err, ErrControlBacklogFull):
		s.Log.Warn("control backlog full; stop run dropped", "overseer_id", run.OverseerID, "run_id", run.ID)
		return nil, connect.NewError(connect.CodeFailedPrecondition, err)
	case err != nil:
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	now := time.Now().UTC()
	return connect.NewResponse(&pb.StopRunResponse{IssuedAt: timestamppb.New(now)}), nil
}

func (s *CastleServer) SendPrompt(context.Context, *connect.Request[pb.SendPromptRequest]) (*connect.Response[pb.SendPromptResponse], error) {
	return nil, connect.NewError(connect.CodeUnimplemented, errors.New("send prompt is not implemented"))
}
