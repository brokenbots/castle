// Operator context + resolution (KB-330): the ONE operator model behind
// every credential class. Privileges derive from the operator's attributes
// (role), never from the credential class the identity arrived through. A
// legacy credential (agent token, orchestrator token, trigger token,
// console session) resolves onto its operator row via the operator id the
// alias columns and the login sessions carry; a token-only operator that
// predates its alias row is synthesized from the class's role so legacy
// tokens keep their exact pre-operators effective scope.
package auth

import (
	"context"
	"errors"

	"github.com/brokenbots/castle/castle/internal/store"
)

// callerOperatorKey is the context key for the authenticated caller's
// operator identity.
type callerOperatorKey struct{}

// WithCallerOperator returns a context with the given operator injected as
// the authenticated caller. The interceptor injects it for every resolved
// credential (operator token, agent token, orchestrator token, trigger
// token, console session). It stays nil for anonymous callers and for the
// direct-call paths (tests) that inject only the legacy kind keys, which
// keep the pre-operators authorization fallbacks.
func WithCallerOperator(ctx context.Context, op *store.Operator) context.Context {
	return context.WithValue(ctx, callerOperatorKey{}, op)
}

// CallerOperator returns the operator injected by AuthInterceptor, or nil
// when the caller carries no operator identity.
func CallerOperator(ctx context.Context) *store.Operator {
	op, _ := ctx.Value(callerOperatorKey{}).(*store.Operator)
	return op
}

// CallerOperatorRole reports the caller's role attribute: the operator's
// role when an operator identity is present, else the role implied by the
// legacy credential kind keys (direct-call paths), else "".
func CallerOperatorRole(ctx context.Context) string {
	if op := CallerOperator(ctx); op != nil {
		return op.Role()
	}
	switch {
	case CallerConsoleUserID(ctx) != "":
		return store.OperatorRoleAdmin
	case CallerCriteriaID(ctx) != "":
		return store.OperatorRoleAgent
	case CallerOrchestratorID(ctx) != "":
		return store.OperatorRoleOrchestrator
	case CallerTriggerID(ctx) != "":
		return store.OperatorRoleTrigger
	}
	return ""
}

// ResolveOperatorToken returns the operator token whose persisted hash
// matches, or nil when none matches (KB-330). Compares against persisted
// SHA-256 hex digests with constant-time equality like every other token
// space.
func ResolveOperatorToken(ctx context.Context, st store.Store, token string) (*store.OperatorToken, error) {
	tokens, err := st.ListOperatorTokens(ctx)
	if err != nil {
		return nil, err
	}
	hash := HashToken(token)
	for _, tok := range tokens {
		if ConstantTimeEqual(hash, tok.TokenHash) {
			return tok, nil
		}
	}
	return nil, nil
}

// RoleAttribute builds an attributes map carrying just the given role.
func RoleAttribute(role string) map[string]string {
	return map[string]string{store.OperatorAttributeRole: role}
}

// operatorForIdentity loads the operator behind a resolved legacy identity
// (KB-330): the alias operator row with the same id. A row that predates
// the alias (or a store that has not carried it) synthesizes the class role
// so the legacy token keeps its exact effective scope rather than falling
// to least-privilege.
func operatorForIdentity(ctx context.Context, st store.Store, id, name, role string) (*store.Operator, error) {
	op, err := st.GetOperator(ctx, id)
	switch {
	case err == nil:
		return op, nil
	case errors.Is(err, store.ErrNotFound):
		return &store.Operator{
			ID:          id,
			Name:        name,
			DisplayName: name,
			Attributes:  RoleAttribute(role),
		}, nil
	default:
		return nil, err
	}
}
