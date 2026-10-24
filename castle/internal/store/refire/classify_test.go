package refire

import (
	"testing"

	"github.com/brokenbots/castle/castle/internal/store"
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
