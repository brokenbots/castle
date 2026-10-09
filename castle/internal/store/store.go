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

// ErrInvalidCursor is returned when a continuation page token cannot be
// decoded (e.g. a hand-crafted or truncated cursor).
var ErrInvalidCursor = errors.New("invalid page token")

// ErrRunTerminal is returned when an operator-initiated terminal transition
// (CRI-142 CancelRun) targets a run that is already in a terminal state.
var ErrRunTerminal = errors.New("run is terminal")

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

// Orchestrator is an operator-facing identity (CRI-133). Orchestrators
// authenticate with accept-token auth like agents but hold read-only
// authority: they observe run lifecycle via the OrchestratorService and may
// not invoke agent-owned write procedures. The TokenHash is the SHA-256 hex
// digest of the accept token.
type Orchestrator struct {
	ID        string
	Name      string
	TokenHash string
	CreatedAt time.Time
}

// ConsoleUser is a human console login identity (CRI-195). The password is
// stored only as a bcrypt hash; plaintext never reaches the store. The
// seeded default user has the fixed ID ConsoleDefaultUserID so a changed
// CASTLE_CONSOLE_USER rotates the same row instead of leaving the old
// username able to log in.
type ConsoleUser struct {
	ID           string
	Username     string
	PasswordHash string
	CreatedAt    time.Time
	UpdatedAt    time.Time
}

// ConsoleSession is a login session issued to a console user (CRI-195).
// TokenHash is the SHA-256 hex digest of the issued bearer token.
type ConsoleSession struct {
	ID        string
	UserID    string
	TokenHash string
	CreatedAt time.Time
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
	Status       string // "pending"|"running"|"succeeded"|"failed"|"paused"|"stopped"|"cancelled"
	CurrentStep  string
	LastSeq      uint64
	CreatedAt    time.Time
	// StartedAt records when the run first entered the running state (CRI-187
	// run-list durations). Nil until the first "running" transition; never
	// rewritten afterwards, so reaped-never-started runs keep a nil StartedAt.
	StartedAt *time.Time
	EndedAt   *time.Time
	// VariableScope holds the JSON-serialised run vars map (W04). Empty string
	// means the run has no captured variable state yet.
	VariableScope string
	// PendingSignal is the signal name the run is waiting for when paused (W05).
	// Empty when not paused.
	PendingSignal string
	// PausedAt records when the run entered the paused state (W05). Nil when not paused.
	PausedAt *time.Time
	// Ticket is the external ticket identifier the run is attached to (e.g.
	// "CRI-104"); empty for agent-initiated runs. CRI-131.
	Ticket string
	// RepoURL is the repository the run operates on; empty for agent-initiated
	// runs. CRI-131.
	RepoURL string
	// PRURL is the pull request URL produced by the run, when an external
	// orchestrator has published it via a run.metadata event. CRI-131.
	PRURL string
	// FailureReason records why the run ended in failure or was cancelled
	// (CRI-142): "agent heartbeat lost" for heartbeat-staleness reaping, the
	// RunFailed event reason for agent-driven failures, or the operator's
	// cancel reason for CancelRun.
	FailureReason string
}

// RunStatusAwaitingHuman is the parked status of a run whose engine reached
// the workflow terminal state awaiting_human (KB-226): the workflow work is
// done and the run is held for a human decision, so it is no longer
// executing and has no agent heartbeat by design. Parked runs are excluded
// from reaping and from workflow-assignment dispatch eligibility.
const RunStatusAwaitingHuman = "awaiting_human"

// ReapReasonAgentHeartbeatLost is the failure reason stamped on runs reaped
// for heartbeat staleness with no human-gate ending in their record (CRI-142).
const ReapReasonAgentHeartbeatLost = "agent heartbeat lost"

// RunReapOutcome is one deterministic reaper verdict for a candidate run
// (KB-226): either the run was reaped terminal-failed (Parked false) or it
// was reclassified to its real parked status instead of failing (Parked
// true). Status always names the stamped status, and Reason names why —
// "agent heartbeat lost" / "created_never_started" for reaped runs, the
// human-gate reclassification reason for parked runs.
type RunReapOutcome struct {
	RunID  string
	Parked bool
	Status string
	Reason string
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

	// Orchestrators (CRI-133)
	// UpsertOrchestrator inserts or replaces the orchestrator record by ID.
	// Updating an existing identity rotates its token: the new TokenHash
	// invalidates previously issued accept tokens. CreatedAt is preserved on
	// update.
	UpsertOrchestrator(ctx context.Context, o *Orchestrator) error
	// ListOrchestrators returns all registered orchestrator identities.
	ListOrchestrators(ctx context.Context) ([]*Orchestrator, error)
	// DeleteOrchestrator removes the orchestrator identity, revoking its
	// accept token (CRI-133). Deleting an unknown ID is a no-op.
	DeleteOrchestrator(ctx context.Context, id string) error

	// Console users and sessions (CRI-195)
	// UpsertConsoleUser inserts or replaces the console user record by ID.
	// Updating an existing identity rotates its username and password hash
	// and preserves CreatedAt.
	UpsertConsoleUser(ctx context.Context, u *ConsoleUser) error
	// GetConsoleUser returns the console user with the given username.
	// Returns ErrNotFound when no user matches.
	GetConsoleUser(ctx context.Context, username string) (*ConsoleUser, error)
	// DeleteConsoleUsers removes every console user and, via cascade, all
	// of their sessions. Used when the console login feature is off.
	DeleteConsoleUsers(ctx context.Context) error
	// CreateConsoleSession persists a login session for a console user.
	CreateConsoleSession(ctx context.Context, s *ConsoleSession) error
	// ListConsoleSessions returns all console login sessions.
	ListConsoleSessions(ctx context.Context) ([]*ConsoleSession, error)
	// DeleteConsoleSessions revokes every console login session.
	DeleteConsoleSessions(ctx context.Context) error
	// DeleteConsoleSessionsByUser revokes all sessions of one console user,
	// e.g. when the operator rotates the seeded password.
	DeleteConsoleSessionsByUser(ctx context.Context, userID string) error

	// Runs
	CreateRun(ctx context.Context, r *Run) error
	GetRun(ctx context.Context, id string) (*Run, error)
	// ListRuns returns runs newest-first (created_at DESC, id DESC). When
	// limit is positive at most limit rows are returned; limit <= 0 returns
	// every matching row. pageToken continues a previous page: pass the
	// token returned by the prior call ("" starts from the newest run). The
	// returned token is "" when no further page exists. A non-empty token
	// that cannot be decoded fails with ErrInvalidCursor.
	ListRuns(ctx context.Context, overseerID, status string, limit int, pageToken string) ([]*Run, string, error)
	UpdateRun(ctx context.Context, r *Run) error
	// SetRunMetadata promotes non-empty metadata values (ticket, repo_url,
	// pr_url) onto the run record without touching run status (CRI-131). Empty
	// values leave the existing column untouched.
	SetRunMetadata(ctx context.Context, runID, ticket, repoURL, prURL string) error

	// Reaping (CRI-142, KB-226)
	// ReapStaleAgentRuns classifies runs in status pending or running whose
	// owning agent's heartbeat (overseers.last_seen_at) is older than
	// staleBefore and stamps each re-validated run from its own record
	// (CRI-142):
	//
	//   - A run whose engine reached a human gate — the awaiting_human
	//     terminal state, or an approval / signal wait parked in its last
	//     event — has no live heartbeat by design: it is parked, not failed
	//     (KB-226). Its status is reclassified to RunStatusAwaitingHuman
	//     with no EndedAt, no FailureReason and no workflow-assignment
	//     change, and the outcome carries Parked true plus the
	//     reclassification reason.
	//   - A run still mid-flight (no human-gate ending in its record) is
	//     stamped failed with reason ReapReasonAgentHeartbeatLost and its
	//     workflow assignment is marked terminal so the dead-agent work is
	//     never re-dispatched.
	//
	// Runs without an owning agent (queued assignment work), paused runs and
	// stopped runs (CRI-207: an operator parked run is reaper-exempt
	// regardless of heartbeat age, because it has no live agent by design)
	// are left alone; terminal runs are never rewritten. Returns one
	// RunReapOutcome per stamped run, grouped by id.
	ReapStaleAgentRuns(ctx context.Context, now time.Time, staleBefore time.Time) ([]RunReapOutcome, error)
	// ReapNeverStartedRuns stamps runs in status pending or running as failed
	// with reason "created_never_started" (KB-233 rule 2) when started_at is
	// still NULL and created_at is older than createdBefore: a run that was
	// minted but never began executing. The verdict is derivable from the run
	// record alone — created_at and started_at — never from heartbeat or
	// assignment heuristics. Paused and stopped runs (operator-parked,
	// CRI-207) are reaper-exempt regardless of age, and terminal runs are
	// never rewritten. A never-started candidate whose record ends at a human
	// gate is parked instead of failed (KB-226), like the heartbeat reaper.
	// Each reaped run's workflow assignment is marked terminal so the dead
	// work is never re-dispatched. Returns one RunReapOutcome per stamped
	// run.
	ReapNeverStartedRuns(ctx context.Context, now time.Time, createdBefore time.Time) ([]RunReapOutcome, error)
	// CancelRun stamps runID terminal as "cancelled" with the given reason
	// (CRI-142). Terminal runs are never rewritten: an already terminal run
	// returns ErrRunTerminal, an unknown id ErrNotFound. The cancelled run
	// record is returned on success.
	CancelRun(ctx context.Context, runID, reason string, now time.Time) (*Run, error)

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
	// Only runs currently in status paused are affected; terminal runs are left untouched.
	ClearRunPaused(ctx context.Context, runID string) error

	// Stop/Resume (CRI-207): STOPPED is a first-class resumable, reaper-exempt
	// run status. Stop parks the operator-intent: the same run id transitions
	// RUNNING -> STOPPED -> RUNNING; checkpoint/state retention is
	// criteria-owned, castle holds only pointers.
	// SetRunStopped parks the run in status stopped and clears any pause
	// state. Never rewrites a terminal run; unknown ids are a no-op. Castle
	// records no dedicated stopped_at column — the stop instant lives in the
	// control command and operator-visible issued_at (CRI-207).
	SetRunStopped(ctx context.Context, runID string) error
	// ClearRunStopped moves a stopped run back to running (the resume path,
	// same run id). Only runs currently in status stopped are affected.
	ClearRunStopped(ctx context.Context, runID string) error
	// MarkRunUnstarted returns a never-started stopped run to the leasable
	// pending bucket (CRI-207 resume path): status='pending' and
	// started_at=NULL, so the dispatch redelivery and lease-expiry paths
	// apply to it again. requeuedAt refreshes created_at: the re-queue is a
	// fresh pending incarnation, giving the KB-233 created_never_started
	// reaper window a restart so a just-resumed run is not killed before its
	// redelivery can land.
	MarkRunUnstarted(ctx context.Context, runID string, requeuedAt time.Time) error

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
