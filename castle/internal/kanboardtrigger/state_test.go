package kanboardtrigger

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	pb "github.com/brokenbots/criteria/sdk/pb/criteria/v1"
)

func TestStateRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	s := (&state{Tickets: map[string]*TicketMemo{
		"KB-1": {PrevColumnID: 2, FireSeq: 4},
	}, Runs: map[string]*RunRecord{
		"run-a": {RunID: "run-a", TicketID: "KB-1", TicketKanboardID: 1, LaneRoute: "kb-develop",
			Class: ClassDevelop, SubmittedAtMs: 12345,
			Settled: true, SeenEvents: true, FinalState: "handler_complete", Succeeded: true},
	}}).normalized()
	st := &fileStore{path: path}
	if err := st.save(s); err != nil {
		t.Fatalf("save: %v", err)
	}

	loaded, err := loadState(path)
	if err != nil {
		t.Fatalf("loadState: %v", err)
	}
	memo := loaded.Tickets["KB-1"]
	if memo == nil || memo.PrevColumnID != 2 || memo.FireSeq != 4 {
		t.Fatalf("memo round-trip = %+v", memo)
	}
	rec := loaded.Runs["run-a"]
	if rec == nil || !rec.Settled || !rec.SeenEvents || rec.FinalState != "handler_complete" ||
		!rec.Succeeded || rec.TicketID != "KB-1" || rec.TicketKanboardID != 1 || rec.SubmittedAtMs != 12345 {
		t.Fatalf("run record round-trip = %+v", rec)
	}

	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Errorf("state file mode = %v, want 0600", perm)
	}
}

func TestStateLoadMissingFileNormalizes(t *testing.T) {
	s, err := loadState(filepath.Join(t.TempDir(), "absent.json"))
	if err != nil {
		t.Fatalf("loadState: %v", err)
	}
	if s.Tickets == nil || s.Runs == nil {
		t.Fatalf("ticket/runs maps nil after normalize: %+v", s)
	}
	if len(s.Tickets) != 0 || len(s.Runs) != 0 {
		t.Errorf("expected empty maps, got %+v", s)
	}
}

func TestStateLoadCorruptFileFails(t *testing.T) {
	path := filepath.Join(t.TempDir(), "broken.json")
	if err := os.WriteFile(path, []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := loadState(path)
	if err == nil || !strings.Contains(err.Error(), "parsing") {
		t.Fatalf("loadState err = %v, want parse error", err)
	}
}

// Atomic save: no .tmp droppings may survive a save.
func TestStateSaveAtomic(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "state.json")
	st := &fileStore{path: path}
	s := (&state{}).normalized()
	if err := st.save(s); err != nil {
		t.Fatalf("first save: %v", err)
	}
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), ".tmp") {
			t.Errorf("tmp file left behind: %s", e.Name())
		}
	}
}

// Restart resilience: state saved by one Adapter must be picked up by a
// second Adapter over the same file, so entry memos and fire sequences
// survive a process restart (the watcher loses this on restart).
func TestStateSurvivesAdapterReconstruction(t *testing.T) {
	fk := newFakeKanboard(t, "KB board", boardColumns()...)
	fk.addCard(t, 101, "Work in progress", "KB-101", "k8s-run", "repo:own/name")
	h := newHarness(t, fk, testLaneConfig(), nil)
	h.poll(t)
	if n := len(h.sub.submitted()); n != 1 {
		t.Fatalf("first poll fired %d times, want 1", n)
	}

	// Simulate restart: fresh Adapter over the same state file.
	a2 := h.rebuild(t, testLaneConfig())
	a2.poll(t)
	if n := len(h.sub.submitted()); n != 1 {
		t.Errorf("restart re-fired %d extra times, want none (memo survives restart)", n-1)
	}

	// Settle the first run (terminal by disposition): re-entry while a run
	// is unsettled stays blocked.
	h.sub.dispState = pb.WorkflowAssignmentState_WORKFLOW_ASSIGNMENT_STATE_TERMINAL
	a2.poll(t)
	if n := len(h.sub.submitted()); n != 1 {
		t.Fatalf("terminal settle poll fired again, want none (%d total)", n)
	}

	// Move the card to another lane column: the re-entry now fires with a
	// fresh sequence (memo.PrevColumnID loaded from disk).
	fk.setCardColumn(101, "Ready")
	a2.poll(t)
	calls := h.sub.submitted()
	if n := len(calls); n != 2 {
		t.Fatalf("re-entry after restart fired %d total, want 2", n)
	}
	if calls[1].IdempotencyKey == calls[0].IdempotencyKey {
		t.Errorf("second key %q must differ from first %q (fresh seq)", calls[1].IdempotencyKey, calls[0].IdempotencyKey)
	}
}
