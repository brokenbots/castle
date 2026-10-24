package rpc

import (
	"context"
	"time"

	"github.com/brokenbots/castle/castle/internal/store"
)

const (
	// refireScanLimit bounds each policy pass's candidate scan. Failed runs
	// older than this stay eligible for later passes (candidates are scanned
	// oldest-failure-first), so the limit is throughput shaping, not
	// correctness (KB-321).
	refireScanLimit = 200
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
