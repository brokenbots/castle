package rpc

import (
	"context"
	"net/http/httptest"
	"testing"

	"connectrpc.com/connect"
	castlev1 "github.com/brokenbots/castle/castle/gen/castle/v1"
	castlev1connect "github.com/brokenbots/castle/castle/gen/castle/v1/castlev1connect"
	"github.com/brokenbots/castle/castle/internal/auth"
	pb "github.com/brokenbots/criteria/sdk/pb/criteria/v1"
	criteriav1connect "github.com/brokenbots/criteria/sdk/pb/criteria/v1/criteriav1connect"
)

// anonReadsFixture arms the KB-326 S3 anonymous-read acceptance matrix: a
// project with two members, a project-scoped run (owner assignment),
// unscoped runs owned by a member (grantable) and by a memberless outsider
// (never grantable), and a console operator session that owns settings
// writes. The interceptor runs at the given flag level; the handler-side
// gate carries the SAME flag, mirroring main.go's wiring.
type anonReadsFixture struct {
	ts           *testStack
	tsrv         *httptest.Server
	sClient      criteriav1connect.ServerServiceClient
	oClient      criteriav1connect.CriteriaServiceClient
	pClient      castlev1connect.ProjectServiceClient
	consoleToken string
	projectID    string
	scopedRun    string // project-scoped assignment run owned by the owner
	memberRun    string // unscoped run owned by a project member
	outsiderRun  string // unscoped run owned by a memberless agent
}

func newAnonReadsFixture(t *testing.T, allowAnonReads bool) *anonReadsFixture {
	t.Helper()
	ctx := context.Background()
	ts := newTestStack(t)
	ts.server.AllowAnonymousReads = allowAnonReads
	tsrv, oClient, sClient := ts.startServer(t, connect.WithInterceptors(auth.NewInterceptor(ts.store, allowAnonReads, auth.WithAnonRegister())))
	pClient := projectClient(tsrv)

	ownerReg, err := oClient.Register(ctx, connect.NewRequest(&pb.RegisterRequest{Name: "owner"}))
	if err != nil {
		t.Fatalf("register owner: %v", err)
	}
	memberReg, err := oClient.Register(ctx, connect.NewRequest(&pb.RegisterRequest{Name: "member"}))
	if err != nil {
		t.Fatalf("register member: %v", err)
	}
	outsiderReg, err := oClient.Register(ctx, connect.NewRequest(&pb.RegisterRequest{Name: "outsider"}))
	if err != nil {
		t.Fatalf("register outsider: %v", err)
	}
	consoleToken := consoleOperatorSession(t, ts, tsrv, "operator", "op-password")

	f := &anonReadsFixture{
		ts:           ts,
		tsrv:         tsrv,
		oClient:      oClient,
		sClient:      sClient,
		pClient:      pClient,
		consoleToken: consoleToken,
		projectID:    "squad",
	}

	createProject(t, ctx, pClient, f.consoleToken, f.projectID, castlev1.ProjectMembershipPolicy_PROJECT_MEMBERSHIP_POLICY_OPEN)
	for _, id := range []string{ownerReg.Msg.CriteriaId, memberReg.Msg.CriteriaId} {
		if _, err := pClient.AddProjectMember(ctx, bearer(&castlev1.AddProjectMemberRequest{
			ProjectId: f.projectID, MemberId: id, Kind: castlev1.ProjectMemberKind_PROJECT_MEMBER_KIND_AI,
		}, consoleToken)); err != nil {
			t.Fatalf("console admits %s: %v", id, err)
		}
	}

	submitted, err := pClient.SubmitProjectAssignment(ctx, bearer(&castlev1.SubmitProjectAssignmentRequest{
		ProjectId:      f.projectID,
		WorkflowName:   "ship-it",
		WorkflowSource: "hcl-source",
		IdempotencyKey: "scoped-run",
	}, ownerReg.Msg.Token))
	if err != nil {
		t.Fatalf("submit project assignment: %v", err)
	}
	f.scopedRun = submitted.Msg.RunId

	memberRun, err := oClient.CreateRun(ctx, bearer(&pb.CreateRunRequest{
		CriteriaId: memberReg.Msg.CriteriaId, WorkflowName: "member-solo",
	}, memberReg.Msg.Token))
	if err != nil {
		t.Fatalf("create member run: %v", err)
	}
	f.memberRun = memberRun.Msg.RunId

	outsiderRun, err := oClient.CreateRun(ctx, bearer(&pb.CreateRunRequest{
		CriteriaId: outsiderReg.Msg.CriteriaId, WorkflowName: "outsider-solo",
	}, outsiderReg.Msg.Token))
	if err != nil {
		t.Fatalf("create outsider run: %v", err)
	}
	f.outsiderRun = outsiderRun.Msg.RunId

	return f
}

// setGrant swaps the project's allow_anonymous_reads setting through the
// etag contract and returns the write error, if any.
func (f *anonReadsFixture) setGrant(t *testing.T, ctx context.Context, value string) error {
	t.Helper()
	read, err := f.pClient.GetProjectSettings(ctx, bearer(&castlev1.GetProjectSettingsRequest{ProjectId: f.projectID}, f.consoleToken))
	if err != nil {
		t.Fatalf("GetProjectSettings: %v", err)
	}
	settings := map[string]string{}
	for k, v := range read.Msg.Settings {
		settings[k] = v
	}
	if value != "" {
		settings["allow_anonymous_reads"] = value
	} else {
		delete(settings, "allow_anonymous_reads")
	}
	_, err = f.pClient.SetProjectSettings(ctx, bearer(&castlev1.SetProjectSettingsRequest{
		ProjectId: f.projectID, ExpectedEtag: read.Msg.Etag, Settings: settings,
	}, f.consoleToken))
	return err
}

// anonGetRun issues a tokenless GetRun and returns the connect code.
func (f *anonReadsFixture) anonGetRun(ctx context.Context, runID string) connect.Code {
	_, err := f.sClient.GetRun(ctx, connect.NewRequest(&pb.GetRunRequest{RunId: runID}))
	return connect.CodeOf(err)
}

// anonWatchCode surfaces a WatchRun's refusal whether it travels on the
// call or the first Receive.
func (f *anonReadsFixture) anonWatchCode(ctx context.Context, runID string) connect.Code {
	w, err := f.sClient.WatchRun(ctx, connect.NewRequest(&pb.WatchRunRequest{RunId: runID}))
	if err != nil {
		return connect.CodeOf(err)
	}
	defer w.Close()
	if w.Receive() {
		return connect.CodeUnknown
	}
	return connect.CodeOf(w.Err())
}

// TestAnonReadWire_FlagOff_FreshCastle is the acceptance matrix for the
// default flip: with the flag off and no per-project grant, every anonymous
// read of a run is refused with uniform NotFound (no existence leak, and
// scoped and unscoped refusals are indistinguishable), while the anonymous
// LISTING still answers unauthenticated.
func TestAnonReadWire_FlagOff_FreshCastle(t *testing.T) {
	f := newAnonReadsFixture(t, false)
	ctx := context.Background()

	for _, runID := range []string{f.scopedRun, f.memberRun, "missing-run"} {
		if code := f.anonGetRun(ctx, runID); code != connect.CodeNotFound {
			t.Fatalf("anon GetRun(%s): got %v, want NotFound", runID, code)
		}
		if _, err := f.sClient.InspectRun(ctx, connect.NewRequest(&pb.InspectRunRequest{RunId: runID})); connect.CodeOf(err) != connect.CodeNotFound {
			t.Fatalf("anon InspectRun(%s): got %v, want NotFound", runID, err)
		}
	}
	if _, err := f.sClient.InspectRun(ctx, connect.NewRequest(&pb.InspectRunRequest{RunId: f.memberRun})); connect.CodeOf(err) != connect.CodeNotFound {
		t.Fatalf("anon InspectRun(memberRun): got %v, want NotFound", err)
	}
	if _, err := f.sClient.InspectRun(ctx, connect.NewRequest(&pb.InspectRunRequest{RunId: f.scopedRun})); connect.CodeOf(err) != connect.CodeNotFound {
		t.Fatalf("anon InspectRun(scopedRun): got %v, want NotFound", err)
	}
	if _, err := f.sClient.ListRunEvents(ctx, connect.NewRequest(&pb.ListRunEventsRequest{RunId: f.memberRun})); connect.CodeOf(err) != connect.CodeNotFound {
		t.Fatalf("anon ListRunEvents(memberRun): got %v, want NotFound", err)
	}
	if code := f.anonWatchCode(ctx, f.memberRun); code != connect.CodeNotFound {
		t.Fatalf("anon WatchRun(memberRun): got %v, want NotFound", code)
	}
	if _, err := f.sClient.ListRuns(ctx, connect.NewRequest(&pb.ListRunsRequest{})); connect.CodeOf(err) != connect.CodeUnauthenticated {
		t.Fatalf("anon ListRuns: got %v, want Unauthenticated", err)
	}
}

// TestAnonReadWire_FlagOn pins the dev override: with the env-equivalent
// flag on, unscoped runs are readable and listable anonymously, scoped runs
// stay refused, and the anonymous listing never shows scoped runs.
func TestAnonReadWire_FlagOn(t *testing.T) {
	f := newAnonReadsFixture(t, true)
	ctx := context.Background()

	if _, err := f.sClient.GetRun(ctx, connect.NewRequest(&pb.GetRunRequest{RunId: f.memberRun})); err != nil {
		t.Fatalf("anon GetRun(memberRun) under flag: %v", err)
	}
	if _, err := f.sClient.InspectRun(ctx, connect.NewRequest(&pb.InspectRunRequest{RunId: f.memberRun})); err != nil {
		t.Fatalf("anon InspectRun(memberRun) under flag: %v", err)
	}
	if code := f.anonGetRun(ctx, f.scopedRun); code != connect.CodeNotFound {
		t.Fatalf("anon GetRun(scopedRun) under flag: got %v, want NotFound", code)
	}
	if _, err := f.sClient.InspectRun(ctx, connect.NewRequest(&pb.InspectRunRequest{RunId: f.scopedRun})); connect.CodeOf(err) != connect.CodeNotFound {
		t.Fatalf("anon InspectRun(scopedRun) under flag: got %v, want NotFound", err)
	}
	if code := f.anonWatchCode(ctx, f.scopedRun); code != connect.CodeNotFound {
		t.Fatalf("anon WatchRun(scopedRun) under flag: got %v, want NotFound", code)
	}

	list, err := f.sClient.ListRuns(ctx, connect.NewRequest(&pb.ListRunsRequest{}))
	if err != nil {
		t.Fatalf("anon ListRuns under flag: %v", err)
	}
	seen := map[string]bool{}
	for _, r := range list.Msg.Runs {
		seen[r.RunId] = true
	}
	if !seen[f.memberRun] {
		t.Fatalf("anon listing misses the unscoped member run: %v", seen)
	}
	if seen[f.scopedRun] {
		t.Fatalf("anon listing leaked the scoped run: %v", seen)
	}
}

// TestAnonReadWire_FlagOff_ProjectGrant pins the per-project grant: a
// squad member's unscoped runs become anonymously readable through the
// allow_anonymous_reads setting while the global flag is OFF (the fresh
// default), the grant never opens the squad's scoped runs, and it cannot
// reach agents outside the squad.
func TestAnonReadWire_FlagOff_ProjectGrant(t *testing.T) {
	f := newAnonReadsFixture(t, false)
	ctx := context.Background()

	if err := f.setGrant(t, ctx, "true"); err != nil {
		t.Fatalf("console grants anonymous reads: %v", err)
	}
	if _, err := f.sClient.GetRun(ctx, connect.NewRequest(&pb.GetRunRequest{RunId: f.memberRun})); err != nil {
		t.Fatalf("anon GetRun(memberRun) under grant: %v", err)
	}
	if _, err := f.sClient.InspectRun(ctx, connect.NewRequest(&pb.InspectRunRequest{RunId: f.memberRun})); err != nil {
		t.Fatalf("anon InspectRun(memberRun) under grant: %v", err)
	}
	if code := f.anonGetRun(ctx, f.scopedRun); code != connect.CodeNotFound {
		t.Fatalf("grant must never open a scoped run: got %v, want NotFound", code)
	}
	if code := f.anonGetRun(ctx, f.outsiderRun); code != connect.CodeNotFound {
		t.Fatalf("grant of one project must not reach an agent outside it: got %v, want NotFound", code)
	}
	if _, err := f.sClient.ListRuns(ctx, connect.NewRequest(&pb.ListRunsRequest{})); connect.CodeOf(err) != connect.CodeUnauthenticated {
		t.Fatalf("anon ListRuns stays flag-gated under a grant: got %v, want Unauthenticated", err)
	}

	// Explicit false and a removed entry both revoke.
	if err := f.setGrant(t, ctx, "false"); err != nil {
		t.Fatalf("console sets explicit false: %v", err)
	}
	if code := f.anonGetRun(ctx, f.memberRun); code != connect.CodeNotFound {
		t.Fatalf("explicit false must revoke: got %v, want NotFound", code)
	}
	if err := f.setGrant(t, ctx, ""); err != nil {
		t.Fatalf("console removes the entry: %v", err)
	}
	if code := f.anonGetRun(ctx, f.memberRun); code != connect.CodeNotFound {
		t.Fatalf("absent entry must revoke: got %v, want NotFound", code)
	}
}

// TestAnonReadWire_GrantUnion pins union semantics: ANY owning project's
// grant suffices — each of two squads' grants opens its own members' runs
// independently.
func TestAnonReadWire_GrantUnion(t *testing.T) {
	f := newAnonReadsFixture(t, false)
	ctx := context.Background()

	// Second squad with the outsider as member, so f.outsiderRun becomes
	// grantable the moment that squad grants.
	const squadB = "squad-b"
	createProject(t, ctx, f.pClient, f.consoleToken, squadB, castlev1.ProjectMembershipPolicy_PROJECT_MEMBERSHIP_POLICY_OPEN)
	outsiderRunRow, err := f.ts.store.GetRun(ctx, f.outsiderRun)
	if err != nil {
		t.Fatalf("get outsider run: %v", err)
	}
	if _, err := f.pClient.AddProjectMember(ctx, bearer(&castlev1.AddProjectMemberRequest{
		ProjectId: squadB, MemberId: outsiderRunRow.OverseerID, Kind: castlev1.ProjectMemberKind_PROJECT_MEMBER_KIND_AI,
	}, f.consoleToken)); err != nil {
		t.Fatalf("console admits outsider into squad-b: %v", err)
	}

	setGrant := func(projectID, value string) {
		t.Helper()
		read, err := f.pClient.GetProjectSettings(ctx, bearer(&castlev1.GetProjectSettingsRequest{ProjectId: projectID}, f.consoleToken))
		if err != nil {
			t.Fatalf("GetProjectSettings(%s): %v", projectID, err)
		}
		_, err = f.pClient.SetProjectSettings(ctx, bearer(&castlev1.SetProjectSettingsRequest{
			ProjectId: projectID, ExpectedEtag: read.Msg.Etag,
			Settings: map[string]string{"allow_anonymous_reads": value},
		}, f.consoleToken))
		if err != nil {
			t.Fatalf("SetProjectSettings(%s): %v", projectID, err)
		}
	}

	// No grants: both runs denied.
	if code := f.anonGetRun(ctx, f.memberRun); code != connect.CodeNotFound {
		t.Fatalf("memberRun without any grant: got %v, want NotFound", code)
	}
	if code := f.anonGetRun(ctx, f.outsiderRun); code != connect.CodeNotFound {
		t.Fatalf("outsiderRun without any grant: got %v, want NotFound", code)
	}

	// Grant on squad-b only: the outsider run opens, the member run stays
	// denied — one project's grant is not another project's.
	setGrant(squadB, "true")
	if _, err := f.sClient.GetRun(ctx, connect.NewRequest(&pb.GetRunRequest{RunId: f.outsiderRun})); err != nil {
		t.Fatalf("anon GetRun(outsiderRun) under squad-b grant: %v", err)
	}
	if code := f.anonGetRun(ctx, f.memberRun); code != connect.CodeNotFound {
		t.Fatalf("memberRun without squad grant: got %v, want NotFound", code)
	}

	// Grant on both: both open — union, no exclusivity.
	setGrant(f.projectID, "true")
	if _, err := f.sClient.GetRun(ctx, connect.NewRequest(&pb.GetRunRequest{RunId: f.memberRun})); err != nil {
		t.Fatalf("anon GetRun(memberRun) under squad grant: %v", err)
	}
	if _, err := f.sClient.GetRun(ctx, connect.NewRequest(&pb.GetRunRequest{RunId: f.outsiderRun})); err != nil {
		t.Fatalf("anon GetRun(outsiderRun) under both grants: %v", err)
	}
}

// TestAnonReadWire_SettingsValidation pins the write-side interpretation
// (KB-326 S3): the only interpreted settings key must be written as a JSON
// boolean; numbers, strings and garbage are rejected as InvalidArgument so
// a grant cannot be mis-spelled into silence (or silence into a grant).
func TestAnonReadWire_SettingsValidation(t *testing.T) {
	f := newAnonReadsFixture(t, false)
	ctx := context.Background()

	read, err := f.pClient.GetProjectSettings(ctx, bearer(&castlev1.GetProjectSettingsRequest{ProjectId: f.projectID}, f.consoleToken))
	if err != nil {
		t.Fatalf("GetProjectSettings: %v", err)
	}
	if read.Msg.Etag != "" {
		t.Fatalf("fresh etag %q, want empty", read.Msg.Etag)
	}

	setSettings := func(settings map[string]string) error {
		t.Helper()
		fresh, err := f.pClient.GetProjectSettings(ctx, bearer(&castlev1.GetProjectSettingsRequest{ProjectId: f.projectID}, f.consoleToken))
		if err != nil {
			t.Fatalf("GetProjectSettings: %v", err)
		}
		_, err = f.pClient.SetProjectSettings(ctx, bearer(&castlev1.SetProjectSettingsRequest{
			ProjectId: f.projectID, ExpectedEtag: fresh.Msg.Etag, Settings: settings,
		}, f.consoleToken))
		return err
	}

	if err := setSettings(map[string]string{"allow_anonymous_reads": "true"}); err != nil {
		t.Fatalf("JSON boolean accepted: %v", err)
	}
	for _, bad := range []string{"1", "yes", "garbage", "\"true\""} {
		if err := setSettings(map[string]string{"allow_anonymous_reads": bad}); connect.CodeOf(err) != connect.CodeInvalidArgument {
			t.Fatalf("grant value %q: got %v, want InvalidArgument", bad, err)
		}
	}
	// An empty value is the convention for "entry absent" — no error.
	if err := setSettings(map[string]string{"allow_anonymous_reads": ""}); err != nil {
		t.Fatalf("empty value = entry absent: %v", err)
	}

	// The same rule holds for the seed settings written at project creation.
	if _, err := f.pClient.CreateProject(ctx, bearer(&castlev1.CreateProjectRequest{Project: &castlev1.Project{
		Id:       "seed-bad",
		Settings: map[string]string{"allow_anonymous_reads": "garbage"},
	}}, f.consoleToken)); connect.CodeOf(err) != connect.CodeInvalidArgument {
		t.Fatalf("CreateProject seed 'garbage': got %v, want InvalidArgument", err)
	}
	if _, err := f.pClient.CreateProject(ctx, bearer(&castlev1.CreateProjectRequest{Project: &castlev1.Project{
		Id:               "seed-good",
		MembershipPolicy: castlev1.ProjectMembershipPolicy_PROJECT_MEMBERSHIP_POLICY_OPEN,
		Settings:         map[string]string{"allow_anonymous_reads": "true", "adapter.concurrency": "4"},
	}}, f.consoleToken)); err != nil {
		t.Fatalf("CreateProject seed boolean accepted: %v", err)
	}
}

// TestAnonReadWire_DirectCallStaysPrivileged pins the no-interceptor plane:
// a direct (in-process) call carries no anonymous marker, so the whole-space
// semantics the package's unit tests depend on survive the S3 gate.
func TestAnonReadWire_DirectCallStaysPrivileged(t *testing.T) {
	f := newAnonReadsFixture(t, false)
	ctx := context.Background()

	resp, err := f.ts.server.GetRun(ctx, connect.NewRequest(&pb.GetRunRequest{RunId: f.scopedRun}))
	if err != nil {
		t.Fatalf("direct GetRun(scopedRun) without the interceptor: %v", err)
	}
	if resp.Msg.RunId != f.scopedRun {
		t.Fatalf("direct GetRun returned %q", resp.Msg.GetRunId())
	}
	list, err := f.ts.server.ListRuns(ctx, connect.NewRequest(&pb.ListRunsRequest{}))
	if err != nil {
		t.Fatalf("direct ListRuns without the interceptor: %v", err)
	}
	if len(list.Msg.Runs) < 3 {
		t.Fatalf("direct ListRuns saw %d runs, want the whole space", len(list.Msg.Runs))
	}
}
