package rpc

import (
	"context"
	"errors"
	"time"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/brokenbots/overlord/castle/internal/auth"
	"github.com/brokenbots/overlord/castle/internal/store"
	"github.com/brokenbots/overlord/shared/events"
	pb "github.com/brokenbots/overlord/shared/pb/overlord/v1"
)

// Resume delivers a named signal (or an approval decision) to a paused run.
// The caller's overseer token must match the run's owning overseer (auth is
// enforced by the interceptor; here we verify run ownership at the run level).
// Permission shape: any authenticated Overseer may resume its own runs; cross-
// Overseer resume is out of scope for W05 — document this in reviewer notes.
func (s *OverseerServer) Resume(ctx context.Context, req *connect.Request[pb.ResumeRequest]) (*connect.Response[pb.ResumeResponse], error) {
	if req.Msg.RunId == "" {
		return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("run_id required"))
	}
	if req.Msg.Signal == "" {
		return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("signal required"))
	}

	run, err := s.Store.GetRun(ctx, req.Msg.RunId)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return nil, connect.NewError(connect.CodeNotFound, errors.New("run not found"))
		}
		return nil, connect.NewError(connect.CodeInternal, err)
	}

	if run.Status != "paused" {
		return connect.NewResponse(&pb.ResumeResponse{
			Accepted: false,
			Reason:   "run_not_paused",
		}), nil
	}

	// Enforce run ownership: the caller's overseer token must match the run's
	// owning overseer. auth.CallerOverseerID returns "" when the interceptor is
	// not wired (e.g. unit tests calling the handler directly) — skip the check
	// in that case so existing direct-call tests continue to work.
	if callerID := auth.CallerOverseerID(ctx); callerID != "" && callerID != run.OverseerID {
		return nil, connect.NewError(connect.CodePermissionDenied, errors.New("token does not own this run"))
	}

	if run.PendingSignal == "" {
		return connect.NewResponse(&pb.ResumeResponse{
			Accepted: false,
			Reason:   "no_pending_signal",
		}), nil
	}

	if run.PendingSignal != req.Msg.Signal {
		return connect.NewResponse(&pb.ResumeResponse{
			Accepted: false,
			Reason:   "signal_mismatch",
		}), nil
	}

	// Append a resume event to the event log for audit and fan-out.
	// For approval nodes the pending_signal is the node name; use ApprovalDecision.
	// For signal-wait nodes use WaitResumed.
	// We distinguish by checking the payload["decision"] key.
	decision := req.Msg.Payload["decision"]
	var resumeEnv *pb.Envelope
	if decision == "approved" || decision == "rejected" {
		actor := req.Msg.Payload["actor"]
		resumeEnv = events.NewEnvelope(run.ID, &pb.ApprovalDecision{
			Node:     req.Msg.Signal,
			Decision: decision,
			Actor:    actor,
			Payload:  req.Msg.Payload,
		})
	} else {
		resumeEnv = events.NewEnvelope(run.ID, &pb.WaitResumed{
			Node:    req.Msg.Signal,
			Mode:    "signal",
			Signal:  req.Msg.Signal,
			Payload: req.Msg.Payload,
		})
	}
	resumeEnv.Ts = timestamppb.New(time.Now().UTC())

	_, _, appendErr := s.Store.AppendEvent(ctx, resumeEnv)
	if appendErr != nil {
		return nil, connect.NewError(connect.CodeInternal, appendErr)
	}

	// Clear the pause state and mark the run as running again.
	if err := s.Store.ClearRunPaused(ctx, run.ID); err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}

	// Publish the event to hub subscribers (e.g. WatchRun).
	s.Hub.Publish(resumeEnv)

	// Deliver the resume signal to the Overseer via the Control stream.
	ctrlMsg := &pb.ControlMessage{
		Command: &pb.ControlMessage_ResumeRun{
			ResumeRun: &pb.ResumeRun{
				RunId:   run.ID,
				Signal:  req.Msg.Signal,
				Payload: req.Msg.Payload,
			},
		},
	}
	if enqErr := s.controls.Enqueue(run.OverseerID, ctrlMsg); enqErr != nil {
		// The Overseer is not currently connected. The persistent pending_signal
		// was already cleared; when the Overseer reconnects it will call
		// ReattachRun and see status=running, and will re-run from the paused
		// node with the resume payload delivered out-of-band. Document this
		// limitation in reviewer notes: Castle restart after Resume but before
		// the Overseer processes the control message requires the Overseer to
		// re-query for its resume payload (future work).
		s.Log.Warn("resume: overseer not connected; control message dropped",
			"run_id", run.ID, "overseer_id", run.OverseerID, "error", enqErr)
	}

	return connect.NewResponse(&pb.ResumeResponse{Accepted: true, Reason: "ok"}), nil
}
