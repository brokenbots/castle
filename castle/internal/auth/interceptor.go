package auth

import (
	"context"
	"errors"
	"net/http"
	"strings"

	"connectrpc.com/connect"

	"github.com/brokenbots/overlord/castle/internal/store"
	"github.com/brokenbots/overlord/shared/pb/overlord/v1/overlordv1connect"
)

// callerOverseerIDKey is the context key for the authenticated caller's overseer ID.
type callerOverseerIDKey struct{}

// CallerOverseerID returns the overseer ID injected by AuthInterceptor, or ""
// if the request was not authenticated (e.g. exempt procedures).
func CallerOverseerID(ctx context.Context) string {
	v, _ := ctx.Value(callerOverseerIDKey{}).(string)
	return v
}

var readOnlyCastleProcedures = map[string]struct{}{
	overlordv1connect.CastleServiceListOverseersProcedure: {},
	overlordv1connect.CastleServiceGetOverseerProcedure:   {},
	overlordv1connect.CastleServiceListRunsProcedure:      {},
	overlordv1connect.CastleServiceGetRunProcedure:        {},
	overlordv1connect.CastleServiceListRunEventsProcedure: {},
	overlordv1connect.CastleServiceWatchRunProcedure:      {},
}

// AuthInterceptor authenticates Connect calls using overseer tokens.
type AuthInterceptor struct {
	store          store.Store
	allowAnonReads bool
}

func NewInterceptor(st store.Store, allowAnonReads bool) *AuthInterceptor {
	return &AuthInterceptor{store: st, allowAnonReads: allowAnonReads}
}

func (i *AuthInterceptor) WrapUnary(next connect.UnaryFunc) connect.UnaryFunc {
	return func(ctx context.Context, req connect.AnyRequest) (connect.AnyResponse, error) {
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

func (i *AuthInterceptor) isExempt(procedure string) bool {
	if procedure == overlordv1connect.OverseerServiceRegisterProcedure {
		return true
	}
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
