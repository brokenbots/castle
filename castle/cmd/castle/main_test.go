package main

import (
	"context"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/brokenbots/castle/castle/internal/store"
	"github.com/brokenbots/castle/castle/internal/store/sqlite"
)

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
