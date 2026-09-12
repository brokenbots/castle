package rpc

import (
	"context"
	"errors"
	"fmt"
	"log/slog"

	"connectrpc.com/connect"

	"github.com/brokenbots/castle/castle/internal/store"
	"github.com/brokenbots/castle/castle/internal/store/sqlite"
	criteria "github.com/brokenbots/criteria/sdk"
	pb "github.com/brokenbots/criteria/sdk/pb/criteria/v1"
)

// OrchestratorServer implements criteria.v1.OrchestratorService (CRI-133):
// the operator-facing polling read path over run lifecycle. Design note:
// docs/adrs/ADR-0005-orchestrator-event-subscription.md.
//
// The service is intentionally read-only and stateless: each call carries its
// own since_seq cursor and the operator persists the returned continuation
// cursor, so polling performs no writes against the single-replica SQLite
// writer (CRI-78) and survives operator restarts.
type OrchestratorServer struct {
	Store store.Store
	Log   *slog.Logger
}

func NewOrchestratorServer(st store.Store, log *slog.Logger) *OrchestratorServer {
	return &OrchestratorServer{Store: st, Log: log}
}

// SubscribeRunEvents returns the persisted events for one run with
// seq > since_seq, ascending by seq, as one bounded page (CRI-133).
//
// Replay is gapless and duplicate-free under interleaved writers: AppendEvent
// assigns per-run monotonic seqs and is idempotent on (run_id,
// correlation_id). Like ServerService.ListRunEvents, polling an unknown
// run_id yields an empty page rather than NotFound; existence is observable
// via GetRun/ListActiveRuns.
func (s *OrchestratorServer) SubscribeRunEvents(ctx context.Context, req *connect.Request[pb.SubscribeRunEventsRequest]) (*connect.Response[pb.SubscribeRunEventsResponse], error) {
	if req.Msg.RunId == "" {
		return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("run_id required"))
	}
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

	resp := &pb.SubscribeRunEventsResponse{Events: events}
	if len(events) > 0 {
		resp.LastSeq = events[len(events)-1].Seq
		if len(events) == limit {
			resp.NextSinceSeq = events[len(events)-1].Seq
		}
	}
	return connect.NewResponse(resp), nil
}

// ListActiveRuns returns runs that have not reached a terminal state
// (pending | running | paused), the discovery step of the operator reconcile
// loop (CRI-133). Terminal runs are excluded; their final events remain
// pollable via SubscribeRunEvents.
func (s *OrchestratorServer) ListActiveRuns(ctx context.Context, req *connect.Request[pb.ListActiveRunsRequest]) (*connect.Response[pb.ListActiveRunsResponse], error) {
	all, err := s.Store.ListRuns(ctx, "", "")
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	out := make([]*pb.Run, 0, len(all))
	for _, r := range all {
		if isTerminalRunStatus(r.Status) {
			continue
		}
		out = append(out, mapRun(r))
	}
	return connect.NewResponse(&pb.ListActiveRunsResponse{Runs: out}), nil
}
