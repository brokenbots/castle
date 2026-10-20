package rpc

import (
	"context"
	"errors"
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
		// The Register flow (shared helper): real agent tokens, not fixtures.
		mountCriteriaHandler(mux, ts, opts...)
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
// the visited-step summary, lifecycle totals, timestamps and run ids. The
// second argument sets the record's archive_key ("" for run-backed records).
func archiveRecordProto(runID, archiveKey string) *castlev1.ArchiveRecord {
	rec := &castlev1.ArchiveRecord{
		SchemaVersion:            1,
		RunId:                    runID,
		ArchiveKey:               archiveKey,
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

func requireStoredRow(t *testing.T, st store.Store, key string) *store.RunArchiveRecord {
	t.Helper()
	rec, err := st.GetRunArchiveRecord(context.Background(), key)
	if err != nil {
		t.Fatalf("stored row for %s: %v", key, err)
	}
	return rec
}

// archiveRecordNoRun builds the same full record for a terminal CR with no
// castle overseer run (KB-308 archive_key contract): the explicit
// CR-derived archive_key carries the identity and run_id stays empty.
func archiveRecordNoRun(key string) *castlev1.ArchiveRecord {
	return archiveRecordProto("", key)
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

	resp, err := client.ArchiveRun(context.Background(), archiveRunReq(archiveRecordProto(runID, "")))
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
	got, err := client.GetRunArchive(context.Background(), connect.NewRequest(&castlev1.GetRunArchiveRequest{Key: runID}))
	if err != nil {
		t.Fatalf("get run archive: %v", err)
	}
	if got.Msg.GetRunId() != runID || got.Msg.GetArchivedAt() == nil {
		t.Fatalf("response metadata lost: %+v", got.Msg)
	}
	if !proto.Equal(redecodeRecord(t, got.Msg.GetRecord()), archiveRecordProto(runID, "")) {
		t.Fatal("record did not round-trip through storage")
	}
}

func TestArchiveRunReplayOverwrites(t *testing.T) {
	ts := newTestStack(t)
	client := archiveClient(archiveOnlyServer(t, ts, false, false))
	runID := "run-replay"

	r1, err := client.ArchiveRun(context.Background(), archiveRunReq(archiveRecordProto(runID, "")))
	if err != nil {
		t.Fatalf("first archive: %v", err)
	}
	if !r1.Msg.GetCreated() {
		t.Fatal("first archive must be created")
	}

	// The operator's retry-with-backoff leg replays; latest body wins and no
	// duplicate history row may appear.
	retry := archiveRecordProto(runID, "")
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

	got, err := client.GetRunArchive(context.Background(), connect.NewRequest(&castlev1.GetRunArchiveRequest{Key: runID}))
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
		// Both ids empty: no run id, no explicit archive key — the CR would
		// have no stable identity to archive under (KB-308 archive_key
		// contract).
		"no run id and no archive key": archiveRunReq(&castlev1.ArchiveRecord{
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
	resp, err := client.GetRunArchive(context.Background(), connect.NewRequest(&castlev1.GetRunArchiveRequest{Key: "run-unknown"}))
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
		if _, err := client.ArchiveRun(context.Background(), archiveRunReq(archiveRecordProto(wireRunID(i), ""))); err != nil {
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

	// Prefix filter narrows without paging surprises...
	pl, err := client.ListRunArchives(context.Background(), connect.NewRequest(&castlev1.ListRunArchivesRequest{KeyPrefix: "run-000", Limit: 2}))
	if err != nil {
		t.Fatalf("prefix list: %v", err)
	}
	if n := len(pl.Msg.GetArchives()); n != 2 {
		t.Fatalf("prefix page = %d records, want 2 of 4", n)
	}
	if pl.Msg.GetArchives()[0].GetArchiveId() != store.ArchiveID(wireRunID(1)) {
		t.Fatalf("prefix summary archive id: %q", pl.Msg.GetArchives()[0].GetArchiveId())
	}

	// Prefix AND cursor in one request: the prefix page walk honors the
	// cursor boundary and stays inside the prefix space.
	pc, err := client.ListRunArchives(context.Background(), connect.NewRequest(&castlev1.ListRunArchivesRequest{KeyPrefix: "run-000", Limit: 2}))
	if err != nil {
		t.Fatalf("prefix+cursor page 1: %v", err)
	}
	if pc.Msg.GetNextCursor() == "" {
		t.Fatal("prefix+cursor page 1 must carry a cursor")
	}
	pc2, err := client.ListRunArchives(context.Background(), connect.NewRequest(&castlev1.ListRunArchivesRequest{KeyPrefix: "run-000", Limit: 2, Cursor: pc.Msg.GetNextCursor()}))
	if err != nil {
		t.Fatalf("prefix+cursor page 2: %v", err)
	}
	if n := len(pc2.Msg.GetArchives()); n != 2 || pc2.Msg.GetArchives()[0].GetRunId() != wireRunID(3) || pc2.Msg.GetNextCursor() != "" {
		t.Fatalf("prefix+cursor page 2 incomplete: n=%d next=%q", n, pc2.Msg.GetNextCursor())
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

func TestGetRunArchiveRequiresKey(t *testing.T) {
	ts := newTestStack(t)
	client := archiveClient(archiveOnlyServer(t, ts, false, false))
	_, err := client.GetRunArchive(context.Background(), connect.NewRequest(&castlev1.GetRunArchiveRequest{}))
	if err == nil {
		t.Fatal("expected InvalidArgument for empty key")
	}
	if connect.CodeOf(err) != connect.CodeInvalidArgument {
		t.Fatalf("code = %s want InvalidArgument", connect.CodeOf(err))
	}
}

func TestArchiveRunWithoutCastleRunID(t *testing.T) {
	// Terminal CRs with no castle overseer run (run that died pre-register)
	// archive under their explicit CR-derived archive_key (KB-308
	// archive_key contract) — the run-id-less record class can never be
	// unarchivable, or the archive-before-finalizer leg would block forever.
	key := "cr-uid/0f31e9a2-7d4c-4b8e-9f01-2a6b5c8d7e10"
	logHandler := &recordingSlogHandler{}
	ts := newTestStackWithLog(t, slog.New(logHandler))
	client := archiveClient(archiveOnlyServer(t, ts, false, false))

	resp, err := client.ArchiveRun(context.Background(), archiveRunReq(archiveRecordNoRun(key)))
	if err != nil {
		t.Fatalf("archive run-id-less record: %v", err)
	}
	if want := store.ArchiveID(key); resp.Msg.GetArchiveId() != want {
		t.Fatalf("archive id = %q want %q", resp.Msg.GetArchiveId(), want)
	}
	if !resp.Msg.GetCreated() {
		t.Fatal("first archive must report created=true")
	}

	// Wire round-trip by key.
	got, err := client.GetRunArchive(context.Background(), connect.NewRequest(&castlev1.GetRunArchiveRequest{Key: key}))
	if err != nil {
		t.Fatalf("get by archive key: %v", err)
	}
	if got.Msg.GetRunId() != "" {
		t.Fatalf("run-id-less record must keep run_id empty, got %q", got.Msg.GetRunId())
	}
	roundTripped := redecodeRecord(t, got.Msg.GetRecord())
	if !proto.Equal(roundTripped, archiveRecordNoRun(key)) {
		t.Fatal("run-id-less record did not round-trip through storage")
	}
	if roundTripped.GetArchiveKey() != key {
		t.Fatalf("archive_key must survive the round trip, got %q", roundTripped.GetArchiveKey())
	}

	// Replay of the same key: created=false, exactly one history row.
	replay, err := client.ArchiveRun(context.Background(), archiveRunReq(archiveRecordNoRun(key)))
	if err != nil {
		t.Fatalf("replay archive: %v", err)
	}
	if replay.Msg.GetCreated() {
		t.Fatal("replay of the same key must report created=false")
	}
	list, err := client.ListRunArchives(context.Background(), connect.NewRequest(&castlev1.ListRunArchivesRequest{KeyPrefix: "cr-uid/"}))
	if err != nil {
		t.Fatalf("list by prefix: %v", err)
	}
	if len(list.Msg.GetArchives()) != 1 || list.Msg.GetArchives()[0].GetRunId() != "" {
		t.Fatalf("run-id-less replay duplicated history: %+v", list.Msg.GetArchives())
	}

	// Log-provable: the key and archive id are logged, never the body.
	found := false
	for _, entry := range logHandler.snapshot() {
		if entry.Message != "run archive stored" {
			continue
		}
		found = true
		if entry.Attrs["archive_key"] != key || entry.Attrs["run_id"] != "" {
			t.Errorf("log identity attrs = %v/%v want run_id empty", entry.Attrs["archive_key"], entry.Attrs["run_id"])
		}
		if _, hasBody := entry.Attrs["record"]; hasBody {
			t.Error("record body must never appear in castle logs")
		}
	}
	if !found {
		t.Fatal(`expected a "run archive stored" log line`)
	}
}

// archiveAuthed builds an archive request bearing the given bearer token.
func archiveAuthed(token string, rec *castlev1.ArchiveRecord) *connect.Request[castlev1.ArchiveRunRequest] {
	req := archiveRunReq(rec)
	req.Header().Set("Authorization", "Bearer "+token)
	return req
}

// newArchiveOwnershipStack wires the archive surface behind the real auth
// interceptor with two registered agents; agent A (token A) owns castle run
// "run-owned", agent B (token B) owns castle run "run-owned-b" — both needed
// because the effective-archive-key ownership gate resolves against the key
// each caller names, so an adversarial request must name a run the caller
// DOES own to prove the gate follows the key and not run_id. Returns the
// owner's criteria id too, for provenance asserts.
func newArchiveOwnershipStack(t *testing.T) (castlev1connect.RunArchiveServiceClient, *testStack, string, string, string) {
	t.Helper()
	ts := newTestStack(t)
	opts := connect.WithInterceptors(auth.NewInterceptor(ts.store, false, auth.WithAnonRegister()))
	tsrv := archiveOnlyServer(t, ts, true, false, opts)
	client := archiveClient(tsrv)
	agentA, tokenA := criteriaRegisterAgent(t, tsrv, "agent-a")
	agentB, tokenB := criteriaRegisterAgent(t, tsrv, "agent-b")

	if err := ts.store.CreateRun(context.Background(), &store.Run{
		ID: "run-owned", OverseerID: agentA, Status: "failed", CreatedAt: time.Now().UTC(),
	}); err != nil {
		t.Fatalf("seed owned run: %v", err)
	}
	if err := ts.store.CreateRun(context.Background(), &store.Run{
		ID: "run-owned-b", OverseerID: agentB, Status: "failed", CreatedAt: time.Now().UTC(),
	}); err != nil {
		t.Fatalf("seed second agent's run: %v", err)
	}
	return client, ts, tokenA, tokenB, agentA
}

func TestArchiveRunOwnershipEnforced(t *testing.T) {
	client, ts, tokenA, tokenB, agentA := newArchiveOwnershipStack(t)
	ctx := context.Background()

	// The owning agent archives its run and replays safely.
	resp, err := client.ArchiveRun(ctx, archiveAuthed(tokenA, archiveRecordProto("run-owned", "")))
	if err != nil {
		t.Fatalf("owner archive: %v", err)
	}
	if !resp.Msg.GetCreated() {
		t.Fatal("owner first archive must be created")
	}
	retry, err := client.ArchiveRun(ctx, archiveAuthed(tokenA, archiveRecordProto("run-owned", "")))
	if err != nil {
		t.Fatalf("owner replay: %v", err)
	}
	if retry.Msg.GetCreated() {
		t.Fatal("owner replay must report created=false")
	}

	// A second agent's token CANNOT forge history for a run it never owned.
	_, err = client.ArchiveRun(ctx, archiveAuthed(tokenB, archiveRecordProto("run-owned", "")))
	if connect.CodeOf(err) != connect.CodePermissionDenied {
		t.Fatalf("cross-agent archive of an owned run = %v want PermissionDenied", err)
	}
	// The attempted forgery must not have touched the stored history.
	row := requireStoredRow(t, ts.store, "run-owned")
	if row.OperatorID != "criteria/"+agentA {
		t.Fatalf("refused overwrite changed ownership stamp: got %q want criteria/%s", row.OperatorID, agentA)
	}

	// Unknown run ids (with the interceptor wired) are NotFound, not a
	// silent synthetic archive.
	_, err = client.ArchiveRun(ctx, archiveAuthed(tokenB, archiveRecordProto("run-never-existed", "")))
	if connect.CodeOf(err) != connect.CodeNotFound {
		t.Fatalf("unknown run archive code = %s want NotFound (err=%v)", connect.CodeOf(err), err)
	}
}

func TestArchiveRunSyntheticKeyProvenanceEnforced(t *testing.T) {
	// Records with no castle run have no owner identity to look up, so
	// cross-writer protection rides the store's first-writer-wins
	// provenance gate: agent B mints its key, agent A cannot overwrite it.
	client, _, tokenA, tokenB, _ := newArchiveOwnershipStack(t)
	ctx := context.Background()

	key := "cr-uid/bbbb"
	if _, err := client.ArchiveRun(ctx, archiveAuthed(tokenB, archiveRecordNoRun(key))); err != nil {
		t.Fatalf("agent B mints its own synthetic-key record: %v", err)
	}

	_, err := client.ArchiveRun(ctx, archiveAuthed(tokenA, archiveRecordNoRun(key)))
	if connect.CodeOf(err) != connect.CodePermissionDenied {
		t.Fatalf("cross-agent overwrite of a synthetic-key record = %v want PermissionDenied", err)
	}

	// The writer itself still replays idempotently.
	replay, err := client.ArchiveRun(ctx, archiveAuthed(tokenB, archiveRecordNoRun(key)))
	if err != nil {
		t.Fatalf("agent B replay: %v", err)
	}
	if replay.Msg.GetCreated() {
		t.Fatal("same-identity replay must report created=false")
	}
}

// TestArchiveRunArchiveKeyCannotBypassOwnership pins the effective-key
// ownership gate (security review round 3, HIGH): the store row hangs off
// the EFFECTIVE key — archive_key when set, else run_id — so an ownership
// gate on run_id alone is bypassable. Before the fix: (a) a caller
// validating its own run while keying the record after another identity's
// run stored forged history under that run's key, and (b) a caller with
// run_id empty skipped the gate and squatted the key — turning the store's
// first-writer-wins provenance gate into a permanent denial of the
// legitimate owner's archive leg (archive failure retries and never skips,
// so #309 retention would delete that run's history unrecoverably). After
// the fix: key-vs-run_id mismatches are InvalidArgument, keys naming
// another identity's run are PermissionDenied, the squatted key stays
// unwritten (verified by absence), and the owner still lands its archive.
func TestArchiveRunArchiveKeyCannotBypassOwnership(t *testing.T) {
	client, ts, tokenA, tokenB, agentA := newArchiveOwnershipStack(t)
	ctx := context.Background()

	expectNoRow := func(key, label string) {
		t.Helper()
		if _, err := ts.store.GetRunArchiveRecord(ctx, key); !errors.Is(err, store.ErrNotFound) {
			t.Fatalf("%s: expected no archive row under %q, got %v", label, key, err)
		}
	}
	expectNoRow("run-owned", "pre-state")

	// (a) Agent B validates its OWN run but keys the record after agent A's
	// run: the effective key names a run B never owns → PermissionDenied,
	// and no history is stored under A's run id.
	_, err := client.ArchiveRun(ctx, archiveAuthed(tokenB, archiveRecordProto("run-owned-b", "run-owned")))
	if connect.CodeOf(err) != connect.CodePermissionDenied {
		t.Fatalf("own run keyed after another run's id = %v want PermissionDenied", connect.CodeOf(err))
	}
	expectNoRow("run-owned", "shape (a)")

	// (b) run_id empty with archive_key naming A's run: the ownership gate
	// must still fire on the effective key → PermissionDenied, no row.
	_, err = client.ArchiveRun(ctx, archiveAuthed(tokenB, archiveRecordProto("", "run-owned")))
	if connect.CodeOf(err) != connect.CodePermissionDenied {
		t.Fatalf("run-id-less key naming another run = %v want PermissionDenied", connect.CodeOf(err))
	}
	expectNoRow("run-owned", "shape (b)")

	// Squat-then-owner (DoS regression): after B's refused squats the
	// legitimate owner STILL lands its archive under its own run id — the
	// squat wrote nothing, so no first-writer-wins stamp blocks it.
	resp, err := client.ArchiveRun(ctx, archiveAuthed(tokenA, archiveRecordProto("run-owned", "")))
	if err != nil {
		t.Fatalf("owner archive after refused squats: %v", err)
	}
	if !resp.Msg.GetCreated() {
		t.Fatal("owner's first archive must be created after refused squats")
	}
	row := requireStoredRow(t, ts.store, "run-owned")
	if row.OperatorID != "criteria/"+agentA {
		t.Fatalf("owner provenance stamp = %q want criteria/%s", row.OperatorID, agentA)
	}

	// Run-backed body forgery (review round 4, R1): archive_key names a run
	// the caller DOES own — the ownership gate passes — so the record's
	// run_id side must be validated independently of whether the effective
	// key resolves. B keying after its own run while claiming A's run id,
	// a ghost id, or nothing (empty run_id under a run key) is the same
	// forged-attribution property, and none of these may write.
	_, err = client.ArchiveRun(ctx, archiveAuthed(tokenB, archiveRecordProto("run-owned", "run-owned-b")))
	if connect.CodeOf(err) != connect.CodeInvalidArgument {
		t.Fatalf("own run key claiming another run's id = %v want InvalidArgument", connect.CodeOf(err))
	}
	expectNoRow("run-owned-b", "run-backed forgery (foreign run_id)")
	_, err = client.ArchiveRun(ctx, archiveAuthed(tokenB, archiveRecordProto("run-ghost-not-a-run", "run-owned-b")))
	if connect.CodeOf(err) != connect.CodeInvalidArgument {
		t.Fatalf("own run key claiming a ghost run id = %v want InvalidArgument", connect.CodeOf(err))
	}
	expectNoRow("run-owned-b", "run-backed forgery (ghost run_id)")
	_, err = client.ArchiveRun(ctx, archiveAuthed(tokenB, archiveRecordProto("", "run-owned-b")))
	if connect.CodeOf(err) != connect.CodeInvalidArgument {
		t.Fatalf("empty run_id under a run key = %v want InvalidArgument", connect.CodeOf(err))
	}
	expectNoRow("run-owned-b", "run-backed forgery (empty run_id)")

	// Mismatch: run_id and archive_key both set and different →
	// InvalidArgument even when the caller owns the run_id side (aliasing
	// an owned run onto an arbitrary archive_key is also refused), and no
	// key is minted.
	_, err = client.ArchiveRun(ctx, archiveAuthed(tokenB, archiveRecordProto("run-owned-b", "cr-uid/mismatch-key")))
	if connect.CodeOf(err) != connect.CodeInvalidArgument {
		t.Fatalf("run_id/archive_key mismatch = %v want InvalidArgument", connect.CodeOf(err))
	}
	expectNoRow("cr-uid/mismatch-key", "mismatch")

	// Namespace: a run-id-less synthetic key outside the documented
	// CR-derived namespace is InvalidArgument, so bare names cannot become
	// squat-able record keys.
	_, err = client.ArchiveRun(ctx, archiveAuthed(tokenB, archiveRecordNoRun("forge-me-a-key")))
	if connect.CodeOf(err) != connect.CodeInvalidArgument {
		t.Fatalf("non-namespaced synthetic key = %v want InvalidArgument", connect.CodeOf(err))
	}
	expectNoRow("forge-me-a-key", "namespace")
}

// archiveAuthHarness carries the seeded identities for the interceptor-level
// archive authorization matrix. The agent token is minted through the real
// Register flow (the podstate feed token path); console and orchestrator
// identities are seeded directly, mirroring the auth package's own harnesses.
type archiveAuthHarness struct {
	ts         *testStack
	client     castlev1connect.RunArchiveServiceClient
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
		ts:     ts,
		client: archiveClient(tsrv),
	}
	h.agentID, h.agentToken = criteriaRegisterAgent(t, tsrv, "agent")

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

	// Seed the castle run behind the matrix record under the registered
	// agent's ownership so the agent matrix row satisfies the per-run
	// ownership gate (KB-308 ArchiveRun authorization); other identities
	// are already token-class-denied before the gate.
	if err := ts.store.CreateRun(context.Background(), &store.Run{
		ID: archiveRunID, OverseerID: h.agentID, Status: "failed",
		CreatedAt: now,
	}); err != nil {
		t.Fatalf("seed matrix run: %v", err)
	}

	// Seed one archive record directly in the store — identity-less, so the
	// identity-bearing matrix writes adopt it — so read-allowed matrix
	// rows assert their authorization verdict against real data (denied
	// callers get PermissionDenied/Unauthenticated, allowed ones reach the
	// handler and find the row).
	body, err := protojson.Marshal(archiveRecordProto(archiveRunID, ""))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ts.store.PutRunArchiveRecord(context.Background(), &store.RunArchiveRecord{
		Key: archiveRunID, RunID: archiveRunID, JobName: "wf-ticket-42-7fb2c9", FinalPhase: "Failed", Ticket: "KB-42",
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
		req := archiveRunReq(archiveRecordProto(archiveRunID, ""))
		setToken(req.Header())
		_, err = client.ArchiveRun(ctx, req)
	case "get":
		req := connect.NewRequest(&castlev1.GetRunArchiveRequest{Key: archiveRunID})
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
		stampReq := connect.NewRequest(&castlev1.ListRunArchivesRequest{KeyPrefix: archiveRunID})
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
