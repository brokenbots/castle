package kanboardtrigger

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
)

// TicketMemo records the durable entry memo (KB-302) for one ticket: the
// column the card occupied at the last poll and the per-ticket fire
// sequence. It replaces the outgoing watcher's in-memory memo, which loses
// its anti-refire protection on restart; here the protection and the fire
// keys' sequence alignment survive restarts.
//
// A failed submit zeroes only PrevColumnID (forcing re-entry) and keeps
// FireSeq: the retry re-derives the failed attempt's key instead of
// reverting to an older fire's sequence.
type TicketMemo struct {
	// PrevColumnID is the column id the card occupied at the last poll; 0
	// is "no valid column" and forces re-entry (e.g. after a failed fire).
	PrevColumnID int `json:"prev_column_id"`
	// FireSeq is the per-ticket fire counter; fire number N derives the
	// idempotency key kanboard:<board-project>:<ticket>:<N>.
	FireSeq int `json:"fire_seq"`
}

// RunRecord tracks one submitted or adopted assignment run for the mirror:
// its ticket + lane (the admission class the assignment carried), and the
// settle verdict the mirror observed.
type RunRecord struct {
	RunID     string `json:"run_id"`
	TicketID  string `json:"ticket_id"`
	LaneRoute string `json:"lane_route"`
	Class     string `json:"class"`
	Settled   bool   `json:"settled,omitempty"`
	// SeenEvents reports whether any run envelope arrived (a record without
	// it has no observable verdict even if terminal-by-disposition).
	SeenEvents bool `json:"seen_events,omitempty"`
	// FinalState is the verdict the mirror observed (settle only).
	FinalState string `json:"final_state,omitempty"`
	// FailureReason is the reason carried by a RunFailed envelope.
	FailureReason string `json:"failure_reason,omitempty"`
	// Succeeded reports whether the terminal was RunCompleted (vs
	// RunFailed).
	Succeeded bool `json:"succeeded,omitempty"`
	// NoVerdict marks a settled record the mirror holds no observable
	// verdict for (rejected assignment, or terminal settled by disposition
	// without envelopes): no column move is derived from it.
	NoVerdict bool `json:"no_verdict,omitempty"`
	// ParkCommented marks the once-per-run failure comment.
	ParkCommented bool `json:"park_commented,omitempty"`
	// Mirrored marks a settled record whose verdict the poll applied to the
	// card (or deliberately abstained from).
	Mirrored bool `json:"mirrored,omitempty"`
	// SubmittedAtMs is the submit time (millis) used to order runs per
	// ticket. Monotonic across restarts by wall clock.
	SubmittedAtMs int64 `json:"submitted_at_ms"`
	// TerminalByDisposition marks a run whose assignment disposition was
	// TERMINAL while its watch was still live: the mirror defers the
	// settle until the watch either replays a terminal envelope (which
	// then wins) or the deferral budget expires (which finalizes the
	// NoVerdict settle even while the stream is still open). Never
	// combined with Settled, with one exception: a budget-expiry settle
	// keeps the marker so reconcileMirrors can complete the record's
	// mirror cycle despite a still-live watch.
	TerminalByDisposition bool `json:"terminal_by_disposition,omitempty"`
	// DeferSinceMs is the wall-clock time (ms) at which the TERMINAL
	// disposition deferral began (set together with
	// TerminalByDisposition). It is persisted so the deferral budget
	// survives restarts; zero (e.g. records written before the field
	// existed) counts as expired.
	DeferSinceMs int64 `json:"defer_since_ms,omitempty"`
	// CommentWaived marks a failed run whose card was already observed in
	// Review with the KB-302 comment considered posted (or first seen
	// parked): the comment obligation never needs a retry.
	CommentWaived bool `json:"comment_waived,omitempty"`
	// ParkFailed marks a failed-run comment post that itself failed while
	// the card was already parked in Review: the comment is retried on a
	// later poll, so the record must not be marked mirrored yet.
	ParkFailed bool `json:"park_failed,omitempty"`
	// TicketKanboardID is the numeric kanboard task id.
	TicketKanboardID int `json:"ticket_kanboard_id"`
}

// state is the adapter's durable snapshot (serialized as JSON).
type state struct {
	Tickets map[string]*TicketMemo `json:"tickets"`
	Runs    map[string]*RunRecord  `json:"runs"` // keyed by run id
}

func (st *state) normalized() *state {
	if st.Tickets == nil {
		st.Tickets = map[string]*TicketMemo{}
	}
	if st.Runs == nil {
		st.Runs = map[string]*RunRecord{}
	}
	return st
}

// fileStore persists the adapter state to JSON with atomic writes (write
// temporary file + rename) so a crash mid-write never truncates the file.
// The in-memory state an Adapter builds from this file is the hot authority;
// poll-goroutine mutations batch to one flush per poll (Adapter.flush), and
// async watch-pump settles save immediately (Adapter.persistLocked) so a
// settle verdict is durable when it happens.
type fileStore struct {
	path string
}

func loadState(path string) (*state, error) {
	if path == "" {
		return nil, fmt.Errorf("state file required for durable entry memo")
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return (&state{}).normalized(), nil
		}
		return nil, fmt.Errorf("reading state file: %w", err)
	}
	var st state
	if err := json.Unmarshal(raw, &st); err != nil {
		return nil, fmt.Errorf("parsing state file: %w", err)
	}
	return st.normalized(), nil
}

func (f fileStore) save(st *state) error {
	raw, err := json.MarshalIndent(st, "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(f.path), 0o700); err != nil {
		return err
	}
	tmp := f.path + ".tmp"
	if err := os.WriteFile(tmp, raw, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, f.path)
}
