package rpc

import (
	"encoding/json"
	"fmt"
	"time"

	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/brokenbots/castle/castle/internal/store"
	criteria "github.com/brokenbots/criteria/sdk"
	pb "github.com/brokenbots/criteria/sdk/pb/criteria/v1"
)

// eventToEnvelope converts a storage-neutral store.Event into a criteria.v1
// Envelope. The payload blob is unmarshaled using protojson according to the
// event's discriminator type. This keeps all protobuf-generated imports in the
// RPC layer and out of storage internals.
func eventToEnvelope(ev *store.Event) (*criteria.Envelope, error) {
	if ev == nil {
		return nil, nil
	}
	env := &criteria.Envelope{
		SchemaVersion: ev.SchemaVersion,
		RunId:         ev.RunID,
		Seq:           ev.Seq,
		CorrelationId: ev.CorrelationID,
	}
	if !ev.Ts.IsZero() {
		env.Ts = timestamppb.New(ev.Ts)
	}
	if len(ev.Payload) == 0 {
		return env, nil
	}
	msg, err := newPayloadForType(ev.Type)
	if err != nil {
		return nil, err
	}
	u := protojson.UnmarshalOptions{DiscardUnknown: true}
	if err := u.Unmarshal(ev.Payload, msg); err != nil {
		return nil, fmt.Errorf("unmarshal %s payload: %w", ev.Type, err)
	}
	setPayload(env, msg)
	return env, nil
}

// envelopeToEvent converts a criteria.v1 Envelope into a store.Event ready for
// persistence. The concrete payload is marshaled to protojson and stored in
// Payload; the discriminator is derived from the envelope's oneof arm.
func envelopeToEvent(env *criteria.Envelope) (*store.Event, error) {
	if env == nil {
		return nil, nil
	}
	ev := &store.Event{
		SchemaVersion: env.SchemaVersion,
		RunID:         env.RunId,
		Seq:           env.Seq,
		Type:          criteria.TypeString(env),
		CorrelationID: env.CorrelationId,
	}
	if ev.Type == "" {
		return nil, fmt.Errorf("unknown or missing payload arm")
	}
	if env.Ts != nil {
		ev.Ts = env.Ts.AsTime()
	} else {
		ev.Ts = time.Now().UTC()
	}
	if env.Payload != nil {
		msg := payloadMessage(env)
		if msg == nil {
			return nil, fmt.Errorf("marshal: unknown payload arm %T", env.Payload)
		}
		b, err := protojson.Marshal(msg)
		if err != nil {
			return nil, fmt.Errorf("marshal %s payload: %w", ev.Type, err)
		}
		ev.Payload = b
	}
	return ev, nil
}

func newPayloadForType(typ string) (proto.Message, error) {
	switch typ {
	case "run.started":
		return &pb.RunStarted{}, nil
	case "run.completed":
		return &pb.RunCompleted{}, nil
	case "run.failed":
		return &pb.RunFailed{}, nil
	case "step.entered":
		return &pb.StepEntered{}, nil
	case "step.outcome":
		return &pb.StepOutcome{}, nil
	case "step.transition":
		return &pb.StepTransition{}, nil
	case "step.log":
		return &pb.StepLog{}, nil
	case "adapter.event":
		return &pb.AdapterEvent{}, nil
	case "criteria.heartbeat":
		return &pb.CriteriaHeartbeat{}, nil
	case "criteria.disconnected":
		return &pb.CriteriaDisconnected{}, nil
	case "step.resumed":
		return &pb.StepResumed{}, nil
	case "watch.ready":
		return &pb.WatchReady{}, nil
	case "variable.set":
		return &pb.VariableSet{}, nil
	case "step.output_captured":
		return &pb.StepOutputCaptured{}, nil
	case "wait.entered":
		return &pb.WaitEntered{}, nil
	case "wait.resumed":
		return &pb.WaitResumed{}, nil
	case "approval.requested":
		return &pb.ApprovalRequested{}, nil
	case "approval.decision":
		return &pb.ApprovalDecision{}, nil
	case "branch.evaluated":
		return &pb.BranchEvaluated{}, nil
	case "for_each.entered":
		return &pb.ForEachEntered{}, nil
	case "step.iteration_started":
		return &pb.StepIterationStarted{}, nil
	case "step.iteration_completed":
		return &pb.StepIterationCompleted{}, nil
	case "step.iteration_item":
		return &pb.StepIterationItem{}, nil
	case "scope.iter_cursor_set":
		return &pb.ScopeIterCursorSet{}, nil
	case "run.outputs":
		return &pb.RunOutputs{}, nil
	default:
		return nil, fmt.Errorf("unknown event type %q", typ)
	}
}

func payloadMessage(env *criteria.Envelope) proto.Message {
	switch p := env.Payload.(type) {
	case *criteria.Envelope_RunStarted:
		return p.RunStarted
	case *criteria.Envelope_RunCompleted:
		return p.RunCompleted
	case *criteria.Envelope_RunFailed:
		return p.RunFailed
	case *criteria.Envelope_StepEntered:
		return p.StepEntered
	case *criteria.Envelope_StepOutcome:
		return p.StepOutcome
	case *criteria.Envelope_StepTransition:
		return p.StepTransition
	case *criteria.Envelope_StepLog:
		return p.StepLog
	case *criteria.Envelope_AdapterEvent:
		return p.AdapterEvent
	case *criteria.Envelope_CriteriaHeartbeat:
		return p.CriteriaHeartbeat
	case *criteria.Envelope_CriteriaDisconnected:
		return p.CriteriaDisconnected
	case *criteria.Envelope_StepResumed:
		return p.StepResumed
	case *criteria.Envelope_WatchReady:
		return p.WatchReady
	case *criteria.Envelope_VariableSet:
		return p.VariableSet
	case *criteria.Envelope_StepOutputCaptured:
		return p.StepOutputCaptured
	case *criteria.Envelope_WaitEntered:
		return p.WaitEntered
	case *criteria.Envelope_WaitResumed:
		return p.WaitResumed
	case *criteria.Envelope_ApprovalRequested:
		return p.ApprovalRequested
	case *criteria.Envelope_ApprovalDecision:
		return p.ApprovalDecision
	case *criteria.Envelope_BranchEvaluated:
		return p.BranchEvaluated
	case *criteria.Envelope_ForEachEntered:
		return p.ForEachEntered
	case *criteria.Envelope_StepIterationStarted:
		return p.StepIterationStarted
	case *criteria.Envelope_StepIterationCompleted:
		return p.StepIterationCompleted
	case *criteria.Envelope_ScopeIterCursorSet:
		return p.ScopeIterCursorSet
	case *criteria.Envelope_StepIterationItem:
		return p.StepIterationItem
	case *pb.Envelope_RunOutputs:
		return p.RunOutputs
	default:
		return nil
	}
}

func setPayload(env *criteria.Envelope, msg proto.Message) {
	switch p := msg.(type) {
	case *pb.RunStarted:
		env.Payload = &criteria.Envelope_RunStarted{RunStarted: p}
	case *pb.RunCompleted:
		env.Payload = &criteria.Envelope_RunCompleted{RunCompleted: p}
	case *pb.RunFailed:
		env.Payload = &criteria.Envelope_RunFailed{RunFailed: p}
	case *pb.StepEntered:
		env.Payload = &criteria.Envelope_StepEntered{StepEntered: p}
	case *pb.StepOutcome:
		env.Payload = &criteria.Envelope_StepOutcome{StepOutcome: p}
	case *pb.StepTransition:
		env.Payload = &criteria.Envelope_StepTransition{StepTransition: p}
	case *pb.StepLog:
		env.Payload = &criteria.Envelope_StepLog{StepLog: p}
	case *pb.AdapterEvent:
		env.Payload = &criteria.Envelope_AdapterEvent{AdapterEvent: p}
	case *pb.CriteriaHeartbeat:
		env.Payload = &criteria.Envelope_CriteriaHeartbeat{CriteriaHeartbeat: p}
	case *pb.CriteriaDisconnected:
		env.Payload = &criteria.Envelope_CriteriaDisconnected{CriteriaDisconnected: p}
	case *pb.StepResumed:
		env.Payload = &criteria.Envelope_StepResumed{StepResumed: p}
	case *pb.WatchReady:
		env.Payload = &criteria.Envelope_WatchReady{WatchReady: p}
	case *pb.VariableSet:
		env.Payload = &criteria.Envelope_VariableSet{VariableSet: p}
	case *pb.StepOutputCaptured:
		env.Payload = &criteria.Envelope_StepOutputCaptured{StepOutputCaptured: p}
	case *pb.WaitEntered:
		env.Payload = &criteria.Envelope_WaitEntered{WaitEntered: p}
	case *pb.WaitResumed:
		env.Payload = &criteria.Envelope_WaitResumed{WaitResumed: p}
	case *pb.ApprovalRequested:
		env.Payload = &criteria.Envelope_ApprovalRequested{ApprovalRequested: p}
	case *pb.ApprovalDecision:
		env.Payload = &criteria.Envelope_ApprovalDecision{ApprovalDecision: p}
	case *pb.BranchEvaluated:
		env.Payload = &criteria.Envelope_BranchEvaluated{BranchEvaluated: p}
	case *pb.ForEachEntered:
		env.Payload = &criteria.Envelope_ForEachEntered{ForEachEntered: p}
	case *pb.StepIterationStarted:
		env.Payload = &criteria.Envelope_StepIterationStarted{StepIterationStarted: p}
	case *pb.StepIterationCompleted:
		env.Payload = &criteria.Envelope_StepIterationCompleted{StepIterationCompleted: p}
	case *pb.ScopeIterCursorSet:
		env.Payload = &criteria.Envelope_ScopeIterCursorSet{ScopeIterCursorSet: p}
	case *pb.StepIterationItem:
		env.Payload = &criteria.Envelope_StepIterationItem{StepIterationItem: p}
	case *pb.RunOutputs:
		env.Payload = &pb.Envelope_RunOutputs{RunOutputs: p}
	}
}

// envelopeJSON is a small shim used by pagination to expose a stable JSON
// representation of a criteria.Envelope. It embeds a JSON-encoded payload
// object under the payload key.
func envelopeJSON(env *criteria.Envelope) (map[string]any, error) {
	if env == nil {
		return nil, nil
	}
	type envelope struct {
		SchemaVersion int32             `json:"schema_version"`
		RunID         string            `json:"run_id"`
		Seq           uint64            `json:"seq"`
		Type          string            `json:"type"`
		Ts            time.Time         `json:"ts"`
		CorrelationID string            `json:"correlation_id,omitempty"`
		Payload       json.RawMessage   `json:"payload"`
		Meta          map[string]string `json:"meta,omitempty"`
	}
	out := envelope{
		SchemaVersion: env.SchemaVersion,
		RunID:         env.RunId,
		Seq:           env.Seq,
		Type:          criteria.TypeString(env),
		CorrelationID: env.CorrelationId,
	}
	if env.Ts != nil {
		out.Ts = env.Ts.AsTime()
	}
	if env.Payload != nil {
		msg := payloadMessage(env)
		if msg == nil {
			return nil, fmt.Errorf("envelopeJSON: unknown payload arm %T", env.Payload)
		}
		b, err := protojson.Marshal(msg)
		if err != nil {
			return nil, err
		}
		out.Payload = b
	}
	var result map[string]any
	b, err := json.Marshal(out)
	if err != nil {
		return nil, err
	}
	if err := json.Unmarshal(b, &result); err != nil {
		return nil, err
	}
	return result, nil
}
