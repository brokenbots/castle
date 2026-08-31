package rpc

import (
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/brokenbots/castle/castle/internal/store"
	pb "github.com/brokenbots/criteria/sdk/pb/criteria/v1"
)

func mapAgent(o *store.Overseer) *pb.Agent {
	if o == nil {
		return nil
	}
	return &pb.Agent{
		CriteriaId:   o.ID,
		Name:         o.Name,
		Labels:       map[string]string{"hostname": o.Hostname, "version": o.Version},
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
		RunId:        r.ID,
		CriteriaId:   r.OverseerID,
		WorkflowName: r.WorkflowName,
		WorkflowHash: r.WorkflowHCL,
		Status:       r.Status,
		CreatedAt:    timestamppb.New(r.CreatedAt),
	}
	if r.EndedAt != nil {
		out.EndedAt = timestamppb.New(*r.EndedAt)
	}
	return out
}
