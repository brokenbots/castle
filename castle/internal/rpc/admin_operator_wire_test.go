package rpc

// KB-331 consumer verification: the admin operator is ONE attributed
// identity reached through the console Login RPC — never a long-lived
// token. The login session carries the admin ROLE on the operator row, so
// the role=admin bypass (membershipScopedCaller + the project-admin gates)
// applies with no membership predicate. Both sides are pinned here: the
// admin login reads and manages the whole space, and a non-admin caller
// under the same fixture keeps its scoped view.

import (
	"context"
	"testing"
	"time"

	castlev1 "github.com/brokenbots/castle/castle/gen/castle/v1"
	"github.com/brokenbots/castle/castle/internal/store/refire"
	pb "github.com/brokenbots/criteria/sdk/pb/criteria/v1"
)

// listRunIDs is defined once per package in operator_scope_wire_test.go.

// TestAdminLoginListRunsSeesWholeSpace verifies the card verbatim as the
// consumer: a login session lists ALL runs, including the unscoped
// (project_id NULL) standalone history and the foreign project-scoped run,
// and the per-run reads (GetRun/ListRunEvents/InspectRun) pass for both.
// The admin reach must come from the operator's role attribute — under the
// same fixture the non-admin agent's listed view stays scoped.
func TestAdminLoginListRunsSeesWholeSpace(t *testing.T) {
	f := newRunScopeFixture(t)
	ctx := context.Background()

	seen := listRunIDs(t, f.sClient, f.consoleToken)
	if len(seen) < 2 {
		t.Fatalf("admin login ListRuns = %d runs, want the whole space (scoped + standalone)", len(seen))
	}
	if !seen[f.scopedRun] || !seen[f.standaloneRun] {
		t.Fatalf("admin login ListRuns must include the project-scoped and the unscoped standalone runs, got %v", seen)
	}

	for _, runID := range []string{f.scopedRun, f.standaloneRun} {
		got, err := f.sClient.GetRun(ctx, bearer(&pb.GetRunRequest{RunId: runID}, f.consoleToken))
		if err != nil {
			t.Fatalf("admin login GetRun(%s): %v", runID, err)
		}
		if got.Msg.RunId != runID {
			t.Fatalf("admin login GetRun(%s) echoed %q", runID, got.Msg.RunId)
		}
		if _, err := f.sClient.ListRunEvents(ctx, bearer(&pb.ListRunEventsRequest{RunId: runID}, f.consoleToken)); err != nil {
			t.Fatalf("admin login ListRunEvents(%s): %v", runID, err)
		}
		insp, err := f.sClient.InspectRun(ctx, bearer(&pb.InspectRunRequest{RunId: runID}, f.consoleToken))
		if err != nil {
			t.Fatalf("admin login InspectRun(%s): %v", runID, err)
		}
		if insp.Msg.RunId != runID {
			t.Fatalf("admin login InspectRun(%s) echoed %q", runID, insp.Msg.RunId)
		}
	}

	// The paired leg: no relaxation of the general scope rule. The
	// non-member agent's view under the SAME fixture still lists only its
	// own unscoped run; the membership predicate binds non-admin
	// operators exactly as KB-324 pinned.
	if outsiderSeen := listRunIDs(t, f.sClient, f.outsiderToken); len(outsiderSeen) != 1 || !outsiderSeen[f.standaloneRun] {
		t.Fatalf("outsider ListRuns = %v, want exactly its own standalone run", outsiderSeen)
	}
}

// TestAdminLoginSessionProjectAdminSurfaces verifies the project-admin
// gates under the admin LOGIN session: membership writes both ways
// (admit/evict), the etag-gated settings doc swap, and the refire policy
// override — the surfaces a Parapet settings page would drive.
func TestAdminLoginSessionProjectAdminSurfaces(t *testing.T) {
	f := newRunScopeFixture(t)
	ctx := context.Background()
	pc := f.pClient

	// Admit the outsider (a membership write only an admin gate admits).
	if _, err := pc.AddProjectMember(ctx, bearer(&castlev1.AddProjectMemberRequest{
		ProjectId: "squad", MemberId: f.outsiderID, Kind: castlev1.ProjectMemberKind_PROJECT_MEMBER_KIND_AI,
	}, f.consoleToken)); err != nil {
		t.Fatalf("admin login AddProjectMember: %v", err)
	}
	members, err := pc.ListProjectMembers(ctx, bearer(&castlev1.ListProjectMembersRequest{ProjectId: "squad"}, f.consoleToken))
	if err != nil {
		t.Fatalf("admin login ListProjectMembers: %v", err)
	}
	var admitted bool
	for _, m := range members.Msg.Members {
		if m.MemberId == f.outsiderID {
			admitted = true
		}
	}
	if !admitted {
		t.Fatalf("admin login ListProjectMembers = %+v, want the admitted outsider", members.Msg.Members)
	}

	// Evict again — the write shape moves admission and eviction to console
	// sessions, so the eviction must also pass under the login.
	if _, err := pc.RemoveProjectMember(ctx, bearer(&castlev1.RemoveProjectMemberRequest{
		ProjectId: "squad", MemberId: f.outsiderID,
	}, f.consoleToken)); err != nil {
		t.Fatalf("admin login RemoveProjectMember: %v", err)
	}

	// The ONE settings doc: read etag, swap whole under it.
	read, err := pc.GetProjectSettings(ctx, bearer(&castlev1.GetProjectSettingsRequest{ProjectId: "squad"}, f.consoleToken))
	if err != nil {
		t.Fatalf("admin login GetProjectSettings: %v", err)
	}
	set, err := pc.SetProjectSettings(ctx, bearer(&castlev1.SetProjectSettingsRequest{
		ProjectId:    "squad",
		Settings:     map[string]string{"run.retention": `"7d"`},
		ExpectedEtag: read.Msg.GetEtag(),
	}, f.consoleToken))
	if err != nil {
		t.Fatalf("admin login SetProjectSettings: %v", err)
	}
	if set.Msg.GetEtag() == "" || set.Msg.GetEtag() == read.Msg.GetEtag() {
		t.Fatalf("admin login settings swap etag = %q, want a rotated etag", set.Msg.GetEtag())
	}
	reread, err := pc.GetProjectSettings(ctx, bearer(&castlev1.GetProjectSettingsRequest{ProjectId: "squad"}, f.consoleToken))
	if err != nil {
		t.Fatalf("admin login settings re-read: %v", err)
	}
	if reread.Msg.GetSettings()["run.retention"] != `"7d"` {
		t.Fatalf("admin login settings doc = %q, want the swapped body", reread.Msg.GetSettings())
	}

	// Refire policy override for a project route (KB-321 rows). The global
	// default row is seeded first, as boot does.
	if err := f.ts.store.UpsertRefirePolicy(ctx, "", "", refire.MustMarshalPolicy(refire.DefaultPolicyDoc()), time.Now().UTC()); err != nil {
		t.Fatalf("seed global default policy: %v", err)
	}
	route := refire.Policy{MaxAttempts: 7, Backoff: 120 * time.Second,
		Transient: []string{"etcdserver: request timed out"}, Parks: []string{"rate_limited"}, Cosmetics: []string{"disk full"}}
	if _, err := pc.SetRefirePolicy(ctx, bearer(&castlev1.SetRefirePolicyRequest{
		ProjectId: "squad", Route: "adapter.shell",
		Policy: policyToWire(route),
	}, f.consoleToken)); err != nil {
		t.Fatalf("admin login SetRefirePolicy: %v", err)
	}
	pol, err := pc.GetRefirePolicy(ctx, bearer(&castlev1.GetRefirePolicyRequest{
		ProjectId: "squad", Route: "adapter.shell",
	}, f.consoleToken))
	if err != nil {
		t.Fatalf("admin login GetRefirePolicy: %v", err)
	}
	if pol.Msg.Policy.MaxAttempts != int32(route.MaxAttempts) || len(pol.Msg.Policy.Parks) != 1 || pol.Msg.Policy.Parks[0] != "rate_limited" {
		t.Fatalf("admin login refire policy read-back: %+v", pol.Msg.Policy)
	}
}
