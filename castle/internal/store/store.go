// Package store defines the persistence interface used by the Castle. A
// SQLite implementation lives in store/sqlite. bbolt or other engines can
// implement the same interface.
package store

import (
	"context"
	"errors"
	"time"

	pb "github.com/brokenbots/overlord/shared/pb/overlord/v1"
)

var ErrNotFound = errors.New("not found")

var ErrInvalidLimit = errors.New("invalid list limit")

// RunAttempt records a single execution attempt for a (run_id, step) pair.
type RunAttempt struct {
	RunID       string
	Step        string
	Attempt     int
	StartedAt   time.Time
	CompletedAt *time.Time
	Outcome     string
}

type Overseer struct {
	ID         string
	Name       string
	Hostname   string
	Version    string
	TokenHash  string
	Status     string // "online" | "offline"
	CreatedAt  time.Time
	LastSeenAt time.Time
}

type Run struct {
	ID           string
	OverseerID   string
	WorkflowName string
	WorkflowHCL  string
	Status       string // "pending"|"running"|"succeeded"|"failed"|"awaiting_human"|"cancelled"
	CurrentStep  string
	LastSeq      uint64
	CreatedAt    time.Time
	EndedAt      *time.Time
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
	// AppendEvent persists env and returns the assigned seq. When env has a
	// non-empty CorrelationId and a row already exists for
	// (run_id, correlation_id), the existing seq is returned and inserted
	// is false. This is the idempotency point for Overseer reconnect
	// replays: a duplicate correlation id MUST NOT produce a new row.
	//
	// AppendEvent mutates env.Seq to the assigned sequence number on
	// successful insert so callers can reuse the message for hub fan-out.
	AppendEvent(ctx context.Context, env *pb.Envelope) (seq uint64, inserted bool, err error)
	ListEvents(ctx context.Context, runID string, since uint64, limit int) ([]*pb.Envelope, error)
	ListStepLogs(ctx context.Context, runID, step string, since uint64, limit int) ([]*pb.Envelope, error)

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

	Close() error
}
