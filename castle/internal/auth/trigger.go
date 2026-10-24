package auth

import (
	"context"

	"github.com/brokenbots/castle/castle/internal/store"
)

// callerTriggerIDKey is the context key for the authenticated trigger
// identity ID (KB-320). Trigger identities are distinct from agent and
// orchestrator identities: CallerCriteriaID and CallerOrchestratorID stay
// empty for triggers, so handlers keyed on those never treat a trigger as a
// board-side agent. Triggers are submit-only board adapters: their
// procedure allowlist is fixed in authorizeTriggerProcedure.
type callerTriggerIDKey struct{}

// CallerTriggerID returns the trigger identity ID injected by
// AuthInterceptor, or "" when the caller is not a trigger (agent or
// orchestrator token, console session, exempt procedure, or direct handler
// calls in tests without the interceptor wired).
func CallerTriggerID(ctx context.Context) string {
	v, _ := ctx.Value(callerTriggerIDKey{}).(string)
	return v
}

// WithCallerTriggerID returns a context with the given trigger identity ID
// injected as the authenticated caller. Use in tests that call handlers
// directly (no HTTP stack) to simulate the identity the interceptor would
// inject.
func WithCallerTriggerID(ctx context.Context, id string) context.Context {
	return context.WithValue(ctx, callerTriggerIDKey{}, id)
}

// AssignmentOwnerID returns the identity that owns workflow assignments
// submitted on this call (KB-320): the criteria agent ID for agent callers,
// or the trigger ID for trigger callers. Assignment rows key ownership on a
// criteria-space ID (assignments.idempotency_key is UNIQUE per owner), so a
// trigger submitting board-lane work owns its assignments through the same
// identity namespace as agents — its submit is idempotently isolated from
// agent submissions by the trigger's own ID. Non-agent, non-trigger callers
// (orchestrators, console sessions) resolve to "" and are rejected by the
// handlers.
func AssignmentOwnerID(ctx context.Context) string {
	if id := CallerCriteriaID(ctx); id != "" {
		return id
	}
	return CallerTriggerID(ctx)
}

// ResolveTriggerToken returns the Trigger whose accept token matches, or nil
// when no trigger token matches (KB-320). Compares against persisted SHA-256
// hashes with constant-time equality like agent and orchestrator token
// resolution.
func ResolveTriggerToken(ctx context.Context, st store.Store, token string) (*store.Trigger, error) {
	list, err := st.ListTriggers(ctx)
	if err != nil {
		return nil, err
	}
	for _, t := range list {
		if ConstantTimeEqual(token, t.TokenHash) {
			return t, nil
		}
	}
	return nil, nil
}
