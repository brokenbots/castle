package reap

import (
	"encoding/json"

	"github.com/brokenbots/castle/castle/internal/store"
)

// Human-gate verdict reasons (KB-226): the reclassification reasons the
// skipped-run log line and the RunReapOutcome carry for parked candidates.
const (
	AwaitingHumanGateVerdictReason = "engine reached terminal human gate awaiting_human"
	ApprovalGateVerdictReason      = "engine parked at human approval gate"
	HumanPauseGateVerdictReason    = "engine parked at human pause gate"
	SignalWaitGateVerdictReason    = "engine parked at human signal wait"
)

// Last-event type names the human-gate classifier keys on. The strings mirror
// the criteria SDK event types (Envelope_RunCompleted -> "run.completed" and
// friends); the store layer stays independent of the generated protobuf types,
// so they are matched by name here.
const (
	evTypeRunCompleted      = "run.completed"
	evTypeApprovalRequested = "approval.requested"
	evTypeRunPaused         = "run.paused"
	evTypeWaitEntered       = "wait.entered"
)

// needAssistanceFinalStates are the engine-side FinalState values a
// run.completed carries when the workflow parked at a human gate (KB-270).
// The engine renamed its awaiting_human label to need_assistance (card 269);
// tree pins move independently of castle's release, so during the transition
// window either spelling can land in the event log and both classify
// identically. This is the engine-side label only: the run STATUS castle
// stamps stays RunStatusAwaitingHuman — that name is castle's own vocabulary
// (Parapet consumers, reaper exclusion, run watchers).
var needAssistanceFinalStates = map[string]struct{}{
	store.RunStatusAwaitingHuman: {},
	"need_assistance":            {},
}

// humanGateEventPayload carries the fields the human-gate classifier may need
// from a last event's payload. FinalState accepts both JSON spellings:
// engine-originated payloads are protojson (finalState), while older rows may
// carry snake_case.
type humanGateEventPayload struct {
	FinalState      string `json:"final_state"`
	FinalStateCamel string `json:"finalState"`
	Mode            string `json:"mode"`
	Signal          string `json:"signal"`
	Actor           string `json:"actor"`
}

// LastEventParksHumanGate classifies a run's most recent persisted event for
// the KB-226 reaper verdict: some events mean the engine reached a human gate
// — the awaiting_human (or, post-rename, need_assistance — KB-270) terminal
// completion, a human approval request, a human pause or a signal-mode wait —
// so the run is parked, not failed, however old its heartbeat is. Returns the
// reclassification reason when the event parks the run.
//
// An event whose verdict depends on a payload field that is missing or
// undecodable never parks: the conservative choice keeps the legacy CRI-142
// verdict ("agent heartbeat lost") for rows this classifier cannot read. An
// approval request is the exception — its type alone carries the verdict.
func LastEventParksHumanGate(evType string, payload []byte) (bool, string) {
	switch evType {
	case evTypeRunCompleted:
		var p humanGateEventPayload
		if json.Unmarshal(payload, &p) != nil {
			return false, ""
		}
		if _, ok := needAssistanceFinalStates[store.FirstNonEmpty(p.FinalState, p.FinalStateCamel)]; ok {
			return true, AwaitingHumanGateVerdictReason
		}
		return false, ""
	case evTypeApprovalRequested:
		return true, ApprovalGateVerdictReason
	case evTypeRunPaused:
		var p humanGateEventPayload
		if json.Unmarshal(payload, &p) != nil {
			return false, ""
		}
		// A duration pause at a wait node resumes on its own — a dead agent
		// waiting out a timer is genuinely dead, so it stays reapable. Signal
		// and external pauses park for a human or a control surface.
		if p.Mode == "signal" || p.Mode == "external" || p.Actor != "" {
			return true, HumanPauseGateVerdictReason
		}
		return false, ""
	case evTypeWaitEntered:
		var p humanGateEventPayload
		if json.Unmarshal(payload, &p) != nil {
			return false, ""
		}
		// A duration wait resumes on its own; a signal wait parks until a
		// human fires the signal.
		if p.Mode == "signal" || p.Signal != "" {
			return true, SignalWaitGateVerdictReason
		}
		return false, ""
	}
	return false, ""
}
