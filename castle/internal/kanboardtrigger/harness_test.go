package kanboardtrigger

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"testing"

	"github.com/brokenbots/castle/castle/internal/kanboard"
)

// testLaneConfig is the default dev+triage config used across tests: the
// develop lane triggers on "Work in progress"/"Ready" (the watcher's lanes
// shape), the triage lane on "Backlog".
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
				Inputs:         map[string]string{},
			},
		},
	}
}

// harness wires a fake kanboard, submitter, run watcher and disposition
// source around one Adapter at a temp state file.
type harness struct {
	fk        *fakeKanboard
	sub       *fakeSubmitter
	watcher   *fakeRunWatcher
	a         *Adapter
	statePath string
}

func newHarness(t *testing.T, fk *fakeKanboard, cfg *Config, tweak func(*fakeSubmitter)) *harness {
	t.Helper()
	statePath := filepath.Join(t.TempDir(), "adapter-state.json")
	sub := &fakeSubmitter{}
	if tweak != nil {
		tweak(sub)
	}
	watcher := newFakeRunWatcher()
	kb := kanboard.New(fk.srv.URL+"/jsonrpc.php", "test-app-token")
	a, err := New(cfg, kb, sub, sub, watcher, slog.New(slog.NewTextHandler(os.Stderr, nil)), Options{
		StatePath:    statePath,
		PollInterval: 0,
		Instance:     "test",
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	a.boardProjectID = 1
	return &harness{fk: fk, sub: sub, watcher: watcher, a: a, statePath: statePath}
}

// poll runs exactly one poll cycle.
func (h *harness) poll(t *testing.T) {
	t.Helper()
	if err := h.a.poll(context.Background()); err != nil {
		t.Fatalf("poll: %v", err)
	}
}

func firedKeys(sub *fakeSubmitter) []string {
	keys := []string{}
	for _, c := range sub.submitted() {
		keys = append(keys, c.IdempotencyKey)
	}
	return keys
}

func fmtTicket(id int) string { return fmt.Sprintf("KB-%d", id) }