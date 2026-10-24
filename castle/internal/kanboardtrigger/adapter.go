package kanboardtrigger

import (
	"context"
	"fmt"
	"log/slog"
	"slices"
	"sync"
	"time"

	"connectrpc.com/connect"

	pb "github.com/brokenbots/criteria/sdk/pb/criteria/v1"

	castlev1 "github.com/brokenbots/castle/castle/gen/castle/v1"

	"github.com/brokenbots/castle/castle/internal/kanboard"
)

// Submitter submits project assignments on board events. Implemented by the
// castle.v1 ProjectService Connect client; the trigger identity's token is
// attached by the transport-level auth (headers on every request).
type Submitter interface {
	SubmitProjectAssignment(ctx context.Context, req *connect.Request[castlev1.SubmitProjectAssignmentRequest]) (*connect.Response[castlev1.SubmitProjectAssignmentResponse], error)
}

// AssignmentReader reads one assignment's disposition by run id. Implemented
// by the criteria.v1 ServerService Connect client.
type AssignmentReader interface {
	GetAssignmentDisposition(ctx context.Context, req *connect.Request[pb.GetAssignmentDispositionRequest]) (*connect.Response[pb.GetAssignmentDispositionResponse], error)
}

// RunWatcher opens a per-run event stream. Implemented by the criteria.v1
// ServerService Connect client (whose ServerStreamForClient satisfies
// RunEventStream).
type RunWatcher interface {
	WatchRun(ctx context.Context, req *connect.Request[pb.WatchRunRequest]) (RunEventStream, error)
}

// RunEventStream is the run-event stream reader the mirror consumes.
type RunEventStream interface {
	Receive() bool
	Msg() *pb.Envelope
	Err() error
	Close() error
}

// Adapter polls the board, fires assignments (firing loop), and mirrors run
// events back onto card columns (board mirror). One process serves the
// configured lanes; the mirror reconciles a ticket's runs across lanes.
type Adapter struct {
	cfg      *Config
	kb       *kanboard.Client
	submit   Submitter
	dispose  AssignmentReader
	watch    RunWatcher
	log      *slog.Logger
	interval time.Duration

	// subscriberID scopes the server-persisted WatchRun cursors for this
	// adapter instance.
	subscriberID string

	statePath      string
	boardProjectID int

	// mu guards all in-memory state (hot authority); persisted snapshots
	// are written through on mutation.
	mu       sync.Mutex
	memos    map[string]*TicketMemo // ticket -> column memo
	runs     map[string]*RunRecord  // run id -> record
	watching map[string]bool        // run ids with a live/attempted watch stream
}

// Options configures New.
type Options struct {
	// StatePath is the durable state file (entry memos + run records). Empty
	// disables the adapter: the entry memo's anti-refire protection must be
	// durable, never in-memory only.
	StatePath string
	// PollInterval is the poll cadence; default 30s.
	PollInterval time.Duration
	// Instance disambiguates WatchRun subscriber cursors between adapter
	// processes sharing one castle; default "default".
	Instance string
}

// New builds an Adapter. It loads the durable state (entry memos, tracked
// runs) and requires a validated config.
func New(cfg *Config, kb *kanboard.Client, submit Submitter, dispose AssignmentReader, runWatch RunWatcher, log *slog.Logger, opts Options) (*Adapter, error) {
	if err := cfg.Validated(); err != nil {
		return nil, err
	}
	if log == nil {
		log = slog.Default()
	}
	st, err := loadState(opts.StatePath)
	if err != nil {
		return nil, err
	}
	if opts.PollInterval <= 0 {
		opts.PollInterval = 30 * time.Second
	}
	if opts.Instance == "" {
		opts.Instance = "default"
	}
	return &Adapter{
		cfg:          cfg,
		kb:           kb,
		submit:       submit,
		dispose:      dispose,
		watch:        runWatch,
		log:          log,
		interval:     opts.PollInterval,
		subscriberID: "kanboard-trigger-adapter:" + opts.Instance,
		statePath:    opts.StatePath,
		memos:        st.Tickets,
		runs:         st.Runs,
		watching:     map[string]bool{},
	}, nil
}

// Run drives the poll loop until ctx is cancelled.
func (a *Adapter) Run(ctx context.Context) error {
	if err := a.initBoard(ctx); err != nil {
		return err
	}
	poll := func() {
		pollCtx, cancel := context.WithTimeout(ctx, 2*time.Minute)
		defer cancel()
		if err := a.poll(pollCtx); err != nil {
			if ctx.Err() == nil {
				a.log.Error("kanboard trigger poll failed", "err", err)
			}
		}
	}
	// Watch any persisted unsettled runs immediately: boot adoption of the
	// mirror (restart must not lose track of in-flight runs).
	poll()
	t := time.NewTicker(a.interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-t.C:
		}
		poll()
	}
}

func (a *Adapter) initBoard(ctx context.Context) error {
	id, err := a.kb.FindProjectID(ctx, a.cfg.BoardProject)
	if err != nil {
		return fmt.Errorf("resolving board project: %w", err)
	}
	a.boardProjectID = id
	return nil
}

// fireKey derives the assignment idempotency key: fire N of a ticket is
// always derived from the same key, so a re-derived fire after state loss
// collides with the existing assignment (created=false → adopt) instead of
// creating a spurious duplicate run.
func fireKey(boardProject, ticket string, seq int) string {
	return fmt.Sprintf("kanboard:%s:%s:%d", boardProject, ticket, seq)
}

// laneFor returns the first lane whose trigger columns contain the named
// column, or nil (no route for this column — like the watcher's ErrNoRoute
// skip). Declared lane order is the deterministic tiebreak.
func (a *Adapter) laneFor(columnName string) *Lane {
	for i := range a.cfg.Lanes {
		if slices.Contains(a.cfg.Lanes[i].TriggerColumns, columnName) {
			return &a.cfg.Lanes[i]
		}
	}
	return nil
}

// hasActiveRun reports whether the ticket has a run the mirror has not seen
// settle (in-flight abstention; fire gates on it).
func (a *Adapter) hasActiveRun(ticket string) bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	for _, r := range a.runs {
		if r.TicketID == ticket && !r.Settled {
			return true
		}
	}
	return false
}

// setMemo records the card's column for the next entry comparison and
// persists the state.
func (a *Adapter) setMemo(ticket string, columnID int) {
	a.mu.Lock()
	defer a.mu.Unlock()
	memo := a.memos[ticket]
	if memo == nil {
		memo = &TicketMemo{}
		a.memos[ticket] = memo
	}
	memo.PrevColumnID = columnID
	if err := (fileStore{a.statePath}).save(&state{Tickets: a.memos, Runs: a.runs}); err != nil {
		a.log.Error("persisting entry memo state", "err", err)
	}
}

// nextFireSeq returns the ticket's next fire sequence without committing it:
// the fire key is deterministic (always seq+1 until a fire succeeds), so a
// retried fire reuses the same key and idempotently adopts an assignment
// the first attempt had already created.
func (a *Adapter) nextFireSeq(ticket string) (int, bool) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if memo := a.memos[ticket]; memo != nil {
		return memo.FireSeq + 1, true
	}
	return 1, false
}

// commitFire persists a successful fire: the fire sequence and the entry
// memo (the entry consumed by this fire must not re-fire on the next poll —
// KB-302).
func (a *Adapter) commitFire(ticket string, seq int) {
	a.mu.Lock()
	defer a.mu.Unlock()
	memo := a.memos[ticket]
	if memo == nil {
		memo = &TicketMemo{}
		a.memos[ticket] = memo
	}
	if memo.FireSeq < seq {
		memo.FireSeq = seq
	}
	if err := (fileStore{a.statePath}).save(&state{Tickets: a.memos, Runs: a.runs}); err != nil {
		a.log.Error("persisting fire sequence", "err", err)
	}
}

// forgetMemo drops a ticket's entry memo after a failed submit: the next
// poll re-enters the card (first sighting) and retries the fire with the
// SAME deterministic key — an assignment accepted by the server before the
// failure is adopted, never duplicated.
func (a *Adapter) forgetMemo(ticket string) {
	a.mu.Lock()
	defer a.mu.Unlock()
	delete(a.memos, ticket)
	if err := (fileStore{a.statePath}).save(&state{Tickets: a.memos, Runs: a.runs}); err != nil {
		a.log.Error("resetting entry memo after failed submit", "err", err)
	}
}

// submitAssignment submits one lane fire and records the run (created or
// adopted). Returns the response run id.
func (a *Adapter) submitAssignment(ctx context.Context, ticket string, taskID int, lane *Lane, fireSeq int) (string, error) {
	inputs := make(map[string]string, len(lane.Inputs)+1)
	for k, v := range lane.Inputs {
		inputs[k] = v
	}
	inputs["ticket_id"] = ticket
	resp, err := a.submit.SubmitProjectAssignment(ctx, connect.NewRequest(&castlev1.SubmitProjectAssignmentRequest{
		ProjectId:      a.cfg.CastleProjectID,
		WorkflowName:   lane.RouteName,
		WorkflowSource: lane.WorkflowSource,
		LockfileSource: lane.LockfileSource,
		InputValues:    inputs,
		IdempotencyKey: fireKey(a.cfg.BoardProject, ticket, fireSeq),
	}))
	if err != nil {
		return "", err
	}
	return resp.Msg.GetRunId(), nil
}
