package rpc

import (
	"errors"
	"log/slog"
	"sync"

	"github.com/brokenbots/overlord/castle/internal/hub"
	"github.com/brokenbots/overlord/castle/internal/store"
	pb "github.com/brokenbots/overlord/shared/pb/overlord/v1"
)

const controlBufferSize = 32

// ErrOverseerNotConnected is returned when no Control subscriber exists for
// the target overseer.
var ErrOverseerNotConnected = errors.New("overseer not connected")

// ErrControlBacklogFull is returned when the Control channel is full and a
// new message cannot be enqueued without blocking.
var ErrControlBacklogFull = errors.New("control backlog full")

type ControlRegistry struct {
	mu    sync.RWMutex
	conns map[string]chan *pb.ControlMessage
}

func NewControlRegistry() *ControlRegistry {
	return &ControlRegistry{conns: make(map[string]chan *pb.ControlMessage)}
}

// Register allocates a buffered channel for an overseer's Control stream. If
// a prior registration exists for the same overseer_id the previous channel
// is closed, evicting the old subscriber so the new one takes over.
func (r *ControlRegistry) Register(overseerID string) (chan *pb.ControlMessage, error) {
	if overseerID == "" {
		return nil, errors.New("overseer_id required")
	}
	ch := make(chan *pb.ControlMessage, controlBufferSize)
	r.mu.Lock()
	if old, ok := r.conns[overseerID]; ok {
		close(old)
	}
	r.conns[overseerID] = ch
	r.mu.Unlock()
	return ch, nil
}

func (r *ControlRegistry) Unregister(overseerID string, ch chan *pb.ControlMessage) {
	r.mu.Lock()
	defer r.mu.Unlock()
	curr, ok := r.conns[overseerID]
	if !ok {
		return
	}
	if ch != nil && ch != curr {
		return
	}
	delete(r.conns, overseerID)
	close(curr)
}

// Enqueue attempts a non-blocking send of msg to the overseer's Control
// channel. Returns ErrOverseerNotConnected if no subscriber is registered
// and ErrControlBacklogFull if the subscriber's buffer is saturated.
func (r *ControlRegistry) Enqueue(overseerID string, msg *pb.ControlMessage) error {
	r.mu.RLock()
	ch, ok := r.conns[overseerID]
	r.mu.RUnlock()
	if !ok {
		return ErrOverseerNotConnected
	}
	select {
	case ch <- msg:
		return nil
	default:
		return ErrControlBacklogFull
	}
}

type OverseerServer struct {
	Store    store.Store
	Hub      *hub.Hub
	Log      *slog.Logger
	controls *ControlRegistry
}

type CastleServer struct {
	Store    store.Store
	Hub      *hub.Hub
	Log      *slog.Logger
	controls *ControlRegistry
}

func NewOverseerServer(st store.Store, h *hub.Hub, log *slog.Logger, controls *ControlRegistry) *OverseerServer {
	if controls == nil {
		controls = NewControlRegistry()
	}
	if log == nil {
		log = slog.Default()
	}
	return &OverseerServer{Store: st, Hub: h, Log: log, controls: controls}
}

func NewCastleServer(st store.Store, h *hub.Hub, log *slog.Logger, controls *ControlRegistry) *CastleServer {
	if controls == nil {
		controls = NewControlRegistry()
	}
	if log == nil {
		log = slog.Default()
	}
	return &CastleServer{Store: st, Hub: h, Log: log, controls: controls}
}
