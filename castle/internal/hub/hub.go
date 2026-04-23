// Package hub fans out events to subscribers (web clients) per run. Subscribers
// receive only events that arrive AFTER they subscribe; for full history they
// should call the REST events endpoint with `since=<seq>` to backfill.
package hub

import (
	"sync"

	"github.com/brokenbots/overlord/shared/events"
)

type Hub struct {
	mu   sync.RWMutex
	subs map[string]map[*Subscriber]struct{} // runID -> subscribers
}

type Subscriber struct {
	C      chan events.Envelope
	hub    *Hub
	runIDs []string
}

func New() *Hub { return &Hub{subs: make(map[string]map[*Subscriber]struct{})} }

// Subscribe to one or more runs. "*" subscribes to every run.
func (h *Hub) Subscribe(runIDs ...string) *Subscriber {
	s := &Subscriber{C: make(chan events.Envelope, 64), hub: h, runIDs: runIDs}
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

func (h *Hub) Unsubscribe(s *Subscriber) {
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
	close(s.C)
}

// Publish delivers env to all subscribers of env.RunID and to wildcard "*".
// Slow subscribers are dropped (their channel is closed and they are removed).
func (h *Hub) Publish(env events.Envelope) {
	h.mu.RLock()
	targets := make([]*Subscriber, 0, 8)
	if set, ok := h.subs[env.RunID]; ok {
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
	for _, s := range targets {
		select {
		case s.C <- env:
		default:
			// Subscriber too slow; drop it.
			h.Unsubscribe(s)
		}
	}
}
