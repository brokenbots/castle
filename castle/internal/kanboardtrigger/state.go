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
type TicketMemo struct {
	// PrevColumnID is the column id the card occupied at the last poll.
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
// the file is written back on every mutation.
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
