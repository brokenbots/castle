package rpc

import (
	"context"
	"errors"
	"time"

	"connectrpc.com/connect"
	"google.golang.org/protobuf/types/known/timestamppb"

	pb "github.com/brokenbots/castle/shared/pb/overlord/v1" // import-lint:allow castle service bindings (W08: move to castle-proto)
	overseer "github.com/brokenbots/castle/shared/sdk/overseer"
)

// Resume delivers a named signal (or an approval decision) to a paused run.
// The authenticated caller must own the run (enforced via requireCallerOwnsRun).
// When the interceptor is not wired (direct-call tests) the ownership check is
// skipped so existing positive-path tests continue to work unchanged.
func (s *OverseerServer) Resume(ctx context.Context, req *connect.Request[pb.ResumeRequest]) (*connect.Response[pb.ResumeResponse], error) {
	if req.Msg.RunId == "" {
		return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("run_id required"))
	}
	if req.Msg.Signal == "" {
		return nil, connect.NewError(connect.CodeInvalidArgument, errors.New("signal required"))
	}

	_, run, err := requireCallerOwnsRun(ctx, s.Store, req.Msg.RunId)
	if err != nil {
		return nil, err
	}

	if run.Status != "paused" {
		return connect.NewResponse(&pb.ResumeResponse{
			Accepted: false,
			Reason:   "run_not_paused",
		}), nil
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
		resumeEnv = overseer.NewEnvelope(run.ID, &pb.ApprovalDecision{
			Node:     req.Msg.Signal,
			Decision: decision,
			Actor:    actor,
			Payload:  req.Msg.Payload,
		})
	} else {
		resumeEnv = overseer.NewEnvelope(run.ID, &pb.WaitResumed{
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
