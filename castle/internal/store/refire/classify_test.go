package refire

import (
	"testing"
	"time"

	"github.com/brokenbots/castle/castle/internal/store"
	"github.com/brokenbots/castle/castle/internal/store/reap"
)

func classify(t *testing.T, p Policy, reason string) (Disposition, string) {
	t.Helper()
	return Classify(p, reason, nil)
}

func TestClassifyStallRefires(t *testing.T) {
	p := Policy{Transient: []string{"stall", "heartbeat", "deadline exceeded"}}
	for _, reason := range []string{
		"agent heartbeat lost",
		"provider stall: upstream 500",
		"deadline exceeded while calling deploy",
	} {
		got, _ := classify(t, p, reason)
		if got != DispositionRefire {
			t.Fatalf("classify(%q) = %s, want refire", reason, got)
		}
	}
}

// TestClassifyCreatedNeverStartedRequeues is the KB-343 failing-first
// acceptance: the dispatch-transport stamp requeues the SAME attempt even
// though the token sits in the transient vocabulary (policy rows keep
// their shape), and it never reaches the refire path — for ANY backoff,
// attempt budget included. Parks still beat the class (fail-safe
// direction), and a real stall reason keeps refiring.
func TestClassifyCreatedNeverStartedRequeues(t *testing.T) {
	p := Policy{
		MaxAttempts: 3,
		Backoff:     30 * time.Second,
		Transient:   []string{"stall", reap.ReasonCreatedNeverStarted, "heartbeat"},
	}
	got, reason := classify(t, p, reap.ReasonCreatedNeverStarted)
	if got != DispositionRequeue {
		t.Fatalf("classify(%q) = %s (%s), want requeue", reap.ReasonCreatedNeverStarted, got, reason)
	}
	if got, _ := classify(t, p, "created_never_started drifted: extra context"); got == DispositionRequeue {
		t.Fatal("substring match reached the dispatch class; must be exact")
	}
	// The class never falls through to refire: any transient vocabulary
	// match on the token is superseded by precedence.
	p2 := Policy{Transient: []string{"created_never_started"}, MaxAttempts: 0}
	if got, _ := classify(t, p2, reap.ReasonCreatedNeverStarted); got != DispositionRequeue {
		t.Fatalf("classify with empty budget = %s, want requeue (budget governs mints, not transport retries)", got)
	}
	// Parks beat the requeue class.
	p3 := Policy{Transient: []string{"stall"}, Parks: []string{reap.ReasonCreatedNeverStarted}}
	if got, _ := classify(t, p3, reap.ReasonCreatedNeverStarted); got != DispositionPark {
		t.Fatalf("classify with park vocabulary = %s, want park", got)
	}
	// A real stall still refires.
	if got, _ := classify(t, p, "provider stall: upstream 500"); got != DispositionRefire {
		t.Fatalf("stall classify = %s, want refire", got)
	}
}

func TestClassifyParkWinsOverRefire(t *testing.T) {
	// A designed park must beat any transient vocabulary: parks never refire,
	// so park is the fail-safe direction (KB-321).
	p := Policy{
		Transient: []string{"failed", "stall"},
		Parks:     []string{"need_help", "need_assistance"},
	}
	for _, reason := range []string{"need_help", "workflow ended in NEED_ASSISTANCE", "need_assistance"} {
		d, _ := classify(t, p, reason)
		if d != DispositionPark {
			t.Fatalf("classify(%q) = %s, want park", reason, d)
		}
	}
}

func TestClassifyGateCheckEvents(t *testing.T) {
	p := Policy{Transient: []string{"failed", "need_assistance"}, Parks: []string{store.RunStatusAwaitingHuman}}
	cases := []struct {
		name    string
		evType  string
		payload string
	}{
		{"approval gate", "approval.requested", `{"request_id":"r1","workflow_id":"w"}`},
		{"completed at human gate", "run.completed", `{"final_state":"awaiting_human"}`},
		{"completed camel spelling", "run.completed", `{"finalState":"awaiting_human"}`},
		{"human pause", "run.paused", `{"mode":"signal","signal":"need_help"}`},
		{"signal wait", "wait.entered", "{\"kind\":\"signal\",\"signal\":\"approve\"}"},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			d, reason := Classify(p, "agent heartbeat lost", &LastEvent{Type: c.evType, Payload: []byte(c.payload)})
			if d != DispositionPark {
				t.Fatalf("classify %s = %s/%s, want park", c.name, d, reason)
			}
		})
	}
}

func TestClassifyCosmeticWinsOverTransient(t *testing.T) {
	p := Policy{Transient: []string{"race", "failed"}, Cosmetics: []string{"emit race"}}
	d, reason := classify(t, p, "pod-state emit race during setup")
	if d != DispositionCosmetic {
		t.Fatalf("classify = %s/%s, want cosmetic", d, reason)
	}
}

func TestClassifyUnclassifiedNone(t *testing.T) {
	p := Policy{Transient: []string{"timeout"}}
	d, reason := classify(t, p, "compiler exploded")
	if d != DispositionNone {
		t.Fatalf("classify = %s/%s, want none", d, reason)
	}
}

func TestClassifyEventBeatsReason(t *testing.T) {
	// The run's own latest event carries the authoritative designed-park
	// verdict (KB-270): a completed event parked at the gate classifies as
	// park even when the failure reason would classify as transient.
	p := Policy{Transient: []string{"failed"}, Parks: []string{"need_help"}}
	d, _ := Classify(p, "run failed", &LastEvent{
		Type:    "run.completed",
		Payload: []byte(`{"final_state":"need_assistance"}`),
	})
	if d != DispositionPark {
		t.Fatalf("classify = %s, want park", d)
	}
}

func TestClassifyCompletedEventPayloadSpellings(t *testing.T) {
	p := Policy{Parks: []string{"need_help", store.RunStatusAwaitingHuman}}
	// Non-completed events never read the parks vocabulary via the event leg.
	d, _ := Classify(p, "", &LastEvent{Type: "run.failed", Payload: []byte(`{"final_state":"awaiting_human"}`)})
	if d != DispositionNone {
		t.Fatalf("non-completed event = %s, want none", d)
	}
	// Malformed payloads fall through the gate check to the reason.
	d, _ = Classify(p, "", &LastEvent{Type: "run.completed", Payload: []byte(`{broken`)})
	if d != DispositionNone {
		t.Fatalf("malformed payload = %s, want none", d)
	}
}
