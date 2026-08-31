// Package store defines the persistence interface used by the Castle. A
// SQLite implementation lives in store/sqlite. bbolt or other engines can
// implement the same interface.
package store

import (
	"context"
	"errors"
	"time"
)

var ErrNotFound = errors.New("not found")

var ErrInvalidLimit = errors.New("invalid list limit")

// EventSchemaVersion is the current persisted event schema version. It tracks
// the criteria.v1 envelope major version and is stored in the events table for
// forward compatibility checks.
const EventSchemaVersion = 1

// RunAttempt records a single execution attempt for a (run_id, step) pair.
type RunAttempt struct {
	RunID       string
	Step        string
	Attempt     int
	StartedAt   time.Time
	CompletedAt *time.Time
	Outcome     string
}

// Event is a storage-neutral representation of a persisted envelope. It
// deliberately avoids generated wire types so persistence internals remain
// independent of the criteria.v1 protobuf contract.
type Event struct {
	SchemaVersion int32
	RunID         string
	Seq           uint64
	Type          string // discriminator, e.g. "run.started"
	Ts            time.Time
	CorrelationID string
	Payload       []byte // protojson of the concrete payload message
}

const (
	WorkflowAssignmentStateQueued   = "queued"
	WorkflowAssignmentStateLeased   = "leased"
	WorkflowAssignmentStateTerminal = "terminal"
	WorkflowAssignmentStateRejected = "rejected"
)

type Overseer struct {
	ID         string
	Name       string
	Hostname   string
	Version    string
	TokenHash  string
	Status     string // "online" | "offline"
	Labels     map[string]string
	CreatedAt  time.Time
	LastSeenAt time.Time
}

// WorkflowAssignment is a durable, idempotent queued workflow submission.
type WorkflowAssignment struct {
	ID               string
	OwnerCriteriaID  string
	RunID            string
	WorkflowName     string
	WorkflowSource   string
	LockfileSource   string
	IdempotencyKey   string
	State            string // WorkflowAssignmentState*
	TerminalReason   string
	LeasedCriteriaID string
	LeaseExpiresAt   *time.Time
	CreatedAt        time.Time
	UpdatedAt        time.Time
	Labels           map[string]string
}

// WorkflowAssignmentLease records a single lease grant to an agent.
type WorkflowAssignmentLease struct {
	ID           string
	AssignmentID string
	CriteriaID   string
	CreatedAt    time.Time
	ExpiresAt    time.Time
}

// WorkflowAssignmentAttempt records a single lease attempt for an assignment.
type WorkflowAssignmentAttempt struct {
	AssignmentID string
	Attempt      int
	CriteriaID   string
	CreatedAt    time.Time
	CompletedAt  *time.Time
	Outcome      string
}

type Run struct {
	ID           string
	OverseerID   string
	WorkflowName string
	WorkflowHCL  string
	Status       string // "pending"|"running"|"succeeded"|"failed"|"paused"|"cancelled"
	CurrentStep  string
	LastSeq      uint64
	CreatedAt    time.Time
	EndedAt      *time.Time
	// VariableScope holds the JSON-serialised run vars map (W04). Empty string
	// means the run has no captured variable state yet.
	VariableScope string
	// PendingSignal is the signal name the run is waiting for when paused (W05).
	// Empty when not paused.
	PendingSignal string
	// PausedAt records when the run entered the paused state (W05). Nil when not paused.
	PausedAt *time.Time
}

// Store is the persistence contract.
type Store interface {
	// Overseers
	CreateOverseer(ctx context.Context, o *Overseer) error
	GetOverseer(ctx context.Context, id string) (*Overseer, error)
	ListOverseers(ctx context.Context) ([]*Overseer, error)
	UpdateOverseerSeen(ctx context.Context, id string, ts time.Time) error
	UpdateOverseerStatus(ctx context.Context, id, status string) error
	MarkOfflineBefore(ctx context.Context, before time.Time) error

	// Runs
	CreateRun(ctx context.Context, r *Run) error
	GetRun(ctx context.Context, id string) (*Run, error)
	ListRuns(ctx context.Context, overseerID, status string) ([]*Run, error)
	UpdateRun(ctx context.Context, r *Run) error

	// Events
	// AppendEvent persists ev and returns the assigned seq. When ev has a
	// non-empty CorrelationID and a row already exists for
	// (run_id, correlation_id), the existing seq is returned and inserted
	// is false. This is the idempotency point for Criteria agent reconnect
	// replays: a duplicate correlation id MUST NOT produce a new row.
	//
	// AppendEvent mutates ev.Seq to the assigned sequence number on
	// successful insert so callers can reuse the event for hub fan-out.
	AppendEvent(ctx context.Context, ev *Event) (seq uint64, inserted bool, err error)
	ListEvents(ctx context.Context, runID string, since uint64, limit int) ([]*Event, error)
	ListStepLogs(ctx context.Context, runID, step string, since uint64, limit int) ([]*Event, error)
	// GetLatestEvent returns the most recently appended event for a run.
	GetLatestEvent(ctx context.Context, runID string) (*Event, error)
	// GetLatestStepEnteredEvent returns the most recent step.entered event for a run.
	GetLatestStepEnteredEvent(ctx context.Context, runID string) (*Event, error)

	// Subscriber cursors
	// UpsertSubscriberCursor records progress for (subscriber_id, run_id) and
	// stores max(existing, lastSeq) so stale reconnects cannot rewind cursors.
	UpsertSubscriberCursor(ctx context.Context, subscriberID, runID string, lastSeq uint64) error
	GetSubscriberCursor(ctx context.Context, subscriberID, runID string) (lastSeq uint64, found bool, err error)

	// Run attempts
	// RecordAttemptStart inserts a new attempt row for (run_id, step, attempt).
	// It is idempotent: if the row already exists it is left unchanged.
	RecordAttemptStart(ctx context.Context, ra *RunAttempt) error
	// RecordAttemptComplete stamps completed_at and outcome for the given attempt.
	RecordAttemptComplete(ctx context.Context, runID, step string, attempt int, outcome string) error
	// GetLatestAttempt returns the highest-numbered attempt row for (run_id, step).
	// Returns ErrNotFound when no rows exist.
	GetLatestAttempt(ctx context.Context, runID, step string) (*RunAttempt, error)

	// Variable scope
	// SetRunVariableScope persists a JSON-encoded vars snapshot for runID (W04).
	SetRunVariableScope(ctx context.Context, runID, scope string) error
	// GetRunVariableScope returns the stored variable scope JSON for runID.
	// Returns ("", nil) when no scope has been persisted yet.
	GetRunVariableScope(ctx context.Context, runID string) (string, error)

	// Pause/Resume (W05)
	// SetRunPaused marks the run as paused with the given pending signal and timestamp.
	SetRunPaused(ctx context.Context, runID, pendingSignal string, pausedAt time.Time) error
	// ClearRunPaused clears the pending_signal and paused_at and sets status back to running.
	ClearRunPaused(ctx context.Context, runID string) error

	// Workflow assignments
	// CreateWorkflowAssignment atomically creates the queued run and assignment
	// record in a single transaction. If an assignment with the same
	// (owner_criteria_id, idempotency_key) already exists, the existing record is
	// returned and created is false.
	CreateWorkflowAssignment(ctx context.Context, a *WorkflowAssignment) (*WorkflowAssignment, bool, error)
	// GetWorkflowAssignment returns a workflow assignment by id.
	GetWorkflowAssignment(ctx context.Context, id string) (*WorkflowAssignment, error)
	// GetWorkflowAssignmentByRunID returns the assignment for a run.
	GetWorkflowAssignmentByRunID(ctx context.Context, runID string) (*WorkflowAssignment, error)
	// LeaseWorkflowAssignment atomically expires stale leases, finds a queued
	// assignment whose labels are satisfied by the agent's labels, and leases it
	// to criteriaID. Returns ErrNotFound when no eligible queued assignment is
	// available. Expired leases are only returned to the queue when their run
	// has not yet started (status='pending'); running runs are left leased.
	LeaseWorkflowAssignment(ctx context.Context, criteriaID string, agentLabels map[string]string, now time.Time, leaseDuration time.Duration) (*WorkflowAssignment, error)
	// ExpireWorkflowAssignmentLeases transitions leased assignments whose lease
	// has expired and whose run is still pending back to queued, clearing their
	// run's overseer_id. It returns the IDs of the assignments that were expired.
	ExpireWorkflowAssignmentLeases(ctx context.Context, now time.Time) ([]string, error)
	// ListLeasedPendingAssignmentsByCriteriaID returns assignments currently
	// leased to criteriaID whose run has not yet started (status='pending').
	// These are active leases that should be redelivered to the agent when it
	// reconnects after a Castle restart.
	ListLeasedPendingAssignmentsByCriteriaID(ctx context.Context, criteriaID string) ([]*WorkflowAssignment, error)
	// RecordWorkflowAssignmentLease persists a lease record.
	RecordWorkflowAssignmentLease(ctx context.Context, lease *WorkflowAssignmentLease) error
	// RecordWorkflowAssignmentAttempt starts a lease attempt.
	RecordWorkflowAssignmentAttempt(ctx context.Context, attempt *WorkflowAssignmentAttempt) error
	// CompleteWorkflowAssignmentAttempt records the outcome of an attempt.
	CompleteWorkflowAssignmentAttempt(ctx context.Context, assignmentID string, attempt int, outcome string) error
	// MarkWorkflowAssignmentTerminal transitions an active assignment to the
	// terminal state, preventing further lease attempts.
	MarkWorkflowAssignmentTerminal(ctx context.Context, runID, reason string) error

	Close() error
}
