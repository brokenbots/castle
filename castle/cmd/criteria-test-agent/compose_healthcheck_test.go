package main

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/brokenbots/criteria/sdk/pb/criteria/v1/criteriav1connect"
)

// TestComposeAgentHealthCheck runs the agent health-check command from
// compose.system.yml against a live agent and Castle server.
//
// It verifies that the process-matching portion of the command finds the
// running criteria-test-agent process. The old pgrep -x pattern fails because
// /proc/<pid>/comm is truncated to 15 characters, so this test acts as a
// regression guard: it would fail when the Compose file uses pgrep -x and
// passes once it uses pgrep -f.
func TestComposeAgentHealthCheck(t *testing.T) {
	if _, err := exec.LookPath("pgrep"); err != nil {
		t.Skip("pgrep not available in PATH")
	}
	if _, err := exec.LookPath("wget"); err != nil {
		t.Skip("wget not available in PATH")
	}

	baseURL, ownerToken := startTestCastle(t)
	srvClient := criteriav1connect.NewServerServiceClient(h2cClient(), baseURL)

	// Build the real agent binary to a path that contains the full binary
	// name, matching how the Compose service runs it.
	agentBin := filepath.Join(t.TempDir(), "criteria-test-agent")
	cmd := exec.Command("go", "build", "-o", agentBin, "./criteria-test-agent")
	cmd.Dir = ".." // castle/cmd
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("build criteria-test-agent: %v\n%s", err, out)
	}

	agentHome := t.TempDir()
	agentCtx, cancelAgent := context.WithCancel(context.Background())
	defer cancelAgent()
	agentCmd := exec.CommandContext(agentCtx, agentBin)
	agentCmd.Env = append(os.Environ(),
		"CASTLE_ADDR="+baseURL,
		"AGENT_NAME=healthcheck-test-agent",
		"AGENT_LABELS=pool=healthcheck",
		"AGENT_HOME_DIR="+agentHome,
	)
	if err := agentCmd.Start(); err != nil {
		t.Fatalf("start agent: %v", err)
	}
	t.Cleanup(func() {
		cancelAgent()
		_ = agentCmd.Wait()
	})

	waitCtx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if err := waitForOnline(waitCtx, srvClient, ownerToken, 1); err != nil {
		t.Fatalf("agent did not register: %v", err)
	}

	// The bug: pgrep -x looks at /proc/<pid>/comm, which is truncated to 15
	// characters for "criteria-test-agent". It must fail to find the process.
	// This is a Linux-specific regression guard; on macOS the comm is not
	// truncated, so the assertion does not hold there.
	if runtime.GOOS == "linux" {
		if out, err := exec.Command("pgrep", "-x", "criteria-test-agent").CombinedOutput(); err == nil {
			t.Errorf("pgrep -x unexpectedly matched a process: %s", out)
		}
	}

	// Extract the health-check command from compose.system.yml and run it
	// against the in-process Castle server.
	healthCmd := composeHealthCheckCommand(t)
	healthCmd = strings.ReplaceAll(healthCmd, "http://castle:8080", baseURL)

	shell := exec.Command("sh", "-c", healthCmd)
	if out, err := shell.CombinedOutput(); err != nil {
		t.Fatalf("compose health-check command failed: %v\n%s", err, out)
	}
}

var composeHealthCheckRE = regexp.MustCompile(`(?s)agent-(?:a|b):.*?test:\s*\["CMD-SHELL",\s*"((?:[^"\\]|\\.)*)"\]`)

// composeHealthCheckCommand returns the shell command portion of the agent
// health check defined in compose.system.yml. Both agent-a and agent-b use
// the same command, so the first match is sufficient.
func composeHealthCheckCommand(t *testing.T) string {
	t.Helper()

	root, err := filepath.Abs("../../..")
	if err != nil {
		t.Fatalf("resolve repo root: %v", err)
	}
	data, err := os.ReadFile(filepath.Join(root, "compose.system.yml"))
	if err != nil {
		t.Fatalf("read compose.system.yml: %v", err)
	}

	m := composeHealthCheckRE.FindStringSubmatch(string(data))
	if m == nil {
		t.Fatalf("could not find agent health-check command in compose.system.yml")
	}
	return m[1]
}
