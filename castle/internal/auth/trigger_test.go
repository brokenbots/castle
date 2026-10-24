package auth

import (
	"testing"

	"connectrpc.com/connect"

	castlev1connect "github.com/brokenbots/castle/castle/gen/castle/v1/castlev1connect"
	"github.com/brokenbots/criteria/sdk/pb/criteria/v1/criteriav1connect"
)

// TestTriggerAllowlistMatrix pins the submit-only surface (KB-320): the
// five procedures the board adapter needs pass for a trigger identity,
// everything representative else is PermissionDenied, and non-trigger
// callers pass through regardless.
func TestTriggerAllowlistMatrix(t *testing.T) {
	allowed := []string{
		criteriav1connect.ServerServiceSubmitWorkflowAssignmentProcedure,
		criteriav1connect.ServerServiceGetAssignmentDispositionProcedure,
		criteriav1connect.ServerServiceListRunEventsProcedure,
		criteriav1connect.ServerServiceWatchRunProcedure,
		castlev1connect.ProjectServiceSubmitProjectAssignmentProcedure,
	}
	denied := []string{
		criteriav1connect.CriteriaServiceRegisterProcedure,
		criteriav1connect.CriteriaServiceCreateRunProcedure,
		criteriav1connect.ServerServiceGetRunProcedure,
		criteriav1connect.ServerServiceStopRunProcedure,
		castlev1connect.ProjectServiceCreateProjectProcedure,
		castlev1connect.ProjectServiceListProjectsProcedure,
		"/castle.v1.ProjectService/SomeFutureProcedure",
	}

	for _, proc := range allowed {
		if !isTriggerAllowed(proc) {
			t.Errorf("isTriggerAllowed(%q) = false, want true", proc)
		}
		err := authorizeTriggerProcedure(WithCallerTriggerID(t.Context(), "trigger-kanboard"), proc)
		if err != nil {
			t.Errorf("trigger x %q: %v, want allowed", proc, err)
		}
	}
	for _, proc := range denied {
		if isTriggerAllowed(proc) {
			t.Errorf("isTriggerAllowed(%q) = true, want false", proc)
		}
		err := authorizeTriggerProcedure(WithCallerTriggerID(t.Context(), "trigger-kanboard"), proc)
		if err == nil || connect.CodeOf(err) != connect.CodePermissionDenied {
			t.Errorf("trigger x %q: err = %v, want PermissionDenied", proc, err)
		}
	}

	// Non-trigger callers never trip the boundary (empty identity).
	for _, proc := range allowed {
		if err := authorizeTriggerProcedure(t.Context(), proc); err != nil {
			t.Errorf("anon caller x %q: %v, want pass-through", proc, err)
		}
	}
}
