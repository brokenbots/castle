package main

import (
	"context"
	"log/slog"
	"path/filepath"
	"testing"

	criteria "github.com/brokenbots/criteria/sdk"
	pb "github.com/brokenbots/criteria/sdk/pb/criteria/v1"
)

func newTestAgent(home string) *agent {
	return &agent{
		log:           slog.Default(),
		state:         agentState{Runs: map[string]*runState{}},
		statePath:     filepath.Join(home, stateFileName),
		resumeCh:      map[string]chan string{},
		pendingResume: map[string]string{},
		runCtx:        map[string]*runHandle{},
	}
}

func TestLoadSaveState(t *testing.T) {
	dir := t.TempDir()
	a := newTestAgent(dir)

	a.state.CriteriaID = "agent-123"
	a.state.Token = "tok-abc"
	a.setRunState(&runState{
		RunID:        "run-1",
		Status:       "running",
		Paused:       true,
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

// TestResumeSignalDeliveredBeforePauseWaitRegistration pins the pause→resume
// handoff race: Castle marks the run paused while acknowledging WaitEntered,
// so a resume signal can arrive before the executor registers its wait. The
// agent must buffer the signal (no drop, no log-less loss) and hand it to the
// executor the moment the wait registers, so the executor always leaves the
// pause wait and the run completes. Against the pre-fix code this test fails:
// handleResumeRun silently discarded the unmatched signal.
func TestResumeSignalDeliveredBeforePauseWaitRegistration(t *testing.T) {
	a := newTestAgent(t.TempDir())

	// The executor has not reached its pause wait — no channel exists yet.
	a.handleResumeRun(context.Background(), &pb.ResumeRun{RunId: "run-1", Signal: "resume-test"})

	a.mu.Lock()
	_, registered := a.resumeCh["run-1"]
	a.mu.Unlock()
	if registered {
		t.Fatal("resume signal registration must not create the wait channel")
	}

	// The executor now reaches the pause wait and registers: the buffered
	// signal must be waiting on the returned channel, so its select returns
	// immediately and the run resumes.
	ch := a.registerResumeWait("run-1")
	select {
	case sig := <-ch:
		if sig != "resume-test" {
			t.Fatalf("buffered signal = %q, want %q", sig, "resume-test")
		}
	default:
		t.Fatal("resume signal delivered before wait registration was dropped")
	}

	// Delivery cleared the pending buffer: registering again (reattach replay)
	// must not re-consume the stale signal.
	a.mu.Lock()
	_, pending := a.pendingResume["run-1"]
	a.mu.Unlock()
	if pending {
		t.Fatal("pending resume signal not consumed by wait registration")
	}
}

// TestResumeSignalDeliveredAfterPauseWaitRegistration covers the registered
// path: the buffered channel carries the signal from delivery to the
// executor's read, and ending the wait removes the stale channel so a later
// signal for the finished wait is logged, not silently lost.
func TestResumeSignalDeliveredAfterPauseWaitRegistration(t *testing.T) {
	a := newTestAgent(t.TempDir())
	ch := a.registerResumeWait("run-1")

	// Signal delivered while the executor is between registration and its
	// read: the buffered channel must carry it.
	a.handleResumeRun(context.Background(), &pb.ResumeRun{RunId: "run-1", Signal: "resume-test"})
	select {
	case sig := <-ch:
		if sig != "resume-test" {
			t.Fatalf("delivered signal = %q, want %q", sig, "resume-test")
		}
	default:
		t.Fatal("resume signal delivered after wait registration did not reach the executor channel")
	}

	// The wait ends: the registry must not keep the stale channel.
	a.releaseResumeWait("run-1")
	a.mu.Lock()
	_, stale := a.resumeCh["run-1"]
	_, pending := a.pendingResume["run-1"]
	a.mu.Unlock()
	if stale {
		t.Fatal("stale resume channel left after the wait ended")
	}
	if pending {
		t.Fatal("pending resume signal left after the wait ended")
	}

	// A finished run must not re-arm a wait: without a registered channel no
	// entry is created, and deleteRunState drops any residual pending signal.
	a.deleteRunState("run-1")
	a.mu.Lock()
	_, pending = a.pendingResume["run-1"]
	a.mu.Unlock()
	if pending {
		t.Fatal("pending resume signal left after the run state was deleted")
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
