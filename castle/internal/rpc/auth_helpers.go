package rpc

import (
	"context"
	"errors"

	"connectrpc.com/connect"

	"github.com/brokenbots/castle/castle/internal/auth"
	"github.com/brokenbots/castle/castle/internal/store"
)

// requireCaller enforces that the authenticated caller is the same as
// requestedCriteriaID (when the latter is non-empty). It returns the caller ID
// on success.
//
// When the auth interceptor is not wired (CallerCriteriaID returns ""), the
// check is skipped and requestedCriteriaID is returned as the effective ID.
// This preserves backward compat for direct-call tests that don't go through
// the HTTP stack. In production the interceptor is always present.
func requireCaller(ctx context.Context, requestedCriteriaID string) (string, error) {
	callerID := auth.CallerCriteriaID(ctx)
	if callerID == "" {
		// Interceptor not wired (direct-call path in tests).
		return requestedCriteriaID, nil
	}
	if requestedCriteriaID != "" && callerID != requestedCriteriaID {
		return "", connect.NewError(connect.CodePermissionDenied, errors.New("caller does not own target criteria agent"))
	}
	return callerID, nil
}

// requireCallerOwnsRun fetches the run and asserts that the authenticated caller
// is the run's owning criteria agent. It returns the caller ID and the fetched
// run so callers can reuse them without a second store lookup.
//
// When CallerCriteriaID returns "" (interceptor not wired) the ownership check
// is skipped for backward compat with direct-call tests.
func requireCallerOwnsRun(ctx context.Context, st store.Store, runID string) (callerID string, run *store.Run, err error) {
	run, err = st.GetRun(ctx, runID)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return "", nil, connect.NewError(connect.CodeNotFound, errors.New("run not found"))
		}
		return "", nil, connect.NewError(connect.CodeInternal, err)
	}
	callerID = auth.CallerCriteriaID(ctx)
	if callerID != "" && callerID != run.OverseerID {
		return "", nil, connect.NewError(connect.CodePermissionDenied, errors.New("caller does not own this run"))
	}
	return callerID, run, nil
}
