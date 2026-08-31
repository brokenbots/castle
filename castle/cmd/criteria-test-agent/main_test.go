package main

import (
	"log/slog"
	"path/filepath"
	"testing"

	criteria "github.com/brokenbots/criteria/sdk"
	pb "github.com/brokenbots/criteria/sdk/pb/criteria/v1"
)

func newTestAgent(home string) *agent {
	return &agent{
		log:       slog.Default(),
		state:     agentState{Runs: map[string]*runState{}},
		statePath: filepath.Join(home, stateFileName),
		resumeCh:  map[string]chan string{},
	}
}

func TestLoadSaveState(t *testing.T) {
	dir := t.TempDir()
	a := newTestAgent(dir)

	a.state.CriteriaID = "agent-123"
	a.state.Token = "tok-abc"
	a.setRunState(&runState{
		RunID:    "run-1",
		Status:   "running",
		Paused:   true,
		ResumeSignal: "go",
	})

	if err := a.saveState(); err != nil {
		t.Fatalf("save state: %v", err)
	}

	b := newTestAgent(dir)
	if err := b.loadState(); err != nil {
		t.Fatalf("load state: %v", err)
	}

	if b.state.CriteriaID != "agent-123" {
		t.Errorf("criteria_id: got %q want %q", b.state.CriteriaID, "agent-123")
	}
	if b.state.Token != "tok-abc" {
		t.Errorf("token: got %q want %q", b.state.Token, "tok-abc")
	}
	rs, ok := b.state.Runs["run-1"]
	if !ok {
		t.Fatalf("run-1 missing from loaded state")
	}
	if rs.Status != "running" || !rs.Paused || rs.ResumeSignal != "go" {
		t.Errorf("run state mismatch: %+v", rs)
	}
}

func TestTokenAndCriteriaID(t *testing.T) {
	a := newTestAgent(t.TempDir())
	a.state.CriteriaID = "cid"
	a.state.Token = "token"

	if got := a.criteriaID(); got != "cid" {
		t.Errorf("criteriaID() = %q, want %q", got, "cid")
	}
	if got := a.token(); got != "token" {
		t.Errorf("token() = %q, want %q", got, "token")
	}
}

func TestDeterministicCorrelationID(t *testing.T) {
	runID := "run-42"

	tests := []struct {
		name string
		env  *criteria.Envelope
		want string
	}{
		{
			name: "step entered",
			env:  criteria.NewEnvelope(runID, &pb.StepEntered{Step: "build"}),
			want: "run-42-step.entered-build",
		},
		{
			name: "step outcome",
			env:  criteria.NewEnvelope(runID, &pb.StepOutcome{Step: "build"}),
			want: "run-42-step.outcome-build",
		},
		{
			name: "wait entered",
			env:  criteria.NewEnvelope(runID, &pb.WaitEntered{Signal: "resume"}),
			want: "run-42-wait.entered-resume",
		},
		{
			name: "run failed",
			env:  criteria.NewEnvelope(runID, &pb.RunFailed{Reason: "boom"}),
			want: "run-42-run.failed",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := deterministicCorrelationID(runID, tt.env)
			if got != tt.want {
				t.Errorf("got %q, want %q", got, tt.want)
			}
			// Determinism: second call must match first.
			if got2 := deterministicCorrelationID(runID, tt.env); got2 != got {
				t.Errorf("not deterministic: %q vs %q", got, got2)
			}
		})
	}
}

func TestParseLabels(t *testing.T) {
	tests := []struct {
		in   string
		want map[string]string
	}{
		{"a=1,b=2", map[string]string{"a": "1", "b": "2"}},
		{" a = 1 , b=2 ", map[string]string{"a": "1", "b": "2"}},
		{"", map[string]string{}},
		{"no-equals,foo=bar", map[string]string{"foo": "bar"}},
	}
	for _, tt := range tests {
		t.Run(tt.in, func(t *testing.T) {
			got := parseLabels(tt.in)
			if len(got) != len(tt.want) {
				t.Fatalf("got %v, want %v", got, tt.want)
			}
			for k, v := range tt.want {
				if got[k] != v {
					t.Errorf("label %q: got %q want %q", k, got[k], v)
				}
			}
		})
	}
}

func TestEnvOrDefault(t *testing.T) {
	t.Setenv("TEST_AGENT_VAR", "override")
	if got := envOrDefault("TEST_AGENT_VAR", "fallback"); got != "override" {
		t.Errorf("env set: got %q want override", got)
	}
	if got := envOrDefault("TEST_AGENT_VAR_NOT_SET", "fallback"); got != "fallback" {
		t.Errorf("env unset: got %q want fallback", got)
	}
}
