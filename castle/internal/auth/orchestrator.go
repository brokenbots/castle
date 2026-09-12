package auth

import (
	"context"

	"github.com/brokenbots/castle/castle/internal/store"
)

// callerOrchestratorIDKey is the context key for the authenticated
// orchestrator identity ID (CRI-133). Orchestrator identities are distinct
// from agent identities: CallerCriteriaID stays empty for orchestrators, so
// handlers keyed on CallerCriteriaID never treat an orchestrator as an agent.
type callerOrchestratorIDKey struct{}

// CallerOrchestratorID returns the orchestrator identity ID injected by
// AuthInterceptor, or "" when the caller is not an orchestrator (agent token,
// exempt procedure, or direct handler calls in tests without the interceptor
// wired).
func CallerOrchestratorID(ctx context.Context) string {
	v, _ := ctx.Value(callerOrchestratorIDKey{}).(string)
	return v
}

// WithCallerOrchestratorID returns a context with the given orchestrator
// identity ID injected as the authenticated caller. Use in tests that call
// handlers directly (no HTTP stack) to simulate the identity the interceptor
// would inject.
func WithCallerOrchestratorID(ctx context.Context, id string) context.Context {
	return context.WithValue(ctx, callerOrchestratorIDKey{}, id)
}

// ResolveOrchestratorToken returns the Orchestrator whose accept token
// matches, or nil when no orchestrator token matches (CRI-133). Compares
// against persisted SHA-256 hashes with constant-time equality like agent
// token resolution.
func ResolveOrchestratorToken(ctx context.Context, st store.Store, token string) (*store.Orchestrator, error) {
	list, err := st.ListOrchestrators(ctx)
	if err != nil {
		return nil, err
	}
	for _, o := range list {
		if ConstantTimeEqual(token, o.TokenHash) {
			return o, nil
		}
	}
	return nil, nil
}
