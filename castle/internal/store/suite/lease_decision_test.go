package suite

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/brokenbots/castle/castle/internal/store"
)

// leaseDecision runs a lease scan and unwraps the KB-343 decision contract:
// a non-matching scan reports ErrNotFound together with the decision.
func leaseDecision(t *testing.T, ctx context.Context, s store.Store, criteriaID string, labels map[string]string, now time.Time) (*store.LeaseDecision, error) {
	t.Helper()
	_, decision, err := s.LeaseWorkflowAssignment(ctx, criteriaID, labels, now, time.Minute)
	if err != nil && !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("LeaseWorkflowAssignment(%s): %v", criteriaID, err)
	}
	return decision, err
}

// registerOverseerFixture registers an agent with the given status/labels.
func registerOverseerFixture(t *testing.T, ctx context.Context, s store.Store, id, status string, labels map[string]string, now time.Time) {
	t.Helper()
	if err := s.CreateOverseer(ctx, &store.Overseer{ID: id, Name: id, TokenHash: id + "-tok", Status: status, Labels: labels, CreatedAt: now, LastSeenAt: now}); err != nil {
		t.Fatalf("CreateOverseer(%s): %v", id, err)
	}
}

// expectLeaseStamp asserts the KB-343 row disposition after a declined scan:
// lease_attempt counts the visit and last_lease_rejection names the rule.
func expectLeaseStamp(t *testing.T, ctx context.Context, s store.Store, a *store.WorkflowAssignment, rejection string) {
	t.Helper()
	got, err := s.GetWorkflowAssignment(ctx, a.ID)
	if err != nil {
		t.Fatalf("GetWorkflowAssignment(%s): %v", a.ID, err)
	}
	if got.LeaseAttempt == 0 {
		t.Fatalf("%s: lease_attempt = 0, want >= 1 after declined scan (rule %s)", a.ID, rejection)
	}
	if got.LastLeaseRejection != rejection {
		t.Fatalf("%s: last_lease_rejection = %q, want %q", a.ID, got.LastLeaseRejection, rejection)
	}
}

// TestLeaseDecisionPerRule pins the KB-343 failure-story contract per dispatch
// rule: an assignment that CANNOT lease returns a named rejection decision
// naming the declined row and stamps the rule onto the row so "why didn't X
// run" is queryable. Each rule runs against its own store so the queue is
// isolated.
func TestLeaseDecisionPerRule(t *testing.T) {
	cases := []struct {
		name string
		run  func(t *testing.T, ctx context.Context, s store.Store, now time.Time)
	}{
		{"agent_offline", func(t *testing.T, ctx context.Context, s store.Store, now time.Time) {
			a := newAssignment(t, ctx, s, "owner-off", "key-off", map[string]string{}, now)
			registerOverseerFixture(t, ctx, s, "ag-offline", "offline", nil, now)
			d, err := leaseDecision(t, ctx, s, "ag-offline", nil, now)
			if !errors.Is(err, store.ErrNotFound) || d == nil || d.Rejection != store.LeaseRejectionAgentOffline || d.AssignmentID != a.ID {
				t.Fatalf("decision = %+v err = %v; want agent_offline naming %s", d, err, a.ID)
			}
			expectLeaseStamp(t, ctx, s, a, store.LeaseRejectionAgentOffline)
		}},
		{"agent_unknown", func(t *testing.T, ctx context.Context, s store.Store, now time.Time) {
			a := newAssignment(t, ctx, s, "owner-unk", "key-unk", map[string]string{}, now)
			d, err := leaseDecision(t, ctx, s, "ag-never-registered", nil, now)
			if !errors.Is(err, store.ErrNotFound) || d == nil || d.Rejection != store.LeaseRejectionAgentUnknown {
				t.Fatalf("decision = %+v err = %v; want agent_unknown", d, err)
			}
			// An unknown agent cannot see the queue, so no row was visited:
			// the stamp must stay untouched.
			if got, gErr := s.GetWorkflowAssignment(ctx, a.ID); gErr != nil || got.LeaseAttempt != 0 {
				t.Fatalf("agent_unknown stamped %s (lease_attempt %d), want untouched", a.ID, got.LeaseAttempt)
			}
		}},
		{"labels_mismatch", func(t *testing.T, ctx context.Context, s store.Store, now time.Time) {
			a := newAssignment(t, ctx, s, "owner-labels", "key-labels", map[string]string{"gpu": "true", "zone": "a"}, now)
			registerOverseerFixture(t, ctx, s, "ag-labels", "online", map[string]string{"gpu": "true"}, now)
			d, err := leaseDecision(t, ctx, s, "ag-labels", map[string]string{"gpu": "true"}, now)
			if !errors.Is(err, store.ErrNotFound) || d == nil || d.Rejection != store.LeaseRejectionLabelsMismatch || d.AssignmentID != a.ID {
				t.Fatalf("decision = %+v err = %v; want labels_mismatch naming %s", d, err, a.ID)
			}
			if !strings.Contains(d.Details, "zone=a") {
				t.Fatalf("details %q want the required pair zone=a", d.Details)
			}
			expectLeaseStamp(t, ctx, s, a, store.LeaseRejectionLabelsMismatch)
		}},
		{"project_not_member", func(t *testing.T, ctx context.Context, s store.Store, now time.Time) {
			newProject(t, ctx, s, "proj-decision", store.ProjectMembershipOpen, now)
			a, created, err := s.CreateWorkflowAssignment(ctx, &store.WorkflowAssignment{
				OwnerCriteriaID: "owner-pm",
				WorkflowName:    "wf",
				WorkflowSource:  "source hcl",
				IdempotencyKey:  "key-pm",
				ProjectID:       "proj-decision",
				CreatedAt:       now,
				UpdatedAt:       now,
			})
			if err != nil || !created {
				t.Fatalf("CreateWorkflowAssignment: %v created=%v", err, created)
			}
			registerOverseerFixture(t, ctx, s, "ag-outsider", "online", nil, now)
			d, err := leaseDecision(t, ctx, s, "ag-outsider", nil, now)
			if !errors.Is(err, store.ErrNotFound) || d == nil || d.Rejection != store.LeaseRejectionProjectNotMember || d.AssignmentID != a.ID {
				t.Fatalf("decision = %+v err = %v; want project_not_member naming %s", d, err, a.ID)
			}
			expectLeaseStamp(t, ctx, s, a, store.LeaseRejectionProjectNotMember)
		}},
		{"agent_pending_lease", func(t *testing.T, ctx context.Context, s store.Store, now time.Time) {
			first := newAssignment(t, ctx, s, "owner-pl", "key-pl-1", map[string]string{}, now)
			registerOverseerFixture(t, ctx, s, "ag-pl", "online", nil, now)
			leased, _, err := s.LeaseWorkflowAssignment(ctx, "ag-pl", nil, now, time.Hour)
			if err != nil {
				t.Fatalf("first lease: %v", err)
			}
			if leased.ID != first.ID {
				t.Fatalf("first lease id = %s, want %s", leased.ID, first.ID)
			}
			// The agent never accepts (RunStarted) the lease; a second queued
			// assignment arrives and the next scan must decline with
			// agent_pending_lease, stamping the declined head row.
			second := newAssignment(t, ctx, s, "owner-pl", "key-pl-2", map[string]string{}, microUTC(now.Add(time.Second)))
			d, err := leaseDecision(t, ctx, s, "ag-pl", nil, now.Add(time.Second))
			if !errors.Is(err, store.ErrNotFound) || d == nil || d.Rejection != store.LeaseRejectionAgentPendingLease || d.AssignmentID != second.ID {
				t.Fatalf("decision = %+v err = %v; want agent_pending_lease naming %s", d, err, second.ID)
			}
			if !strings.Contains(d.Details, first.ID) {
				t.Fatalf("details %q want the holding assignment %s", d.Details, first.ID)
			}
			expectLeaseStamp(t, ctx, s, second, store.LeaseRejectionAgentPendingLease)
			// The holding lease is bookkeeping, not a rejection: it must not
			// be re-stamped by the pending-lease gate.
			if got, gErr := s.GetWorkflowAssignment(ctx, first.ID); gErr != nil || got.LastLeaseRejection != "" {
				t.Fatalf("holding lease stamped %q (err %v), want clean", got.LastLeaseRejection, gErr)
			}
		}},
		{"run_not_executable", func(t *testing.T, ctx context.Context, s store.Store, now time.Time) {
			a := newAssignment(t, ctx, s, "owner-ne", "key-ne", map[string]string{}, now)
			registerOverseerFixture(t, ctx, s, "ag-ne", "online", nil, now)
			// Park the run via SetRunStopped — the engine must not lease the
			// assignment again (CRI-207); the scan reports the rule instead.
			if err := s.SetRunStopped(ctx, a.RunID); err != nil {
				t.Fatalf("SetRunStopped: %v", err)
			}
			d, err := leaseDecision(t, ctx, s, "ag-ne", nil, now)
			if !errors.Is(err, store.ErrNotFound) || d == nil || d.Rejection != store.LeaseRejectionRunNotExecutable || d.AssignmentID != a.ID {
				t.Fatalf("decision = %+v err = %v; want run_not_executable naming %s", d, err, a.ID)
			}
			expectLeaseStamp(t, ctx, s, a, store.LeaseRejectionRunNotExecutable)
		}},
	}
	for _, tc := range cases {
		tc := tc
		t.Run(tc.name, func(t *testing.T) {
			runWithCtx(t, func(t *testing.T, ctx context.Context, s store.Store) {
				now := microUTC(time.Now())
				tc.run(t, ctx, s, now)
			})
		})
	}
}

// TestLeaseDecisionGrantAndEmptyQueue covers the two non-rejection shapes:
// a granted lease clears the stamp and advances lease_attempt, and an empty
// queue reports an unnamed no-match (Debug territory in the dispatch layer).
func TestLeaseDecisionGrantAndEmptyQueue(t *testing.T) {
	runWithCtx(t, func(t *testing.T, ctx context.Context, s store.Store) {
		now := microUTC(time.Now())
		a := newAssignment(t, ctx, s, "owner-grant", "key-grant", map[string]string{"gpu": "true"}, now)
		registerOverseerFixture(t, ctx, s, "ag-miss", "online", map[string]string{"gpu": "false"}, now)
		d, err := leaseDecision(t, ctx, s, "ag-miss", map[string]string{"gpu": "false"}, now)
		if !errors.Is(err, store.ErrNotFound) || d == nil || d.Rejection != store.LeaseRejectionLabelsMismatch {
			t.Fatalf("mismatch scan decision = %+v err = %v; want labels_mismatch", d, err)
		}
		expectLeaseStamp(t, ctx, s, a, store.LeaseRejectionLabelsMismatch)

		registerOverseerFixture(t, ctx, s, "ag-granted", "online", map[string]string{"gpu": "true"}, now)
		leased, _, err := s.LeaseWorkflowAssignment(ctx, "ag-granted", map[string]string{"gpu": "true"}, microUTC(now.Add(time.Second)), time.Minute)
		if err != nil {
			t.Fatalf("lease after mismatch: %v", err)
		}
		if leased.ID != a.ID || leased.State != store.WorkflowAssignmentStateLeased {
			t.Fatalf("leased = %+v", leased)
		}
		if leased.LastLeaseRejection != "" {
			t.Fatalf("leased row carries rejection %q, want cleared", leased.LastLeaseRejection)
		}
		if leased.LeaseAttempt == 0 {
			t.Fatalf("leased row lease_attempt = 0, want >= 1 (mismatch visit + grant)")
		}
	})
}

// TestLeaseDecisionEmptyQueueUnnamed pins the quiet shape of an empty queue:
// ErrNotFound with a decision that names no rule (the dispatch layer renders
// that as Debug, not operator telemetry).
func TestLeaseDecisionEmptyQueueUnnamed(t *testing.T) {
	runWithCtx(t, func(t *testing.T, ctx context.Context, s store.Store) {
		now := microUTC(time.Now())
		registerOverseerFixture(t, ctx, s, "ag-quiet", "online", nil, now)
		d, err := leaseDecision(t, ctx, s, "ag-quiet", nil, now)
		if !errors.Is(err, store.ErrNotFound) || d == nil || d.Rejection != "" || d.Matched {
			t.Fatalf("decision = %+v err = %v; want unnamed no-match for an empty queue", d, err)
		}
	})
}
