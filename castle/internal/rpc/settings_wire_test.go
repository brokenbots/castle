package rpc

import (
	"context"
	"net/http/httptest"
	"strings"
	"testing"

	"connectrpc.com/connect"

	pb "github.com/brokenbots/criteria/sdk/pb/criteria/v1"
	criteriav1connect "github.com/brokenbots/criteria/sdk/pb/criteria/v1/criteriav1connect"

	castlev1 "github.com/brokenbots/castle/castle/gen/castle/v1"
	castlev1connect "github.com/brokenbots/castle/castle/gen/castle/v1/castlev1connect"
	"github.com/brokenbots/castle/castle/internal/auth"
)

// consoleOperatorSession enables console login (CRI-195), provisions the
// operator account, and mints a wire session token — the credential class
// KB-324 puts on the settings/policy/membership writes.
func consoleOperatorSession(t *testing.T, ts *testStack, tsrv *httptest.Server, username, password string) string {
	t.Helper()
	ts.server.EnableConsoleLogin()
	if err := ProvisionConsoleUser(context.Background(), ts.store, username, password, nil); err != nil {
		t.Fatalf("provision console user: %v", err)
	}
	resp, err := consoleClient(tsrv).Login(context.Background(), connect.NewRequest(&castlev1.LoginRequest{Username: username, Password: password}))
	if err != nil {
		t.Fatalf("console login: %v", err)
	}
	return resp.Msg.SessionToken
}

// settingsConsolesAndAgents returns the fixture every settings/policy wire
// contract shares: a console operator session, one agent token, and a
// project the agent minted (project creation stays agent-owned).
type settingsFixture struct {
	ts           *testStack
	tsrv         *httptest.Server
	oClient      criteriav1connect.CriteriaServiceClient
	pClient      castlev1connect.ProjectServiceClient
	consoleToken string
	agentToken   string
	projectID    string
}

func newSettingsFixture(t *testing.T) *settingsFixture {
	t.Helper()
	ctx := context.Background()
	ts := newTestStack(t)
	tsrv, oClient, _ := ts.startServer(t,
		connect.WithInterceptors(auth.NewInterceptor(ts.store, false, auth.WithAnonRegister())),
	)
	reg, err := oClient.Register(ctx, connect.NewRequest(&pb.RegisterRequest{Name: "agent"}))
	if err != nil {
		t.Fatalf("register agent: %v", err)
	}
	return &settingsFixture{
		ts:           ts,
		tsrv:         tsrv,
		oClient:      oClient,
		pClient:      projectClient(tsrv),
		consoleToken: consoleOperatorSession(t, ts, tsrv, "operator", "op-password"),
		agentToken:   reg.Msg.Token,
		projectID:    "settings-pool",
	}
}

// TestProjectSettingsWireEtagContract pins the settings swap contract over
// the wire: the console session owns the write, the empty doc reads back
// with an empty etag, the whole-document swap is content-addressed so the
// identical content is a no-op, a stale expectation loses with
// FailedPrecondition, and agent members read the result but cannot write.
func TestProjectSettingsWireEtagContract(t *testing.T) {
	f := newSettingsFixture(t)
	ctx := context.Background()
	createProject(t, ctx, f.pClient, f.agentToken, f.projectID, castlev1.ProjectMembershipPolicy_PROJECT_MEMBERSHIP_POLICY_OPEN)

	// A fresh project has the empty doc and the empty (unset) etag.
	get := func(token string) (*castlev1.GetProjectSettingsResponse, error) {
		t.Helper()
		resp, err := f.pClient.GetProjectSettings(ctx, bearer(&castlev1.GetProjectSettingsRequest{ProjectId: f.projectID}, token))
		if err != nil {
			return nil, err
		}
		return resp.Msg, nil
	}
	fresh, err := get(f.agentToken)
	if err != nil {
		t.Fatalf("agent GetProjectSettings: %v", err)
	}
	if len(fresh.Settings) != 0 || fresh.Etag != "" {
		t.Fatalf("fresh settings = %+v etag %q, want empty/empty", fresh.Settings, fresh.Etag)
	}

	// Wrong credential class: the agent's member read does not carry write
	// rights (membership rights, protocol-token write shape).
	if _, err := f.pClient.SetProjectSettings(ctx, bearer(&castlev1.SetProjectSettingsRequest{
		ProjectId: f.projectID, ExpectedEtag: "",
		Settings: map[string]string{"adapter.concurrency": "4"},
	}, f.agentToken)); connect.CodeOf(err) != connect.CodePermissionDenied {
		t.Fatalf("agent settings write: got %v, want PermissionDenied", err)
	}

	if _, err := f.pClient.SetProjectSettings(ctx, bearer(&castlev1.SetProjectSettingsRequest{
		ProjectId:    f.projectID,
		ExpectedEtag: "stale-expectation",
		Settings:     map[string]string{"adapter.concurrency": "4"},
	}, f.consoleToken)); connect.CodeOf(err) != connect.CodeFailedPrecondition {
		t.Fatalf("stale etag: got %v, want FailedPrecondition", err)
	}

	set := func(token, expected string, settings map[string]string) (*castlev1.SetProjectSettingsResponse, error) {
		t.Helper()
		resp, err := f.pClient.SetProjectSettings(ctx, bearer(&castlev1.SetProjectSettingsRequest{
			ProjectId: f.projectID, ExpectedEtag: expected, Settings: settings,
		}, token))
		if err != nil {
			return nil, err
		}
		return resp.Msg, nil
	}
	first, err := set(f.consoleToken, "", map[string]string{"adapter.concurrency": "4"})
	if err != nil {
		t.Fatalf("console first set: %v", err)
	}
	if first.Etag == "" {
		t.Fatalf("first set returned empty etag")
	}

	// The identical content reconciles as a no-op: same etag back.
	again, err := set(f.consoleToken, first.Etag, map[string]string{"adapter.concurrency": "4"})
	if err != nil {
		t.Fatalf("identical set: %v", err)
	}
	if again.Etag != first.Etag {
		t.Fatalf("identical etag = %q, want %q (deterministic content address)", again.Etag, first.Etag)
	}

	// The swap is whole-document: removing an entry requires writing the
	// doc without it. The empty value is the "entry absent" convention.
	second, err := set(f.consoleToken, first.Etag, map[string]string{
		"adapter.concurrency": "8",
		"relocation.mode":     "\"detached\"",
	})
	if err != nil {
		t.Fatalf("second set: %v", err)
	}
	docs, err := get(f.consoleToken)
	if err != nil {
		t.Fatalf("console GetProjectSettings: %v", err)
	}
	if docs.Etag != second.Etag || len(docs.Settings) != 2 ||
		docs.Settings["adapter.concurrency"] != "8" || docs.Settings["relocation.mode"] != "\"detached\"" {
		t.Fatalf("settings after swap = %+v etag %q", docs.Settings, docs.Etag)
	}
	third, err := set(f.consoleToken, second.Etag, map[string]string{"adapter.concurrency": "8"})
	if err != nil {
		t.Fatalf("entry-removal set: %v", err)
	}
	docs, err = get(f.agentToken)
	if err != nil {
		t.Fatalf("agent GetProjectSettings after removal: %v", err)
	}
	if docs.Etag != third.Etag || len(docs.Settings) != 1 || docs.Settings["adapter.concurrency"] != "8" {
		t.Fatalf("settings after removal = %+v etag %q", docs.Settings, docs.Etag)
	}

	// Shape discipline: non-JSON values rejected, oversized values rejected.
	if _, err := f.pClient.SetProjectSettings(ctx, bearer(&castlev1.SetProjectSettingsRequest{
		ProjectId: f.projectID, ExpectedEtag: docs.Etag,
		Settings: map[string]string{"adapter.concurrency": "four"},
	}, f.consoleToken)); connect.CodeOf(err) != connect.CodeInvalidArgument {
		t.Fatalf("non-JSON value: got %v, want InvalidArgument", err)
	}
	if _, err := f.pClient.SetProjectSettings(ctx, bearer(&castlev1.SetProjectSettingsRequest{
		ProjectId: f.projectID, ExpectedEtag: docs.Etag,
		Settings: map[string]string{"big": "\"" + strings.Repeat("x", 17*1024) + "\""},
	}, f.consoleToken)); connect.CodeOf(err) != connect.CodeInvalidArgument {
		t.Fatalf("oversized value: got %v, want InvalidArgument", err)
	}

	// Wrong credential class: the agent's member read does not carry write
	// rights (membership rights, protocol-token write shape).
	if _, err := f.pClient.SetProjectSettings(ctx, bearer(&castlev1.SetProjectSettingsRequest{
		ProjectId: f.projectID, ExpectedEtag: docs.Etag,
		Settings: map[string]string{"relocation.mode": "broken"},
	}, f.agentToken)); connect.CodeOf(err) != connect.CodePermissionDenied {
		t.Fatalf("agent settings write: got %v, want PermissionDenied", err)
	}

	// Missing project: NotFound; malformed project id: InvalidArgument.
	if _, err := f.pClient.GetProjectSettings(ctx, bearer(&castlev1.GetProjectSettingsRequest{
		ProjectId: "ghost-project",
	}, f.consoleToken)); connect.CodeOf(err) != connect.CodeNotFound {
		t.Fatalf("missing project: got %v, want NotFound", err)
	}
	if _, err := f.pClient.SetProjectSettings(ctx, bearer(&castlev1.SetProjectSettingsRequest{
		ProjectId: "no such project",
		Settings:  map[string]string{"a": "1"},
	}, f.consoleToken)); connect.CodeOf(err) != connect.CodeInvalidArgument {
		t.Fatalf("malformed project id: got %v, want InvalidArgument", err)
	}
}
