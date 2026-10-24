// Package refire implements the refire/classification policy engine
// (KB-321): policies are data — rows keyed (project_id, route) holding the
// policy JSON verbatim — and the disposition of a terminal-failed run
// (refire / park / cosmetic / none) is derived from that data plus castle's
// own deterministic failure stamps. The verdict machinery shared by both
// store engines lives here (candidate scan SQL, classification, attempt
// lineage and the requeue pass); engines keep only their plumbing, like the
// reap package (KB-226).
package refire

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"
	"time"

	"github.com/brokenbots/castle/castle/internal/store"
)

// Policy is one resolved refire/classification policy (KB-321).
type Policy struct {
	// MaxAttempts is the family attempt budget, counting the original
	// attempt (max_attempts=1 never refires; 0 disables refiring).
	MaxAttempts int
	// Backoff is the minimum delay between a failed attempt's ended_at and
	// the next family attempt.
	Backoff time.Duration
	// Transient are failure-reason substrings classifying the ending as a
	// recoverable stall: refire under the attempt/backoff limits
	// (case-insensitive match).
	Transient []string
	// Parks are designed-park spellings (need_help / need_assistance /
	// awaiting_human): matched case-insensitively against a run.completed
	// final state and the failure reason, parking the run for a human
	// decision; a parked run never refires.
	Parks []string
	// Cosmetics are failure-reason substrings for known-benign endings
	// (pod-state emit races): leave terminal-failed, take no action.
	Cosmetics []string
}

// policyJSON is the on-the-wire policy document — the same shape in the
// refire_policies.policy column and in --refire-policy-file entries.
type policyJSON struct {
	MaxAttempts    int      `json:"max_attempts,omitempty"`
	BackoffSeconds int      `json:"backoff_seconds,omitempty"`
	Transient      []string `json:"transient,omitempty"`
	Parks          []string `json:"parks,omitempty"`
	Cosmetics      []string `json:"cosmetics,omitempty"`
}

// AttemptSuffixSeparator is the attempt-count marker the refire idempotency
// keys carry ("#attempt-N"), making every reconciliation pass idempotent.
const AttemptSuffixSeparator = "#attempt-"

// DefaultPolicyDoc is the compiled-in global default policy (KB-321): the
// built-in classification vocabulary — provider stalls and the store's own
// deterministic reaper verdicts ("agent heartbeat lost", the never-started
// rule) are transient, designed human-gate parks never refire, and pod-state
// emit races are cosmetic. Unclassified endings default to no action.
//
// This default is also seeded as the (”, ”) policy row at boot (store
// upserts insert-if-missing), so the effective default is always visible and
// editable as data.
func DefaultPolicyDoc() Policy {
	return Policy{
		MaxAttempts: 3,
		Backoff:     30 * time.Second,
		Transient: []string{
			"stall",
			"heartbeat",
			"timeout",
			"deadline exceeded",
			"unavailable",
			"rate limit",
			"transient",
			"connection refused",
			"connection reset",
			"created_never_started",
		},
		Parks: []string{
			"need_help",
			"need_assistance",
			store.RunStatusAwaitingHuman,
		},
		Cosmetics: []string{
			"pod-state emit race",
			"pod state emit race",
			"pod phase race",
			"emit race",
		},
	}
}

func (p Policy) doc() policyJSON {
	return policyJSON{
		MaxAttempts:    p.MaxAttempts,
		BackoffSeconds: int(p.Backoff / time.Second),
		Transient:      p.Transient,
		Parks:          p.Parks,
		Cosmetics:      p.Cosmetics,
	}
}

// MarshalPolicy encodes the policy document (snake_case, the same shape in
// policy rows and in seed files).
func MarshalPolicy(p Policy) (string, error) {
	b, err := json.Marshal(p.doc())
	if err != nil {
		return "", fmt.Errorf("marshal refire policy: %w", err)
	}
	return string(b), nil
}

// MustMarshalPolicy is MarshalPolicy for policies that only carry built-in
// (always JSON-encodable) values; it panics on the impossible marshal error.
func MustMarshalPolicy(p Policy) string {
	s, err := MarshalPolicy(p)
	if err != nil {
		panic(err)
	}
	return s
}

// ParsePolicy decodes a policy document. Decoding is strict: unknown keys are
// rejected so a mistuned policy fails loudly instead of silently refiring on
// the default budget, and validation requires non-negative numbers.
func ParsePolicy(doc string) (Policy, error) {
	var raw policyJSON
	dec := json.NewDecoder(strings.NewReader(doc))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&raw); err != nil {
		return Policy{}, fmt.Errorf("parse refire policy %q: %w", doc, err)
	}
	if err := ensureTrailingEOF(dec); err != nil {
		return Policy{}, fmt.Errorf("parse refire policy %q: %w", doc, err)
	}
	if raw.MaxAttempts < 0 {
		return Policy{}, fmt.Errorf("parse refire policy %q: negative max_attempts", doc)
	}
	if raw.BackoffSeconds < 0 {
		return Policy{}, fmt.Errorf("parse refire policy %q: negative backoff_seconds", doc)
	}
	return Policy{
		MaxAttempts: raw.MaxAttempts,
		Backoff:     time.Duration(raw.BackoffSeconds) * time.Second,
		Transient:   raw.Transient,
		Parks:       raw.Parks,
		Cosmetics:   raw.Cosmetics,
	}, nil
}

// ensureTrailingEOF requires nothing but whitespace after the decoded value:
// a second document or stray token is a malformed policy.
func ensureTrailingEOF(dec *json.Decoder) error {
	if _, err := dec.Token(); err != io.EOF {
		if err == nil {
			return fmt.Errorf("trailing data")
		}
		return err
	}
	return nil
}

// PolicyFileSpec is one policy-file entry: the row key plus the policy
// document. Empty ProjectID/Route mean the default slot.
type PolicyFileSpec struct {
	ProjectID string
	Route     string
	Policy    Policy
}

type policyFileEntry struct {
	ProjectID string          `json:"project_id,omitempty"`
	Route     string          `json:"route,omitempty"`
	Policy    json.RawMessage `json:"policy"`
}

// LoadPolicyFile reads a refire policy seed file: a JSON array of
// {project_id, route, policy} entries, decoded strictly (unknown keys
// rejected, policies validated) so a mistyped operator file fails closed at
// boot.
func LoadPolicyFile(path string) ([]PolicyFileSpec, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read refire policy file: %w", err)
	}
	return ParsePolicyFile(b, path)
}

// ParsePolicyFile decodes a policy seed file's bytes (LoadPolicyFile's body,
// exposed for tests).
func ParsePolicyFile(b []byte, path string) ([]PolicyFileSpec, error) {
	var entries []policyFileEntry
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&entries); err != nil {
		return nil, fmt.Errorf("parse refire policy file %s: %w", path, err)
	}
	if err := ensureTrailingEOF(dec); err != nil {
		return nil, fmt.Errorf("parse refire policy file %s: %w", path, err)
	}
	specs := make([]PolicyFileSpec, 0, len(entries))
	for i, e := range entries {
		if len(e.Policy) == 0 {
			return nil, fmt.Errorf("refire policy file %s entry %d: missing policy", path, i)
		}
		p, err := ParsePolicy(string(e.Policy))
		if err != nil {
			return nil, fmt.Errorf("refire policy file %s entry %d: policy: %w", path, i, err)
		}
		specs = append(specs, PolicyFileSpec{
			ProjectID: e.ProjectID,
			Route:     e.Route,
			Policy:    p,
		})
	}
	return specs, nil
}
