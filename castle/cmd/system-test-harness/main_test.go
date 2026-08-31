package main

import (
	"os"
	"path/filepath"
	"testing"

	pb "github.com/brokenbots/criteria/sdk/pb/criteria/v1"
)

func TestEnvOrDefaultHarness(t *testing.T) {
	t.Setenv("TEST_HARNESS_VAR", "override")
	if got := envOrDefault("TEST_HARNESS_VAR", "fallback"); got != "override" {
		t.Errorf("env set: got %q want override", got)
	}
	if got := envOrDefault("TEST_HARNESS_VAR_NOT_SET", "fallback"); got != "fallback" {
		t.Errorf("env unset: got %q want fallback", got)
	}
}

func TestH2CClient(t *testing.T) {
	c := h2cClient()
	if c == nil {
		t.Fatal("h2cClient returned nil")
	}
	if c.Transport == nil {
		t.Fatal("h2cClient transport is nil")
	}
}

func TestFixturesDefined(t *testing.T) {
	if len(fixtures) == 0 {
		t.Fatal("no fixtures defined")
	}
	for _, fx := range fixtures {
		if fx.name == "" || fx.source == "" || fx.wantAgent == "" || fx.wantStatus == "" {
			t.Errorf("incomplete fixture: %+v", fx)
		}
	}
}

func TestAssertNoDuplicateEventsInList(t *testing.T) {
	if err := assertNoDuplicateEventsInList([]*pb.Envelope{
		{CorrelationId: "a"},
		{CorrelationId: "b"},
		{CorrelationId: "c"},
	}); err != nil {
		t.Fatalf("unique list: %v", err)
	}

	wantErr := "duplicate correlation_id \"a\""
	gotErr := assertNoDuplicateEventsInList([]*pb.Envelope{
		{CorrelationId: "a"},
		{CorrelationId: "b"},
		{CorrelationId: "a"},
	})
	if gotErr == nil || gotErr.Error() != wantErr {
		t.Fatalf("duplicate list: got %v, want %v", gotErr, wantErr)
	}
}

func TestLoadAgentToken(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "agent-state.json")
	if err := os.WriteFile(path, []byte(`{"token":"agent-token-xyz"}`), 0o600); err != nil {
		t.Fatalf("write state: %v", err)
	}
	token, err := loadAgentToken(path)
	if err != nil {
		t.Fatalf("load token: %v", err)
	}
	if token != "agent-token-xyz" {
		t.Errorf("token = %q, want agent-token-xyz", token)
	}

	if _, err := loadAgentToken(filepath.Join(dir, "missing.json")); err == nil {
		t.Error("expected error for missing file")
	}
}
