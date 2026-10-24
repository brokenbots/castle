package rpc

import (
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/brokenbots/castle/castle/internal/auth"
	"github.com/brokenbots/castle/castle/internal/store"
	castlev1 "github.com/brokenbots/castle/castle/gen/castle/v1"
	pb "github.com/brokenbots/criteria/sdk/pb/criteria/v1"
)

func mapAgent(o *store.Overseer) *pb.Agent {
	if o == nil {
		return nil
	}
	labels := make(map[string]string, len(o.Labels)+2)
	for k, v := range o.Labels {
		labels[k] = v
	}
	if o.Hostname != "" {
		labels["hostname"] = o.Hostname
	}
	if o.Version != "" {
		labels["version"] = o.Version
	}
	// KB-318 identity echo: ListAgents/GetAgent expose the persisted identity
	// block under the reserved "identity" label (canonical JSON from
	// store.IdentityJSON). This is a wire-layer projection only — the agent's
	// own stored labels are never mutated, so identity must not leak into
	// eligibility matching. A nil or zero block echoes nothing, keeping
	// pre-KB-318 agents unchanged; a marshal anomaly (unreachable for
	// validated blocks) skips the echo rather than serving malformed JSON.
	if block, err := store.IdentityJSON(o.Identity); err == nil && block != "" {
		labels[auth.IdentityEchoLabel] = block
	}
	return &pb.Agent{
		CriteriaId:   o.ID,
		Name:         o.Name,
		Labels:       labels,
		Status:       o.Status,
		RegisteredAt: timestamppb.New(o.CreatedAt),
		LastSeenAt:   timestamppb.New(o.LastSeenAt),
	}
}

func mapRun(r *store.Run) *pb.Run {
	if r == nil {
		return nil
	}
	out := &pb.Run{
		RunId:         r.ID,
		CriteriaId:    r.OverseerID,
		WorkflowName:  r.WorkflowName,
		WorkflowHash:  r.WorkflowHCL,
		Status:        r.Status,
		CreatedAt:     timestamppb.New(r.CreatedAt),
		FailureReason: r.FailureReason,
	}
	if r.EndedAt != nil {
		out.EndedAt = timestamppb.New(*r.EndedAt)
	}
	if r.StartedAt != nil {
		out.StartedAt = timestamppb.New(*r.StartedAt)
	}
	return out
}

// mapConsoleSession projects a session ledger row (KB-325) onto the wire:
// the credential class fixed to the console-session kind (the ledger lists
// login sessions), the issue-time membership snapshot, and the stamps. The
// snapshot is display metadata only — live member rights always resolve
// through the shared membership set.
func mapConsoleSession(sess *store.ConsoleSession) *castlev1.SessionInfo {
	if sess == nil {
		return nil
	}
	info := &castlev1.SessionInfo{
		SessionId:  sess.ID,
		Kind:       castlev1.SessionKind_SESSION_KIND_CONSOLE,
		Membership: sess.Membership,
		CreatedAt:  timestamppb.New(sess.CreatedAt),
	}
	if sess.RevokedAt != nil {
		info.RevokedAt = timestamppb.New(*sess.RevokedAt)
	}
	return info
}
