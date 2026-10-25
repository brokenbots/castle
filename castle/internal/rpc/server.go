package rpc

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"sort"
	"sync"
	"time"

	"github.com/brokenbots/castle/castle/internal/hub"
	"github.com/brokenbots/castle/castle/internal/store"
	pb "github.com/brokenbots/criteria/sdk/pb/criteria/v1"
)

const controlBufferSize = 32

// ErrAgentNotConnected is returned when no Control subscriber exists for
// the target criteria agent.
var ErrAgentNotConnected = errors.New("criteria agent not connected")

// ErrControlBacklogFull is returned when the Control channel is full and a
// new message cannot be enqueued without blocking.
var ErrControlBacklogFull = errors.New("control backlog full")

type ControlRegistry struct {
	mu      sync.RWMutex
	leaseMu sync.Mutex
	conns   map[string]chan *pb.ControlMessage
}

func NewControlRegistry() *ControlRegistry {
	return &ControlRegistry{conns: make(map[string]chan *pb.ControlMessage)}
}

// LeaseLock locks the global lease serialization mutex. Lease attempts are
// serialized across the registry to prevent two concurrent dispatchers from
// granting an agent two unstarted leases at once.
func (r *ControlRegistry) LeaseLock() {
	r.leaseMu.Lock()
}

// LeaseUnlock releases the global lease serialization mutex.
func (r *ControlRegistry) LeaseUnlock() {
	r.leaseMu.Unlock()
}

// Assignment delivery conventions (KB-318, workflow-generic): project/owner
// conventions are stored structured on the assignment row (input_values,
// target_metadata) and delivered to the executing agent by merging them into
// the control message's labels under reserved key prefixes:
//
//	criteria.input.<name>    <- assignment input_values
//	criteria.target.<key>    <- assignment target_metadata (env hints such as
//	                            adapter.<kind>.image; castle never interprets
//	                            target values)
//
// PROXY PRINCIPLE: the operator owns the run lifecycle. Castle dispatches
// assignments and records events, but run pods talk to the OPERATOR, never to
// castle — the control message merely carries the workflow and its
// conventions; everything the run needs must travel inside this message or
// live with the operator's own endpoints.
//
// Reserved keys overwrite an agent-provided label of the same name on the
// delivered message only; the stored labels row is untouched.
func mergeAssignmentConventions(labels map[string]string, a *store.WorkflowAssignment) map[string]string {
	out := make(map[string]string, len(labels)+len(a.InputValues)+len(a.TargetMetadata))
	for k, v := range labels {
		out[k] = v
	}
	for name, value := range a.InputValues {
		out["criteria.input."+name] = value
	}
	for key, value := range a.TargetMetadata {
		out["criteria.target."+key] = value
	}
	return out
}

// enqueueWorkflowAssignment pushes a WorkflowAssignment control message to the
// connected agent identified by criteriaID. It re-reads the assignment from
// the store immediately before sending so an assignment whose lease just
// expired is not delivered to a stale subscriber.
func enqueueWorkflowAssignment(st store.Store, controls *ControlRegistry, criteriaID string, a *store.WorkflowAssignment, log *slog.Logger) error {
	fresh, err := st.GetWorkflowAssignment(context.Background(), a.ID)
	if err != nil {
		return err
	}
	if fresh.State != store.WorkflowAssignmentStateLeased || fresh.LeasedCriteriaID != criteriaID {
		return errors.New("assignment no longer leased to this agent")
	}
	return controls.Enqueue(criteriaID, &pb.ControlMessage{Command: &pb.ControlMessage_WorkflowAssignment{WorkflowAssignment: &pb.WorkflowAssignment{
		RunId:          fresh.RunID,
		WorkflowName:   fresh.WorkflowName,
		WorkflowSource: fresh.WorkflowSource,
		LockfileSource: fresh.LockfileSource,
		Labels:         mergeAssignmentConventions(fresh.Labels, fresh),
	}}})
}

// Register allocates a buffered channel for a criteria agent's Control stream.
// If a prior registration exists for the same criteria_id the previous channel
// is closed, evicting the old subscriber so the new one takes over.
func (r *ControlRegistry) Register(criteriaID string) (chan *pb.ControlMessage, error) {
	if criteriaID == "" {
		return nil, errors.New("criteria_id required")
	}
	ch := make(chan *pb.ControlMessage, controlBufferSize)
	r.mu.Lock()
	if old, ok := r.conns[criteriaID]; ok {
		close(old)
	}
	r.conns[criteriaID] = ch
	r.mu.Unlock()
	return ch, nil
}

func (r *ControlRegistry) Unregister(criteriaID string, ch chan *pb.ControlMessage) {
	r.mu.Lock()
	defer r.mu.Unlock()
	curr, ok := r.conns[criteriaID]
	if !ok {
		return
	}
	if ch != nil && ch != curr {
		return
	}
	delete(r.conns, criteriaID)
	close(curr)
}

// Enqueue attempts a non-blocking send of msg to the criteria agent's Control
// channel. Returns ErrAgentNotConnected if no subscriber is registered
// and ErrControlBacklogFull if the subscriber's buffer is saturated.
func (r *ControlRegistry) Enqueue(criteriaID string, msg *pb.ControlMessage) error {
	r.mu.RLock()
	ch, ok := r.conns[criteriaID]
	r.mu.RUnlock()
	if !ok {
		return ErrAgentNotConnected
	}
	select {
	case ch <- msg:
		return nil
	default:
		return ErrControlBacklogFull
	}
}

// Registered returns a snapshot of currently registered criteria IDs. The order
// is deterministic (sorted) but not significant; callers must not assume the
// returned agents remain connected after the call returns.
func (r *ControlRegistry) Registered() []string {
	r.mu.RLock()
	defer r.mu.RUnlock()
	out := make([]string, 0, len(r.conns))
	for id := range r.conns {
		out = append(out, id)
	}
	sort.Strings(out)
	return out
}

// CriteriaServer implements pb.v1.CriteriaService (agent-facing RPCs).
type CriteriaServer struct {
	Store                   store.Store
	Hub                     *hub.Hub
	Log                     *slog.Logger
	scope                   *scopeCoalescer
	controls                *ControlRegistry
	assignmentLeaseDuration time.Duration
}

// ServerServer implements pb.v1.ServerService (UI/tool-facing RPCs).
type ServerServer struct {
	Store                   store.Store
	Hub                     *hub.Hub
	Log                     *slog.Logger
	controls                *ControlRegistry
	assignmentLeaseDuration time.Duration
	consoleLogin            bool // console username+password login enabled (CRI-195)
	// AllowAnonymousReads is the --allow-anon-reads dev flag (KB-326 S3):
	// when true, anonymous callers may read legacy UNscoped runs on the
	// run-read surfaces. Project-scoped runs are never anonymously
	// readable regardless of this flag; with the flag off, individual
	// agents' unscoped runs are anonymously readable only through their
	// owning project's allow_anonymous_reads grant (see rpc/anon_reads.go).
	AllowAnonymousReads bool
}

func NewCriteriaServer(st store.Store, h *hub.Hub, log *slog.Logger, controls *ControlRegistry) *CriteriaServer {
	if controls == nil {
		controls = NewControlRegistry()
	}
	if log == nil {
		log = slog.Default()
	}
	return &CriteriaServer{
		Store:                   st,
		Hub:                     h,
		Log:                     log,
		scope:                   newScopeCoalescer(st, log),
		controls:                controls,
		assignmentLeaseDuration: defaultAssignmentLeaseDuration,
	}
}

func NewServerServer(st store.Store, h *hub.Hub, log *slog.Logger, controls *ControlRegistry) *ServerServer {
	if controls == nil {
		controls = NewControlRegistry()
	}
	if log == nil {
		log = slog.Default()
	}
	return &ServerServer{
		Store:                   st,
		Hub:                     h,
		Log:                     log,
		controls:                controls,
		assignmentLeaseDuration: defaultAssignmentLeaseDuration,
	}
}

// SetAssignmentLeaseDuration configures how long an agent has to emit
// RunStarted before its lease expires. It is intended for tests; production
// code should rely on the default.
func (s *CriteriaServer) SetAssignmentLeaseDuration(d time.Duration) {
	if d <= 0 {
		d = defaultAssignmentLeaseDuration
	}
	s.assignmentLeaseDuration = d
}

// SetAssignmentLeaseDuration configures how long an agent has to emit
// RunStarted before its lease expires. It is intended for tests; production
// code should rely on the default.
func (s *ServerServer) SetAssignmentLeaseDuration(d time.Duration) {
	if d <= 0 {
		d = defaultAssignmentLeaseDuration
	}
	s.assignmentLeaseDuration = d
}

func (s *CriteriaServer) leaseDuration() time.Duration {
	if s.assignmentLeaseDuration <= 0 {
		return defaultAssignmentLeaseDuration
	}
	return s.assignmentLeaseDuration
}

func (s *ServerServer) leaseDuration() time.Duration {
	if s.assignmentLeaseDuration <= 0 {
		return defaultAssignmentLeaseDuration
	}
	return s.assignmentLeaseDuration
}

// LeaseDuration returns the currently configured assignment lease duration.
func (s *CriteriaServer) LeaseDuration() time.Duration {
	return s.leaseDuration()
}

// DrainBackgroundWriters stops and flushes the server's debounced
// persistence writers (the variable-scope coalescer). Called on the graceful
// shutdown path before the store closes, so no scheduled write races the DB
// teardown; tests use it during stack teardown for the same reason.
func (s *CriteriaServer) DrainBackgroundWriters() { s.scope.Stop() }

// LeaseDuration returns the currently configured assignment lease duration.
func (s *ServerServer) LeaseDuration() time.Duration {
	return s.leaseDuration()
}

// scopeCoalescer debounces variable-scope writes to SQLite. Each mutation is
// queued and a 250ms timer is started per run_id (reset on each new mutation
// within that window). When the timer fires all pending mutations are applied
// in a single read-modify-write. This mirrors the cursor-writer coalescing
// pattern and prevents hot-path SQLite writes on every VariableSet event.
type scopeCoalescer struct {
	mu      sync.Mutex
	stopped bool
	pending map[string][]func(map[string]interface{})
	timers  map[string]*time.Timer
	// appliedInflight counts flush/FlushNow applications that already
	// extracted their mutations and are applying them outside mu; Stop
	// waits for them, so no debounced write can race Store.Close.
	appliedInflight sync.WaitGroup
	store           store.Store
	log             *slog.Logger
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
	if c.stopped {
		c.mu.Unlock()
		// A stopped coalescer drops the mutation: this only happens during
		// shutdown while an in-flight handler still holds a server stream.
		// The drop is logged rather than silent because the mutation is
		// lost — nothing else will persist it.
		c.log.Warn("scope coalescer stopped; dropping scope mutation", "run_id", runID)
		return
	}
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
	// Add under mu so Stop's mu-guarded Wait cannot race an Add at zero:
	// any flush that Adds either releases mu before Stop's critical section
	// (Add happens-before Wait) or returns without Adding (stopped check).
	if len(mutations) > 0 {
		c.appliedInflight.Add(1)
	}
	c.mu.Unlock()

	if len(mutations) == 0 {
		return
	}
	defer c.appliedInflight.Done()
	c.applyMutations(ctx, runID, mutations)
}

func (c *scopeCoalescer) flush(runID string) {
	c.mu.Lock()
	if c.stopped {
		c.mu.Unlock()
		return
	}
	mutations := c.pending[runID]
	delete(c.pending, runID)
	delete(c.timers, runID)
	if len(mutations) > 0 {
		c.appliedInflight.Add(1)
	}
	c.mu.Unlock()

	if len(mutations) == 0 {
		return
	}
	defer c.appliedInflight.Done()
	c.applyMutations(context.Background(), runID, mutations)
}

// Stop halts the coalescer deterministically. All pending per-run timers are
// stopped (so no debounced write can race Store.Close during shutdown or test
// teardown) and any mutations already queued are applied once before Stop
// returns; Stop also waits for applications that already started (flush
// callbacks that extracted their mutations before Stop was called). Enqueue
// after Stop is ignored and logged. Stop is idempotent.
func (c *scopeCoalescer) Stop() {
	c.mu.Lock()
	for runID, t := range c.timers {
		if t != nil {
			t.Stop()
		}
		delete(c.timers, runID)
	}
	c.stopped = true
	pending := c.pending
	c.pending = make(map[string][]func(map[string]interface{}))
	c.mu.Unlock()

	for runID, mutations := range pending {
		c.applyMutations(context.Background(), runID, mutations)
	}
	c.appliedInflight.Wait()
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
