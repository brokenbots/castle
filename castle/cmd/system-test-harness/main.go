// system-test-harness drives the Castle plus Criteria-agent Compose system test.
//
// Subcommands:
//
//	smoke   (default) — submit fixtures, watch runs, restart agents/Castle,
//	                    verify failure visibility and pause/resume/stop.
//	control — issue a single StopRun, PauseRun, or ResumeRun from a client container.
package main

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/exec"
	"strings"
	"time"

	"connectrpc.com/connect"
	"golang.org/x/net/http2"

	criteria "github.com/brokenbots/criteria/sdk"
	pb "github.com/brokenbots/criteria/sdk/pb/criteria/v1"
	"github.com/brokenbots/criteria/sdk/pb/criteria/v1/criteriav1connect"
)

const (
	defaultCastleAddr          = "http://castle:8080"
	defaultDockerSocket        = "/var/run/docker.sock"
	defaultProjectName         = "castle-system-test"
	defaultComposeFile         = "/src/compose.system.yml"
	defaultAgentAContainer     = "castle-system-test-agent-a-1"
	defaultAgentBContainer     = "castle-system-test-agent-b-1"
	defaultCastleContainer     = "castle-system-test-castle-1"
	defaultSubmissionContainer = "castle-system-test-submission-1"
)

var fixtures = []struct {
	name        string
	labels      map[string]string
	source      string
	wantAgent   string
	wantStatus  string
	description string
}{
	{
		name:        "valid-alpha",
		labels:      map[string]string{"pool": "alpha"},
		source:      "# valid alpha fixture\nvalid",
		wantAgent:   "agent-a",
		wantStatus:  "succeeded",
		description: "basic fixture routed to agent-a",
	},
	{
		name:        "valid-beta",
		labels:      map[string]string{"pool": "beta"},
		source:      "# valid beta fixture\nvalid",
		wantAgent:   "agent-b",
		wantStatus:  "succeeded",
		description: "basic fixture routed to agent-b",
	},
}

func envOrDefault(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

func h2cClient() *http.Client {
	return &http.Client{
		Transport: &http2.Transport{
			AllowHTTP: true,
			DialTLSContext: func(ctx context.Context, network, addr string, _ *tls.Config) (net.Conn, error) {
				return (&net.Dialer{}).DialContext(ctx, network, addr)
			},
		},
	}
}

type harness struct {
	log             *slog.Logger
	castleAddr      string
	dockerSocket    string
	projectName     string
	composeFile     string
	agentAContainer string
	agentBContainer string
	castleContainer string
	client          *http.Client
	criClient       criteriav1connect.CriteriaServiceClient
	srvClient       criteriav1connect.ServerServiceClient
	token           string
	newExecCmd      func(ctx context.Context, name string, arg ...string) *exec.Cmd
}

func main() {
	log := slog.New(slog.NewJSONHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo}))

	if len(os.Args) > 1 && os.Args[1] == "control" {
		if err := runControl(os.Args[2:]); err != nil {
			log.Error("control command failed", "err", err)
			os.Exit(1)
		}
		return
	}

	if err := runSmoke(log, os.Args[1:]); err != nil {
		log.Error("smoke test failed", "err", err)
		collectLogs(log)
		os.Exit(1)
	}
	log.Info("smoke test passed")
}

// controlClientToken is the JSON shape written by the agent to its home dir.
type controlClientToken struct {
	Token string `json:"token"`
}

func loadAgentToken(path string) (string, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	var s controlClientToken
	if err := json.Unmarshal(data, &s); err != nil {
		return "", err
	}
	if s.Token == "" {
		return "", fmt.Errorf("no token found in %s", path)
	}
	return s.Token, nil
}

func runControl(args []string) error {
	fs := flag.NewFlagSet("control", flag.ExitOnError)
	castleAddr := fs.String("castle", defaultCastleAddr, "Castle base URL")
	op := fs.String("op", "", "operation: stop|pause|resume")
	runID := fs.String("run-id", "", "run id to control")
	agentTokenFile := fs.String("agent-token-file", "", "path to the owning agent's persisted state file")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if *op == "" || *runID == "" || *agentTokenFile == "" {
		return errors.New("--op, --run-id and --agent-token-file are required")
	}

	token, err := loadAgentToken(*agentTokenFile)
	if err != nil {
		return fmt.Errorf("load agent token: %w", err)
	}

	httpClient := h2cClient()
	srvClient := criteriav1connect.NewServerServiceClient(httpClient, *castleAddr)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	auth := func(req connect.AnyRequest) {
		req.Header().Set("Authorization", "Bearer "+token)
	}

	switch *op {
	case "stop":
		req := connect.NewRequest(&pb.StopRunRequest{RunId: *runID, Reason: "system test control client"})
		auth(req)
		_, err := srvClient.StopRun(ctx, req)
		return err
	case "pause":
		req := connect.NewRequest(&pb.PauseRunRequest{RunId: *runID})
		auth(req)
		_, err := srvClient.PauseRun(ctx, req)
		return err
	case "resume":
		req := connect.NewRequest(&pb.ResumeRunRequest{RunId: *runID})
		auth(req)
		_, err := srvClient.ResumeRun(ctx, req)
		return err
	default:
		return fmt.Errorf("unknown operation: %s", *op)
	}
}

func runSmoke(log *slog.Logger, args []string) error {
	fs := flag.NewFlagSet("smoke", flag.ExitOnError)
	castleAddr := fs.String("castle", envOrDefault("CASTLE_ADDR", defaultCastleAddr), "Castle base URL")
	dockerSocket := fs.String("docker-socket", envOrDefault("DOCKER_SOCKET", defaultDockerSocket), "Docker socket path")
	projectName := fs.String("project", envOrDefault("COMPOSE_PROJECT_NAME", defaultProjectName), "Compose project name")
	composeFile := fs.String("compose-file", envOrDefault("COMPOSE_FILE", defaultComposeFile), "Compose file path")
	agentA := fs.String("agent-a", envOrDefault("AGENT_A_CONTAINER", defaultAgentAContainer), "agent-a container name")
	agentB := fs.String("agent-b", envOrDefault("AGENT_B_CONTAINER", defaultAgentBContainer), "agent-b container name")
	castle := fs.String("castle-container", envOrDefault("CASTLE_CONTAINER", defaultCastleContainer), "Castle container name")
	if err := fs.Parse(args); err != nil {
		return err
	}

	h := &harness{
		log:             log,
		castleAddr:      *castleAddr,
		dockerSocket:    *dockerSocket,
		projectName:     *projectName,
		composeFile:     *composeFile,
		agentAContainer: *agentA,
		agentBContainer: *agentB,
		castleContainer: *castle,
		client:          h2cClient(),
		criClient:       criteriav1connect.NewCriteriaServiceClient(h2cClient(), *castleAddr),
		srvClient:       criteriav1connect.NewServerServiceClient(h2cClient(), *castleAddr),
		newExecCmd:      exec.CommandContext,
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	if err := h.waitForCastle(ctx); err != nil {
		return fmt.Errorf("castle not healthy: %w", err)
	}
	if err := h.registerClient(ctx); err != nil {
		return fmt.Errorf("register submission client: %w", err)
	}
	if err := h.waitForAgents(ctx, 2); err != nil {
		return fmt.Errorf("agents not registered: %w", err)
	}

	// Phase 1: submit two fixtures and verify routing/completion.
	runIDs := make([]string, len(fixtures))
	for i, fx := range fixtures {
		runID, err := h.submitAssignment(ctx, fx.name, fx.source, fx.labels, fmt.Sprintf("key-%d", i))
		if err != nil {
			return fmt.Errorf("submit %s: %w", fx.name, err)
		}
		runIDs[i] = runID
		log.Info("submitted fixture", "name", fx.name, "run_id", runID, "labels", fx.labels)
	}

	for i, fx := range fixtures {
		if err := h.watchRunToTerminal(ctx, runIDs[i], fx.wantStatus); err != nil {
			return fmt.Errorf("watch %s (%s): %w", fx.name, runIDs[i], err)
		}
	}

	for i, fx := range fixtures {
		if err := h.assertRunRoutedToAgent(ctx, runIDs[i], fx.wantAgent); err != nil {
			return fmt.Errorf("routing %s (%s): %w", fx.name, runIDs[i], err)
		}
	}
	log.Info("phase 1 complete: fixtures finished and routed correctly")

	// Phase 2: restart agent-a during a run and verify reattach + exactly-once replay.
	if err := h.testAgentRestart(ctx, "agent-a"); err != nil {
		return fmt.Errorf("agent restart test: %w", err)
	}
	log.Info("phase 2 complete: agent restart recovery verified")

	// Phase 3: restart Castle during a run and verify durable queue/history/watch continuation.
	if err := h.testCastleRestart(ctx); err != nil {
		return fmt.Errorf("castle restart test: %w", err)
	}
	log.Info("phase 3 complete: castle restart recovery verified")

	// Phase 4: invalid workflow submission is centrally visible as a failed run.
	if err := h.testInvalidWorkflow(ctx); err != nil {
		return fmt.Errorf("invalid workflow test: %w", err)
	}
	log.Info("phase 4 complete: invalid workflow failure visible")

	// Phase 5: pause/resume and stop from a separate client container.
	if err := h.testControlClient(ctx); err != nil {
		return fmt.Errorf("control client test: %w", err)
	}
	log.Info("phase 5 complete: control client operations verified")

	return nil
}

// registerClient obtains a Criteria token for the submission client so that
// ServerService RPCs that require an authenticated caller can succeed.
func (h *harness) registerClient(ctx context.Context) error {
	req := connect.NewRequest(&pb.RegisterRequest{Name: "system-test-harness"})
	resp, err := h.criClient.Register(ctx, req)
	if err != nil {
		return err
	}
	h.token = resp.Msg.Token
	h.log.Info("submission client registered", "criteria_id", resp.Msg.CriteriaId)
	return nil
}

// auth sets the Bearer token on a connect request when the harness has one.
func (h *harness) auth(req connect.AnyRequest) {
	if h.token != "" {
		req.Header().Set("Authorization", "Bearer "+h.token)
	}
}

func (h *harness) waitForCastle(ctx context.Context) error {
	for {
		req := connect.NewRequest(&pb.ListAgentsRequest{Limit: 10})
		h.auth(req)
		_, err := h.srvClient.ListAgents(ctx, req)
		if err == nil {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(1 * time.Second):
		}
	}
}

func (h *harness) waitForAgents(ctx context.Context, want int) error {
	for {
		req := connect.NewRequest(&pb.ListAgentsRequest{Limit: 10})
		h.auth(req)
		resp, err := h.srvClient.ListAgents(ctx, req)
		if err != nil {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(1 * time.Second):
				continue
			}
		}
		online := 0
		for _, a := range resp.Msg.Agents {
			if a.Status == "online" {
				online++
			}
		}
		if online >= want {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(1 * time.Second):
		}
	}
}

func (h *harness) submitAssignment(ctx context.Context, name, source string, labels map[string]string, idempotencyKey string) (string, error) {
	req := connect.NewRequest(&pb.SubmitWorkflowAssignmentRequest{
		WorkflowName:   name,
		WorkflowSource: source,
		Labels:         labels,
		IdempotencyKey: idempotencyKey,
	})
	h.auth(req)
	resp, err := h.srvClient.SubmitWorkflowAssignment(ctx, req)
	if err != nil {
		return "", err
	}
	return resp.Msg.RunId, nil
}

func (h *harness) watchRunToTerminal(ctx context.Context, runID, wantStatus string) error {
	req := connect.NewRequest(&pb.WatchRunRequest{RunId: runID, SubscriberId: "harness"})
	h.auth(req)
	stream, err := h.srvClient.WatchRun(ctx, req)
	if err != nil {
		return err
	}
	defer stream.Close()

	deadline := time.After(60 * time.Second)
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-deadline:
			return fmt.Errorf("timeout waiting for run %s to finish", runID)
		default:
		}
		if !stream.Receive() {
			err := stream.Err()
			if err == nil {
				return fmt.Errorf("watch stream closed for %s", runID)
			}
			return err
		}
		msg := stream.Msg()
		if criteria.IsTerminal(msg) {
			got := ""
			switch msg.Payload.(type) {
			case *pb.Envelope_RunCompleted:
				got = "succeeded"
			case *pb.Envelope_RunFailed:
				got = "failed"
			}
			if got != wantStatus {
				return fmt.Errorf("run %s finished with status %q, want %q", runID, got, wantStatus)
			}
			return nil
		}
	}
}

func (h *harness) assertRunRoutedToAgent(ctx context.Context, runID, wantAgentName string) error {
	req := connect.NewRequest(&pb.GetRunRequest{RunId: runID})
	h.auth(req)
	resp, err := h.srvClient.GetRun(ctx, req)
	if err != nil {
		return err
	}
	run := resp.Msg

	listReq := connect.NewRequest(&pb.ListAgentsRequest{Limit: 10})
	h.auth(listReq)
	agents, err := h.srvClient.ListAgents(ctx, listReq)
	if err != nil {
		return err
	}
	var ownerName string
	for _, a := range agents.Msg.Agents {
		if a.CriteriaId == run.CriteriaId {
			ownerName = a.Name
			break
		}
	}
	if ownerName != wantAgentName {
		return fmt.Errorf("run %s owned by agent %q, want %q", runID, ownerName, wantAgentName)
	}
	return nil
}

func (h *harness) testAgentRestart(ctx context.Context, agentName string) error {
	labels := map[string]string{"pool": "alpha"}
	runID, err := h.submitAssignment(ctx, "agent-restart", "# long enough to restart\nvalid\nlong", labels, "agent-restart-key")
	if err != nil {
		return err
	}
	h.log.Info("submitted agent-restart fixture", "run_id", runID)

	// Wait until the run is running before restarting the agent.
	if err := h.waitForRunStatus(ctx, runID, "running"); err != nil {
		return err
	}

	container := h.agentAContainer
	if agentName == "agent-b" {
		container = h.agentBContainer
	}
	h.log.Info("restarting agent container", "container", container)
	if err := h.dockerRestart(ctx, container); err != nil {
		return err
	}

	// Allow the agent to reconnect and reattach.
	if err := h.waitForAgents(ctx, 2); err != nil {
		return err
	}

	if err := h.watchRunToTerminal(ctx, runID, "succeeded"); err != nil {
		return err
	}

	// Verify exactly-once event replay: no duplicate correlation ids in events.
	return h.assertNoDuplicateEvents(ctx, runID)
}

func (h *harness) testCastleRestart(ctx context.Context) error {
	labels := map[string]string{"pool": "beta"}
	runID, err := h.submitAssignment(ctx, "castle-restart", "# long enough to restart castle\nvalid\nlong", labels, "castle-restart-key")
	if err != nil {
		return err
	}
	h.log.Info("submitted castle-restart fixture", "run_id", runID)

	if err := h.waitForRunStatus(ctx, runID, "running"); err != nil {
		return err
	}

	h.log.Info("restarting castle container", "container", h.castleContainer)
	if err := h.dockerRestart(ctx, h.castleContainer); err != nil {
		return err
	}

	if err := h.waitForCastle(ctx); err != nil {
		return err
	}
	if err := h.waitForAgents(ctx, 2); err != nil {
		return err
	}

	// Watch continuation: the same run history and queue state survive.
	if err := h.watchRunToTerminal(ctx, runID, "succeeded"); err != nil {
		return err
	}

	// The run history is still queryable.
	listReq := connect.NewRequest(&pb.ListRunEventsRequest{RunId: runID, Limit: 100})
	h.auth(listReq)
	events, err := h.srvClient.ListRunEvents(ctx, listReq)
	if err != nil {
		return err
	}
	if len(events.Msg.Events) == 0 {
		return fmt.Errorf("run %s history empty after castle restart", runID)
	}
	return nil
}

func (h *harness) testInvalidWorkflow(ctx context.Context) error {
	runID, err := h.submitAssignment(ctx, "invalid", "invalid workflow source", map[string]string{"pool": "alpha"}, "invalid-key")
	if err != nil {
		return err
	}
	h.log.Info("submitted invalid fixture", "run_id", runID)
	return h.watchRunToTerminal(ctx, runID, "failed")
}

func (h *harness) testControlClient(ctx context.Context) error {
	// Use the pause fixture so we can pause/resume it from a separate container.
	runID, err := h.submitAssignment(ctx, "pause-resume", "# pause fixture\nvalid\npause", map[string]string{"pool": "alpha"}, "pause-key")
	if err != nil {
		return err
	}
	h.log.Info("submitted pause-resume fixture", "run_id", runID)

	if err := h.waitForRunStatus(ctx, runID, "paused"); err != nil {
		return err
	}

	// Issue resume from a separate control-client container.
	// The run is owned by agent-a, so authenticate as agent-a.
	if err := h.runControlClient(ctx, "resume", runID, "agent-a"); err != nil {
		return err
	}

	if err := h.watchRunToTerminal(ctx, runID, "succeeded"); err != nil {
		return err
	}

	// Test stop: submit a long-running run and stop it.
	stopRunID, err := h.submitAssignment(ctx, "stop-test", "# stop fixture\nvalid\nlong", map[string]string{"pool": "beta"}, "stop-key")
	if err != nil {
		return err
	}
	h.log.Info("submitted stop fixture", "run_id", stopRunID)

	if err := h.waitForRunStatus(ctx, stopRunID, "running"); err != nil {
		return err
	}

	// The run is owned by agent-b, so authenticate as agent-b.
	if err := h.runControlClient(ctx, "stop", stopRunID, "agent-b"); err != nil {
		return err
	}

	return h.watchRunToTerminal(ctx, stopRunID, "failed")
}

func (h *harness) waitForRunStatus(ctx context.Context, runID, want string) error {
	for {
		req := connect.NewRequest(&pb.GetRunRequest{RunId: runID})
		h.auth(req)
		resp, err := h.srvClient.GetRun(ctx, req)
		if err != nil {
			select {
			case <-ctx.Done():
				return ctx.Err()
			case <-time.After(500 * time.Millisecond):
				continue
			}
		}
		if resp.Msg.Status == want {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(500 * time.Millisecond):
		}
	}
}

func (h *harness) agentVolumeName(agentName string) string {
	switch agentName {
	case "agent-a":
		return h.projectName + "_agent-a-home"
	case "agent-b":
		return h.projectName + "_agent-b-home"
	default:
		return h.projectName + "_" + agentName + "-home"
	}
}

func (h *harness) runControlClient(ctx context.Context, op, runID, agentName string) error {
	h.log.Info("running control client", "op", op, "run_id", runID, "agent", agentName)
	cmd := h.controlClientCmd(ctx, op, runID, agentName)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	return cmd.Run()
}

func (h *harness) controlClientCmd(ctx context.Context, op, runID, agentName string) *exec.Cmd {
	volume := h.agentVolumeName(agentName)
	// The agent container writes state to /var/lib/agent/agent-state.json;
	// mount the owning agent's named volume at the same path read-only.
	mountPath := "/var/lib/agent"
	tokenFile := mountPath + "/agent-state.json"

	// Run the helper as a one-off Compose service container. Compose labels
	// one-off containers with com.docker.compose.oneoff=True, and the
	// docker compose up --abort-on-container-exit monitor ignores those
	// events, so a successful helper exit does not abort the submission
	// service.
	return h.execCmd(ctx, "docker", "compose",
		"-f", h.composeFile,
		"-p", h.projectName,
		"run", "--rm", "--no-deps",
		"-v", volume+":"+mountPath+":ro",
		"control",
		"control", "--op", op, "--run-id", runID, "--agent-token-file", tokenFile,
	)
}

func (h *harness) execCmd(ctx context.Context, name string, arg ...string) *exec.Cmd {
	if h.newExecCmd != nil {
		return h.newExecCmd(ctx, name, arg...)
	}
	return exec.CommandContext(ctx, name, arg...)
}

func (h *harness) dockerRestart(ctx context.Context, container string) error {
	cmd := h.execCmd(ctx, "docker", "--host", "unix://"+h.dockerSocket, "restart", container)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	return cmd.Run()
}

func (h *harness) assertNoDuplicateEvents(ctx context.Context, runID string) error {
	req := connect.NewRequest(&pb.ListRunEventsRequest{RunId: runID, Limit: 500})
	h.auth(req)
	events, err := h.srvClient.ListRunEvents(ctx, req)
	if err != nil {
		return err
	}
	return assertNoDuplicateEventsInList(events.Msg.Events)
}

// assertNoDuplicateEventsInList checks that no two events share the same
// correlation_id. Castle deduplicates SubmitEvents by correlation id, so a
// replayed event must not produce a duplicate.
func assertNoDuplicateEventsInList(events []*pb.Envelope) error {
	seen := map[string]struct{}{}
	for _, ev := range events {
		if _, ok := seen[ev.CorrelationId]; ok {
			return fmt.Errorf("duplicate correlation_id %q", ev.CorrelationId)
		}
		seen[ev.CorrelationId] = struct{}{}
	}
	return nil
}

func collectLogs(log *slog.Logger) {
	dockerSocket := envOrDefault("DOCKER_SOCKET", defaultDockerSocket)
	containers := []string{
		envOrDefault("CASTLE_CONTAINER", defaultCastleContainer),
		envOrDefault("AGENT_A_CONTAINER", defaultAgentAContainer),
		envOrDefault("AGENT_B_CONTAINER", defaultAgentBContainer),
		envOrDefault("SUBMISSION_CONTAINER", defaultSubmissionContainer),
	}
	fmt.Fprintln(os.Stdout, "--- COMPOSE LOGS ---")
	for _, c := range containers {
		fmt.Fprintf(os.Stdout, "\n--- logs from %s ---\n", c)
		cmd := exec.Command("docker", "--host", "unix://"+dockerSocket, "logs", "--tail", "500", c)
		out, err := cmd.CombinedOutput()
		if err != nil {
			fmt.Fprintf(os.Stdout, "failed to collect logs for %s: %v\n", c, err)
			continue
		}
		_, _ = io.Copy(os.Stdout, strings.NewReader(string(out)))
	}
	fmt.Fprintln(os.Stdout, "\n--- END COMPOSE LOGS ---")
}
