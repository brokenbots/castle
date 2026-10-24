package relocate

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/brokenbots/castle/castle/internal/store"
	"github.com/brokenbots/castle/castle/internal/store/pg"
	"github.com/brokenbots/castle/castle/internal/store/pg/pgtest"
	"github.com/brokenbots/castle/castle/internal/store/sqlite"
)

// seedSource writes a representative slice of the live data model through the
// sqlite store API: an overseer with labels, an agent-initiated run with
// metadata, an assignment-born stub run with its event stream and an attempt,
// a queued assignment with labels, an orchestrator, a console user with a
// session, and a run archive row. Returns both run ids.
func seedSource(t *testing.T, st store.Store) (agentRunID, stubRunID string) {
	t.Helper()
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Microsecond)

	ov := &store.Overseer{
		ID: "ov-import", Name: "agent-a", Hostname: "host-a", Version: "v1.2.3",
		TokenHash: "hash-a", Status: "online", Labels: map[string]string{"pool": "gpu"},
		CreatedAt: now, LastSeenAt: now,
	}
	if err := st.CreateOverseer(ctx, ov); err != nil {
		t.Fatalf("CreateOverseer: %v", err)
	}

	r := &store.Run{
		ID: "run-agent", OverseerID: ov.ID, WorkflowName: "wf-agent", WorkflowHCL: "job {}",
		Status: "running", CreatedAt: now, StartedAt: &now,
		Ticket: "KB-1", RepoURL: "https://example/repo",
	}
	if err := st.CreateRun(ctx, r); err != nil {
		t.Fatalf("CreateRun: %v", err)
	}
	if err := st.SetRunMetadata(ctx, r.ID, "TK-9", "https://repo", "https://pr"); err != nil {
		t.Fatalf("SetRunMetadata: %v", err)
	}

	// Assignment-born runs: the queue creates a stub run with a NULL
	// overseer_id (sqlite 0007), so both run shapes exist in the import.
	stub := &store.Run{ID: "stub-run", WorkflowName: "wf-brief", WorkflowHCL: "inline", Status: "pending", CreatedAt: now}
	a, created, err := st.CreateWorkflowAssignment(ctx, &store.WorkflowAssignment{
		OwnerCriteriaID: "owner-1", RunID: stub.ID, WorkflowName: "wf-brief",
		WorkflowSource: "inline", LockfileSource: "lock-y", IdempotencyKey: "idem-1",
		State: store.WorkflowAssignmentStateQueued, CreatedAt: now, UpdatedAt: now,
		Labels: map[string]string{"pool": "gpu", "slot": "a"},
	})
	if err != nil || !created {
		t.Fatalf("CreateWorkflowAssignment: err=%v created=%v", err, created)
	}
	stubRunID = a.RunID
	corrs := []string{"", "corr-1", ""}
	for i, corr := range corrs {
		ev := &store.Event{
			SchemaVersion: store.EventSchemaVersion,
			RunID:         stubRunID,
			Type:          "run.step-entered",
			Ts:            now.Add(time.Duration(i) * time.Second),
			CorrelationID: corr,
			Payload:       []byte(`{"step":"boot","count":1}`),
		}
		if _, inserted, err := st.AppendEvent(ctx, ev); err != nil || !inserted {
			t.Fatalf("AppendEvent %d: err=%v inserted=%v", i, err, inserted)
		}
	}
	if err := st.RecordAttemptStart(ctx, &store.RunAttempt{
		RunID: stubRunID, Step: "boot", Attempt: 1, StartedAt: now,
	}); err != nil {
		t.Fatalf("RecordAttemptStart: %v", err)
	}
	if err := st.RecordAttemptComplete(ctx, stubRunID, "boot", 1, "ok"); err != nil {
		t.Fatalf("RecordAttemptComplete: %v", err)
	}

	if err := st.UpsertOrchestrator(ctx, &store.Orchestrator{
		ID: "or-1", Name: "ops-one", TokenHash: "hasho", CreatedAt: now,
	}); err != nil {
		t.Fatalf("UpsertOrchestrator: %v", err)
	}
	u := &store.ConsoleUser{
		ID: "cu-1", Username: "alice", PasswordHash: "pw", CreatedAt: now, UpdatedAt: now,
	}
	if err := st.UpsertConsoleUser(ctx, u); err != nil {
		t.Fatalf("UpsertConsoleUser: %v", err)
	}
	if err := st.CreateConsoleSession(ctx, &store.ConsoleSession{
		ID: "cs-1", UserID: u.ID, TokenHash: "tokhash", CreatedAt: now,
	}); err != nil {
		t.Fatalf("CreateConsoleSession: %v", err)
	}
	if _, err := st.PutRunArchiveRecord(ctx, &store.RunArchiveRecord{
		Key: "run-archive/" + stubRunID, RunID: stubRunID, JobName: "brief",
		FinalPhase: "done", Ticket: "KB-1", Record: `{"k":1,"z":9}`,
		OperatorID: "ops-one", ArchivedAt: now,
	}); err != nil {
		t.Fatalf("PutRunArchiveRecord: %v", err)
	}
	return r.ID, stubRunID
}

// TestImportRoundTrip drives the full cutover contract through real engines:
// sqlite writes via the store API, a cold import into an empty pg database,
// row-for-row verification, then API-level reads back over the pg store.
func TestImportRoundTrip(t *testing.T) {
	ctx := context.Background()
	srcPath := filepath.Join(t.TempDir(), "castle.db")

	sst, err := sqlite.Open(srcPath)
	if err != nil {
		t.Fatalf("open sqlite store: %v", err)
	}
	agentRunID, stubRunID := seedSource(t, sst)
	if err := sst.Close(); err != nil {
		t.Fatalf("close sqlite store: %v", err)
	}

	src, err := OpenSource(srcPath)
	if err != nil {
		t.Fatalf("OpenSource: %v", err)
	}
	defer src.Close()

	pgDSN := pgtest.DSN(t)

	// pg.Open runs the embedded migrations; the importer then works on a raw
	// connection to the migrated database.
	migrated, err := pg.Open(pgDSN)
	if err != nil {
		t.Fatalf("migrate target: %v", err)
	}
	if err := migrated.Close(); err != nil {
		t.Fatalf("close migrated target: %v", err)
	}
	dst, err := OpenTarget(pgDSN)
	if err != nil {
		t.Fatalf("OpenTarget: %v", err)
	}
	defer dst.Close()

	reports, err := Import(ctx, src, dst, 1000, false)
	if err != nil {
		t.Fatalf("Import: %v", err)
	}
	seen := map[string]TableReport{}
	for _, rep := range reports {
		if !rep.Verified {
			t.Errorf("%s: not verified: %s", rep.Name, rep.Detail)
		}
		seen[rep.Name] = rep
	}
	if rep := seen["events"]; rep.Source != 3 || rep.Imported != 3 {
		t.Errorf("events: source %d imported %d", rep.Source, rep.Imported)
	}
	if rep := seen["workflow_assignment_labels"]; rep.Source != 2 || rep.Imported != 2 {
		t.Errorf("labels: source %d imported %d", rep.Source, rep.Imported)
	}
	if rep := seen["run_archive"]; rep.Source != 1 || rep.Imported != 1 {
		t.Errorf("run_archive: source %d imported %d", rep.Source, rep.Imported)
	}

	// Re-open through the store API and prove the imported data serves
	// heartbeat, run reads, event replay, assignment reads, console reads
	// and archive reads on the pg engine.
	target, err := pg.Open(pgDSN)
	if err != nil {
		t.Fatalf("pg.Open after import: %v", err)
	}
	defer target.Close()

	if err := target.UpdateOverseerSeen(ctx, "ov-import", time.Now().UTC()); err != nil {
		t.Fatalf("UpdateOverseerSeen on imported db: %v", err)
	}
	overs, err := target.ListOverseers(ctx)
	if err != nil {
		t.Fatalf("ListOverseers on imported db: %v", err)
	}
	var imported *store.Overseer
	for _, o := range overs {
		if o.ID == "ov-import" {
			imported = o
		}
	}
	if imported == nil || imported.Labels["pool"] != "gpu" {
		t.Fatalf("imported overseer/labels: %+v", imported)
	}

	runs, _, err := target.ListRuns(ctx, "", "", 10, "")
	if err != nil {
		t.Fatalf("ListRuns on imported db: %v", err)
	}
	// Two runs: the agent-initiated one and the assignment-born stub.
	if len(runs) != 2 {
		t.Fatalf("ListRuns on imported db: got %d runs, want 2", len(runs))
	}
	var agentRun, stubRun *store.Run
	for _, r := range runs {
		switch r.ID {
		case agentRunID:
			agentRun = r
		case stubRunID:
			stubRun = r
		}
	}
	if agentRun == nil || stubRun == nil {
		t.Fatalf("imported runs missing shapes: %+v", runs)
	}
	// Agent run: metadata columns survive the import; assignment stub keeps
	// its NULL overseer and never-started shape.
	if agentRun.Ticket != "TK-9" || agentRun.RepoURL != "https://repo" || agentRun.PRURL != "https://pr" {
		t.Fatalf("agent run metadata not imported: %+v", agentRun)
	}
	if stubRun.StartedAt != nil {
		t.Fatalf("stub run started_at should stay NULL: %+v", stubRun)
	}
	evs, err := target.ListEvents(ctx, stubRunID, 0, 100)
	if err != nil {
		t.Fatalf("ListEvents on imported db: %v", err)
	}
	if len(evs) != 3 || evs[1].CorrelationID != "corr-1" {
		t.Fatalf("imported events: %d entries, corr=%q", len(evs), evs[1].CorrelationID)
	}
	a, err := target.GetWorkflowAssignmentByRunID(ctx, stubRunID)
	if err != nil {
		t.Fatalf("GetWorkflowAssignmentByRunID on imported db: %v", err)
	}
	if a.IdempotencyKey != "idem-1" || a.Labels["slot"] != "a" {
		t.Fatalf("imported assignment: %+v", a)
	}
	latt, err := target.GetLatestAttempt(ctx, stubRunID, "boot")
	if err != nil || latt == nil || latt.Outcome != "ok" {
		t.Fatalf("GetLatestAttempt on imported db: %v %+v", err, latt)
	}
	orch, err := target.ListOrchestrators(ctx)
	if err != nil || len(orch) != 1 || orch[0].Name != "ops-one" {
		t.Fatalf("ListOrchestrators on imported db: %v %+v", err, orch)
	}
	u, err := target.GetConsoleUser(ctx, "alice")
	if err != nil || u == nil {
		t.Fatalf("GetConsoleUser on imported db: %v", err)
	}
	sess, err := target.ListConsoleSessions(ctx)
	if err != nil || len(sess) != 1 || sess[0].TokenHash != "tokhash" {
		t.Fatalf("ListConsoleSessions on imported db: %v", err)
	}
	rec, err := target.GetRunArchiveRecord(ctx, "run-archive/"+stubRunID)
	if err != nil || rec == nil {
		t.Fatalf("GetRunArchiveRecord on imported db: %v", err)
	}
	if rec.Record != `{"k":1,"z":9}` || rec.OperatorID != "ops-one" {
		t.Fatalf("imported archive record: %+v", rec)
	}
}

// TestImportRefusesNonEmpty pins the fresh-target gate: a second import into
// a used pg database must fail, and the override must be explicit.
func TestImportRefusesNonEmpty(t *testing.T) {
	ctx := context.Background()
	srcPath := filepath.Join(t.TempDir(), "castle.db")
	sst, err := sqlite.Open(srcPath)
	if err != nil {
		t.Fatalf("open sqlite store: %v", err)
	}
	defer sst.Close()
	seedSource(t, sst)

	src, err := OpenSource(srcPath)
	if err != nil {
		t.Fatalf("OpenSource: %v", err)
	}
	defer src.Close()

	pgDSN := pgtest.DSN(t)

	// pg.Open runs the embedded migrations; the importer then works on a raw
	// connection to the migrated database.
	migrated, err := pg.Open(pgDSN)
	if err != nil {
		t.Fatalf("migrate target: %v", err)
	}
	if err := migrated.Close(); err != nil {
		t.Fatalf("close migrated target: %v", err)
	}
	dst, err := OpenTarget(pgDSN)
	if err != nil {
		t.Fatalf("OpenTarget: %v", err)
	}
	defer dst.Close()

	if _, err := Import(ctx, src, dst, 100, false); err != nil {
		t.Fatalf("first import: %v", err)
	}
	if _, err := Import(ctx, src, dst, 100, false); err == nil {
		t.Fatal("second import into non-empty target: want error, got nil")
	}
	// Override succeeds only when explicitly requested; the identical source
	// is already present so the sample compare and counts agree again.
	if _, err := Import(ctx, src, dst, 100, true); err != nil {
		t.Fatalf("overridden import: %v", err)
	}
}

// TestParseSQLTimestamp pins the sqlite->pg timestamp conversion.
func TestParseSQLTimestamp(t *testing.T) {
	for _, in := range []string{
		"2024-01-02T03:04:05.123456Z",
		"2024-01-02T03:04:05Z",
		"2023-12-31T23:59:59.999999Z",
	} {
		if _, err := ParseSQLTimestamp(in); err != nil {
			t.Errorf("ParseSQLTimestamp(%q): %v", in, err)
		}
	}
	if _, err := ParseSQLTimestamp("not-a-time"); err == nil {
		t.Error("ParseSQLTimestamp(garbage): want error")
	}
}
