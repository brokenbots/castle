package rpc

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"sync"
	"time"

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
	scope    *scopeCoalescer
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
	return &OverseerServer{Store: st, Hub: h, Log: log, scope: newScopeCoalescer(st, log), controls: controls}
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

// scopeCoalescer debounces variable-scope writes to SQLite. Each mutation is
// queued and a 250ms timer is started per run_id (reset on each new mutation
// within that window). When the timer fires all pending mutations are applied
// in a single read-modify-write. This mirrors the cursor-writer coalescing
// pattern and prevents hot-path SQLite writes on every VariableSet event.
type scopeCoalescer struct {
	mu      sync.Mutex
	pending map[string][]func(map[string]interface{})
	timers  map[string]*time.Timer
	store   store.Store
	log     *slog.Logger
}

const scopeFlushInterval = 250 * time.Millisecond

func newScopeCoalescer(st store.Store, log *slog.Logger) *scopeCoalescer {
	if log == nil {
		log = slog.Default()
	}
	return &scopeCoalescer{
		pending: make(map[string][]func(map[string]interface{})),
		timers:  make(map[string]*time.Timer),
		store:   st,
		log:     log,
	}
}

// Enqueue queues a scope mutation for runID. The flush is debounced by
// scopeFlushInterval so bursts of events produce a single DB write.
func (c *scopeCoalescer) Enqueue(runID string, mutate func(map[string]interface{})) {
	c.mu.Lock()
	c.pending[runID] = append(c.pending[runID], mutate)
	if t, ok := c.timers[runID]; ok {
		// Reset the existing timer rather than adding a second one.
		t.Reset(scopeFlushInterval)
	} else {
		c.timers[runID] = time.AfterFunc(scopeFlushInterval, func() {
			c.flush(runID)
		})
	}
	c.mu.Unlock()
}

// FlushNow forces an immediate flush for runID, bypassing the debounce delay.
// Used in tests and on graceful shutdown paths.
func (c *scopeCoalescer) FlushNow(ctx context.Context, runID string) {
	c.mu.Lock()
	if t, ok := c.timers[runID]; ok {
		t.Stop()
		delete(c.timers, runID)
	}
	mutations := c.pending[runID]
	delete(c.pending, runID)
	c.mu.Unlock()

	if len(mutations) == 0 {
		return
	}
	c.applyMutations(ctx, runID, mutations)
}

func (c *scopeCoalescer) flush(runID string) {
	c.mu.Lock()
	mutations := c.pending[runID]
	delete(c.pending, runID)
	delete(c.timers, runID)
	c.mu.Unlock()

	if len(mutations) == 0 {
		return
	}
	c.applyMutations(context.Background(), runID, mutations)
}

func (c *scopeCoalescer) applyMutations(ctx context.Context, runID string, mutations []func(map[string]interface{})) {
	existing, err := c.store.GetRunVariableScope(ctx, runID)
	if err != nil && !errors.Is(err, store.ErrNotFound) {
		c.log.Error("scope coalescer: read scope failed", "run_id", runID, "err", err)
		return
	}
	scope := map[string]interface{}{}
	if existing != "" {
		if jsonErr := json.Unmarshal([]byte(existing), &scope); jsonErr != nil {
			scope = map[string]interface{}{}
		}
	}
	for _, m := range mutations {
		m(scope)
	}
	b, err := json.Marshal(scope)
	if err != nil {
		c.log.Error("scope coalescer: marshal failed", "run_id", runID, "err", err)
		return
	}
	if err := c.store.SetRunVariableScope(ctx, runID, string(b)); err != nil {
		c.log.Error("scope coalescer: write scope failed", "run_id", runID, "err", err)
	}
}
