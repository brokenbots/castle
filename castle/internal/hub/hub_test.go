package hub

import (
	"sync"
	"testing"

	pb "github.com/brokenbots/castle/shared/sdk/overseer"
)

// TestUnsubscribeIdempotent ensures the channel is closed at most once, so a
// slow-subscriber eviction from Publish followed by a handler's deferred
// Unsubscribe cannot panic on double-close.
func TestUnsubscribeIdempotent(t *testing.T) {
	h := New()
	sub := h.Subscribe("r1")

	// Fill the buffer (cap 64) with messages so the 65th Publish drops the
	// subscriber and closes its channel.
	for i := 0; i < cap(sub.C); i++ {
		h.Publish(&pb.Envelope{RunId: "r1"})
	}
	// This publish finds the buffer full and evicts the subscriber.
	h.Publish(&pb.Envelope{RunId: "r1"})

	// Handler cleanup path: must not panic even though the channel is
	// already closed.
	h.Unsubscribe(sub)

	// Extra cleanup call is also safe (e.g. if a future refactor adds one).
	h.Unsubscribe(sub)
}

// TestUnsubscribeRaceSlowSubscriber runs concurrent Publish and Unsubscribe
// to smoke-test the close-once guard under -race.
func TestUnsubscribeRaceSlowSubscriber(t *testing.T) {
	h := New()
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		sub := h.Subscribe("r1")
		wg.Add(2)
		go func() {
			defer wg.Done()
			for j := 0; j < 200; j++ {
				h.Publish(&pb.Envelope{RunId: "r1"})
			}
		}()
		go func(s *Subscriber) {
			defer wg.Done()
			h.Unsubscribe(s)
		}(sub)
	}
	wg.Wait()
}

// TestUnsubscribeNilSafe guards against nil subscriber crashes.
func TestUnsubscribeNilSafe(t *testing.T) {
	h := New()
	h.Unsubscribe(nil)
}

func TestPublish_TerminalForgetsRunBuffer(t *testing.T) {
	h := NewWithBuffer(8, nil)

	h.Publish(&pb.Envelope{RunId: "r1", Seq: 1, Payload: &pb.Envelope_StepEntered{StepEntered: &pb.StepEntered{Step: "s", Adapter: "shell", Attempt: 1}}})
	if got := len(h.Since("r1", 0)); got != 1 {
		t.Fatalf("before terminal, buffered=%d want 1", got)
	}

	terminal := pb.NewEnvelope("r1", &pb.RunCompleted{Success: true})
	terminal.Seq = 2
	h.Publish(terminal)

	if got := len(h.Since("r1", 0)); got != 0 {
		t.Fatalf("after terminal, buffered=%d want 0", got)
	}
}
