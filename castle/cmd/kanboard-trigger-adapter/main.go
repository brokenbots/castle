// Command kanboard-trigger-adapter is the KB-320 thin board adapter: it
// polls one Kanboard project, fires Castle workflow assignments from card
// entries (via castle.v1 SubmitProjectAssignment under a submit-only
// trigger identity), and mirrors run events back onto card columns via
// WatchRun streams (the outgoing criteria-k8s watcher's reconcile
// replacement). It holds no routing logic: lanes carry static route names.
package main

import (
	"context"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"connectrpc.com/connect"

	criteriav1 "github.com/brokenbots/criteria/sdk/pb/criteria/v1"
	"github.com/brokenbots/criteria/sdk/pb/criteria/v1/criteriav1connect"

	castlev1 "github.com/brokenbots/castle/castle/gen/castle/v1"
	"github.com/brokenbots/castle/castle/gen/castle/v1/castlev1connect"

	"github.com/brokenbots/castle/castle/internal/kanboard"
	"github.com/brokenbots/castle/castle/internal/kanboardtrigger"
)

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

func run(args []string, stdout, stderr io.Writer) int {
	envOrDefault := func(key, def string) string {
		if v, set := os.LookupEnv(key); set {
			return v
		}
		return def
	}

	fs := flag.NewFlagSet("kanboard-trigger-adapter", flag.ContinueOnError)
	fs.SetOutput(stderr)
	configPath := fs.String("config", envOrDefault("CASTLE_ADAPTER_CONFIG", ""), "lanes config JSON path (required; CASTLE_ADAPTER_CONFIG)")
	statePath := fs.String("state-file", envOrDefault("CASTLE_ADAPTER_STATE_PATH", ""), "durable state file (required; CASTLE_ADAPTER_STATE_PATH)")
	kanboardURL := fs.String("kanboard-url", envOrDefault("CASTLE_KANBOARD_URL", ""), "Kanboard base URL including /jsonrpc.php (CASTLE_KANBOARD_URL)")
	kanboardToken := fs.String("kanboard-token", envOrDefault("CASTLE_KANBOARD_APP_TOKEN", ""), "Kanboard app token (CASTLE_KANBOARD_APP_TOKEN)")
	castleURL := fs.String("castle-url", envOrDefault("CASTLE_URL", ""), "castle base URL (or CASTLE_URL)")
	castleToken := fs.String("castle-token", envOrDefault("CASTLE_TRIGGER_TOKEN", ""), "castle trigger-identity token (or CASTLE_TRIGGER_TOKEN)")
	boardProject := fs.String("board-project", "", "override the config's board_project")
	castleProject := fs.String("castle-project-id", "", "override the config's castle_project_id")
	triggerTag := fs.String("trigger-tag", "", "override the config's trigger_tag")
	reviewColumn := fs.String("review-column", "", "override the config's review_column")
	doneColumn := fs.String("done-column", "", "override the config's done_column")
	pollInterval := fs.Duration("poll-interval", 30*time.Second, "board poll interval")
	instance := fs.String("instance", "default", "instance id (scopes WatchRun subscriber cursors)")
	if err := fs.Parse(args); err != nil {
		return 2
	}
	if fs.NArg() != 0 {
		fmt.Fprintln(stderr, "unexpected positional arguments")
		return 2
	}

	log := slog.New(slog.NewJSONHandler(stderr, &slog.HandlerOptions{Level: slog.LevelInfo}))

	if *configPath == "" || *statePath == "" || *kanboardURL == "" || *castleURL == "" || *castleToken == "" {
		fmt.Fprintln(stderr, "--config, --state-file, --kanboard-url, --castle-url and --castle-token are required")
		return 2
	}
	if *kanboardToken == "" {
		fmt.Fprintln(stderr, "--kanboard-token is required (a Kanboard app token; do not run writeable board clients unauthenticated)")
		return 2
	}

	cfg, err := kanboardtrigger.LoadConfigFile(*configPath)
	if err != nil {
		fmt.Fprintln(stderr, err.Error())
		return 2
	}
	if *boardProject != "" {
		cfg.BoardProject = *boardProject
	}
	if *castleProject != "" {
		cfg.CastleProjectID = *castleProject
	}
	if *triggerTag != "" {
		cfg.TriggerTag = *triggerTag
	}
	if *reviewColumn != "" {
		cfg.ReviewColumn = *reviewColumn
	}
	if *doneColumn != "" {
		cfg.DoneColumn = *doneColumn
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	kb := kanboard.New(*kanboardURL, *kanboardToken)
	if err := kb.Ping(ctx); err != nil {
		log.Error("kanboard is not reachable", "err", err)
		return 1
	}

	clients := newCastleClients(http.DefaultClient, *castleURL, *castleToken)

	adapter, err := kanboardtrigger.New(cfg, kb, clients, clients, clients, log, kanboardtrigger.Options{
		StatePath:    *statePath,
		PollInterval: *pollInterval,
		Instance:     *instance,
	})
	if err != nil {
		log.Error("starting adapter", "err", err)
		return 1
	}
	if err := adapter.Run(ctx); err != nil && ctx.Err() == nil {
		log.Error("adapter stopped", "err", err)
		return 1
	}
	return 0
}

// castleClients implements the adapter's three engine interfaces against the
// castle Connect endpoints, attaching the trigger identity's bearer token to
// every request.
type castleClients struct {
	project castlev1connect.ProjectServiceClient
	server  criteriav1connect.ServerServiceClient
	token   string
}

func newCastleClients(hc connect.HTTPClient, baseURL, token string) *castleClients {
	return &castleClients{
		project: castlev1connect.NewProjectServiceClient(hc, baseURL),
		server:  criteriav1connect.NewServerServiceClient(hc, baseURL),
		token:   token,
	}
}

func (c *castleClients) auth(h http.Header) { h.Set("Authorization", "Bearer "+c.token) }

func (c *castleClients) SubmitProjectAssignment(ctx context.Context, req *connect.Request[castlev1.SubmitProjectAssignmentRequest]) (*connect.Response[castlev1.SubmitProjectAssignmentResponse], error) {
	c.auth(req.Header())
	return c.project.SubmitProjectAssignment(ctx, req)
}

func (c *castleClients) GetAssignmentDisposition(ctx context.Context, req *connect.Request[criteriav1.GetAssignmentDispositionRequest]) (*connect.Response[criteriav1.GetAssignmentDispositionResponse], error) {
	c.auth(req.Header())
	return c.server.GetAssignmentDisposition(ctx, req)
}

func (c *castleClients) WatchRun(ctx context.Context, req *connect.Request[criteriav1.WatchRunRequest]) (*connect.ServerStreamForClient[criteriav1.Envelope], error) {
	c.auth(req.Header())
	return c.server.WatchRun(ctx, req)
}
