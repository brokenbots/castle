package main

import (
	"context"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"testing"

	pb "github.com/brokenbots/criteria/sdk/pb/criteria/v1"
)

func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

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

func TestControlClientCmdUsesIsolatedNetwork(t *testing.T) {
	h := &harness{
		log:              discardLogger(),
		projectName:      "castle-system-test",
		castleAddr:       "http://castle:8080",
		controlClientImg: "castle-system-test-harness",
		controlNetwork:   "castle-system-test_control",
	}
	cmd := h.controlClientCmd(context.Background(), "resume", "run-123", "agent-a")

	want := []string{
		"docker", "run", "--rm",
		"--network", "castle-system-test_control",
		"-e", "CASTLE_ADDR=http://castle:8080",
		"-v", "castle-system-test_agent-a-home:/var/lib/agent:ro",
		"castle-system-test-harness",
		"control", "--op", "resume", "--run-id", "run-123", "--agent-token-file", "/var/lib/agent/agent-state.json",
	}
	if !reflect.DeepEqual(cmd.Args, want) {
		t.Errorf("cmd.Args = %v, want %v", cmd.Args, want)
	}
}

func TestControlClientCmdFallsBackToProjectNetwork(t *testing.T) {
	h := &harness{
		log:              discardLogger(),
		projectName:      "castle-system-test",
		castleAddr:       "http://castle:8080",
		controlClientImg: "castle-system-test-harness",
	}
	cmd := h.controlClientCmd(context.Background(), "stop", "run-456", "agent-b")

	if got := cmd.Args[4]; got != "castle-system-test_default" {
		t.Errorf("fallback network = %q, want castle-system-test_default", got)
	}
}

func TestSetupControlNetworkCommands(t *testing.T) {
	var recorded [][]string
	h := &harness{
		log:             discardLogger(),
		projectName:     "castle-system-test",
		castleContainer: "castle-system-test-castle-1",
		dockerSocket:    "/var/run/docker.sock",
		newExecCmd: func(_ context.Context, name string, arg ...string) *exec.Cmd {
			recorded = append(recorded, append([]string{name}, arg...))
			// Return a command that does nothing so Run() succeeds.
			return exec.CommandContext(context.Background(), "true")
		},
	}

	if err := h.setupControlNetwork(context.Background()); err != nil {
		t.Fatalf("setup control network: %v", err)
	}
	if h.controlNetwork != "castle-system-test_control" {
		t.Errorf("controlNetwork = %q, want castle-system-test_control", h.controlNetwork)
	}
	if len(recorded) < 3 {
		t.Fatalf("expected at least 3 docker commands, got %d: %v", len(recorded), recorded)
	}

	// First command is a best-effort cleanup of stale network.
	if got, want := recorded[0], []string{"docker", "--host", "unix:///var/run/docker.sock", "network", "rm", "castle-system-test_control"}; !reflect.DeepEqual(got, want) {
		t.Errorf("cleanup command = %v, want %v", got, want)
	}
	// Second command creates the network.
	if got, want := recorded[1], []string{"docker", "--host", "unix:///var/run/docker.sock", "network", "create", "--driver", "bridge", "castle-system-test_control"}; !reflect.DeepEqual(got, want) {
		t.Errorf("create command = %v, want %v", got, want)
	}
	// Third command connects castle with the "castle" alias.
	if got, want := recorded[2], []string{"docker", "--host", "unix:///var/run/docker.sock", "network", "connect", "--alias", "castle", "castle-system-test_control", "castle-system-test-castle-1"}; !reflect.DeepEqual(got, want) {
		t.Errorf("connect command = %v, want %v", got, want)
	}
}

func TestTeardownControlNetworkCommands(t *testing.T) {
	var recorded [][]string
	h := &harness{
		log:             discardLogger(),
		projectName:     "castle-system-test",
		castleContainer: "castle-system-test-castle-1",
		dockerSocket:    "/var/run/docker.sock",
		controlNetwork:  "castle-system-test_control",
		newExecCmd: func(_ context.Context, name string, arg ...string) *exec.Cmd {
			recorded = append(recorded, append([]string{name}, arg...))
			return exec.CommandContext(context.Background(), "true")
		},
	}

	h.teardownControlNetwork(context.Background())
	if h.controlNetwork != "" {
		t.Errorf("controlNetwork not cleared, got %q", h.controlNetwork)
	}
	if len(recorded) != 2 {
		t.Fatalf("expected 2 docker commands, got %d: %v", len(recorded), recorded)
	}
	if got, want := recorded[0], []string{"docker", "--host", "unix:///var/run/docker.sock", "network", "disconnect", "castle-system-test_control", "castle-system-test-castle-1"}; !reflect.DeepEqual(got, want) {
		t.Errorf("disconnect command = %v, want %v", got, want)
	}
	if got, want := recorded[1], []string{"docker", "--host", "unix:///var/run/docker.sock", "network", "rm", "castle-system-test_control"}; !reflect.DeepEqual(got, want) {
		t.Errorf("remove command = %v, want %v", got, want)
	}
}
