package auth

import (
	"context"
	"errors"
	"net/http"
	"strings"

	"connectrpc.com/connect"

	"github.com/brokenbots/castle/castle/internal/store"
	criteria "github.com/brokenbots/criteria/sdk"
	"github.com/brokenbots/criteria/sdk/pb/criteria/v1/criteriav1connect"
)

// callerCriteriaIDKey is the context key for the authenticated caller's criteria agent ID.
type callerCriteriaIDKey struct{}

// CallerCriteriaID returns the criteria agent ID injected by AuthInterceptor,
// or "" if the request was not authenticated (e.g. exempt procedures or
// direct handler calls in tests without the interceptor wired).
func CallerCriteriaID(ctx context.Context) string {
	v, _ := ctx.Value(callerCriteriaIDKey{}).(string)
	return v
}

// WithCallerCriteriaID returns a context with the given criteria agent ID
// injected as the authenticated caller. Use in tests that call handlers
// directly (no HTTP stack) to simulate the identity the interceptor would
// inject.
func WithCallerCriteriaID(ctx context.Context, id string) context.Context {
	return context.WithValue(ctx, callerCriteriaIDKey{}, id)
}

// readOnlyServerProcedures are the read-only ServerService observation
// surfaces: the calls exempted by dev-mode anonymous reads
// (--allow-anon-reads). InspectRun is included (CRI-194): it is a read-only
// inspection of run state, and omitting it made the Parapet run detail page
// the only page whose data call rejected a token every other page accepted.
// Per-caller authorization on InspectRun (caller-owns-run) stays enforceable
// because presented tokens are validated even on this surface — see
// authenticateAnonReadToken.
var readOnlyServerProcedures = map[string]struct{}{
	criteriav1connect.ServerServiceListAgentsProcedure:    {},
	criteriav1connect.ServerServiceGetAgentProcedure:      {},
	criteriav1connect.ServerServiceListRunsProcedure:      {},
	criteriav1connect.ServerServiceGetRunProcedure:        {},
	criteriav1connect.ServerServiceListRunEventsProcedure: {},
	criteriav1connect.ServerServiceWatchRunProcedure:      {},
	criteriav1connect.ServerServiceInspectRunProcedure:    {},
}

// orchestratorProcedures are the OrchestratorService RPCs (CRI-133). They are
// read-only observation surfaces for the operator reconcile loop.
var orchestratorProcedures = map[string]struct{}{
	criteriav1connect.OrchestratorServiceSubscribeRunEventsProcedure: {},
	criteriav1connect.OrchestratorServiceListActiveRunsProcedure:     {},
}

// orchestratorWriteProcedures are OrchestratorService RPCs that mutate run
// state on the operator's behalf (CRI-142 CancelRun). Orchestrator and agent
// identities may invoke them, but — unlike the read surfaces above — they are
// never exempted by dev-mode anonymous reads, so unauthenticated callers
// cannot mutate run state even with --allow-anon-reads.
var orchestratorWriteProcedures = map[string]struct{}{
	criteriav1connect.OrchestratorServiceCancelRunProcedure: {},
}

// isOrchestratorAllowed reports whether an orchestrator identity may invoke
// the procedure (CRI-133). Orchestrators get the read-only ServerService
// surface, OrchestratorService (including CRI-142 CancelRun), and nothing
// else: every agent-owned write procedure (CriteriaService, ServerService
// writes) is denied.
func isOrchestratorAllowed(procedure string) bool {
	if _, ok := orchestratorProcedures[procedure]; ok {
		return true
	}
	if _, ok := orchestratorWriteProcedures[procedure]; ok {
		return true
	}
	if _, ok := readOnlyServerProcedures[procedure]; ok {
		return true
	}
	return false
}

// InterceptorOption configures an AuthInterceptor.
type InterceptorOption func(*AuthInterceptor)

// WithBootstrapToken configures the raw bootstrap token required for Register.
// The interceptor hashes it and validates incoming X-Server-Bootstrap headers.
// If not set, Register is disabled (returns Unimplemented).
func WithBootstrapToken(token string) InterceptorOption {
	return func(i *AuthInterceptor) {
		if token != "" {
			i.bootstrapTokenHash = HashToken(token)
		}
	}
}

// WithAnonRegister allows Register without a bootstrap token. Use only in dev
// mode (--dev-allow-anon-register flag) or in tests.
func WithAnonRegister() InterceptorOption {
	return func(i *AuthInterceptor) {
		i.allowAnonRegister = true
	}
}

// AuthInterceptor authenticates Connect calls using criteria agent tokens.
type AuthInterceptor struct {
	store              store.Store
	allowAnonReads     bool
	bootstrapTokenHash string // SHA-256 hex of the bootstrap token; empty = Register disabled
	allowAnonRegister  bool   // dev mode: bypass bootstrap check on Register
}

// NewInterceptor creates an AuthInterceptor. Options are applied in order and
// may override defaults. This function is variadic for backward compatibility;
// existing callers that pass only (store, allowAnonReads) continue to work.
func NewInterceptor(st store.Store, allowAnonReads bool, opts ...InterceptorOption) *AuthInterceptor {
	i := &AuthInterceptor{store: st, allowAnonReads: allowAnonReads}
	for _, opt := range opts {
		opt(i)
	}
	return i
}

func (i *AuthInterceptor) WrapUnary(next connect.UnaryFunc) connect.UnaryFunc {
	return func(ctx context.Context, req connect.AnyRequest) (connect.AnyResponse, error) {
		if req.Spec().Procedure == criteria.RegisterProcedure {
			return i.handleRegister(ctx, req, next)
		}
		if i.isExempt(req.Spec().Procedure) {
			newCtx, err := i.authenticateAnonReadToken(ctx, req.Header(), req.Spec().Procedure)
			if err != nil {
				return nil, err
			}
			return next(newCtx, req)
		}
		newCtx, err := i.authenticateHeaders(ctx, req.Header())
		if err != nil {
			return nil, err
		}
		if err := authorizeOrchestratorProcedure(newCtx, req.Spec().Procedure); err != nil {
			return nil, err
		}
		return next(newCtx, req)
	}
}

func (i *AuthInterceptor) WrapStreamingClient(next connect.StreamingClientFunc) connect.StreamingClientFunc {
	return next
}

func (i *AuthInterceptor) WrapStreamingHandler(next connect.StreamingHandlerFunc) connect.StreamingHandlerFunc {
	return func(ctx context.Context, conn connect.StreamingHandlerConn) error {
		if i.isExempt(conn.Spec().Procedure) {
			newCtx, err := i.authenticateAnonReadToken(ctx, conn.RequestHeader(), conn.Spec().Procedure)
			if err != nil {
				return err
			}
			return next(newCtx, conn)
		}
		newCtx, err := i.authenticateHeaders(ctx, conn.RequestHeader())
		if err != nil {
			return err
		}
		if err := authorizeOrchestratorProcedure(newCtx, conn.Spec().Procedure); err != nil {
			return err
		}
		return next(newCtx, conn)
	}
}

// authorizeOrchestratorProcedure enforces the orchestrator auth boundary
// (CRI-133): an orchestrator identity may invoke only the read-only
// observation procedures. Agent-owned writes must be rejected so an operator
// token can never be used to mutate agent state. Agent callers pass through
// (CallerOrchestratorID is empty for them).
func authorizeOrchestratorProcedure(ctx context.Context, procedure string) error {
	if CallerOrchestratorID(ctx) == "" {
		return nil
	}
	if isOrchestratorAllowed(procedure) {
		return nil
	}
	return connect.NewError(connect.CodePermissionDenied, errors.New("orchestrator identities cannot invoke agent-owned procedures"))
}

// anonReadableProcedure reports whether the procedure belongs to the
// dev-mode anonymous-read surface: the read-only ServerService reads and the
// OrchestratorService subscription APIs. Every member of this set is also in
// the orchestrator-allowed set, so identity injection here never needs the
// orchestrator boundary re-check.
func anonReadableProcedure(procedure string) bool {
	if _, ok := readOnlyServerProcedures[procedure]; ok {
		return true
	}
	_, ok := orchestratorProcedures[procedure]
	return ok
}

// authenticateAnonReadToken optionally authenticates a presented token on
// the dev-mode anonymous-read surface (CRI-194). A caller without a token
// keeps the anonymous pass-through --allow-anon-reads promises. A presented
// token must be valid: its identity is injected so per-caller authorization
// stays enforceable (InspectRun requires run ownership), and an invalid
// token is rejected as unauthenticated instead of silently succeeding. That
// last property is what lets the Parapet login gate's token probe (a
// ListAgents call carrying the candidate token) actually detect a bad token
// rather than accept anything and trap the UI in a login loop on a
// deep-linked route. Health and reflection exemptions are not anon-readable
// surfaces and stay fully unauthenticated.
func (i *AuthInterceptor) authenticateAnonReadToken(ctx context.Context, h http.Header, procedure string) (context.Context, error) {
	if !i.allowAnonReads || !anonReadableProcedure(procedure) {
		return ctx, nil
	}
	if _, ok := TokenFromHeaders(h); !ok {
		return ctx, nil
	}
	return i.authenticateHeaders(ctx, h)
}

// authenticateHeaders validates the token and returns a context with the
// caller's identity injected: criteria agent ID for agent tokens,
// orchestrator ID for orchestrator tokens (CRI-133). Agent tokens take
// precedence when both match: the per-table token-hash UNIQUE indexes cannot
// detect identical token material across tables, so in that (operator
// misconfiguration) case the token acts as an agent and the orchestrator
// identity it shadows is unreachable.
func (i *AuthInterceptor) authenticateHeaders(ctx context.Context, h http.Header) (context.Context, error) {
	tok, ok := TokenFromHeaders(h)
	if !ok {
		return ctx, connect.NewError(connect.CodeUnauthenticated, errors.New("missing token"))
	}
	o, err := ResolveToken(ctx, i.store, tok)
	if err != nil {
		return ctx, connect.NewError(connect.CodeInternal, err)
	}
	if o != nil {
		return context.WithValue(ctx, callerCriteriaIDKey{}, o.ID), nil
	}
	orch, err := ResolveOrchestratorToken(ctx, i.store, tok)
	if err != nil {
		return ctx, connect.NewError(connect.CodeInternal, err)
	}
	if orch == nil {
		return ctx, connect.NewError(connect.CodeUnauthenticated, errors.New("invalid token"))
	}
	return context.WithValue(ctx, callerOrchestratorIDKey{}, orch.ID), nil
}

// handleRegister enforces the bootstrap-token gate for the Register RPC.
//
//   - allowAnonRegister=true (--dev-allow-anon-register): pass through.
//   - bootstrapTokenHash set: require a matching X-Server-Bootstrap header.
//   - Neither: Register is disabled; Unimplemented is returned.
func (i *AuthInterceptor) handleRegister(ctx context.Context, req connect.AnyRequest, next connect.UnaryFunc) (connect.AnyResponse, error) {
	if i.allowAnonRegister {
		return next(ctx, req)
	}
	if i.bootstrapTokenHash == "" {
		return nil, connect.NewError(connect.CodeUnimplemented, errors.New("register is disabled: no bootstrap token configured"))
	}
	tok := strings.TrimSpace(req.Header().Get("X-Server-Bootstrap"))
	if tok == "" {
		return nil, connect.NewError(connect.CodeUnauthenticated, errors.New("X-Server-Bootstrap header required for Register"))
	}
	if !ConstantTimeEqual(tok, i.bootstrapTokenHash) {
		return nil, connect.NewError(connect.CodeUnauthenticated, errors.New("invalid bootstrap token"))
	}
	return next(ctx, req)
}

func (i *AuthInterceptor) isExempt(procedure string) bool {
	if i.allowAnonReads {
		// Read-only observation surfaces (ServerService reads and the
		// OrchestratorService subscription APIs, CRI-133) stay anonymously
		// readable in dev mode so local tooling keeps working. Production
		// TLS deployments do not enable this flag.
		if _, ok := readOnlyServerProcedures[procedure]; ok {
			return true
		}
		if _, ok := orchestratorProcedures[procedure]; ok {
			return true
		}
	}
	if strings.HasPrefix(procedure, "/grpc.health.v1.Health/") {
		return true
	}
	if strings.HasPrefix(procedure, "/grpc.reflection.v1.ServerReflection/") {
		return true
	}
	if strings.HasPrefix(procedure, "/grpc.reflection.v1alpha.ServerReflection/") {
		return true
	}
	return false
}
