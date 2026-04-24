package rpc

import (
	"fmt"
	"time"

	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/brokenbots/overlord/castle/internal/store"
	"github.com/brokenbots/overlord/shared/events"
	pb "github.com/brokenbots/overlord/shared/pb/overlord/v1"
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

func toStoreEnvelope(env *pb.Envelope) (events.Envelope, error) {
	if env == nil {
		return events.Envelope{}, fmt.Errorf("nil envelope")
	}
	if env.RunId == "" {
		return events.Envelope{}, fmt.Errorf("run_id required")
	}

	typ, payload, err := payloadFromProto(env)
	if err != nil {
		return events.Envelope{}, err
	}

	ts := time.Time{}
	if env.Ts != nil {
		ts = env.Ts.AsTime().UTC()
	}

	return events.Envelope{
		SchemaVersion: int(env.SchemaVersion),
		RunID:         env.RunId,
		Seq:           env.Seq,
		Type:          typ,
		Timestamp:     ts,
		CorrelationID: env.CorrelationId,
		Payload:       payload,
	}, nil
}

func toProtoEnvelope(env events.Envelope) (*pb.Envelope, error) {
	p := &pb.Envelope{
		SchemaVersion: int32(env.SchemaVersion),
		RunId:         env.RunID,
		Seq:           env.Seq,
		Ts:            timestamppb.New(env.Timestamp.UTC()),
		CorrelationId: env.CorrelationID,
	}
	if p.SchemaVersion == 0 {
		p.SchemaVersion = int32(events.SchemaVersion)
	}
	if err := setProtoPayload(p, env.Type, env.Payload); err != nil {
		return nil, err
	}
	return p, nil
}

func payloadFromProto(env *pb.Envelope) (events.Type, []byte, error) {
	switch p := env.Payload.(type) {
	case *pb.Envelope_RunStarted:
		b, err := protojson.Marshal(p.RunStarted)
		return events.TypeRunStarted, b, err
	case *pb.Envelope_RunCompleted:
		b, err := protojson.Marshal(p.RunCompleted)
		return events.TypeRunCompleted, b, err
	case *pb.Envelope_RunFailed:
		b, err := protojson.Marshal(p.RunFailed)
		return events.TypeRunFailed, b, err
	case *pb.Envelope_StepEntered:
		b, err := protojson.Marshal(p.StepEntered)
		return events.TypeStepEntered, b, err
	case *pb.Envelope_StepOutcome:
		b, err := protojson.Marshal(p.StepOutcome)
		return events.TypeStepOutcome, b, err
	case *pb.Envelope_StepTransition:
		b, err := protojson.Marshal(p.StepTransition)
		return events.TypeStepTransition, b, err
	case *pb.Envelope_StepLog:
		b, err := protojson.Marshal(p.StepLog)
		return events.TypeStepLog, b, err
	case *pb.Envelope_AdapterEvent:
		b, err := protojson.Marshal(p.AdapterEvent)
		return events.TypeAdapterEvent, b, err
	case *pb.Envelope_OverseerHeartbeat:
		b, err := protojson.Marshal(p.OverseerHeartbeat)
		return events.TypeOverseerHeartbeat, b, err
	case *pb.Envelope_OverseerDisconnected:
		b, err := protojson.Marshal(p.OverseerDisconnected)
		return events.TypeOverseerDisconnected, b, err
	default:
		return "", nil, fmt.Errorf("unsupported payload")
	}
}

func setProtoPayload(out *pb.Envelope, typ events.Type, payload []byte) error {
	unmarshal := func(msg proto.Message) error {
		if len(payload) == 0 {
			payload = []byte("{}")
		}
		return protojson.Unmarshal(payload, msg)
	}

	switch typ {
	case events.TypeRunStarted:
		msg := &pb.RunStarted{}
		if err := unmarshal(msg); err != nil {
			return err
		}
		out.Payload = &pb.Envelope_RunStarted{RunStarted: msg}
	case events.TypeRunCompleted:
		msg := &pb.RunCompleted{}
		if err := unmarshal(msg); err != nil {
			return err
		}
		out.Payload = &pb.Envelope_RunCompleted{RunCompleted: msg}
	case events.TypeRunFailed:
		msg := &pb.RunFailed{}
		if err := unmarshal(msg); err != nil {
			return err
		}
		out.Payload = &pb.Envelope_RunFailed{RunFailed: msg}
	case events.TypeStepEntered:
		msg := &pb.StepEntered{}
		if err := unmarshal(msg); err != nil {
			return err
		}
		out.Payload = &pb.Envelope_StepEntered{StepEntered: msg}
	case events.TypeStepOutcome:
		msg := &pb.StepOutcome{}
		if err := unmarshal(msg); err != nil {
			return err
		}
		out.Payload = &pb.Envelope_StepOutcome{StepOutcome: msg}
	case events.TypeStepTransition:
		msg := &pb.StepTransition{}
		if err := unmarshal(msg); err != nil {
			return err
		}
		out.Payload = &pb.Envelope_StepTransition{StepTransition: msg}
	case events.TypeStepLog:
		msg := &pb.StepLog{}
		if err := unmarshal(msg); err != nil {
			return err
		}
		out.Payload = &pb.Envelope_StepLog{StepLog: msg}
	case events.TypeAdapterEvent:
		msg := &pb.AdapterEvent{}
		if err := unmarshal(msg); err != nil {
			return err
		}
		out.Payload = &pb.Envelope_AdapterEvent{AdapterEvent: msg}
	case events.TypeOverseerHeartbeat:
		msg := &pb.OverseerHeartbeat{}
		if err := unmarshal(msg); err != nil {
			return err
		}
		out.Payload = &pb.Envelope_OverseerHeartbeat{OverseerHeartbeat: msg}
	case events.TypeOverseerDisconnected:
		msg := &pb.OverseerDisconnected{}
		if err := unmarshal(msg); err != nil {
			return err
		}
		out.Payload = &pb.Envelope_OverseerDisconnected{OverseerDisconnected: msg}
	default:
		return fmt.Errorf("unsupported event type %q", typ)
	}
	return nil
}
