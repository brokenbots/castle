// Package hub fans out events to subscribers (Castle WatchRun clients) per
// run. Subscribers receive only events that arrive AFTER they subscribe; for
// full history they should call ListRunEvents with `since_seq` to backfill.
package hub

import (
	"sync"

	pb "github.com/brokenbots/overlord/shared/pb/overlord/v1"
)

type Hub struct {
	mu   sync.RWMutex
	subs map[string]map[*Subscriber]struct{} // runID -> subscribers
}

type Subscriber struct {
	C      chan *pb.Envelope
	hub    *Hub
	runIDs []string

	// mu guards closed and the non-blocking send in Publish. All sends to C
	// happen under mu so Unsubscribe's close is mutually exclusive with any
	// in-flight send; this prevents the `send on closed channel` race when
	// Publish evicts a slow subscriber concurrently with a handler's
	// deferred Unsubscribe.
	mu     sync.Mutex
	closed bool
}

func New() *Hub { return &Hub{subs: make(map[string]map[*Subscriber]struct{})} }

// Subscribe to one or more runs. "*" subscribes to every run.
func (h *Hub) Subscribe(runIDs ...string) *Subscriber {
	s := &Subscriber{C: make(chan *pb.Envelope, 64), hub: h, runIDs: runIDs}
	h.mu.Lock()
	defer h.mu.Unlock()
	for _, id := range runIDs {
		if _, ok := h.subs[id]; !ok {
			h.subs[id] = make(map[*Subscriber]struct{})
		}
		h.subs[id][s] = struct{}{}
	}
	return s
}

// Unsubscribe removes s from the hub and closes its channel. It is
// idempotent: concurrent callers (e.g. Publish's slow-subscriber eviction
// and the handler's deferred cleanup) are safe.
func (h *Hub) Unsubscribe(s *Subscriber) {
	if s == nil {
		return
	}
	h.removeFromIndex(s)
	s.mu.Lock()
	if !s.closed {
		s.closed = true
		close(s.C)
	}
	s.mu.Unlock()
}

// removeFromIndex drops s from the run->subscriber map. It does not touch
// s.C; Unsubscribe / the slow-subscriber eviction path handle channel close
// under s.mu.
func (h *Hub) removeFromIndex(s *Subscriber) {
	h.mu.Lock()
	defer h.mu.Unlock()
	for _, id := range s.runIDs {
		if set, ok := h.subs[id]; ok {
			delete(set, s)
			if len(set) == 0 {
				delete(h.subs, id)
			}
		}
	}
}

// Publish delivers env to all subscribers of env.RunId and to wildcard "*".
// Slow subscribers (buffer full) are dropped and their channel is closed.
func (h *Hub) Publish(env *pb.Envelope) {
	if env == nil {
		return
	}
	h.mu.RLock()
	targets := make([]*Subscriber, 0, 8)
	if set, ok := h.subs[env.RunId]; ok {
		for s := range set {
			targets = append(targets, s)
		}
	}
	if set, ok := h.subs["*"]; ok {
		for s := range set {
			targets = append(targets, s)
		}
	}
	h.mu.RUnlock()

	var toEvict []*Subscriber
	for _, s := range targets {
		s.mu.Lock()
		if s.closed {
			s.mu.Unlock()
			continue
		}
		select {
		case s.C <- env:
			s.mu.Unlock()
		default:
			// Subscriber too slow; close the channel while still under
			// mu so no other goroutine can send after the close, then
			// schedule removal from the hub index outside of s.mu to
			// avoid lock-order inversion (h.mu > s.mu).
			s.closed = true
			close(s.C)
			s.mu.Unlock()
			toEvict = append(toEvict, s)
		}
	}
	for _, s := range toEvict {
		h.removeFromIndex(s)
	}
}
