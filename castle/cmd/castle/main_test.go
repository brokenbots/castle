package main

import (
	"context"
	"flag"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/brokenbots/castle/castle/internal/store"
	"github.com/brokenbots/castle/castle/internal/store/pg/pgtest"
	"github.com/brokenbots/castle/castle/internal/store/refire"
	"github.com/brokenbots/castle/castle/internal/store/sqlite"
)

// anonReadsFlagDefault pins the KB-326 (S3) flag resolution: off by default
// (a fresh castle refuses anonymous reads), on via the
// CASTLE_ALLOW_ANON_READS env override for local dev, off again when an
// explicit CLI flag overrides the env, and fallback on unparseable env
// values. The resolution lives in anonReadsFlag (main.go) so the flag
// assembly does not have to be re-created here.
func TestAnonReadsFlagDefault(t *testing.T) {
	cases := []struct {
		name string
		env  string
		args []string
		want bool
	}{
		{name: "fresh default is off", env: "", args: nil, want: false},
		{name: "env override enables", env: "true", args: nil, want: true},
		{name: "env override parses 1", env: "1", args: nil, want: true},
		{name: "unparseable env falls back off", env: "yes-please", args: nil, want: false},
		{name: "explicit flag wins over env enable", env: "true", args: []string{"--allow-anon-reads=false"}, want: false},
		{name: "explicit flag wins over env disable", env: "false", args: []string{"--allow-anon-reads"}, want: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv("CASTLE_ALLOW_ANON_READS", tc.env)
			fs := flag.NewFlagSet(t.Name(), flag.ContinueOnError)
			anon := anonReadsFlag(fs)
			if err := fs.Parse(tc.args); err != nil {
				t.Fatalf("parse %v: %v", tc.args, err)
			}
			if *anon != tc.want {
				t.Fatalf("anon reads = %v, want %v (env=%q args=%v)", *anon, tc.want, tc.env, tc.args)
			}
		})
	}
}

// TestReapStaleRunsOnce pins the main.go reaper wiring (CRI-142): the
// staleness passed by the ticker must be subtracted from now (not added), and
// a reaped pass stamps exactly the stale agent's active runs.
func TestReapStaleRunsOnce(t *testing.T) {
	s, err := sqlite.Open(t.TempDir() + "/castle.db")
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer s.Close()
	ctx := context.Background()
	log := slog.New(slog.NewTextHandler(testWriter{}, nil))

	now := time.Now().UTC()
	staleSeen := now.Add(-10 * time.Minute)
	freshSeen := now.Add(-5 * time.Second)
	for _, o := range []struct {
		id   string
		seen time.Time
	}{
		{"agent-dead", staleSeen},
		{"agent-alive", freshSeen},
	} {
		if err := s.CreateOverseer(ctx, &store.Overseer{
			ID: o.id, Name: o.id, TokenHash: "x", Status: "online",
			CreatedAt: o.seen, LastSeenAt: o.seen,
		}); err != nil {
			t.Fatalf("create overseer %s: %v", o.id, err)
		}
	}
	for _, r := range []struct{ id, agent string }{
		{"r-zombie", "agent-dead"},
		{"r-live", "agent-alive"},
	} {
		if err := s.CreateRun(ctx, &store.Run{
			ID: r.id, OverseerID: r.agent, WorkflowName: "wf", Status: "running", CreatedAt: now,
		}); err != nil {
			t.Fatalf("create run %s: %v", r.id, err)
		}
	}

	reapStaleRunsOnce(ctx, s, log, 60*time.Second)

	zombie, err := s.GetRun(ctx, "r-zombie")
	if err != nil {
		t.Fatalf("get zombie: %v", err)
	}
	if zombie.Status != "failed" || zombie.FailureReason != "agent heartbeat lost" || zombie.EndedAt == nil {
		t.Fatalf("zombie not reaped: status=%q reason=%q ended=%v", zombie.Status, zombie.FailureReason, zombie.EndedAt)
	}
	live, err := s.GetRun(ctx, "r-live")
	if err != nil {
		t.Fatalf("get live: %v", err)
	}
	if live.Status != "running" || live.FailureReason != "" {
		t.Fatalf("live run reaped: status=%q reason=%q", live.Status, live.FailureReason)
	}
}

// TestReapNeverStartedRunsOnce pins the main.go wiring for the KB-233 rule-2
// reaper: the window configured by the operator selects exactly the old
// never-started records — and leaves in-window and started runs alone.
func TestReapNeverStartedRunsOnce(t *testing.T) {
	s, err := sqlite.Open(t.TempDir() + "/castle.db")
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer s.Close()
	ctx := context.Background()
	log := slog.New(slog.NewTextHandler(testWriter{}, nil))

	now := time.Now().UTC()
	old := now.Add(-15 * time.Minute)
	if err := s.CreateOverseer(ctx, &store.Overseer{
		ID: "agent-x", Name: "agent-x", TokenHash: "x", Status: "online",
		CreatedAt: old, LastSeenAt: old,
	}); err != nil {
		t.Fatalf("create overseer: %v", err)
	}
	for _, r := range []struct {
		id        string
		createdAt time.Time
	}{
		{"r-never-started", old},
		{"r-created-recently", now},
	} {
		if err := s.CreateRun(ctx, &store.Run{
			ID: r.id, OverseerID: "agent-x", WorkflowName: "wf", Status: "pending", CreatedAt: r.createdAt,
		}); err != nil {
			t.Fatalf("create run %s: %v", r.id, err)
		}
	}

	reapNeverStartedRunsOnce(ctx, s, log, 10*time.Minute)

	orphan, err := s.GetRun(ctx, "r-never-started")
	if err != nil {
		t.Fatalf("get orphan: %v", err)
	}
	if orphan.Status != "failed" || orphan.FailureReason != "created_never_started" || orphan.EndedAt == nil {
		t.Fatalf("orphan not reaped: status=%q reason=%q ended=%v", orphan.Status, orphan.FailureReason, orphan.EndedAt)
	}
	recent, err := s.GetRun(ctx, "r-created-recently")
	if err != nil {
		t.Fatalf("get recent: %v", err)
	}
	if recent.Status != "pending" || recent.FailureReason != "" {
		t.Fatalf("in-window run reaped: status=%q reason=%q", recent.Status, recent.FailureReason)
	}
}

type testWriter struct{}

func (testWriter) Write(p []byte) (int, error) { return len(p), nil }

// capturingWriter records everything the logger writes so tests can assert
// on the operator-facing reaper log lines.
type capturingWriter struct {
	b []byte
}

func (w *capturingWriter) Write(p []byte) (int, error) {
	w.b = append(w.b, p...)
	return len(p), nil
}

func (w *capturingWriter) String() string { return string(w.b) }

// TestReapStaleRunsOnce_ParkedRunLogsReclassification pins the KB-226
// operator-visible contract: when a heartbeat-stale run's record ends at the
// awaiting_human terminal completion, the pass does NOT emit "reaped run
// with stale agent heartbeat" — it logs a reclassification naming the parked
// status and the reason, past any threshold.
func TestReapStaleRunsOnce_ParkedRunLogsReclassification(t *testing.T) {
	s, err := sqlite.Open(t.TempDir() + "/castle.db")
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer s.Close()
	ctx := context.Background()
	cw := &capturingWriter{}
	log := slog.New(slog.NewTextHandler(cw, nil))

	now := time.Now().UTC()
	// The heartbeat went quiet 2h ago: stale past ANY 1h reap threshold.
	staleSeen := now.Add(-120 * time.Minute)
	if err := s.CreateOverseer(ctx, &store.Overseer{
		ID: "agent-dead", Name: "runner", TokenHash: "x", Status: "online",
		CreatedAt: staleSeen, LastSeenAt: staleSeen,
	}); err != nil {
		t.Fatalf("create overseer: %v", err)
	}
	if err := s.CreateRun(ctx, &store.Run{
		ID: "r-parked", OverseerID: "agent-dead", WorkflowName: "wf", Status: "running", CreatedAt: now,
	}); err != nil {
		t.Fatalf("create run: %v", err)
	}
	if _, inserted, err := s.AppendEvent(ctx, &store.Event{
		SchemaVersion: store.EventSchemaVersion,
		RunID:         "r-parked",
		Type:          "run.completed",
		Ts:            now,
		CorrelationID: "corr-terminal",
		Payload:       []byte(`{"finalState":"awaiting_human","success":false}`),
	}); err != nil || !inserted {
		t.Fatalf("append terminal event: err=%v inserted=%v", err, inserted)
	}

	// An hour past the threshold: parked, not failed, with the
	// reclassification named in the log.
	reapStaleRunsOnce(ctx, s, log, time.Hour)

	got, err := s.GetRun(ctx, "r-parked")
	if err != nil {
		t.Fatalf("get run: %v", err)
	}
	if got.Status != store.RunStatusAwaitingHuman || got.FailureReason != "" || got.EndedAt != nil {
		t.Fatalf("parked run: status=%q reason=%q ended=%v", got.Status, got.FailureReason, got.EndedAt)
	}
	logs := cw.String()
	for _, want := range []string{
		"msg=\"reaper skipped run parked at human gate\"",
		"run_id=r-parked",
		"reclassified_to=awaiting_human",
		`reason="engine reached terminal human gate awaiting_human"`,
	} {
		if !strings.Contains(logs, want) {
			t.Fatalf("missing %q in log:\n%s", want, logs)
		}
	}
	if strings.Contains(logs, "reaped run with stale agent heartbeat") {
		t.Fatalf("parked run logged as reaped:\n%s", logs)
	}
}

// TestReapStaleRunsOnce_MidFlightLogsReaped is the counterpart of the parked
// log contract: a run whose agent vanished without reaching a human gate
// still logs the legacy reaped line with the heartbeat-lost reason.
func TestReapStaleRunsOnce_MidFlightLogsReaped(t *testing.T) {
	s, err := sqlite.Open(t.TempDir() + "/castle.db")
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer s.Close()
	ctx := context.Background()
	cw := &capturingWriter{}
	log := slog.New(slog.NewTextHandler(cw, nil))

	now := time.Now().UTC()
	staleSeen := now.Add(-10 * time.Minute)
	if err := s.CreateOverseer(ctx, &store.Overseer{
		ID: "agent-dead", Name: "runner", TokenHash: "x", Status: "online",
		CreatedAt: staleSeen, LastSeenAt: staleSeen,
	}); err != nil {
		t.Fatalf("create overseer: %v", err)
	}
	if err := s.CreateRun(ctx, &store.Run{
		ID: "r-dead", OverseerID: "agent-dead", WorkflowName: "wf", Status: "running", CreatedAt: now,
	}); err != nil {
		t.Fatalf("create run: %v", err)
	}

	reapStaleRunsOnce(ctx, s, log, 60*time.Second)

	got, err := s.GetRun(ctx, "r-dead")
	if err != nil {
		t.Fatalf("get run: %v", err)
	}
	if got.Status != "failed" || got.FailureReason != "agent heartbeat lost" || got.EndedAt == nil {
		t.Fatalf("mid-flight stale run: status=%q reason=%q ended=%v", got.Status, got.FailureReason, got.EndedAt)
	}
	logs := cw.String()
	for _, want := range []string{
		"msg=\"reaped run with stale agent heartbeat\"",
		"run_id=r-dead",
		`reason="agent heartbeat lost"`,
	} {
		if !strings.Contains(logs, want) {
			t.Fatalf("missing %q in log:\n%s", want, logs)
		}
	}
}

// TestOpenStoreProfile pins the --store profile wiring (KB-317): sqlite stays
// the default local/dev profile, pg requires an explicit DSN, unknown
// profiles are rejected, and profile parsing is case-insensitive.
func TestOpenStoreProfile_SQLite(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "castle.db")
	st, err := openStoreProfile("sqlite", dbPath, "")
	if err != nil {
		t.Fatalf("openStoreProfile(sqlite): %v", err)
	}
	if st == nil {
		t.Fatal("nil store")
	}
	st.Close()

	// Empty profile falls back to the sqlite default.
	st, err = openStoreProfile("", dbPath, "")
	if err != nil {
		t.Fatalf("openStoreProfile(\"\"): %v", err)
	}
	defer st.Close()
}

func TestOpenStoreProfile_Pg(t *testing.T) {
	dsn := pgtest.DSN(t)
	st, err := openStoreProfile("pg", "", dsn)
	if err != nil {
		t.Fatalf("openStoreProfile(pg): %v", err)
	}
	if st == nil {
		t.Fatal("nil store")
	}
	st.Close()

	// Profile parsing tolerates case and surrounding whitespace.
	st, err = openStoreProfile("  PG  ", "", dsn)
	if err != nil {
		t.Fatalf("openStoreProfile(PG padded): %v", err)
	}
	defer st.Close()
}

func TestOpenStoreProfile_Rejects(t *testing.T) {
	if _, err := openStoreProfile("pg", "unused.db", ""); err == nil {
		t.Fatal("pg profile without DSN: want error, got nil")
	}
	if _, err := openStoreProfile("mysql", "unused.db", ""); err == nil {
		t.Fatal("unknown profile: want error, got nil")
	}
}

// TestRefireBootstrapOnce pins the refire policy boot wiring (KB-321): the
// built-in default policy is seeded exactly once into an empty table, an
// operator-tuned default row is never overwritten at boot, the policy seed
// file's entries are upserted under their parsed (project_id, route) keys,
// and a malformed or missing policy file aborts boot without applying any
// file row.
func TestRefireBootstrapOnce(t *testing.T) {
	newStore := func(t *testing.T) store.Store {
		t.Helper()
		s, err := sqlite.Open(t.TempDir() + "/castle.db")
		if err != nil {
			t.Fatalf("open: %v", err)
		}
		t.Cleanup(func() { s.Close() })
		return s
	}
	ctx := context.Background()
	log := slog.New(slog.NewTextHandler(testWriter{}, nil))

	// defaultRows asserts the table's exact policy-row shape (and returns
	// its parsed default slot) so each subtest pins the whole table, not
	// just the row it cares about.
	defaultRows := func(t *testing.T, s store.Store) (refire.Policy, []store.RefirePolicyRow) {
		t.Helper()
		rows, err := s.ListRefirePolicies(ctx)
		if err != nil {
			t.Fatalf("ListRefirePolicies: %v", err)
		}
		for _, r := range rows {
			if r.ProjectID == "" && r.Route == "" {
				p, err := refire.ParsePolicy(r.Policy)
				if err != nil {
					t.Fatalf("parse default row: %v", err)
				}
				return p, rows
			}
		}
		return refire.Policy{}, rows
	}

	t.Run("empty table seeds default exactly once", func(t *testing.T) {
		s := newStore(t)
		if err := refireBootstrapOnce(ctx, s, log, ""); err != nil {
			t.Fatalf("refireBootstrapOnce: %v", err)
		}
		p, rows := defaultRows(t, s)
		if len(rows) != 1 {
			t.Fatalf("rows = %d, want exactly the default row", len(rows))
		}
		want := refire.DefaultPolicyDoc()
		if p.MaxAttempts != want.MaxAttempts || p.Backoff != want.Backoff {
			t.Fatalf("seeded default = %+v, want %+v", p, want)
		}
		if len(p.Parks) == 0 || p.Parks[0] != "need_help" {
			t.Fatalf("seeded parks vocabulary = %v, want the human-gate vocabulary first", p.Parks)
		}
		// A second boot over the seeded table must not duplicate the row.
		if err := refireBootstrapOnce(ctx, s, log, ""); err != nil {
			t.Fatalf("second refireBootstrapOnce: %v", err)
		}
		if _, rows := defaultRows(t, s); len(rows) != 1 {
			t.Fatalf("rows after second boot = %d, want 1", len(rows))
		}
	})

	t.Run("tuned default survives boot", func(t *testing.T) {
		s := newStore(t)
		tuned := refire.DefaultPolicyDoc()
		tuned.MaxAttempts = 7
		tuned.Backoff = 5 * time.Minute
		if err := s.UpsertRefirePolicy(ctx, "", "",
			refire.MustMarshalPolicy(tuned), time.Now().UTC()); err != nil {
			t.Fatalf("tune default row: %v", err)
		}
		if err := refireBootstrapOnce(ctx, s, log, ""); err != nil {
			t.Fatalf("refireBootstrapOnce: %v", err)
		}
		p, rows := defaultRows(t, s)
		if len(rows) != 1 {
			t.Fatalf("rows = %d, want exactly the tuned default row", len(rows))
		}
		if p.MaxAttempts != 7 || p.Backoff != 5*time.Minute {
			t.Fatalf("default row = %+v, want the tuned %+v (boot clobbered the operator)", p, tuned)
		}
	})

	t.Run("policy file upserts parsed keys", func(t *testing.T) {
		s := newStore(t)
		path := filepath.Join(t.TempDir(), "refire-policy.json")
		file := `[
			{"policy": {"max_attempts": 5, "backoff_seconds": 10,
			 "transient": ["stall"], "parks": ["need_help"], "cosmetics": []}},
			{"project_id": "proj-1", "route": "kb-deploy",
			 "policy": {"max_attempts": 2, "backoff_seconds": 0,
			  "transient": [], "parks": ["need_assistance"], "cosmetics": ["emit race"]}}
		]`
		if err := os.WriteFile(path, []byte(file), 0o644); err != nil {
			t.Fatalf("write policy file: %v", err)
		}
		if err := refireBootstrapOnce(ctx, s, log, path); err != nil {
			t.Fatalf("refireBootstrapOnce: %v", err)
		}
		rows, err := s.ListRefirePolicies(ctx)
		if err != nil {
			t.Fatalf("ListRefirePolicies: %v", err)
		}
		if len(rows) != 2 {
			t.Fatalf("rows = %d, want default slot + scoped row: %+v", len(rows), rows)
		}
		p, _ := defaultRows(t, s)
		if p.MaxAttempts != 5 || p.Backoff != 10*time.Second {
			t.Fatalf("file default slot = %+v, want the file's override", p)
		}
		var scoped *store.RefirePolicyRow
		for i := range rows {
			if rows[i].ProjectID == "proj-1" && rows[i].Route == "kb-deploy" {
				scoped = &rows[i]
			}
		}
		if scoped == nil {
			t.Fatalf("rows = %+v, want the (proj-1, kb-deploy) row", rows)
		}
		sp, err := refire.ParsePolicy(scoped.Policy)
		if err != nil {
			t.Fatalf("parse scoped row: %v", err)
		}
		if sp.MaxAttempts != 2 || sp.Backoff != 0 || len(sp.Cosmetics) != 1 || sp.Cosmetics[0] != "emit race" {
			t.Fatalf("scoped policy = %+v, want the file's scoped row", sp)
		}
	})

	t.Run("malformed file aborts boot with no file rows", func(t *testing.T) {
		s := newStore(t)
		path := filepath.Join(t.TempDir(), "refire-policy.json")
		// The first entry is valid, the second is mistyped: whole-file
		// parsing must reject the file before any file row is applied.
		file := `[
			{"policy": {"max_attempts": 9, "backoff_seconds": 0,
			 "transient": [], "parks": [], "cosmetics": []}},
			{"policy": {"max_attempt": 9}}
		]`
		if err := os.WriteFile(path, []byte(file), 0o644); err != nil {
			t.Fatalf("write policy file: %v", err)
		}
		if err := refireBootstrapOnce(ctx, s, log, path); err == nil {
			t.Fatal("refireBootstrapOnce(malformed file) = nil, want boot-abort error")
		}
		p, rows := defaultRows(t, s)
		if len(rows) != 1 {
			t.Fatalf("rows = %+v, want only the seeded default (no partially applied file row)", rows)
		}
		if p.MaxAttempts != refire.DefaultPolicyDoc().MaxAttempts {
			t.Fatalf("default row = %+v, want the built-in seed (the file's valid entry must also be un-applied)", p)
		}
	})

	t.Run("missing policy file aborts boot", func(t *testing.T) {
		s := newStore(t)
		missing := filepath.Join(t.TempDir(), "absent.json")
		if err := refireBootstrapOnce(ctx, s, log, missing); err == nil {
			t.Fatal("refireBootstrapOnce(missing file) = nil, want boot-abort error")
		}
		p, rows := defaultRows(t, s)
		if len(rows) != 1 || p.MaxAttempts != refire.DefaultPolicyDoc().MaxAttempts {
			t.Fatalf("rows = %+v default = %+v, want the untouched seed", rows, p)
		}
	})

	t.Run("unreadable policy file aborts boot", func(t *testing.T) {
		s := newStore(t)
		path := filepath.Join(t.TempDir(), "unreadable.json")
		if err := os.WriteFile(path, []byte(`[]`), 0o000); err != nil {
			t.Fatalf("write policy file: %v", err)
		}
		if _, err := refire.LoadPolicyFile(path); err == nil {
			t.Skip("unreadable-by-0o000 is not reproducible under this uid; missing-path subtest covers the error leg")
		}
		if err := refireBootstrapOnce(ctx, s, log, path); err == nil {
			t.Fatal("refireBootstrapOnce(unreadable file) = nil, want boot-abort error")
		}
	})
}
