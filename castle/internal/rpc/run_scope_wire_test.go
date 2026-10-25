package rpc

import (
	"context"
	"testing"
	"time"

	"connectrpc.com/connect"
	castlev1connect "github.com/brokenbots/castle/castle/gen/castle/v1/castlev1connect"
	"google.golang.org/protobuf/types/known/timestamppb"
	"net/http/httptest"

	pb "github.com/brokenbots/criteria/sdk/pb/criteria/v1"                // import-lint:allow castle service bindings (W08: move to castle-proto)
	"github.com/brokenbots/criteria/sdk/pb/criteria/v1/criteriav1connect" // import-lint:allow castle service bindings (W08: move to castle-proto)

	castlev1 "github.com/brokenbots/castle/castle/gen/castle/v1"
	"github.com/brokenbots/castle/castle/internal/auth"
)

// runScopeFixture is the KB-324 read-visibility acceptance matrix: a project
// with two members, a non-member outsider, a project-scoped assignment run,
// a standalone unscoped run owned by the non-member, and a console operator
// session above them all.
type runScopeFixture struct {
	ts            *testStack
	tsrv          *httptest.Server
	oClient       criteriav1connect.CriteriaServiceClient
	sClient       criteriav1connect.ServerServiceClient
	pClient       castlev1connect.ProjectServiceClient
	consoleToken  string
	ownerID       string
	ownerToken    string
	memberID      string
	memberToken   string
	outsiderID    string
	outsiderToken string
	scopedRun     string // project-scoped assignment run
	standaloneRun string // unscoped CreateRun owned by the outsider agent
}

func newRunScopeFixture(t *testing.T) *runScopeFixture {
	t.Helper()
	ctx := context.Background()
	ts := newTestStack(t)
	tsrv, oClient, sClient := ts.startServer(t, connect.WithInterceptors(auth.NewInterceptor(ts.store, false, auth.WithAnonRegister())))
	pClient := projectClient(tsrv)

	const projectID = "squad"
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
	createProject(t, ctx, pClient, ownerReg.Msg.Token, projectID, castlev1.ProjectMembershipPolicy_PROJECT_MEMBERSHIP_POLICY_OPEN)
	for _, id := range []string{ownerReg.Msg.CriteriaId, memberReg.Msg.CriteriaId} {
		if _, err := pClient.AddProjectMember(ctx, bearer(&castlev1.AddProjectMemberRequest{
			ProjectId: projectID, MemberId: id, Kind: castlev1.ProjectMemberKind_PROJECT_MEMBER_KIND_AI,
		}, consoleToken)); err != nil {
			t.Fatalf("console admits %s: %v", id, err)
		}
	}

	// The project-scoped run: the owner submits board work into the project —
	// an assignment write, protocol-token owned; the run inherits the
	// assignment's project at mint time.
	submitted, err := pClient.SubmitProjectAssignment(ctx, bearer(&castlev1.SubmitProjectAssignmentRequest{
		ProjectId:      projectID,
		WorkflowName:   "ship-it",
		WorkflowSource: "hcl-source",
		IdempotencyKey: "scoped",
	}, ownerReg.Msg.Token))
	if err != nil {
		t.Fatalf("submit project assignment: %v", err)
	}
	scoped, err := ts.store.GetRun(ctx, submitted.Msg.RunId)
	if err != nil {
		t.Fatalf("get scoped run: %v", err)
	}
	if scoped.ProjectID != projectID {
		t.Fatalf("scoped run project_id = %q, want %q", scoped.ProjectID, projectID)
	}

	// A standalone unscoped run owned by the non-member agent.
	created, err := oClient.CreateRun(ctx, bearer(&pb.CreateRunRequest{
		CriteriaId: outsiderReg.Msg.CriteriaId, WorkflowName: "solo",
	}, outsiderReg.Msg.Token))
	if err != nil {
		t.Fatalf("create standalone run: %v", err)
	}
	standalone, err := ts.store.GetRun(ctx, created.Msg.RunId)
	if err != nil {
		t.Fatalf("get standalone run: %v", err)
	}
	if standalone.ProjectID != "" {
		t.Fatalf("standalone run project_id = %q, want empty", standalone.ProjectID)
	}

	return &runScopeFixture{
		ts:            ts,
		tsrv:          tsrv,
		oClient:       oClient,
		sClient:       sClient,
		pClient:       pClient,
		consoleToken:  consoleToken,
		ownerID:       ownerReg.Msg.CriteriaId,
		ownerToken:    ownerReg.Msg.Token,
		memberID:      memberReg.Msg.CriteriaId,
		memberToken:   memberReg.Msg.Token,
		outsiderID:    outsiderReg.Msg.CriteriaId,
		outsiderToken: outsiderReg.Msg.Token,
		scopedRun:     submitted.Msg.RunId,
		standaloneRun: created.Msg.RunId,
	}
}

// TestRunReadsAreMembershipScoped pins the KB-324 read boundary over the
// wire: a member agent reads its membership's runs (and its own), a
// non-member gets NotFound (no existence leak), the console session reads
// across everything, and listing is scoped exactly like the point lookups.
func TestRunReadsAreMembershipScoped(t *testing.T) {
	f := newRunScopeFixture(t)
	ctx := context.Background()

	getRun := func(token, runID string) error {
		_, err := f.sClient.GetRun(ctx, bearer(&pb.GetRunRequest{RunId: runID}, token))
		return err
	}

	// Member: inside the project — the scoped run is readable through the
	// membership row, and its event history opens the same way.
	if _, err := f.sClient.GetRun(ctx, bearer(&pb.GetRunRequest{RunId: f.scopedRun}, f.memberToken)); err != nil {
		t.Fatalf("member GetRun scoped run: %v", err)
	}
	if _, err := f.sClient.ListRunEvents(ctx, bearer(&pb.ListRunEventsRequest{RunId: f.scopedRun}, f.memberToken)); err != nil {
		t.Fatalf("member ListRunEvents scoped run: %v", err)
	}

	// The outsider's standalone run is invisible to a member of the project —
	// membership does not grant whole-space reads.
	if err := getRun(f.memberToken, f.standaloneRun); connect.CodeOf(err) != connect.CodeNotFound {
		t.Fatalf("member GetRun outsider standalone run: got %v, want NotFound", err)
	}

	// Non-member: the project run and a missing id are indistinguishable —
	// NotFound both ways, no existence leak.
	if err := getRun(f.outsiderToken, f.scopedRun); connect.CodeOf(err) != connect.CodeNotFound {
		t.Fatalf("outsider GetRun scoped run: got %v, want NotFound", err)
	}
	if err := getRun(f.outsiderToken, "no-such-run"); connect.CodeOf(err) != connect.CodeNotFound {
		t.Fatalf("outsider GetRun missing id: got %v, want NotFound", err)
	}

	// The non-member still reads its OWN unscoped run (the agent's own
	// work — the conformance posture): membership is not needed for your own.
	if _, err := f.sClient.GetRun(ctx, bearer(&pb.GetRunRequest{RunId: f.standaloneRun}, f.outsiderToken)); err != nil {
		t.Fatalf("outsider GetRun own standalone run: %v", err)
	}

	// Console session: whole-space reads across every membership.
	if _, err := f.sClient.GetRun(ctx, bearer(&pb.GetRunRequest{RunId: f.scopedRun}, f.consoleToken)); err != nil {
		t.Fatalf("console GetRun scoped run: %v", err)
	}
	if _, err := f.sClient.GetRun(ctx, bearer(&pb.GetRunRequest{RunId: f.standaloneRun}, f.consoleToken)); err != nil {
		t.Fatalf("console GetRun standalone run: %v", err)
	}

	// ListRuns: the outsider's listing shows only its own run; the member's
	// listing shows only the membership scope's run; the console listing
	// shows both.
	listIDs := func(token string, mustHave, mustNotHave []string) {
		t.Helper()
		resp, err := f.sClient.ListRuns(ctx, bearer(&pb.ListRunsRequest{}, token))
		if err != nil {
			t.Fatalf("ListRuns: %v", err)
		}
		seen := map[string]bool{}
		for _, r := range resp.Msg.Runs {
			seen[r.RunId] = true
		}
		for _, id := range mustHave {
			if !seen[id] {
				t.Fatalf("ListRuns missing %q for caller: %v", id, seen)
			}
		}
		for _, id := range mustNotHave {
			if seen[id] {
				t.Fatalf("ListRuns must not include %q for this caller: %v", id, seen)
			}
		}
	}
	listIDs(f.outsiderToken, []string{f.standaloneRun}, []string{f.scopedRun})
	listIDs(f.memberToken, []string{f.scopedRun}, []string{f.standaloneRun})
	listIDs(f.consoleToken, []string{f.scopedRun, f.standaloneRun}, nil)
}

// TestRunWatchIsMembershipScoped pins the stream half: WatchRun opens for a
// member and refuses a non-member (NotFound) before any replay.
func TestRunWatchIsMembershipScoped(t *testing.T) {
	f := newRunScopeFixture(t)
	ctx := context.Background()

	watch, err := f.sClient.WatchRun(ctx, bearer(&pb.WatchRunRequest{RunId: f.scopedRun}, f.memberToken))
	if err != nil {
		t.Fatalf("member WatchRun: %v", err)
	}
	defer watch.Close()
	if !watch.Receive() {
		t.Fatalf("member WatchRun first frame failed")
	}
	if m := watch.Msg(); m == nil || m.GetWatchReady() == nil {
		t.Fatalf("expected WatchReady first, got %+v", m)
	}

	// Anonymous read (KB-326 S3: per-run reads pass through in both flag
	// states) is refused at the handler grant: a scoped run is never
	// anonymously readable, and the refusal is the uniform NotFound —
	// indistinguishable from a missing run.
	if _, err := f.sClient.GetRun(ctx, connect.NewRequest(&pb.GetRunRequest{RunId: f.scopedRun})); connect.CodeOf(err) != connect.CodeNotFound {
		t.Fatalf("anon GetRun: got %v, want NotFound", err)
	}

	// Non-member watch: refused without leaking existence. connect streams
	// surface a pre-stream handler error on the call; fall back to the first
	// Receive for transports that carry it there.
	watchCode := func(token string) connect.Code {
		w, err := f.sClient.WatchRun(ctx, bearer(&pb.WatchRunRequest{RunId: f.scopedRun}, token))
		if err != nil {
			return connect.CodeOf(err)
		}
		defer w.Close()
		if w.Receive() {
			return connect.CodeUnknown
		}
		return connect.CodeOf(w.Err())
	}
	if code := watchCode(f.outsiderToken); code != connect.CodeNotFound {
		t.Fatalf("outsider WatchRun: got %v, want NotFound", code)
	}
}

// TestScopedRunEventsReachMembers is a canary that project-scoped runs keep
// appending events into the normal event stream the member reads.
func TestScopedRunEventsReachMembers(t *testing.T) {
	f := newRunScopeFixture(t)
	ctx := context.Background()

	env := &pb.Envelope{SchemaVersion: 1, RunId: f.scopedRun, Ts: timestamppb.New(time.Now().UTC()), Payload: &pb.Envelope_StepEntered{StepEntered: &pb.StepEntered{Step: "s1", Adapter: "shell", Attempt: 1}}}
	if _, _, err := f.ts.store.AppendEvent(ctx, mustStoreEvent(t, env)); err != nil {
		t.Fatalf("append event: %v", err)
	}

	resp, err := f.sClient.ListRunEvents(ctx, bearer(&pb.ListRunEventsRequest{RunId: f.scopedRun}, f.memberToken))
	if err != nil {
		t.Fatalf("member ListRunEvents: %v", err)
	}
	if len(resp.Msg.Events) != 1 {
		t.Fatalf("member sees %d events, want 1", len(resp.Msg.Events))
	}
}
