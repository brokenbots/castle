package refire

import (
	"encoding/json"
	"strings"

	"github.com/brokenbots/castle/castle/internal/store"
	"github.com/brokenbots/castle/castle/internal/store/reap"
)

// Disposition is the policy engine's decision for one terminal-failed run
// family attempt (KB-321): refire / park / cosmetic / requeue / none,
// matching the store.RefireDisposition* constants.
type Disposition string

const (
	DispositionRefire   = store.RefireDispositionRefire
	DispositionPark     = store.RefireDispositionPark
	DispositionCosmetic = store.RefireDispositionCosmetic
	DispositionRequeue  = store.RefireDispositionRequeue
	DispositionNone     = store.RefireDispositionNone
)

// LastEvent is the shape Classify needs from a run's most recent persisted
// event: the event type name ("run.completed", "approval.requested", ...) and
// the raw JSON payload. Runs without persisted events (a run that never
// began executing — KB-233 rule 2) classify from the failure reason alone.
type LastEvent struct {
	Type    string
	Payload []byte
}

// Classify evaluates the classification precedence for one failed run
// (KB-321): designed parks first (the gate check — a run whose record ends at
// a human gate, or whose reason carries a parks spelling, is held for a human
// decision), then cosmetics, then transient stalls, then no action. A run
// that never ends is parked and a parked run never refires without a human
// ack, so parks are the highest-precedence verdict — the fail-safe direction.
func Classify(p Policy, failureReason string, ev *LastEvent) (Disposition, string) {
	if ev != nil {
		if parked, reason := reap.LastEventParksHumanGate(ev.Type, ev.Payload); parked {
			return DispositionPark, reason
		}
		if fs, ok := runCompletedFinalState(ev.Type, ev.Payload); ok {
			if needle, ok := matchAny(p.Parks, fs); ok {
				return DispositionPark, "parks vocabulary match: " + needle
			}
		}
	}
	if needle, ok := matchAny(p.Parks, failureReason); ok {
		return DispositionPark, "parks vocabulary match: " + needle
	}
	// KB-343: the dispatch-transport class — a run reaped by the KB-233
	// rule-2 stamp (created, but execution never began) is a dispatch-layer
	// condition, not a provider stall. Matching is EXACT: only the reaper's
	// own verdict reason enters this class, since the general vocabulary is
	// substring-matched and policy rows keep their shape
	// (created_never_started stays in the default Transient list — the row
	// data is unchanged, the classifier precedence takes it out of the
	// budget path). The verdict requeues the SAME attempt: the attempt
	// budget must only be consumed by real execution failures.
	if failureReason == reap.ReasonCreatedNeverStarted {
		return DispositionRequeue,
			"dispatch-layer reap: the run was created but execution never began (requeue, attempt budget untouched)"
	}
	if needle, ok := matchAny(p.Cosmetics, failureReason); ok {
		return DispositionCosmetic, "cosmetics vocabulary match: " + needle
	}
	if needle, ok := matchAny(p.Transient, failureReason); ok {
		return DispositionRefire, "transient vocabulary match: " + needle
	}
	return DispositionNone, "unclassified terminal failure"
}

// runCompletedFinalState returns (final state spelling, true) for a
// run.completed event payload: engine-originated payloads are protojson
// (finalState), older rows may carry snake_case (final_state).
func runCompletedFinalState(evType string, payload []byte) (string, bool) {
	if evType != "run.completed" {
		return "", false
	}
	var p struct {
		FinalState      string `json:"final_state"`
		FinalStateCamel string `json:"finalState"`
	}
	if json.Unmarshal(payload, &p) != nil {
		return "", false
	}
	if s := store.FirstNonEmpty(p.FinalState, p.FinalStateCamel); s != "" {
		return s, true
	}
	return "", false
}

// matchAny reports the first needle that occurs in the haystack
// (case-insensitive substring match, so vocabulary spellings stay lowercase
// in policy documents without matching brittleness).
func matchAny(needles []string, haystack string) (string, bool) {
	if len(needles) == 0 || haystack == "" {
		return "", false
	}
	h := strings.ToLower(haystack)
	for _, n := range needles {
		if n == "" {
			continue
		}
		if strings.Contains(h, strings.ToLower(n)) {
			return n, true
		}
	}
	return "", false
}
