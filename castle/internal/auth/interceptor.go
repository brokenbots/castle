package auth

import (
	"context"
	"errors"
	"net/http"
	"strings"

	"connectrpc.com/connect"

	"github.com/brokenbots/overlord/castle/internal/store"
	"github.com/brokenbots/overlord/shared/pb/overlord/v1/overlordv1connect" // import-lint:allow castle service bindings (W08: move to castle-proto)
	overseer "github.com/brokenbots/overlord/shared/sdk/overseer"
)

// callerOverseerIDKey is the context key for the authenticated caller's overseer ID.
type callerOverseerIDKey struct{}

// CallerOverseerID returns the overseer ID injected by AuthInterceptor, or ""
// if the request was not authenticated (e.g. exempt procedures or direct
// handler calls in tests without the interceptor wired).
func CallerOverseerID(ctx context.Context) string {
	v, _ := ctx.Value(callerOverseerIDKey{}).(string)
	return v
}

// WithCallerOverseerID returns a context with the given overseer ID injected
// as the authenticated caller. Use in tests that call handlers directly (no
// HTTP stack) to simulate the identity the interceptor would inject.
func WithCallerOverseerID(ctx context.Context, id string) context.Context {
	return context.WithValue(ctx, callerOverseerIDKey{}, id)
}

var readOnlyCastleProcedures = map[string]struct{}{
	overlordv1connect.CastleServiceListOverseersProcedure: {},
	overlordv1connect.CastleServiceGetOverseerProcedure:   {},
	overlordv1connect.CastleServiceListRunsProcedure:      {},
	overlordv1connect.CastleServiceGetRunProcedure:        {},
	overlordv1connect.CastleServiceListRunEventsProcedure: {},
	overlordv1connect.CastleServiceWatchRunProcedure:      {},
}

// InterceptorOption configures an AuthInterceptor.
type InterceptorOption func(*AuthInterceptor)

// WithBootstrapToken configures the raw bootstrap token required for Register.
// The interceptor hashes it and validates incoming X-Castle-Bootstrap headers.
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

// AuthInterceptor authenticates Connect calls using overseer tokens.
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
		if req.Spec().Procedure == overseer.RegisterProcedure {
			return i.handleRegister(ctx, req, next)
		}
		if i.isExempt(req.Spec().Procedure) {
			return next(ctx, req)
		}
		newCtx, err := i.authenticateHeaders(ctx, req.Header())
		if err != nil {
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
			return next(ctx, conn)
		}
		newCtx, err := i.authenticateHeaders(ctx, conn.RequestHeader())
		if err != nil {
			return err
		}
		return next(newCtx, conn)
	}
}

// authenticateHeaders validates the token and returns a context with the
// caller's overseer ID injected.
func (i *AuthInterceptor) authenticateHeaders(ctx context.Context, h http.Header) (context.Context, error) {
	tok, ok := TokenFromHeaders(h)
	if !ok {
		return ctx, connect.NewError(connect.CodeUnauthenticated, errors.New("missing token"))
	}
	o, err := ResolveToken(ctx, i.store, tok)
	if err != nil {
		return ctx, connect.NewError(connect.CodeInternal, err)
	}
	if o == nil {
		return ctx, connect.NewError(connect.CodeUnauthenticated, errors.New("invalid token"))
	}
	return context.WithValue(ctx, callerOverseerIDKey{}, o.ID), nil
}

// handleRegister enforces the bootstrap-token gate for the Register RPC.
//
//   - allowAnonRegister=true (--dev-allow-anon-register): pass through.
//   - bootstrapTokenHash set: require a matching X-Castle-Bootstrap header.
//   - Neither: Register is disabled; Unimplemented is returned.
func (i *AuthInterceptor) handleRegister(ctx context.Context, req connect.AnyRequest, next connect.UnaryFunc) (connect.AnyResponse, error) {
	if i.allowAnonRegister {
		return next(ctx, req)
	}
	if i.bootstrapTokenHash == "" {
		return nil, connect.NewError(connect.CodeUnimplemented, errors.New("register is disabled: no bootstrap token configured"))
	}
	tok := strings.TrimSpace(req.Header().Get("X-Castle-Bootstrap"))
	if tok == "" {
		return nil, connect.NewError(connect.CodeUnauthenticated, errors.New("X-Castle-Bootstrap header required for Register"))
	}
	if !ConstantTimeEqual(tok, i.bootstrapTokenHash) {
		return nil, connect.NewError(connect.CodeUnauthenticated, errors.New("invalid bootstrap token"))
	}
	return next(ctx, req)
}

func (i *AuthInterceptor) isExempt(procedure string) bool {
	if i.allowAnonReads {
		if _, ok := readOnlyCastleProcedures[procedure]; ok {
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
