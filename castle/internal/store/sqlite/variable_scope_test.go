package sqlite_test

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/brokenbots/overlord/castle/internal/store"
	"github.com/brokenbots/overlord/castle/internal/store/sqlite"
)

func mustCreateOverseer(t *testing.T, s *sqlite.Store, ctx context.Context) {
	t.Helper()
	now := time.Now().UTC()
	err := s.CreateOverseer(ctx, &store.Overseer{
		ID:         "ov-1",
		Name:       "test-overseer",
		TokenHash:  "testhash",
		Status:     "online",
		CreatedAt:  now,
		LastSeenAt: now,
	})
	if err != nil {
		t.Fatalf("CreateOverseer: %v", err)
	}
}

func mustCreateRun(t *testing.T, s *sqlite.Store, ctx context.Context, id string) {
	t.Helper()
	err := s.CreateRun(ctx, &store.Run{
		ID:           id,
		OverseerID:   "ov-1",
		WorkflowName: "test",
		WorkflowHCL:  "{}",
		Status:       "running",
	})
	if err != nil {
		t.Fatalf("CreateRun: %v", err)
	}
}

func TestVariableScope_SetAndGet(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	mustCreateOverseer(t, s, ctx)
	mustCreateRun(t, s, ctx, "run-scope-1")

	// No scope yet: should return empty string without error.
	scope, err := s.GetRunVariableScope(ctx, "run-scope-1")
	if err != nil {
		t.Fatalf("GetRunVariableScope (empty): %v", err)
	}
	if scope != "" {
		t.Errorf("expected empty scope, got %q", scope)
	}

	// Set a scope and read it back.
	scopeJSON := `{"var":{"x":"42"},"steps":{"build":{"stdout":"artifact\n"}}}`
	if err := s.SetRunVariableScope(ctx, "run-scope-1", scopeJSON); err != nil {
		t.Fatalf("SetRunVariableScope: %v", err)
	}

	got, err := s.GetRunVariableScope(ctx, "run-scope-1")
	if err != nil {
		t.Fatalf("GetRunVariableScope (set): %v", err)
	}
	if got != scopeJSON {
		t.Errorf("scope = %q, want %q", got, scopeJSON)
	}
}

func TestVariableScope_Overwrite(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	mustCreateOverseer(t, s, ctx)
	mustCreateRun(t, s, ctx, "run-scope-2")

	if err := s.SetRunVariableScope(ctx, "run-scope-2", `{"var":{}}`); err != nil {
		t.Fatalf("SetRunVariableScope first: %v", err)
	}
	if err := s.SetRunVariableScope(ctx, "run-scope-2", `{"var":{"y":"new"}}`); err != nil {
		t.Fatalf("SetRunVariableScope overwrite: %v", err)
	}
	got, err := s.GetRunVariableScope(ctx, "run-scope-2")
	if err != nil {
		t.Fatalf("GetRunVariableScope: %v", err)
	}
	if got != `{"var":{"y":"new"}}` {
		t.Errorf("scope = %q, want overwritten value", got)
	}
}

func TestVariableScope_NotFound(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	_, err := s.GetRunVariableScope(ctx, "nonexistent-run")
	if err == nil {
		t.Error("expected error for nonexistent run, got nil")
	}
}

func TestRun_VariableScopeField(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	mustCreateOverseer(t, s, ctx)
	mustCreateRun(t, s, ctx, "run-scope-3")

	if err := s.SetRunVariableScope(ctx, "run-scope-3", `{"var":{"z":"scope_val"}}`); err != nil {
		t.Fatalf("SetRunVariableScope: %v", err)
	}

	// GetRun should include VariableScope.
	got, err := s.GetRun(ctx, "run-scope-3")
	if err != nil {
		t.Fatalf("GetRun: %v", err)
	}
	if got.VariableScope != `{"var":{"z":"scope_val"}}` {
		t.Errorf("GetRun.VariableScope = %q, want scope JSON", got.VariableScope)
	}
}

// TestReattachScopeRoundtrip simulates the crash-recovery flow:
//  1. A run accumulates variable scope via sequential SetRunVariableScope calls.
//  2. GetRun returns the latest scope (as ReattachRun would).
//  3. The scope JSON can be unmarshalled and contains the expected keys.
func TestReattachScopeRoundtrip(t *testing.T) {
	s := newTestStore(t)
	ctx := context.Background()

	mustCreateOverseer(t, s, ctx)
	mustCreateRun(t, s, ctx, "run-reattach-1")

	// Simulate VariableSet event (default seeded).
	scope1 := `{"var":{"env":"staging"},"steps":{}}`
	if err := s.SetRunVariableScope(ctx, "run-reattach-1", scope1); err != nil {
		t.Fatalf("SetRunVariableScope (initial): %v", err)
	}

	// Simulate StepOutputCaptured event (build step completed).
	scope2 := `{"var":{"env":"staging"},"steps":{"build":{"stdout":"artifact.bin\n","exit_code":"0"}}}`
	if err := s.SetRunVariableScope(ctx, "run-reattach-1", scope2); err != nil {
		t.Fatalf("SetRunVariableScope (after build): %v", err)
	}

	// ReattachRun reads the run — scope must be current.
	run, err := s.GetRun(ctx, "run-reattach-1")
	if err != nil {
		t.Fatalf("GetRun: %v", err)
	}
	if run.VariableScope != scope2 {
		t.Errorf("reattach scope = %q, want latest scope", run.VariableScope)
	}

	// Scope JSON must parse correctly (as RestoreVarScope would).
	var raw map[string]interface{}
	if err := json.Unmarshal([]byte(run.VariableScope), &raw); err != nil {
		t.Fatalf("scope JSON invalid: %v", err)
	}
	varMap, _ := raw["var"].(map[string]interface{})
	if varMap["env"] != "staging" {
		t.Errorf("var.env = %v, want 'staging'", varMap["env"])
	}
	stepsMap, _ := raw["steps"].(map[string]interface{})
	buildMap, _ := stepsMap["build"].(map[string]interface{})
	if buildMap["stdout"] != "artifact.bin\n" {
		t.Errorf("steps.build.stdout = %v, want 'artifact.bin\\n'", buildMap["stdout"])
	}
}

func newTestStore(t *testing.T) *sqlite.Store {
	t.Helper()
	s, err := sqlite.Open(":memory:")
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}
