package rpc

import (
	"context"
	"testing"
	"time"

	"connectrpc.com/connect"

	castlev1 "github.com/brokenbots/castle/castle/gen/castle/v1"
	"github.com/brokenbots/castle/castle/internal/store/refire"
)

// policyToWire maps a store-side policy onto the wire message; the inverse
// of the handler's mapRefirePolicy, only for assertions.
func policyToWire(p refire.Policy) *castlev1.RefirePolicy {
	return &castlev1.RefirePolicy{
		MaxAttempts:    int32(p.MaxAttempts),
		BackoffSeconds: int32(p.Backoff / time.Second),
		Transient:      p.Transient,
		Parks:          p.Parks,
		Cosmetics:      p.Cosmetics,
	}
}

// TestRefireWirePolicyGetSet pins the castle.v1 refire RPC contract (KB-324):
// the pair sits over the existing store policy rows keyed by
// (project_id, route); the global default ("" , "") and project defaults read
// back exactly what was set through the same strictness MarshalPolicy/
// ParsePolicy apply at boot; absent keys are NotFound; the console session
// owns the write and agent identities read.
func TestRefireWirePolicyGetSet(t *testing.T) {
	f := newSettingsFixture(t)
	ctx := context.Background()
	createProject(t, ctx, f.pClient, f.agentToken, f.projectID, castlev1.ProjectMembershipPolicy_PROJECT_MEMBERSHIP_POLICY_OPEN)

	// Seed the boot-time global default exactly like main.go does; the
	// seeded row has no project and no route.
	def := refire.DefaultPolicyDoc()
	if err := f.ts.store.UpsertRefirePolicy(ctx, "", "", refire.MustMarshalPolicy(def), time.Now().UTC()); err != nil {
		t.Fatalf("seed global default: %v", err)
	}

	global, err := f.pClient.GetRefirePolicy(ctx, bearer(&castlev1.GetRefirePolicyRequest{}, f.consoleToken))
	if err != nil {
		t.Fatalf("global default get: %v", err)
	}
	got := global.Msg.Policy
	if got.MaxAttempts != int32(def.MaxAttempts) || got.BackoffSeconds != int32(def.Backoff/time.Second) ||
		len(got.Transient) != len(def.Transient) || len(got.Parks) != len(def.Parks) || len(got.Cosmetics) != len(def.Cosmetics) {
		t.Fatalf("global default = %+v, want %+v", got, def)
	}
	if global.Msg.UpdatedAt == nil {
		t.Fatalf("global default UpdatedAt missing")
	}

	// Per-route override on the project: set, get back exact.
	route := &refire.Policy{MaxAttempts: 7, Backoff: 120 * time.Second,
		Transient: []string{"etcdserver: request timed out"}, Parks: []string{"rate_limited"}, Cosmetics: []string{"disk full"}}
	setResp, err := f.pClient.SetRefirePolicy(ctx, bearer(&castlev1.SetRefirePolicyRequest{
		ProjectId: f.projectID, Route: "adapter.shell", Policy: policyToWire(*route),
	}, f.consoleToken))
	if err != nil {
		t.Fatalf("per-route set: %v", err)
	}
	if setResp.Msg.UpdatedAt == nil {
		t.Fatalf("set returned nil UpdatedAt")
	}
	gotResp, err := f.pClient.GetRefirePolicy(ctx, bearer(&castlev1.GetRefirePolicyRequest{ProjectId: f.projectID, Route: "adapter.shell"}, f.agentToken))
	if err != nil {
		t.Fatalf("agent per-route get: %v", err)
	}
	if p := gotResp.Msg.Policy; p.MaxAttempts != 7 || p.BackoffSeconds != 120 || len(p.Transient) != 1 ||
		p.Transient[0] != "etcdserver: request timed out" || len(p.Parks) != 1 || len(p.Cosmetics) != 1 {
		t.Fatalf("round-trip policy = %+v", p)
	}

	// Project default (project, no route) is a distinct row.
	if err := f.ts.store.UpsertRefirePolicy(ctx, f.projectID, "", refire.MustMarshalPolicy(refire.Policy{MaxAttempts: 2, Backoff: 5 * time.Second}), time.Now().UTC()); err != nil {
		t.Fatalf("seed project default: %v", err)
	}
	projDefault, err := f.pClient.GetRefirePolicy(ctx, bearer(&castlev1.GetRefirePolicyRequest{ProjectId: f.projectID}, f.consoleToken))
	if err != nil {
		t.Fatalf("project default get: %v", err)
	}
	if got := projDefault.Msg.Policy; got.MaxAttempts != 2 || got.BackoffSeconds != 5 {
		t.Fatalf("project default = %+v, want 2/5s", got)
	}

	// Absent key: NotFound — no silent fallback to the default here; the
	// engine's resolution ladder is the fallback path, not the RPC.
	if _, err := f.pClient.GetRefirePolicy(ctx, bearer(&castlev1.GetRefirePolicyRequest{ProjectId: f.projectID, Route: "adapter.oci"}, f.consoleToken)); connect.CodeOf(err) != connect.CodeNotFound {
		t.Fatalf("absent route: got %v, want NotFound", err)
	}

	// Validation: negative budgets, malformed project id, oversized vocab.
	if _, err := f.pClient.SetRefirePolicy(ctx, bearer(&castlev1.SetRefirePolicyRequest{
		ProjectId: f.projectID, Route: "adapter.shell",
		Policy: &castlev1.RefirePolicy{MaxAttempts: -1},
	}, f.consoleToken)); connect.CodeOf(err) != connect.CodeInvalidArgument {
		t.Fatalf("negative max_attempts: got %v, want InvalidArgument", err)
	}
	if _, err := f.pClient.SetRefirePolicy(ctx, bearer(&castlev1.SetRefirePolicyRequest{
		ProjectId: "NOT A SLUG", Route: "adapter.shell",
		Policy: &castlev1.RefirePolicy{MaxAttempts: 3, BackoffSeconds: 30},
	}, f.consoleToken)); connect.CodeOf(err) != connect.CodeInvalidArgument {
		t.Fatalf("malformed project id: got %v, want InvalidArgument", err)
	}
	if _, err := f.pClient.SetRefirePolicy(ctx, bearer(&castlev1.SetRefirePolicyRequest{
		ProjectId: f.projectID, Route: "adapter.shell",
		Policy: &castlev1.RefirePolicy{MaxAttempts: 3, BackoffSeconds: 30, Transient: []string{""}},
	}, f.consoleToken)); connect.CodeOf(err) != connect.CodeInvalidArgument {
		t.Fatalf("empty vocabulary entry: got %v, want InvalidArgument", err)
	}

	// Wrong credential class on the write; reads stay open.
	if _, err := f.pClient.SetRefirePolicy(ctx, bearer(&castlev1.SetRefirePolicyRequest{
		ProjectId: f.projectID, Route: "adapter.shell",
		Policy: &castlev1.RefirePolicy{MaxAttempts: 3, BackoffSeconds: 30},
	}, f.agentToken)); connect.CodeOf(err) != connect.CodePermissionDenied {
		t.Fatalf("agent policy write: got %v, want PermissionDenied", err)
	}
	if _, err := f.pClient.GetRefirePolicy(ctx, bearer(&castlev1.GetRefirePolicyRequest{ProjectId: f.projectID, Route: "adapter.shell"}, f.agentToken)); err != nil {
		t.Fatalf("agent policy read: %v", err)
	}
}
