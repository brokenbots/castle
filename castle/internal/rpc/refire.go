package rpc

import (
	"context"
	"time"

	"github.com/brokenbots/castle/castle/internal/store"
	"github.com/brokenbots/castle/castle/internal/store/refire"
)

const (
	// refireScanLimit bounds each policy pass's candidate scan to
	// refire.ScanLimit (freshest failure first). The bound is a batch size,
	// not a cap on eligibility: rows evaluated to a final disposition carry
	// a non-NULL runs.refire_disposition stamp and age out of the candidate
	// set, so the working set drains as verdicts land (KB-321) and no
	// family is indefinitely displaced from the scan.
	refireScanLimit = refire.ScanLimit
)

// RefireFailedRunsNow evaluates the refire policy for currently-eligible
// failed runs (KB-321) and dispatches the successor assignments it mints to
// connected eligible agents, mirroring ExpireLeasesNow. Parked runs are
// never re-dispatched (they must pass their human gate first); deferred,
// exhausted, skipped, and cosmetic verdicts are reported without action.
func (s *ServerServer) RefireFailedRunsNow(ctx context.Context) ([]store.RefireOutcome, error) {
	outcomes, err := s.Store.RefireFailedRuns(ctx, time.Now().UTC(), refireScanLimit)
	if err != nil {
		return nil, err
	}
	for _, o := range outcomes {
		if o.Disposition != store.RefireDispositionRefire || o.RefireAssignmentID == "" {
			continue
		}
		a, err := s.Store.GetWorkflowAssignment(ctx, o.RefireAssignmentID)
		if err != nil {
			s.Log.Debug("refire failed runs: cannot load successor assignment",
				"assignment_id", o.RefireAssignmentID, "err", err)
			continue
		}
		s.dispatchAssignment(ctx, a)
	}
	// Requeued dispatch-layer reaps (KB-343): the restored SAME assignment is
	// handed to the connected-eligible dispatch path, mirroring the
	// ExpireLeasesNow symmetry — a requeued row should not wait for the next
	// control stream event to reach a connected agent.
	for _, o := range outcomes {
		if o.Disposition != store.RefireDispositionRequeue {
			continue
		}
		a, err := s.Store.GetWorkflowAssignmentByRunID(ctx, o.RunID)
		if err != nil {
			s.Log.Debug("refire failed runs: cannot load requeued assignment",
				"run_id", o.RunID, "err", err)
			continue
		}
		if a.State == store.WorkflowAssignmentStateQueued {
			s.dispatchAssignment(ctx, a)
		}
	}
	return outcomes, nil
}

// RunRefirePass performs one policy pass of the refire goroutine,
// translating refire verdicts into action and log lines: refired runs
// dispatch through the normal assignment pipeline, parked runs are logged
// with their reason, and deferred/exhausted/skipped verdicts are debug
// noise (KB-321). Failed passes log and are retried on the next tick.
func (s *ServerServer) RunRefirePass(ctx context.Context) {
	outcomes, err := s.RefireFailedRunsNow(ctx)
	if err != nil {
		s.Log.Error("refire: policy pass failed", "err", err)
		return
	}
	for _, o := range outcomes {
		switch o.Disposition {
		case store.RefireDispositionRefire:
			s.Log.Info("refire: requeued failed run under policy",
				"run_id", o.RunID, "attempt", o.RefireAttempt,
				"root_run_id", o.RootRunID, "refire_run_id", o.RefireRunID,
				"reason", o.Reason)
		case store.RefireDispositionRequeue:
			// KB-343: a dispatch-layer reap restored its same attempt —
			// operator-visible at Info because this is the pass proving the
			// attempt budget was NOT consumed by a transport condition.
			s.Log.Info("refire: requeued dispatch-layer reap, attempt budget untouched",
				"run_id", o.RunID, "root_run_id", o.RootRunID, "reason", o.Reason)
		case store.RefireDispositionPark:
			s.Log.Info("refire: parked failed run at human gate",
				"run_id", o.RunID, "root_run_id", o.RootRunID, "reason", o.Reason)
		case store.RefireDispositionDeferred:
			s.Log.Debug("refire: deferred failed evaluation",
				"run_id", o.RootRunID, "reason", o.Reason)
		case store.RefireDispositionExhausted:
			s.Log.Debug("refire: attempts exhausted",
				"run_id", o.RootRunID, "reason", o.Reason)
		default:
			s.Log.Debug("refire: skipped candidate",
				"run_id", o.RootRunID, "reason", o.Reason)
		}
	}
}
