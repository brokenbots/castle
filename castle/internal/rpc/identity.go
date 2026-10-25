// IdentityService handler (KB-333): the self-scoped operator identity read.
// The console (Parapet) renders its identity badge and gates
// project-administration surfaces from this response instead of assuming
// privileges from the credential class the request traveled as. The response
// carries the caller's own unified operator attributes (KB-330) and the
// membership projection of the SAME shared membership set the run read scope
// consults, so the UI never re-derives authorization semantics.
package rpc

import (
	"context"
	"errors"

	"connectrpc.com/connect"

	castlev1 "github.com/brokenbots/castle/castle/gen/castle/v1"
	"github.com/brokenbots/castle/castle/gen/castle/v1/castlev1connect"
	"github.com/brokenbots/castle/castle/internal/auth"
	"github.com/brokenbots/castle/castle/internal/store"
)

// GetCallerIdentity answers the caller's own identity (KB-333). Every
// authenticated caller may read exactly its own identity; the interceptor
// denies anonymous callers upstream and the handler keeps a fail-closed
// re-check for direct-call contexts that inject no identity at all.
func (s *ServerServer) GetCallerIdentity(ctx context.Context, req *connect.Request[castlev1.GetCallerIdentityRequest]) (*connect.Response[castlev1.GetCallerIdentityResponse], error) {
	op := auth.CallerOperator(ctx)
	if op == nil && auth.CallerOperatorRole(ctx) == "" {
		// No operator identity and no legacy kind key: nothing to read
		// itself from (anonymous callers never reach a scoped view).
		return nil, connect.NewError(connect.CodeUnauthenticated, errors.New("missing token"))
	}

	resp := &castlev1.GetCallerIdentityResponse{}
	if op != nil {
		resp.OperatorId = op.ID
		resp.Name = op.Name
		resp.Role = op.Role()
		resp.DisplayName = op.DisplayName
		if display := op.Attributes[store.OperatorAttributeDisplay]; display != "" {
			resp.DisplayName = display
		}
		if resp.DisplayName == "" {
			resp.DisplayName = op.Name
		}
		if len(op.Attributes) > 0 {
			resp.Attributes = make(map[string]string, len(op.Attributes))
			for k, v := range op.Attributes {
				resp.Attributes[k] = v
			}
		}
	} else {
		// Direct-call path (legacy kind keys only, pre-merge wire tests):
		// mirror CallerOperatorRole's synthesis so unwired callers keep
		// their exact pre-operators effective identity.
		resp.Role = auth.CallerOperatorRole(ctx)
		switch {
		case auth.CallerConsoleUserID(ctx) != "":
			resp.OperatorId = auth.CallerConsoleUserID(ctx)
		case auth.CallerCriteriaID(ctx) != "":
			resp.OperatorId = auth.CallerCriteriaID(ctx)
		case auth.CallerOrchestratorID(ctx) != "":
			resp.OperatorId = auth.CallerOrchestratorID(ctx)
		case auth.CallerTriggerID(ctx) != "":
			resp.OperatorId = auth.CallerTriggerID(ctx)
		}
		resp.Name = resp.OperatorId
		resp.DisplayName = resp.OperatorId
		if resp.Role != "" {
			resp.Attributes = auth.RoleAttribute(resp.Role)
		}
	}

	// Membership mirrors the run read scope exactly (membershipScopedCaller):
	// scoped callers report their membership so the UI can distinguish "no
	// membership rows" from "the projects I am a member of hold no runs";
	// unscoped callers (admin, orchestrator) are never scoped by membership —
	// their list is display metadata only.
	_, scoped := membershipScopedCaller(ctx)
	resp.MembershipScoped = scoped

	memberID := resp.GetOperatorId()
	if op != nil {
		memberID = op.ID
	}
	membership, err := s.Store.ListMemberProjects(ctx, memberID)
	if err != nil {
		return nil, connect.NewError(connect.CodeInternal, err)
	}
	resp.Membership = membership
	return connect.NewResponse(resp), nil
}

// ServerServer implements the castle-owned identity surface (KB-333).
var _ castlev1connect.IdentityServiceHandler = (*ServerServer)(nil)