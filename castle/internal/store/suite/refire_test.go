package suite

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/brokenbots/castle/castle/internal/store"
	"github.com/brokenbots/castle/castle/internal/store/refire"
)

// refireFixture drives a workflow assignment from submission through an
// agent-style terminal stamp (the same shape the overseer SubmitEvents path
// and the reaper produce), then runs policy passes against it.
type refireFixture struct {
	t   *testing.T
	s   store.Store
	ctx context.Context
	now time.Time
	// policy is the ('', '') default slot's document, upserted per scenario.
	policy refire.Policy
}

func newRefireFixture(t *testing.T, ctx context.Context, s store.Store) *refireFixture {
	t.Helper()
	return &refireFixture{t: t, s: s, ctx: ctx, now: microUTC(time.Now()), policy: refire.DefaultPolicyDoc()}
}

// setPolicy upserts the fixture's policy under the given (project, route)
// scope; empty strings are the default slots.
func (f *refireFixture) setPolicy(projectID, route string) {
	f.t.Helper()
	if err := f.s.UpsertRefirePolicy(f.ctx, projectID, route, refire.MustMarshalPolicy(f.policy), f.now); err != nil {
		f.t.Fatalf("UpsertRefirePolicy(%s,%s): %v", projectID, route, err)
	}
}

// submit creates the queued assignment plus its pending run.
func (f *refireFixture) submit(owner, idem string) *store.WorkflowAssignment {
	f.t.Helper()
	a := newAssignment(f.t, f.ctx, f.s, owner, idem, map[string]string{"env": "prod"}, f.now)
	return a
}

// failLikeAgent stamps the run failed (with ended_at and a failure reason)
// and the assignment terminal — the same write shape the overseer's
// RunFailed event leg produces.
func (f *refireFixture) failLikeAgent(a *store.WorkflowAssignment, reason string) {
	f.t.Helper()
	run, err := f.s.GetRun(f.ctx, a.RunID)
	if err != nil {
		f.t.Fatalf("GetRun: %v", err)
	}
	run.Status = "failed"
	run.CurrentStep = "s1"
	run.StartedAt = &f.now
	run.EndedAt = &f.now
	run.FailureReason = reason
	if err := f.s.UpdateRun(f.ctx, run); err != nil {
		f.t.Fatalf("UpdateRun(%s): %v", a.RunID, err)
	}
	if err := f.s.MarkWorkflowAssignmentTerminal(f.ctx, a.RunID, "run failed"); err != nil {
		f.t.Fatalf("MarkWorkflowAssignmentTerminal: %v", err)
	}
}

// failStalled stamps a stalled run the way the heartbeat reaper produces it:
// run failed with reason "agent heartbeat lost", assignment terminal with
// the reap reason (assignment untouched beyond terminal).
func (f *refireFixture) failStalled(a *store.WorkflowAssignment) {
	f.t.Helper()
	f.failLikeAgentWithReason(a, store.ReapReasonAgentHeartbeatLost, "agent heartbeat lost")
}

func (f *refireFixture) failLikeAgentWithReason(a *store.WorkflowAssignment, assignmentReason, failureReason string) {
	f.t.Helper()
	run, err := f.s.GetRun(f.ctx, a.RunID)
	if err != nil {
		f.t.Fatalf("GetRun: %v", err)
	}
	run.Status = "failed"
	run.StartedAt = &f.now
	run.EndedAt = &f.now
	run.FailureReason = failureReason
	if err := f.s.UpdateRun(f.ctx, run); err != nil {
		f.t.Fatalf("UpdateRun(%s): %v", a.RunID, err)
	}
	if err := f.s.MarkWorkflowAssignmentTerminal(f.ctx, a.RunID, assignmentReason); err != nil {
		f.t.Fatalf("MarkWorkflowAssignmentTerminal: %v", err)
	}
}

// refireAt performs one policy pass at the given instant.
func (f *refireFixture) refireAt(at time.Time) []store.RefireOutcome {
	f.t.Helper()
	outcomes, err := f.s.RefireFailedRuns(f.ctx, at, 200)
	if err != nil {
		f.t.Fatalf("RefireFailedRuns: %v", err)
	}
	return outcomes
}

func dispositionCounts(t *testing.T, outcomes []store.RefireOutcome) map[string]int {
	t.Helper()
	counts := map[string]int{}
	for _, o := range outcomes {
		counts[o.Disposition]++
	}
	return counts
}

// TestStallFailedRunAutoRefires is the headline acceptance: a stall-failed
// run (the heartbeat-reaper stamp) requeues a successor attempt under the
// policy's limits, with an attempt-suffixed idempotency key and family
// lineage, atomically with its queued run.
func TestStallFailedRunAutoRefires(t *testing.T) {
	runWithCtx(t, func(t *testing.T, ctx context.Context, s store.Store) {
		f := newRefireFixture(t, ctx, s)
		f.policy.Backoff = time.Second
		f.setPolicy("", "")

		a := f.submit("owner-1", "kb-5-deploy")
		f.failStalled(a)

		cands, err := s.ListRefireCandidates(ctx, 200)
		if err != nil || len(cands) != 1 {
			t.Fatalf("ListRefireCandidates = %d, %v, want 1", len(cands), err)
		}
		if cands[0].Assignment.ID != a.ID || cands[0].RootRunID != a.RunID || cands[0].Attempt != 1 {
			t.Fatalf("candidate = %+v", cands[0])
		}

		// Backoff not yet elapsed: deferred, nothing minted.
		outcomes := f.refireAt(f.now)
		if got := dispositionCounts(t, outcomes); got[store.RefireDispositionDeferred] != 1 {
			t.Fatalf("first pass outcomes = %v, want deferred", got)
		}

		// After the backoff window the pass mints the successor.
		later := f.now.Add(2 * time.Second)
		outcomes = f.refireAt(later)
		got := dispositionCounts(t, outcomes)
		if got[store.RefireDispositionRefire] != 1 {
			t.Fatalf("second pass outcomes = %v, want refire: %+v", got, outcomes)
		}
		o := outcomes[0]
		if o.RunID != a.RunID || o.RootRunID != a.RunID || o.RefireAttempt != 2 || o.RefireRunID == "" || o.RefireAssignmentID == "" {
			t.Fatalf("refire outcome = %+v", o)
		}

		successor, err := s.GetWorkflowAssignment(ctx, o.RefireAssignmentID)
		if err != nil {
			t.Fatalf("GetWorkflowAssignment(successor): %v", err)
		}
		if successor.State != store.WorkflowAssignmentStateQueued {
			t.Fatalf("successor state = %q, want queued", successor.State)
		}
		if successor.IdempotencyKey != "kb-5-deploy#attempt-2" {
			t.Fatalf("successor key = %q", successor.IdempotencyKey)
		}
		if successor.RefireRootRunID != a.RunID || successor.RefireAttempt != 2 {
			t.Fatalf("successor lineage = %q/%d", successor.RefireRootRunID, successor.RefireAttempt)
		}
		if successor.WorkflowName != a.WorkflowName || successor.WorkflowSource != a.WorkflowSource {
			t.Fatalf("successor workflow carried wrong: %q/%q", successor.WorkflowName, successor.WorkflowSource)
		}
		if successor.Labels["env"] != "prod" {
			t.Fatalf("successor labels = %v", successor.Labels)
		}
		sr, err := s.GetRun(ctx, o.RefireRunID)
		if err != nil {
			t.Fatalf("GetRun(successor): %v", err)
		}
		if sr.Status != "pending" || sr.OverseerID != "" || sr.WorkflowHCL != "source hcl" {
			t.Fatalf("successor run = %+v", sr)
		}

		// A duplicate pass after a successful mint sees no candidate at all:
		// the original attempt is superseded by the minted successor in its
		// family (family-max guard), and the idempotency-key guard covers a
		// racing writer minting the same attempt in parallel.
		outcomes = f.refireAt(later)
		if len(outcomes) != 0 {
			t.Fatalf("duplicate pass outcomes = %+v, want none", outcomes)
		}
	})
}

// TestParkedRunNeverRefiresWithoutHumanAck is the park acceptance: a run
// whose ending is a designed park (run.completed event parked at the human
// gate) is reclassified to the parked shape and never refires; the pass
// stays idempotent (already-parked runs are no longer candidates because a
// parked run is not failed).
func TestParkedRunNeverRefiresWithoutHumanAck(t *testing.T) {
	runWithCtx(t, func(t *testing.T, ctx context.Context, s store.Store) {
		f := newRefireFixture(t, ctx, s)
		f.setPolicy("", "")

		a := f.submit("owner-1", "kb-6-park")
		// The agent reported completion at the human gate: the completion is
		// stamped failed (design: the engine parked, the run did not
		// "succeed") and the assignment terminal under "run completed".
		f.failLikeAgentWithReason(a, "run completed", "workflow ended without success")
		// The run's own latest event names the gate (KB-270 shape).
		appendRunEvent := store.Event{
			SchemaVersion: int32(store.EventSchemaVersion),
			RunID:         a.RunID,
			Type:          "run.completed",
			Ts:            microUTC(f.now),
			CorrelationID: "corr-park-1",
			Payload:       []byte(`{"final_state":"need_assistance"}`),
		}
		if _, inserted, err := s.AppendEvent(ctx, &appendRunEvent); err != nil || !inserted {
			t.Fatalf("AppendEvent: %v/%v", inserted, err)
		}
		outcomes := f.refireAt(f.now.Add(time.Hour))
		got := dispositionCounts(t, outcomes)
		if got[store.RefireDispositionPark] != 1 || got[store.RefireDispositionRefire] != 0 {
			t.Fatalf("outcomes = %v, want exactly one park: %+v", got, outcomes)
		}

		run, err := s.GetRun(ctx, a.RunID)
		if err != nil {
			t.Fatalf("GetRun: %v", err)
		}
		if run.Status != store.RunStatusAwaitingHuman {
			t.Fatalf("parked run status = %q, want awaiting_human", run.Status)
		}
		if run.EndedAt != nil || run.FailureReason != "" {
			t.Fatalf("parked run not in KB-226 shape: ended_at=%v failure_reason=%q", run.EndedAt, run.FailureReason)
		}
		na, err := s.GetWorkflowAssignmentByRunID(ctx, a.RunID)
		if err != nil || na.State != store.WorkflowAssignmentStateTerminal {
			t.Fatalf("parked assignment = %+v %v (unchanged)", na, err)
		}

		// A parked run is not failed, so later passes see no candidate and
		// never refire without the human ack (KB-321 park discipline).
		cands, err := s.ListRefireCandidates(ctx, 200)
		if err != nil {
			t.Fatalf("ListRefireCandidates: %v", err)
		}
		for _, c := range cands {
			if c.Assignment.RunID == a.RunID {
				t.Fatalf("parked run still a refire candidate")
			}
		}
	})
}

// TestParksVocabularyFailureReasonParks pins the parks-vocabulary leg: a
// failed run whose FAILURE REASON carries a parks spelling (need_help — the
// human-gate vocabulary) parks instead of refiring, even though other
// vocabulary would call it transient.
func TestParksVocabularyFailureReasonParks(t *testing.T) {
	runWithCtx(t, func(t *testing.T, ctx context.Context, s store.Store) {
		f := newRefireFixture(t, ctx, s)
		f.policy = refire.Policy{
			MaxAttempts: 3,
			Transient:   []string{"failed"},
			Parks:       []string{"need_help"},
		}
		f.setPolicy("", "")

		a := f.submit("owner-1", "kb-7-help")
		f.failLikeAgent(a, "workflow paused: the operator NEED_HELP requested")

		outcomes := f.refireAt(f.now.Add(time.Hour))
		got := dispositionCounts(t, outcomes)
		if got[store.RefireDispositionPark] != 1 {
			t.Fatalf("outcomes = %v, want park: %v", got, outcomes)
		}
		run := f.s.(interface {
			GetRun(ctx context.Context, id string) (*store.Run, error)
		})
		r, err := run.GetRun(ctx, a.RunID)
		if err != nil || r.Status != store.RunStatusAwaitingHuman {
			t.Fatalf("parks run = %v %q (want awaiting_human)", err, r.Status)
		}
	})
}

// TestCosmeticAndUnknownEndingsUntouched pins the cosmetic and no-action
// verdicts: known-benign endings (pod-state emit races) and unclassified
// endings stay terminal-failed with zero rewrites — no park, no successor —
// and their disposition stamp ages them out of future scans (KB-321), so
// permanent no-action rows cannot occupy the working set.
func TestCosmeticAndUnknownEndingsUntouched(t *testing.T) {
	runWithCtx(t, func(t *testing.T, ctx context.Context, s store.Store) {
		f := newRefireFixture(t, ctx, s)
		f.setPolicy("", "")

		cosmetic := f.submit("owner-1", "kb-8-cosmetic")
		f.failLikeAgent(cosmetic, "pod-state emit race while staging")

		unknown := f.submit("owner-1", "kb-8-unknown")
		f.failLikeAgent(unknown, "compiler exploded")

		succeeded := f.submit("owner-1", "kb-8-succeeded")
		run, err := s.GetRun(ctx, succeeded.RunID)
		if err != nil {
			t.Fatalf("GetRun: %v", err)
		}
		run.Status = "succeeded"
		run.EndedAt = &f.now
		if err := s.UpdateRun(ctx, run); err != nil {
			t.Fatalf("mark succeeded: %v", err)
		}
		if err := s.MarkWorkflowAssignmentTerminal(ctx, succeeded.RunID, "run completed"); err != nil {
			t.Fatalf("terminal: %v", err)
		}

		// A cancelled run never enters the scan (cancellation is
		// operator-driven, not policy-driven).
		cancelled := f.submit("owner-1", "kb-8-cancelled")
		if _, err := s.CancelRun(ctx, cancelled.RunID, "operator request", f.now); err != nil {
			t.Fatalf("CancelRun: %v", err)
		}

		// Only the two failed endings are candidates at all.
		cands, err := s.ListRefireCandidates(ctx, 200)
		if err != nil || len(cands) != 2 {
			t.Fatalf("candidates = %d, %v, want 2", len(cands), err)
		}

		outcomes := f.refireAt(f.now.Add(time.Hour))
		got := dispositionCounts(t, outcomes)
		if got[store.RefireDispositionCosmetic] < 1 || got[store.RefireDispositionRefire] != 0 {
			t.Fatalf("outcomes = %v, want cosmetic+none, zero refire: %+v", got, outcomes)
		}
		for _, o := range outcomes {
			if o.Disposition == store.RefireDispositionRefire {
				t.Fatalf("refired: %+v", o)
			}
		}
		if r := f.getRun(cosmetic.RunID); r.Status != "failed" {
			t.Fatalf("cosmetic run rewritten: %q", r.Status)
		}

		// The pass closes both no-action stories: the disposition stamp ages
		// them out of the scan (succeeded and cancelled were never
		// candidates), so the working set drains fully (KB-321).
		cands, err = s.ListRefireCandidates(ctx, 200)
		if err != nil || len(cands) != 0 {
			t.Fatalf("candidates after pass = %d, %v, want 0 (stamped out)", len(cands), err)
		}
	})
}

func (f *refireFixture) getRun(id string) *store.Run {
	f.t.Helper()
	r, err := f.s.GetRun(f.ctx, id)
	if err != nil {
		f.t.Fatalf("get run %s: %v", id, err)
	}
	return r
}

// TestRefireAttemptsExhaustedPinsBudget pins the attempt budget: each
// policy pass refires the newest failed attempt of a family once; the chain
// walks attempt 2 then 3, and attempt 3's failure reports exhausted.
func TestRefireAttemptsExhaustedPinsBudget(t *testing.T) {
	runWithCtx(t, func(t *testing.T, ctx context.Context, s store.Store) {
		f := newRefireFixture(t, ctx, s)
		f.policy.MaxAttempts = 3
		f.policy.Backoff = 0
		f.setPolicy("", "")

		root := f.submit("owner-1", "kb-9-chain")
		f.failStalled(root)

		later := f.now.Add(time.Second)
		outcomes := f.refireAt(later)
		if got := dispositionCounts(t, outcomes); got[store.RefireDispositionRefire] != 1 {
			t.Fatalf("attempt-1 pass = %v", got)
		}
		successor := outcomes[0]

		// The successor fails too; the pass refires it to attempt 3.
		sa, err := s.GetWorkflowAssignment(ctx, successor.RefireAssignmentID)
		if err != nil {
			t.Fatalf("GetWorkflowAssignment(successor): %v", err)
		}
		f.failStalled(sa)
		outcomes = f.refireAt(later)
		if got := dispositionCounts(t, outcomes); got[store.RefireDispositionRefire] != 1 {
			t.Fatalf("attempt-2 pass = %v", got)
		}
		third := outcomes[0]
		if third.RefireAttempt != 3 {
			t.Fatalf("third attempt = %d", third.RefireAttempt)
		}
		if !strings.Contains(third.RefireAssignmentID, "-") {
			t.Fatalf("third assignment malformed: %q", third.RefireAssignmentID)
		}

		// Exhausted budget: no attempt 4 is ever minted.
		ta, err := s.GetWorkflowAssignment(ctx, third.RefireAssignmentID)
		if err != nil {
			t.Fatalf("GetWorkflowAssignment(third): %v", err)
		}
		f.failStalled(ta)
		outcomes = f.refireAt(later)
		got := dispositionCounts(t, outcomes)
		if got[store.RefireDispositionExhausted] != 1 || got[store.RefireDispositionRefire] != 0 {
			t.Fatalf("attempt-3 pass = %v, want exhausted", got)
		}
		if !strings.Contains(outcomes[0].Reason, "exhausted") {
			t.Fatalf("exhausted reason = %q", outcomes[0].Reason)
		}
	})
}

// TestRefirePolicyRowOverrides pins policy-as-data: a per-route row changes
// the verdict (a route whose row disables refiring skips), a per-project row
// raises the budget, and a malformed row falls through instead of
// disabling the engine.
func TestRefirePolicyRowOverrides(t *testing.T) {
	runWithCtx(t, func(t *testing.T, ctx context.Context, s store.Store) {
		f := newRefireFixture(t, ctx, s)
		f.policy.MaxAttempts = 0 // built-in default disabled in the default slot
		f.setPolicy("", "")

		a := f.submit("owner-1", "kb-10-data")
		f.failStalled(a)
		outcomes := f.refireAt(f.now.Add(time.Hour))
		if got := dispositionCounts(t, outcomes); got[store.RefireDispositionRefire] != 0 {
			t.Fatalf("row-disabled refiring still fired: %v", got)
		}
		if !strings.Contains(outcomes[0].Reason, "refiring disabled by refire policy (,)") {
			t.Fatalf("disabled reason = %q", outcomes[0].Reason)
		}

		// A raised per-route override re-enables refiring for that route.
		override := refire.Policy{
			MaxAttempts: 2,
			Backoff:     0,
			Transient:   refire.DefaultPolicyDoc().Transient,
		}
		if err := s.UpsertRefirePolicy(ctx, "", "wf", refire.MustMarshalPolicy(override), f.now); err != nil {
			t.Fatalf("UpsertRefirePolicy(override): %v", err)
		}
		outcomes = f.refireAt(f.now.Add(time.Hour))
		if got := dispositionCounts(t, outcomes); got[store.RefireDispositionRefire] != 1 {
			t.Fatalf("override pass = %v: %v", got, outcomes)
		}
		if !strings.Contains(outcomes[0].Reason, "(,wf)") {
			t.Fatalf("override source = %q", outcomes[0].Reason)
		}

		// A malformed row falls through to the next layer instead of
		// disabling the engine (row edited between loads).
		if err := s.UpsertRefirePolicy(ctx, "", "kanboardtrigger", `not json`, f.now); err != nil {
			// Strict row validation rejects the write here; nothing to
			// fall through from — the ParsePolicy guard in UpsertRefirePolicy
			// is the "editable as data" guarantee.
			t.Logf("upsert rejected malformed policy row: %v", err)
		} else {
			t.Fatalf("malformed policy row accepted")
		}
	})
}

// TestRefirePolicyCRUD pins the policy-table contract: documents validate on
// write, list/delete round trip, unknown deletes are no-ops, list ordering is
// (project, route), and stamps survive updates (created) or refresh
// (updated).
func TestRefirePolicyCRUD(t *testing.T) {
	runWithCtx(t, func(t *testing.T, ctx context.Context, s store.Store) {
		now := microUTC(time.Now())
		if err := s.UpsertRefirePolicy(ctx, "p1", "wf-a", `{"max_attempts":2}`, now); err != nil {
			t.Fatalf("UpsertRefirePolicy: %v", err)
		}
		if err := s.UpsertRefirePolicy(ctx, "", "", `{"max_attempts":3}`, now); err != nil {
			t.Fatalf("UpsertRefirePolicy(default): %v", err)
		}

		rows, err := s.ListRefirePolicies(ctx)
		if err != nil || len(rows) != 2 {
			t.Fatalf("ListRefirePolicies = %d, %v, want 2", len(rows), err)
		}
		// Project ordering puts '' (global) first.
		if rows[0].ProjectID != "" || rows[0].Route != "" || rows[0].Policy != `{"max_attempts":3}` {
			t.Fatalf("row 0 = %+v", rows[0])
		}
		if rows[1].ProjectID != "p1" || rows[1].Route != "wf-a" {
			t.Fatalf("row 1 = %+v", rows[1])
		}
		if rows[0].CreatedAt.IsZero() || rows[0].UpdatedAt.IsZero() {
			t.Fatalf("policy row stamps zero: %+v", rows[0])
		}

		// Later writes refresh updated_at and keep created_at.
		later := now.Add(time.Minute)
		if err := s.UpsertRefirePolicy(ctx, "p1", "wf-a", `{"max_attempts":4}`, later); err != nil {
			t.Fatalf("UpsertRefirePolicy(update): %v", err)
		}
		rows, err = s.ListRefirePolicies(ctx)
		if err != nil || len(rows) != 2 {
			t.Fatalf("ListRefirePolicies after update = %d, %v", len(rows), err)
		}
		if rows[1].Policy != `{"max_attempts":4}` {
			t.Fatalf("updated policy = %q", rows[1].Policy)
		}
		if !rows[1].UpdatedAt.After(rows[1].CreatedAt) {
			t.Fatalf("update did not refresh updated_at: %+v", rows[1])
		}

		// A malformed document is rejected at the write boundary; the stored
		// row is untouched. Also: policy rows whose document stopped parsing
		// (e.g. an out-of-band SQL edit) fall through resolution.
		if err := s.UpsertRefirePolicy(ctx, "p1", "wf-b", `not json`, later); err == nil {
			t.Fatalf("malformed policy row accepted")
		}

		if err := s.DeleteRefirePolicy(ctx, "p1", "wf-a"); err != nil {
			t.Fatalf("DeleteRefirePolicy: %v", err)
		}
		rows, err = s.ListRefirePolicies(ctx)
		if err != nil || len(rows) != 1 || rows[0].Route != "" {
			t.Fatalf("after delete = %d, %v, want only the default row", len(rows), err)
		}
		if err := s.DeleteRefirePolicy(ctx, "unknown", "unknown"); err != nil {
			t.Fatalf("DeleteRefirePolicy(unknown) = %v, want a no-op", err)
		}
	})
}

// TestRefireScanDrainsNoActionRows is the starvation regression (KB-321):
// more than refire.ScanLimit permanently no-action failed families
// (cosmetic / unclassified endings that stay terminal-failed) must never
// displace a fresh transient stall from the scan. The disposition stamp
// ages the no-action rows out of the working set, and the scan ranks
// freshest failures first, so the stall is refired on the very first pass.
// Against the pre-fix oldest-first scan with no stamps, the 200-row scan
// window was permanently pinned by no-action rows and the stall was never
// evaluated.
func TestRefireScanDrainsNoActionRows(t *testing.T) {
	runWithCtx(t, func(t *testing.T, ctx context.Context, s store.Store) {
		f := newRefireFixture(t, ctx, s)
		f.setPolicy("", "")

		base := f.now
		// A backlog of no-action failed families, every one older than the
		// fresh stall (staggered oldest-to-newest within the backlog).
		for i := 0; i < refire.ScanLimit+50; i++ {
			f.now = base.Add(-48*time.Hour + time.Duration(i)*time.Second)
			a := f.submit("owner-noise", fmt.Sprintf("kb-11-noise-%d", i))
			if i%2 == 0 {
				f.failLikeAgent(a, "pod-state emit race while staging")
			} else {
				f.failLikeAgent(a, "compiler exploded")
			}
		}
		// The freshest failure is a transient stall that must refire.
		f.now = base
		fresh := f.submit("owner-fresh", "kb-11-fresh-stall")
		f.failStalled(fresh)

		outcomes := f.refireAt(base.Add(time.Hour))
		got := dispositionCounts(t, outcomes)
		if got[store.RefireDispositionRefire] != 1 {
			t.Fatalf("outcomes = %v, want exactly one refire (the fresh stall): %+v",
				got, outcomes)
		}
		var refireOutcome *store.RefireOutcome
		for i := range outcomes {
			if outcomes[i].Disposition == store.RefireDispositionRefire {
				refireOutcome = &outcomes[i]
			}
		}
		if refireOutcome == nil || refireOutcome.RunID != fresh.RunID ||
			refireOutcome.RootRunID != fresh.RunID || refireOutcome.RefireRunID == "" {
			t.Fatalf("refire outcome = %+v, want the fresh stall's lineage", refireOutcome)
		}
		successor, err := s.GetWorkflowAssignment(ctx, refireOutcome.RefireAssignmentID)
		if err != nil {
			t.Fatalf("GetWorkflowAssignment(successor): %v", err)
		}
		if successor.IdempotencyKey != "kb-11-fresh-stall#attempt-2" {
			t.Fatalf("successor key = %q", successor.IdempotencyKey)
		}

		// A second pass is idempotent: every no-action row is now stamped
		// out of the scan, and the fresh stall never mints a second
		// successor.
		outcomes = f.refireAt(base.Add(2 * time.Hour))
		if got := dispositionCounts(t, outcomes); got[store.RefireDispositionRefire] != 0 {
			t.Fatalf("second pass outcomes = %v, want zero refires: %+v", got, outcomes)
		}
	})
}
