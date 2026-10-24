package kanboardtrigger

import (
	"context"
	"io"
	"log/slog"
	"path/filepath"
	"testing"
	"time"

	"github.com/brokenbots/castle/castle/internal/kanboard"
)

// testLaneConfig is the default dev+triage config used across tests: the
// develop lane triggers on "Work in progress"/"Ready", the triage lane on
// "Backlog" — mirroring the outgoing watcher's lane shape.
func testLaneConfig() *Config {
	return &Config{
		BoardProject:    "KB board",
		CastleProjectID: "kb",
		TriggerTag:      "k8s-run",
		ReviewColumn:    "Review",
		DoneColumn:      "Done",
		Lanes: []Lane{
			{
				RouteName:      "kb-develop",
				Class:          ClassDevelop,
				TriggerColumns: []string{"Work in progress", "Ready"},
				WorkflowSource: "hermes",
				LockfileSource: "lock:main",
				Inputs:         map[string]string{"base_branch": "main"},
			},
			{
				RouteName:      "kb-triage",
				Class:          ClassTriage,
				TriggerColumns: []string{"Backlog"},
				WorkflowSource: "hermes",
			},
		},
	}
}

func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// boardColumns is the standard fake board column layout.
func boardColumns() []string {
	return []string{"Backlog", "Work in progress", "Ready", "Review", "Done"}
}

// harness wires one fake kanboard, submitter, run watcher and disposition
// source around one Adapter at a temp state file.
type harness struct {
	fk        *fakeKanboard
	sub       *fakeSubmitter
	watcher   *fakeRunWatcher
	colID     map[string]int
	a         *Adapter
	statePath string
}

func newHarness(t *testing.T, fk *fakeKanboard, cfg *Config, tweak func(*fakeSubmitter), opts ...func(*Options)) *harness {
	t.Helper()
	statePath := filepath.Join(t.TempDir(), "adapter-state.json")
	sub := &fakeSubmitter{}
	if tweak != nil {
		tweak(sub)
	}
	watcher := newFakeRunWatcher()
	kb := kanboard.New(fk.srv.URL+"/jsonrpc.php", "test-app-token")
	o := Options{
		StatePath:    statePath,
		PollInterval: 0,
		Instance:     "test",
	}
	for _, f := range opts {
		f(&o)
	}
	a, err := New(cfg, kb, sub, sub, watcher, discardLogger(), o)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	a.boardProjectID = 1
	t.Cleanup(func() { quiescePumps(t, watcher, a) })
	return &harness{fk: fk, sub: sub, watcher: watcher, colID: fk.columnIDOf, a: a, statePath: statePath}
}

// poll runs exactly one poll cycle.
func (h *harness) poll(t *testing.T) {
	t.Helper()
	if err := h.a.poll(context.Background()); err != nil {
		t.Fatalf("poll: %v", err)
	}
}

// rebuild returns a fresh Adapter over the same board, submitter and state
// file, as a process restart would produce.
func (h *harness) rebuild(t *testing.T, cfg *Config, opts ...func(*Options)) *harness {
	watcher := newFakeRunWatcher()
	kb := kanboard.New(h.fk.srv.URL+"/jsonrpc.php", "test-app-token")
	o := Options{
		StatePath:    h.statePath,
		PollInterval: 0,
		Instance:     "test",
	}
	for _, f := range opts {
		f(&o)
	}
	a, err := New(cfg, kb, h.sub, h.sub, watcher, discardLogger(), o)
	if err != nil {
		t.Fatalf("rebuild: %v", err)
	}
	a.boardProjectID = 1
	t.Cleanup(func() { quiescePumps(t, watcher, a) })
	return &harness{fk: h.fk, sub: h.sub, watcher: watcher, colID: h.colID, a: a, statePath: h.statePath}
}

// quiescePumps releases held streams and waits for watch goroutines to
// exit, so no pump writes state after the test's TempDir was removed.
func quiescePumps(t *testing.T, watcher *fakeRunWatcher, a *Adapter) {
	t.Helper()
	watcher.releaseAll()
	done := make(chan struct{})
	go func() {
		a.pumps.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Log("kanboardtrigger: watch pumps still live at cleanup")
	}
}
