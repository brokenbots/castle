package rpc

import (
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"connectrpc.com/connect"
	"golang.org/x/net/http2"
	"golang.org/x/net/http2/h2c"
	"google.golang.org/protobuf/encoding/protojson"
	"google.golang.org/protobuf/proto"
	"google.golang.org/protobuf/types/known/structpb"

	castlev1 "github.com/brokenbots/castle/castle/gen/castle/v1"
	castlev1connect "github.com/brokenbots/castle/castle/gen/castle/v1/castlev1connect"
	"github.com/brokenbots/castle/castle/internal/auth"
	"github.com/brokenbots/castle/castle/internal/store"
	pb "github.com/brokenbots/criteria/sdk/pb/criteria/v1"                // import-lint:allow castle service bindings (W08: move to castle-proto)
	"github.com/brokenbots/criteria/sdk/pb/criteria/v1/criteriav1connect" // import-lint:allow castle service bindings (W08: move to castle-proto)
)

// archiveStartedAt is the fixed record timestamp so round-trip comparisons
// are exact.
var archiveStartedAt = time.Date(2025, 10, 20, 7, 30, 0, 123456789, time.UTC)

// archiveOnlyServer mounts the archive service (and, when auth is requested,
// the criteria service for anonymous Register) so archive wire tests exercise
// exactly the KB-308 surface without re-tooling the shared all-services mux.
func archiveOnlyServer(t *testing.T, ts *testStack, withAuth, allowAnonReads bool, opts ...connect.HandlerOption) *httptest.Server {
	t.Helper()
	mux := http.NewServeMux()
	if withAuth {
		regPath, regHandler := criteriav1connect.NewCriteriaServiceHandler(ts.criteria, opts...)
		mux.Handle(regPath, regHandler)
	}
	path, handler := castlev1connect.NewRunArchiveServiceHandler(ts.server, opts...)
	mux.Handle(path, handler)
	tsrv := httptest.NewUnstartedServer(h2c.NewHandler(mux, &http2.Server{}))
	tsrv.Start()
	t.Cleanup(tsrv.Close)
	return tsrv
}

// archiveClient builds a RunArchiveServiceClient against the test server.
func archiveClient(tsrv *httptest.Server) castlev1connect.RunArchiveServiceClient {
	return castlev1connect.NewRunArchiveServiceClient(h2cClient(), tsrv.URL)
}

// archiveRunID is the shared id for identity-matrix runs; ArchiveRun is an
// upsert, so matrix callers re-archiving the same id stay isolated.
const archiveRunID = "run-matrix"

// archiveRecordProto builds a full KB-308 archive record: workflow identity,
// checksums, operator version, every monitored condition, a failure verdict,
// the visited-step summary, lifecycle totals, timestamps and run ids.
func archiveRecordProto(runID string) *castlev1.ArchiveRecord {
	rec := &castlev1.ArchiveRecord{
		SchemaVersion:            1,
		RunId:                    runID,
		JobName:                  "wf-ticket-42-7fb2c9",
		Ticket:                   "KB-42",
		Repo:                     "git@github.com:brokenbots/workflow-example.git",
		WorkflowName:             "criteria-agent-run",
		WorkflowRef:              "refs/heads/main",
		TreeDigest:               "sha256:9f2c",
		BindingConfigmapChecksum: "cm-binding:8e91",
		ProjectConfigmapChecksum: "cm-project:77aa",
		OperatorVersion:          "operator-v2025.10.3",
		FinalPhase:               "Failed",
		Conditions: []*castlev1.ArchiveCondition{
			{Type: "StallWatchdog", Status: "False", Reason: "NotStalled"},
			{Type: "BaseImageMismatch", Status: "False", Reason: "ImageMatched"},
			{Type: "ConfigRenderDrift", Status: "False", Reason: "RenderStable"},
			{Type: "TerminalCleanup", Status: "True", Reason: "TerminalRestartPending"},
		},
		FailureReason:    "verdict: agent failed: tests exited non-zero",
		Steps:            []*castlev1.ArchiveStep{{Name: "prepare", Outcome: "succeeded"}, {Name: "run", Outcome: "failed"}},
		Restarts:         2,
		BackoffEvictions: 1,
		TerminalCleanup:  "deleted",
		StartedAt:        archiveStartedAt.Format(time.RFC3339Nano),
		EndedAt:          archiveStartedAt.Add(41 * time.Minute).Format(time.RFC3339Nano),
	}
	return rec
}

func archiveRunReq(rec *castlev1.ArchiveRecord) *connect.Request[castlev1.ArchiveRunRequest] {
	return connect.NewRequest(&castlev1.ArchiveRunRequest{Record: rec})
}

func requireStoredRow(t *testing.T, st store.Store, runID string) *store.RunArchiveRecord {
	t.Helper()
	rec, err := st.GetRunArchiveRecord(context.Background(), runID)
	if err != nil {
		t.Fatalf("stored row for %s: %v", runID, err)
	}
	return rec
}

// redecodeRecord re-serializes the structpb.Struct body served by
// GetRunArchive back into the ArchiveRecord message so tests can compare
// field-for-field with proto.Equal.
func redecodeRecord(t *testing.T, body *structpb.Struct) *castlev1.ArchiveRecord {
	t.Helper()
	b, err := protojson.Marshal(body)
	if err != nil {
		t.Fatalf("re-marshal record struct: %v", err)
	}
	rec := &castlev1.ArchiveRecord{}
	opts := protojson.UnmarshalOptions{DiscardUnknown: true}
	if err := opts.Unmarshal(b, rec); err != nil {
		t.Fatalf("re-decode record: %v", err)
	}
	return rec
}

func TestArchiveRunStoresAndRoundTrips(t *testing.T) {
	handler := &recordingSlogHandler{}
	ts := newTestStackWithLog(t, slog.New(handler))
	client := archiveClient(archiveOnlyServer(t, ts, false, false))
	runID := "run-roundtrip"

	resp, err := client.ArchiveRun(context.Background(), archiveRunReq(archiveRecordProto(runID)))
	if err != nil {
		t.Fatalf("archive run: %v", err)
	}
	if got, want := resp.Msg.GetArchiveId(), store.ArchiveID(runID); got != want {
		t.Fatalf("archive id = %q want %q", got, want)
	}
	if !resp.Msg.GetCreated() {
		t.Fatal("first archive of a run must report created=true")
	}
	if resp.Msg.GetArchivedAt() == nil {
		t.Fatal("response must stamp archived_at")
	}

	// Log-provable gate: a "run archive stored" line carrying the ids the
	// operator stamps into the CR status, and never the record body (it can
	// carry ticket titles and repo URLs not cleared for castle logs).
	found := false
	for _, entry := range handler.snapshot() {
		if entry.Message != "run archive stored" {
			continue
		}
		found = true
		if entry.Attrs["run_id"] != runID {
			t.Errorf("log run_id = %v want %q", entry.Attrs["run_id"], runID)
		}
		if entry.Attrs["archive_id"] != store.ArchiveID(runID) {
			t.Errorf("log archive_id = %v want %q", entry.Attrs["archive_id"], store.ArchiveID(runID))
		}
		if _, hasBody := entry.Attrs["record"]; hasBody {
			t.Error("record body must never appear in castle logs")
		}
	}
	if !found {
		t.Fatal(`expected a "run archive stored" log line`)
	}

	// Storage-neutral columns the compact list surface reads.
	row := requireStoredRow(t, ts.store, runID)
	if row.JobName != "wf-ticket-42-7fb2c9" || row.FinalPhase != "Failed" || row.Ticket != "KB-42" {
		t.Fatalf("stored columns lost data: job=%q phase=%q ticket=%q", row.JobName, row.FinalPhase, row.Ticket)
	}
	if row.ArchivedAt.IsZero() {
		t.Fatal("archived_at must be persisted")
	}

	// The full record round-trips through the stored JSON exactly.
	got, err := client.GetRunArchive(context.Background(), connect.NewRequest(&castlev1.GetRunArchiveRequest{RunId: runID}))
	if err != nil {
		t.Fatalf("get run archive: %v", err)
	}
	if got.Msg.GetRunId() != runID || got.Msg.GetArchivedAt() == nil {
		t.Fatalf("response metadata lost: %+v", got.Msg)
	}
	if !proto.Equal(redecodeRecord(t, got.Msg.GetRecord()), archiveRecordProto(runID)) {
		t.Fatal("record did not round-trip through storage")
	}
}

func TestArchiveRunReplayOverwrites(t *testing.T) {
	ts := newTestStack(t)
	client := archiveClient(archiveOnlyServer(t, ts, false, false))
	runID := "run-replay"

	r1, err := client.ArchiveRun(context.Background(), archiveRunReq(archiveRecordProto(runID)))
	if err != nil {
		t.Fatalf("first archive: %v", err)
	}
	if !r1.Msg.GetCreated() {
		t.Fatal("first archive must be created")
	}

	// The operator's retry-with-backoff leg replays; latest body wins and no
	// duplicate history row may appear.
	retry := archiveRecordProto(runID)
	retry.FinalPhase = "Succeeded"
	retry.FailureReason = ""
	r2, err := client.ArchiveRun(context.Background(), archiveRunReq(retry))
	if err != nil {
		t.Fatalf("retry archive: %v", err)
	}
	if r2.Msg.GetCreated() {
		t.Fatal("re-archive must report created=false")
	}

	list, err := client.ListRunArchives(context.Background(), connect.NewRequest(&castlev1.ListRunArchivesRequest{}))
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(list.Msg.GetArchives()) != 1 {
		t.Fatalf("list records = %d, want exactly 1", len(list.Msg.GetArchives()))
	}

	got, err := client.GetRunArchive(context.Background(), connect.NewRequest(&castlev1.GetRunArchiveRequest{RunId: runID}))
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if !proto.Equal(redecodeRecord(t, got.Msg.GetRecord()), retry) {
		t.Fatal("latest replayed archive body must win")
	}
}

func TestArchiveRunInvalidArguments(t *testing.T) {
	ts := newTestStack(t)
	client := archiveClient(archiveOnlyServer(t, ts, false, false))

	cases := map[string]*connect.Request[castlev1.ArchiveRunRequest]{
		"nil record": connect.NewRequest(&castlev1.ArchiveRunRequest{Record: nil}),
		"empty run_id": archiveRunReq(&castlev1.ArchiveRecord{
			SchemaVersion: 1, FinalPhase: "Failed",
		}),
	}
	for name, req := range cases {
		_, err := client.ArchiveRun(context.Background(), req)
		if err == nil {
			t.Fatalf("%s: expected InvalidArgument", name)
		}
		if connect.CodeOf(err) != connect.CodeInvalidArgument {
			t.Fatalf("%s: code = %s want InvalidArgument", name, connect.CodeOf(err))
		}
	}
}

func TestGetRunArchiveNotFound(t *testing.T) {
	ts := newTestStack(t)
	client := archiveClient(archiveOnlyServer(t, ts, false, false))
	resp, err := client.GetRunArchive(context.Background(), connect.NewRequest(&castlev1.GetRunArchiveRequest{RunId: "run-unknown"}))
	if resp != nil || err == nil {
		t.Fatalf("expected NotFound, got resp=%v err=%v", resp, err)
	}
	if connect.CodeOf(err) != connect.CodeNotFound {
		t.Fatalf("code = %s want NotFound", connect.CodeOf(err))
	}
}

func wireRunID(i int) string { return fmt.Sprintf("run-%04d", i) }

func TestListRunArchivesWirePaging(t *testing.T) {
	ts := newTestStack(t)
	client := archiveClient(archiveOnlyServer(t, ts, false, false))
	for i := 1; i <= 4; i++ {
		if _, err := client.ArchiveRun(context.Background(), archiveRunReq(archiveRecordProto(wireRunID(i)))); err != nil {
			t.Fatalf("archive %d: %v", i, err)
		}
	}

	// Default listing: all four, ascending, no cursor.
	all, err := client.ListRunArchives(context.Background(), connect.NewRequest(&castlev1.ListRunArchivesRequest{}))
	if err != nil {
		t.Fatalf("default list: %v", err)
	}
	if got := len(all.Msg.GetArchives()); got != 4 {
		t.Fatalf("default list = %d records, want 4", got)
	}
	if all.Msg.GetNextCursor() != "" {
		t.Fatalf("default list must be complete, got cursor %q", all.Msg.GetNextCursor())
	}
	for i, s := range all.Msg.GetArchives() {
		if want := wireRunID(i + 1); s.GetRunId() != want {
			t.Fatalf("order: position %d = %q want %q", i, s.GetRunId(), want)
		}
	}
	// Summary mirrors the storage-neutral columns.
	s := all.Msg.GetArchives()[0]
	if s.GetArchiveId() != store.ArchiveID(s.GetRunId()) || s.GetFinalPhase() != "Failed" || s.GetTicket() != "KB-42" {
		t.Fatalf("summary lost columns: %+v", s)
	}

	// Cursored walk at limit 2.
	page, err := client.ListRunArchives(context.Background(), connect.NewRequest(&castlev1.ListRunArchivesRequest{Limit: 2}))
	if err != nil {
		t.Fatalf("page 1: %v", err)
	}
	if n := len(page.Msg.GetArchives()); n != 2 {
		t.Fatalf("page 1 = %d records, want 2", n)
	}
	cur := page.Msg.GetNextCursor()
	if cur == "" {
		t.Fatal("page 1 must carry a cursor")
	}
	page2, err := client.ListRunArchives(context.Background(), connect.NewRequest(&castlev1.ListRunArchivesRequest{Limit: 2, Cursor: cur}))
	if err != nil {
		t.Fatalf("page 2: %v", err)
	}
	if n := len(page2.Msg.GetArchives()); n != 2 {
		t.Fatalf("page 2 = %d records, want 2", n)
	}
	if page2.Msg.GetArchives()[0].GetRunId() != wireRunID(3) || page2.Msg.GetArchives()[1].GetRunId() != wireRunID(4) {
		t.Fatalf("page 2 order wrong: %s, %s", page2.Msg.GetArchives()[0].GetRunId(), page2.Msg.GetArchives()[1].GetRunId())
	}
	if page2.Msg.GetNextCursor() != "" {
		t.Fatalf("walk must end with an empty cursor, got %q", page2.Msg.GetNextCursor())
	}

	// Prefix filter narrows without paging surprises.
	pl, err := client.ListRunArchives(context.Background(), connect.NewRequest(&castlev1.ListRunArchivesRequest{RunIdPrefix: "run-000", Limit: 2}))
	if err != nil {
		t.Fatalf("prefix list: %v", err)
	}
	if n := len(pl.Msg.GetArchives()); n != 2 {
		t.Fatalf("prefix page = %d records, want 2 of 4", n)
	}
}

func TestListRunArchivesInvalidCursor(t *testing.T) {
	ts := newTestStack(t)
	client := archiveClient(archiveOnlyServer(t, ts, false, false))
	_, err := client.ListRunArchives(context.Background(), connect.NewRequest(&castlev1.ListRunArchivesRequest{Cursor: "!!not-a-cursor!!"}))
	if err == nil {
		t.Fatal("expected InvalidArgument")
	}
	if connect.CodeOf(err) != connect.CodeInvalidArgument {
		t.Fatalf("code = %s want InvalidArgument", connect.CodeOf(err))
	}
}

// archiveAuthHarness carries the seeded identities for the interceptor-level
// archive authorization matrix. The agent token is minted through the real
// Register flow (the podstate feed token path); console and orchestrator
// identities are seeded directly, mirroring the auth package's own harnesses.
type archiveAuthHarness struct {
	ts         *testStack
	client     castlev1connect.RunArchiveServiceClient
	oClient    criteriav1connect.CriteriaServiceClient
	agentID    string
	agentToken string
	orchToken  string
	consoleTok string
}

func newArchiveAuthHarness(t *testing.T, allowAnonReads bool) *archiveAuthHarness {
	t.Helper()
	ts := newTestStack(t)
	opts := connect.WithInterceptors(auth.NewInterceptor(ts.store, allowAnonReads, auth.WithAnonRegister()))
	tsrv := archiveOnlyServer(t, ts, true, allowAnonReads, opts)
	h := &archiveAuthHarness{
		ts:      ts,
		client:  archiveClient(tsrv),
		oClient: criteriav1connect.NewCriteriaServiceClient(h2cClient(), tsrv.URL),
	}
	reg, err := h.oClient.Register(context.Background(), connect.NewRequest(&pb.RegisterRequest{Name: "agent"}))
	if err != nil {
		t.Fatalf("register agent: %v", err)
	}
	h.agentID, h.agentToken = reg.Msg.GetCriteriaId(), reg.Msg.GetToken()

	provisionOrchestratorIdentity(t, ts.store, "orch-1", "orch-token-1")
	h.orchToken = "orch-token-1"

	hash, err := auth.HashPassword("op-password")
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	if err := ts.store.UpsertConsoleUser(context.Background(), &store.ConsoleUser{
		ID: "console-1", Username: "operator", PasswordHash: hash, CreatedAt: now, UpdatedAt: now,
	}); err != nil {
		t.Fatal(err)
	}
	sessTok, err := auth.NewSessionToken()
	if err != nil {
		t.Fatal(err)
	}
	if err := ts.store.CreateConsoleSession(context.Background(), &store.ConsoleSession{
		ID: "sess-1", UserID: "console-1", TokenHash: auth.HashToken(sessTok), CreatedAt: now,
	}); err != nil {
		t.Fatal(err)
	}
	h.consoleTok = sessTok

	// Seed one archive record directly in the store so read-allowed matrix
	// rows assert their authorization verdict against real data (denied
	// callers get PermissionDenied/Unauthenticated, allowed ones reach the
	// handler and find the row).
	body, err := protojson.Marshal(archiveRecordProto(archiveRunID))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ts.store.PutRunArchiveRecord(context.Background(), &store.RunArchiveRecord{
		RunID: archiveRunID, JobName: "wf-ticket-42-7fb2c9", FinalPhase: "Failed", Ticket: "KB-42",
		Record: string(body), ArchivedAt: now,
	}); err != nil {
		t.Fatalf("seed archive row: %v", err)
	}
	return h
}

// archiveCall issues one archive-surface call as the holder of the given
// bearer token ("" = no Authorization header at all).
func archiveCall(ctx context.Context, client castlev1connect.RunArchiveServiceClient, method, token string) error {
	setToken := func(h http.Header) {
		if token != "" {
			h.Set("Authorization", "Bearer "+token)
		}
	}
	var err error
	switch method {
	case "archive":
		req := archiveRunReq(archiveRecordProto(archiveRunID))
		setToken(req.Header())
		_, err = client.ArchiveRun(ctx, req)
	case "get":
		req := connect.NewRequest(&castlev1.GetRunArchiveRequest{RunId: archiveRunID})
		setToken(req.Header())
		_, err = client.GetRunArchive(ctx, req)
	case "list":
		req := connect.NewRequest(&castlev1.ListRunArchivesRequest{})
		setToken(req.Header())
		_, err = client.ListRunArchives(ctx, req)
	default:
		err = fmt.Errorf("unknown method %q", method)
	}
	return err
}

// archiveCodeOf maps the matrix's recorded error to its connect Code; an
// absent error maps to Unknown so every matrix row asserts exactly once.
func archiveCodeOf(t *testing.T, err error) connect.Code {
	t.Helper()
	if err == nil {
		return connect.CodeUnknown
	}
	return connect.CodeOf(err)
}

func TestArchiveRunAuthMatrix(t *testing.T) {
	t.Run("no token", func(t *testing.T) {
		h := newArchiveAuthHarness(t, false)
		ctx := context.Background()
		if err := archiveCall(ctx, h.client, "archive", ""); archiveCodeOf(t, err) != connect.CodeUnauthenticated {
			t.Fatalf("archive: %v", err)
		}
		if err := archiveCall(ctx, h.client, "get", ""); archiveCodeOf(t, err) != connect.CodeUnauthenticated {
			t.Fatalf("get: %v", err)
		}
		if err := archiveCall(ctx, h.client, "list", ""); archiveCodeOf(t, err) != connect.CodeUnauthenticated {
			t.Fatalf("list: %v", err)
		}
	})
	t.Run("no token with anon reads", func(t *testing.T) {
		h := newArchiveAuthHarness(t, true)
		ctx := context.Background()
		if err := archiveCall(ctx, h.client, "get", ""); err != nil {
			t.Fatalf("get: %v", err)
		}
		if err := archiveCall(ctx, h.client, "list", ""); err != nil {
			t.Fatalf("list: %v", err)
		}
		// The write is never anon-readable, even with allowAnonReads on.
		if err := archiveCall(ctx, h.client, "archive", ""); archiveCodeOf(t, err) != connect.CodeUnauthenticated {
			t.Fatalf("archive: %v", err)
		}
	})
	t.Run("agent", func(t *testing.T) {
		h := newArchiveAuthHarness(t, false)
		ctx := context.Background()
		if err := archiveCall(ctx, h.client, "archive", h.agentToken); err != nil {
			t.Fatalf("archive: %v", err)
		}
		if err := archiveCall(ctx, h.client, "get", h.agentToken); err != nil {
			t.Fatalf("get: %v", err)
		}
		if err := archiveCall(ctx, h.client, "list", h.agentToken); err != nil {
			t.Fatalf("list: %v", err)
		}
		// Provenance stamp: the agent's criteria id is recorded for the
		// CR-side archive-id audit.
		stampReq := connect.NewRequest(&castlev1.ListRunArchivesRequest{RunIdPrefix: archiveRunID})
		stampReq.Header().Set("Authorization", "Bearer "+h.agentToken)
		list, err := h.client.ListRunArchives(ctx, stampReq)
		if err != nil {
			t.Fatalf("list: %v", err)
		}
		if len(list.Msg.GetArchives()) != 1 {
			t.Fatalf("archives = %d, want 1", len(list.Msg.GetArchives()))
		}
		if got, want := list.Msg.GetArchives()[0].GetOperatorId(), "criteria/"+h.agentID; got != want {
			t.Fatalf("operator_id = %q want %q", got, want)
		}
	})
	t.Run("orchestrator", func(t *testing.T) {
		h := newArchiveAuthHarness(t, false)
		ctx := context.Background()
		if err := archiveCall(ctx, h.client, "archive", h.orchToken); archiveCodeOf(t, err) != connect.CodePermissionDenied {
			t.Fatalf("archive: %v", err)
		}
		if err := archiveCall(ctx, h.client, "get", h.orchToken); err != nil {
			t.Fatalf("get: %v", err)
		}
		if err := archiveCall(ctx, h.client, "list", h.orchToken); err != nil {
			t.Fatalf("list: %v", err)
		}
	})
	t.Run("console", func(t *testing.T) {
		h := newArchiveAuthHarness(t, false)
		ctx := context.Background()
		if err := archiveCall(ctx, h.client, "archive", h.consoleTok); archiveCodeOf(t, err) != connect.CodePermissionDenied {
			t.Fatalf("archive: %v", err)
		}
		if err := archiveCall(ctx, h.client, "get", h.consoleTok); err != nil {
			t.Fatalf("get: %v", err)
		}
		if err := archiveCall(ctx, h.client, "list", h.consoleTok); err != nil {
			t.Fatalf("list: %v", err)
		}
	})
}

// compile-time guard: the archive handler surface stays implemented.
var _ castlev1connect.RunArchiveServiceHandler = (*ServerServer)(nil)
