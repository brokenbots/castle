package rpc

import (
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/brokenbots/overlord/castle/internal/store"
	pb "github.com/brokenbots/overlord/shared/pb/overlord/v1" // import-lint:allow castle service bindings (W08: move to castle-proto)
)

func mapOverseer(o *store.Overseer) *pb.Overseer {
	if o == nil {
		return nil
	}
	return &pb.Overseer{
		OverseerId:   o.ID,
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
		OverseerId:   r.OverseerID,
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
