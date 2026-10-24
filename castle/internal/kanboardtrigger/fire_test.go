package kanboardtrigger

import (
	"fmt"
	"testing"

	pb "github.com/brokenbots/criteria/sdk/pb/criteria/v1"
)

// TestFireGateMatrix walks the gate order of the fire loop: trigger tag,
// lane, in-flight, repo resolution (fail-closed), entry. Each subtest runs
// on its own board and state file.
func TestFireGateMatrix(t *testing.T) {
	cases := []struct {
		name   string
		card   int
		col    string
		title  string
		tags   []string
		want   int
		wantWF string
	}{
		{
			name: "backlog_without_trigger_tag_stays",
			card: 10, col: "Backlog", tags: []string{"repo:own/name"},
			want: 0,
		},
		{
			name: "triage_lane_fire_from_backlog",
			card: 11, col: "Backlog", tags: []string{"k8s-run", "repo:own/name"},
			want: 1, wantWF: "kb-triage",
		},
		{
			name: "no_fire_without_lane_column",
			card: 12, col: "Review", tags: []string{"k8s-run", "repo:own/name"},
			want: 0,
		},
		{
			name: "repo_fail_closed_without_binding",
			card: 13, col: "Work in progress", tags: []string{"k8s-run"},
			want: 0,
		},
		{
			name: "lane_column_without_tag_skipped",
			card: 14, col: "Ready", tags: []string{"repo:own/name"},
			want: 0,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fk := newFakeKanboard(t, "KB board", boardColumns()...)
			fk.addCard(t, tc.card, tc.col, fmt.Sprintf("KB-%d", tc.card), tc.tags...)
			h := newHarness(t, fk, testLaneConfig(), nil)
			h.poll(t)
			calls := h.sub.submitted()
			if n := len(calls); n != tc.want {
				t.Fatalf("fired %d times, want %d", n, tc.want)
			}
			if tc.wantWF != "" && (len(calls) == 0 || calls[0].WorkflowName != tc.wantWF) {
				t.Errorf("workflow = %v, want %q", calls, tc.wantWF)
			}
		})
	}

	t.Run("same_column_second_poll_does_not_refire", func(t *testing.T) {
		fk := newFakeKanboard(t, "KB board", boardColumns()...)
		fk.addCard(t, 15, "Work in progress", "KB-15", "k8s-run", "repo:own/name")
		h := newHarness(t, fk, testLaneConfig(), nil)
		h.poll(t)
		h.poll(t)
		if n := len(h.sub.submitted()); n != 1 {
			t.Errorf("fired %d times over two polls, want 1 (KB-302 anti-refire)", n)
		}
	})
	t.Run("in_flight_run_blocks_re_entry", func(t *testing.T) {
		fk := newFakeKanboard(t, "KB board", boardColumns()...)
		fk.addCard(t, 16, "Work in progress", "KB-16", "k8s-run", "repo:own/name")
		h := newHarness(t, fk, testLaneConfig(), nil)
		h.poll(t) // fire 1
		fk.setCardColumn(16, "Ready")
		h.poll(t) // entry into second column, but run unsettled
		if n := len(h.sub.submitted()); n != 1 {
			t.Errorf("fired %d times, want 1 (in-flight abstention)", n)
		}
	})
}

// TestFireReentryAfterSettle: a card re-entering a lane after its previous
// run settles fires again with a fresh key (out-and-back).
func TestFireReentryAfterSettle(t *testing.T) {
	fk := newFakeKanboard(t, "KB board", boardColumns()...)
	fk.addCard(t, 17, "Work in progress", "KB-17", "k8s-run", "repo:own/name")
	h := newHarness(t, fk, testLaneConfig(), nil)
	h.poll(t) // fire 1
	h.sub.dispState = pb.WorkflowAssignmentState_WORKFLOW_ASSIGNMENT_STATE_TERMINAL
	h.poll(t) // settle the first run
	fk.setCardColumn(17, "Ready")
	h.poll(t) // re-entry (different column): fire 2
	calls := h.sub.submitted()
	if n := len(calls); n != 2 {
		t.Fatalf("fired %d times, want 2", n)
	}
	if calls[1].IdempotencyKey == calls[0].IdempotencyKey {
		t.Errorf("second fire reused key %q", calls[1].IdempotencyKey)
	}
}

// TestFireRetryKeepsDeterministicKey: a failed submit forgets the entry memo
// so the next poll re-enters the card and retries with the SAME key — the
// server adopts an already-accepted assignment instead of duplicating it.
func TestFireRetryKeepsDeterministicKey(t *testing.T) {
	fk := newFakeKanboard(t, "KB board", boardColumns()...)
	fk.addCard(t, 20, "Work in progress", "KB-20", "k8s-run", "repo:own/name")
	h := newHarness(t, fk, testLaneConfig(), func(sub *fakeSubmitter) {
		sub.err = errSubmitDenied
	})
	h.poll(t)
	if n := len(h.sub.submitted()); n != 1 {
		t.Fatalf("first attempt logged %d calls, want 1", n)
	}
	firstKey := h.sub.submitted()[0].IdempotencyKey

	// Adopt from the second attempt on: server accepted the first fire.
	h.sub.err = nil
	h.sub.adopt = true
	h.poll(t)
	calls := h.sub.submitted()
	if n := len(calls); n != 2 {
		t.Fatalf("retry produced %d calls, want 2", n)
	}
	if calls[1].IdempotencyKey != firstKey {
		t.Errorf("retry key = %q, want unchanged %q", calls[1].IdempotencyKey, firstKey)
	}

	// After adoption the run is settled and the card must not re-fire from
	// the same column (entry consumed, memo back in place).
	h.poll(t)
	if n := len(h.sub.submitted()); n != 2 {
		t.Errorf("post-retry poll fired again: %d calls, want 2", n)
	}
}

// TestFirePayloadIsLaneDriven checks the submitted payload: lane route name
// as workflow name, lane sources, lane inputs merged with ticket_id.
func TestFirePayloadIsLaneDriven(t *testing.T) {
	fk := newFakeKanboard(t, "KB board", boardColumns()...)
	fk.addCard(t, 21, "Work in progress", "KB-21", "k8s-run", "repo:own/name")
	h := newHarness(t, fk, testLaneConfig(), nil)
	h.poll(t)
	calls := h.sub.submitted()
	if n := len(calls); n != 1 {
		t.Fatalf("fired %d times, want 1", n)
	}
	c := calls[0]
	if c.WorkflowName != "kb-develop" || c.WorkflowSource != "hermes" {
		t.Errorf("workflow = %q/%q, want kb-develop/hermes", c.WorkflowName, c.WorkflowSource)
	}
	if c.LockfileSource != "lock:main" {
		t.Errorf("lockfile source = %q, want lock:main", c.LockfileSource)
	}
	if c.InputValues["ticket_id"] != "KB-21" || c.InputValues["base_branch"] != "main" {
		t.Errorf("inputs = %v, want ticket_id=KB-21 + base_branch=main", c.InputValues)
	}
	if want := "kanboard:KB board:KB-21:1"; c.IdempotencyKey != want {
		t.Errorf("fire key = %q, want %q", c.IdempotencyKey, want)
	}
}

// TestLaneCollisionFirstDeclaredWins: two lanes may declare the same
// trigger column; the first declared lane wins deterministically.
func TestLaneCollisionFirstDeclaredWins(t *testing.T) {
	cfg := &Config{
		BoardProject:    "KB board",
		CastleProjectID: "kb",
		TriggerTag:      "k8s-run",
		ReviewColumn:    "Review",
		DoneColumn:      "Done",
		Lanes: []Lane{
			{RouteName: "first", Class: ClassDevelop, TriggerColumns: []string{"WIP"}, WorkflowSource: "hermes"},
			{RouteName: "second", Class: ClassDevelop, TriggerColumns: []string{"WIP"}, WorkflowSource: "hermes"},
		},
	}
	if err := cfg.Validated(); err != nil {
		t.Fatalf("Validated: %v", err)
	}
	fk := newFakeKanboard(t, "KB board", "WIP", "Review", "Done")
	fk.addCard(t, 22, "WIP", "KB-22", "k8s-run", "repo:own/name")
	h := newHarness(t, fk, cfg, nil)
	h.poll(t)
	calls := h.sub.submitted()
	if n := len(calls); n != 1 {
		t.Fatalf("fired %d times, want 1", n)
	}
	if calls[0].WorkflowName != "first" {
		t.Errorf("workflow = %q, want first (declared first wins)", calls[0].WorkflowName)
	}
}

// TestRepoDefaultFiresWithoutTag: a lane repo default satisfies the repo
// gate without a repo: tag on the card.
func TestRepoDefaultFiresWithoutTag(t *testing.T) {
	cfg := testLaneConfig()
	fk := newFakeKanboard(t, "KB board", boardColumns()...)
	fk.addCard(t, 23, "Backlog", "KB-23", "k8s-run")
	h := newHarness(t, fk, cfg, nil)
	h.poll(t)
	if n := len(h.sub.submitted()); n != 0 {
		t.Fatalf("no-default fire = %d, want 0", n)
	}
	// Add the lane default and re-poll: same column, but the previous poll
	// recorded the memo without consuming the entry... the gate order means
	// the card is still on first sighting for the entry check.
	cfg.RepoDefault = "own/default"
	h2 := newHarness(t, fk, cfg, nil)
	h2.poll(t)
	if n := len(h2.sub.submitted()); n != 1 {
		t.Fatalf("default-bound fire = %d, want 1", n)
	}
}
