// Package store defines the persistence interface used by the Castle. A
// SQLite implementation lives in store/sqlite. bbolt or other engines can
// implement the same interface.
package store

import (
	"context"
	"errors"
	"time"

	"github.com/brokenbots/overlord/shared/events"
)

var ErrNotFound = errors.New("not found")

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
	// non-empty CorrelationID and a row already exists for
	// (run_id, correlation_id), the existing seq is returned and inserted
	// is false. This is the idempotency point for Overseer reconnect
	// replays: a duplicate correlation id MUST NOT produce a new row.
	AppendEvent(ctx context.Context, env events.Envelope) (seq uint64, inserted bool, err error)
	ListEvents(ctx context.Context, runID string, since uint64, limit int) ([]events.Envelope, error)
	ListStepLogs(ctx context.Context, runID, step string, since uint64, limit int) ([]events.Envelope, error)

	Close() error
}
