package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"connectrpc.com/connect"
	"connectrpc.com/grpchealth"
	"connectrpc.com/grpcreflect"
	"golang.org/x/net/http2"
	"golang.org/x/net/http2/h2c"

	"github.com/brokenbots/castle/castle/internal/auth"
	"github.com/brokenbots/castle/castle/internal/hub"
	"github.com/brokenbots/castle/castle/internal/rpc"
	"github.com/brokenbots/castle/castle/internal/store"
	"github.com/brokenbots/castle/castle/internal/store/pg"
	"github.com/brokenbots/castle/castle/internal/store/refire"
	"github.com/brokenbots/castle/castle/internal/store/sqlite"
	criteria "github.com/brokenbots/criteria/sdk"
	"github.com/brokenbots/criteria/sdk/pb/criteria/v1/criteriav1connect"

	castlev1connect "github.com/brokenbots/castle/castle/gen/castle/v1/castlev1connect"
)

// openStoreProfile opens the store for the requested profile. The pg profile
// (KB-317) runs against Postgres through the same store.Store surface; sqlite
// remains the local/dev default.
func openStoreProfile(profile, dbPath, pgDSN string) (store.Store, error) {
	switch strings.ToLower(strings.TrimSpace(profile)) {
	case "", "sqlite":
		return sqlite.Open(dbPath)
	case "pg":
		if strings.TrimSpace(pgDSN) == "" {
			return nil, errors.New("--store pg requires --pg-dsn (or CASTLE_PG_DSN)")
		}
		return pg.Open(pgDSN)
	default:
		return nil, fmt.Errorf("unknown --store profile %q (want sqlite or pg)", profile)
	}
}

func envOrDefault(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

func envOrDefaultBool(key string, fallback bool) bool {
	v := os.Getenv(key)
	if v == "" {
		return fallback
	}
	b, err := strconv.ParseBool(v)
	if err != nil {
		return fallback
	}
	return b
}

func envOrDefaultInt(key string, fallback int) int {
	v := os.Getenv(key)
	if v == "" {
		return fallback
	}
	out, err := strconv.Atoi(v)
	if err != nil {
		return fallback
	}
	return out
}

func envOrDefaultDuration(key string, fallback time.Duration) time.Duration {
	v := os.Getenv(key)
	if v == "" {
		return fallback
	}
	out, err := time.ParseDuration(v)
	if err != nil {
		return fallback
	}
	return out
}

func flagWasSet(name string) bool {
	set := false
	flag.Visit(func(f *flag.Flag) {
		if f.Name == name {
			set = true
		}
	})
	return set
}

// anonReadsFlag registers --allow-anon-reads on fs with the KB-326 S3
// resolution: explicit flag value > the CASTLE_ALLOW_ANON_READS env override
// (left in for local dev) > off. main passes flag.CommandLine, which is why
// the flag set is a parameter — the resolution lives here so tests can
// exercise it without running main.
func anonReadsFlag(fs *flag.FlagSet) *bool {
	return fs.Bool("allow-anon-reads", envOrDefaultBool("CASTLE_ALLOW_ANON_READS", false),
		"allow anonymous read RPCs for legacy unscoped runs (default false; CASTLE_ALLOW_ANON_READS=true for local dev)")
}

func buildInterceptorOpts(bootstrapToken string, allowAnonRegister bool) []auth.InterceptorOption {
	var opts []auth.InterceptorOption
	if allowAnonRegister {
		opts = append(opts, auth.WithAnonRegister())
	} else if bootstrapToken != "" {
		opts = append(opts, auth.WithBootstrapToken(bootstrapToken))
	}
	return opts
}

func main() {
	addr := flag.String("addr", envOrDefault("CASTLE_ADDR", ":8080"), "listen address (or CASTLE_ADDR)")
	dbPath := flag.String("db", envOrDefault("CASTLE_DB_PATH", "./castle.db"), "sqlite database path (or CASTLE_DB_PATH)")
	storeProfile := flag.String("store", envOrDefault("CASTLE_STORE", "sqlite"), "storage engine profile: sqlite (local/dev) or pg (server, KB-317) (or CASTLE_STORE)")
	pgDSN := flag.String("pg-dsn", envOrDefault("CASTLE_PG_DSN", ""), "Postgres DSN for --store pg (or CASTLE_PG_DSN)")
	tlsCert := flag.String("tls-cert", envOrDefault("CASTLE_TLS_CERT", ""), "TLS certificate path (or CASTLE_TLS_CERT)")
	tlsKey := flag.String("tls-key", envOrDefault("CASTLE_TLS_KEY", ""), "TLS key path (or CASTLE_TLS_KEY)")
	tlsCA := flag.String("tls-ca", envOrDefault("CASTLE_TLS_CA", ""), "TLS CA bundle path (or CASTLE_TLS_CA)")
	tlsClientCA := flag.String("tls-client-ca", envOrDefault("CASTLE_TLS_CLIENT_CA", ""), "mTLS client CA path (or CASTLE_TLS_CLIENT_CA)")
	bootstrapToken := flag.String("bootstrap-token", envOrDefault("OVERLORD_CASTLE_BOOTSTRAP_TOKEN", ""), "bootstrap token for Register (or OVERLORD_CASTLE_BOOTSTRAP_TOKEN); empty = Register disabled")
	// Orchestrator identity provisioning (CRI-133). The token is supplied via
	// flag or env and persisted as a SHA-256 hash; it is never logged. An
	// empty value REVOKES the operator identity: the previously provisioned
	// row is deleted, so its accept token stops authenticating immediately.
	orchestratorToken := flag.String("orchestrator-token", envOrDefault("CASTLE_ORCHESTRATOR_TOKEN", ""), "accept token for the orchestrator operator identity (or CASTLE_ORCHESTRATOR_TOKEN); empty revokes the previously provisioned operator identity (disables orchestrator auth)")
	// Trigger identity provisioning (KB-320). Least-privilege board adapter:
	// submits tagged board work into the assignment queue and mirrors the
	// resulting runs back to board columns. The token is supplied via flag or
	// env and persisted as a SHA-256 hash; it is never logged. An empty value
	// REVOKES the trigger identity, disabling board-trigger auth.
	triggerToken := flag.String("trigger-token", envOrDefault("CASTLE_TRIGGER_TOKEN", ""), "accept token for the kanboard trigger identity (or CASTLE_TRIGGER_TOKEN); empty revokes the previously provisioned trigger identity (disables board-trigger auth)")
	operatorToken := flag.String("operator-token", envOrDefault("CASTLE_OPERATOR_TOKEN", ""), "accept token for the admin operator identity (or CASTLE_OPERATOR_TOKEN); empty revokes the previously provisioned admin operator token (disables operator-token admin auth)")
	devAllowAnonRegister := flag.Bool("dev-allow-anon-register", false, "dev mode: allow Register without bootstrap token (unsafe in production)")
	tlsDefault := *tlsCert != "" || *tlsKey != ""
	grpcReflection := flag.Bool("grpc-reflection", envOrDefaultBool("CASTLE_GRPC_REFLECTION", !tlsDefault), "enable gRPC reflection")
	// KB-326 (S3): the anonymous-read default flipped to false — the flip the
	// pre-split deprecation notice promised. anonReadsFlag owns the
	// resolution (explicit flag value > CASTLE_ALLOW_ANON_READS env override
	// for local dev > false). Project-scoped runs are never anonymously
	// readable regardless of this switch; the per-project
	// allow_anonymous_reads setting grants only the legacy unscoped space.
	allowAnonReads := anonReadsFlag(flag.CommandLine)
	eventBufferCapacity := flag.Int("event-buffer-capacity", envOrDefaultInt("CASTLE_EVENT_BUFFER_CAPACITY", hub.DefaultEventBufferCapacity), "in-memory events retained per run for WatchRun replay")
	agentHeartbeatInterval := flag.Duration("agent-heartbeat-interval", envOrDefaultDuration("CASTLE_AGENT_HEARTBEAT_INTERVAL", 10*time.Second), "nominal criteria agent heartbeat cadence (or CASTLE_AGENT_HEARTBEAT_INTERVAL); the run reaper multiplies this to derive staleness")
	runReapMultiplier := flag.Int("run-reap-multiplier", envOrDefaultInt("CASTLE_RUN_REAP_MULTIPLIER", 6), "reap pending/running runs whose agent heartbeat is older than this multiple of the heartbeat interval (or CASTLE_RUN_REAP_MULTIPLIER); 0 disables run reaping")
	createdNeverStartedWindow := flag.Duration("created-never-started-window", envOrDefaultDuration("CASTLE_CREATED_NEVER_STARTED_WINDOW", 10*time.Minute), "reap pending/running runs whose started_at is still unset this long after created_at (KB-233 rule 2: CreateRun that never reached RunStarted, reason created_never_started) (or CASTLE_CREATED_NEVER_STARTED_WINDOW); 0 disables created_never_started reaping")
	refireInterval := flag.Duration("refire-interval", envOrDefaultDuration("CASTLE_REFIRE_INTERVAL", 15*time.Second), "run the refire/classification policy pass this often (KB-321: failed runs requeue under policy limits, designed-park endings reclassify to awaiting_human); 0 disables refiring")
	refirePolicyFile := flag.String("refire-policy-file", envOrDefault("CASTLE_REFIRE_POLICY_FILE", ""), "policy file to upsert as refire_policies rows at boot (KB-321); rows are (project_id,route,policy) records, empty strings are the global default scope")
	flag.Parse()

	tlsEnabled := *tlsCert != "" || *tlsKey != ""
	if !flagWasSet("grpc-reflection") {
		*grpcReflection = !tlsEnabled
	}
	// KB-326 (S3): the allow-anon-reads TLS-mode recompute is retired — the
	// flag decision is CLI > CASTLE_ALLOW_ANON_READS env > false, so the env
	// override actually works when no CLI flag is passed. The
	// --grpc-reflection default keeps its TLS-mode recompute.

	log := slog.New(slog.NewJSONHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo}))
	if *eventBufferCapacity <= 0 {
		log.Error("invalid event buffer capacity", "event_buffer_capacity", *eventBufferCapacity)
		os.Exit(1)
	}
	runReapStaleness := *agentHeartbeatInterval * time.Duration(*runReapMultiplier)
	if *agentHeartbeatInterval <= 0 {
		log.Error("invalid agent heartbeat interval", "agent_heartbeat_interval", *agentHeartbeatInterval)
		os.Exit(1)
	}
	if *runReapMultiplier < 0 {
		log.Error("invalid run reap multiplier", "run_reap_multiplier", *runReapMultiplier)
		os.Exit(1)
	}
	if *createdNeverStartedWindow < 0 {
		log.Error("invalid created never-started window", "created_never_started_window", *createdNeverStartedWindow)
		os.Exit(1)
	}
	if *refireInterval < 0 {
		log.Error("invalid refire interval", "refire_interval", *refireInterval)
		os.Exit(1)
	}

	// KB-326 (S3): --allow-anon-reads defaults to false everywhere. When anon
	// reads are enabled (CLI flag or env), warn that it is a dev-only escape
	// hatch: anonymous reads reach only legacy unscoped runs and the
	// observation surfaces — project-scoped runs stay dark.
	if *allowAnonReads {
		log.Warn("anonymous reads enabled: --allow-anon-reads is a dev-only escape hatch (CASTLE_ALLOW_ANON_READS); project-scoped runs are never anonymously readable")
	}
	if *grpcReflection && !tlsEnabled {
		log.Warn("dev-mode default: --grpc-reflection=true; this default will be flipped to false in a future release")
	}

	var st store.Store
	var err error
	// Profile selection: pg requires an explicit DSN; sqlite keeps the
	// historical --db path (KB-317: local/dev stays sqlite, server opts in).
	st, err = openStoreProfile(*storeProfile, *dbPath, *pgDSN)
	if err != nil {
		log.Error("open store", "err", err)
		os.Exit(1)
	}
	defer st.Close()

	h := hub.NewWithBuffer(*eventBufferCapacity, log)
	controls := rpc.NewControlRegistry()

	criteriaRPC := rpc.NewCriteriaServer(st, h, log, controls)
	serverRPC := rpc.NewServerServer(st, h, log, controls)
	// KB-326 S3: the handler owns the anonymous-run-read grant decision; the
	// resolved flag feeds it directly instead of the old TLS-mode recompute.
	serverRPC.AllowAnonymousReads = *allowAnonReads
	orchestratorRPC := rpc.NewOrchestratorServer(st, log)

	// Refire policy bootstrap (KB-321): policy rows ARE the classification
	// configuration (the retired arc-shepherd cronjob encoded it). The
	// built-in default policy is inserted once if the table is empty; an
	// operator policy file is upserted on top verbatim, so deployment
	// configuration stays editable as data without touching code.
	if err := refireBootstrapOnce(context.Background(), st, log, *refirePolicyFile); err != nil {
		log.Error("refire policy bootstrap", "err", err)
		os.Exit(1)
	}

	// Provision or revoke the orchestrator operator identity (CRI-133).
	// Upserting is idempotent and rotates the token when the configured token
	// changes; running with an empty token deletes the identity so a retired
	// credential stops authenticating.
	if *orchestratorToken != "" {
		if err := st.UpsertOrchestrator(context.Background(), &store.Orchestrator{
			ID:        "orchestrator-operator",
			Name:      "operator",
			TokenHash: auth.HashToken(*orchestratorToken),
			CreatedAt: time.Now().UTC(),
		}); err != nil {
			log.Error("provision orchestrator identity", "err", err)
			os.Exit(1)
		}
		log.Info("orchestrator identity provisioned", "orchestrator_id", "orchestrator-operator")
	} else {
		if err := st.DeleteOrchestrator(context.Background(), "orchestrator-operator"); err != nil {
			log.Error("revoke orchestrator identity", "err", err)
			os.Exit(1)
		}
	}

	// Provision or revoke the kanboard trigger identity (KB-320). Same
	// idempotent upsert/rotate/revoke semantics as the orchestrator identity.
	if *triggerToken != "" {
		if err := st.UpsertTrigger(context.Background(), &store.Trigger{
			ID:        "trigger-kanboard",
			TokenHash: auth.HashToken(*triggerToken),
			CreatedAt: time.Now().UTC(),
		}); err != nil {
			log.Error("provision trigger identity", "err", err)
			os.Exit(1)
		}
		log.Info("trigger identity provisioned", "trigger_id", "trigger-kanboard")
	} else {
		if err := st.DeleteTrigger(context.Background(), "trigger-kanboard"); err != nil {
			log.Error("revoke trigger identity", "err", err)
			os.Exit(1)
		}
	}

	// Provision or revoke the admin operator token (KB-330). New admin
	// credentials are operator tokens: the token-only operator carries no
	// password and its token row rotates verbatim like every token
	// credential. Running with an empty token deletes the token row and the
	// operator identity so a retired credential stops authenticating.
	if *operatorToken != "" {
		now := time.Now().UTC()
		if err := st.UpsertOperator(context.Background(), &store.Operator{
			ID:          "operator-admin",
			Name:        "operator-admin",
			DisplayName: "Admin Operator",
			Attributes:  map[string]string{store.OperatorAttributeRole: store.OperatorRoleAdmin},
			CreatedAt:   now,
			UpdatedAt:   now,
		}); err != nil {
			log.Error("provision admin operator", "err", err)
			os.Exit(1)
		}
		if err := st.UpsertOperatorToken(context.Background(), &store.OperatorToken{
			ID:         "operator-admin-default",
			OperatorID: "operator-admin",
			TokenHash:  auth.HashToken(*operatorToken),
			CreatedAt:  now,
		}); err != nil {
			log.Error("provision admin operator token", "err", err)
			os.Exit(1)
		}
		log.Info("admin operator token provisioned", "operator_id", "operator-admin")
	} else {
		if err := st.DeleteOperatorToken(context.Background(), "operator-admin-default"); err != nil {
			log.Error("revoke admin operator token", "err", err)
			os.Exit(1)
		}
		if err := st.DeleteOperator(context.Background(), "operator-admin"); err != nil {
			log.Error("revoke admin operator", "err", err)
			os.Exit(1)
		}
	}

	// Provision or revoke the default console user (CRI-195). Seeding is
	// idempotent and rotates the stored bcrypt hash when the configured
	// password changes; existing console sessions are revoked on rotation.
	// The username is safe to log; the password is never logged. Both env
	// vars must be set: with any part missing, login stays disabled and any
	// previously provisioned console state is revoked (fail-closed — never an
	// open default).
	consoleUser := os.Getenv("CASTLE_CONSOLE_USER")
	consolePassword := os.Getenv("CASTLE_CONSOLE_PASSWORD")
	switch {
	case consoleUser != "" && consolePassword != "":
		if err := rpc.ProvisionConsoleUser(context.Background(), st, consoleUser, consolePassword, log); err != nil {
			log.Error("provision console user", "err", err)
			os.Exit(1)
		}
		serverRPC.EnableConsoleLogin()
		log.Info("console login enabled", "username", consoleUser)
	case consoleUser == "" && consolePassword == "":
		if err := rpc.RevokeConsoleAuth(context.Background(), st); err != nil {
			log.Error("revoke console auth", "err", err)
			os.Exit(1)
		}
	default:
		log.Warn("console login disabled: both CASTLE_CONSOLE_USER and CASTLE_CONSOLE_PASSWORD must be set",
			"castle_console_user_set", consoleUser != "",
			"castle_console_password_set", consolePassword != "")
		if err := rpc.RevokeConsoleAuth(context.Background(), st); err != nil {
			log.Error("revoke console auth", "err", err)
			os.Exit(1)
		}
	}

	interceptors := []connect.Interceptor{
		auth.NewLoggingInterceptor(log),
		auth.NewInterceptor(st, *allowAnonReads, buildInterceptorOpts(*bootstrapToken, *devAllowAnonRegister)...),
	}

	mux := http.NewServeMux()
	critPath, critHandler := criteria.NewServiceHandler(criteriaRPC, connect.WithInterceptors(interceptors...))
	serverPath, serverHandler := criteriav1connect.NewServerServiceHandler(serverRPC, connect.WithInterceptors(interceptors...))
	orchestratorPath, orchestratorHandler := criteriav1connect.NewOrchestratorServiceHandler(orchestratorRPC, connect.WithInterceptors(interceptors...))
	// Console login is castle-owned (CRI-195): served on its own
	// /castle.v1.ConsoleService/ prefix; upstream criteria does not ship it.
	consolePath, consoleHandler := castlev1connect.NewConsoleServiceHandler(serverRPC, connect.WithInterceptors(interceptors...))
	// Run archive is castle-owned (KB-308): the operator archives terminal
	// run records here before CR retention deletes them.
	runArchivePath, runArchiveHandler := castlev1connect.NewRunArchiveServiceHandler(serverRPC, connect.WithInterceptors(interceptors...))
	// Projects are castle-owned (KB-318): agents register into a project and
	// dispatch is restricted to the project's member set.
	projectPath, projectHandler := castlev1connect.NewProjectServiceHandler(serverRPC, connect.WithInterceptors(interceptors...))
	healthPath, healthHandler := grpchealth.NewHandler(grpchealth.NewStaticChecker(
		criteria.ServiceName,
		criteriav1connect.ServerServiceName,
		criteriav1connect.OrchestratorServiceName,
		castlev1connect.ConsoleServiceName,
		castlev1connect.RunArchiveServiceName,
		castlev1connect.ProjectServiceName,
	))
	mux.Handle(critPath, critHandler)
	mux.Handle(serverPath, serverHandler)
	mux.Handle(orchestratorPath, orchestratorHandler)
	mux.Handle(consolePath, consoleHandler)
	mux.Handle(runArchivePath, runArchiveHandler)
	mux.Handle(projectPath, projectHandler)
	mux.Handle(healthPath, healthHandler)

	if *grpcReflection {
		reflector := grpcreflect.NewStaticReflector(
			criteria.ServiceName,
			criteriav1connect.ServerServiceName,
			criteriav1connect.OrchestratorServiceName,
			grpchealth.HealthV1ServiceName,
		)
		rPathV1, rHandlerV1 := grpcreflect.NewHandlerV1(reflector)
		rPathV1Alpha, rHandlerV1Alpha := grpcreflect.NewHandlerV1Alpha(reflector)
		mux.Handle(rPathV1, rHandlerV1)
		mux.Handle(rPathV1Alpha, rHandlerV1Alpha)
	}

	tlsCfg, err := auth.BuildTLSConfig(*tlsCert, *tlsKey, *tlsCA, *tlsClientCA)
	if err != nil {
		log.Error("tls config", "err", err)
		os.Exit(1)
	}

	var handler http.Handler = mux
	if tlsCfg == nil {
		handler = h2c.NewHandler(mux, &http2.Server{})
	}

	srv := &http.Server{
		Addr:              *addr,
		Handler:           handler,
		ReadHeaderTimeout: 10 * time.Second,
		TLSConfig:         tlsCfg,
	}

	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()

	// Background: mark stale overseers offline.
	go func() {
		t := time.NewTicker(15 * time.Second)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				_ = st.MarkOfflineBefore(context.Background(), time.Now().Add(-30*time.Second))
			}
		}
	}()

	// Background: expire unstarted workflow assignment leases and redispatch.
	go func() {
		d := criteriaRPC.LeaseDuration()
		if d <= 0 {
			d = 5 * time.Minute
		}
		t := time.NewTicker(d / 2)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				_ = serverRPC.ExpireLeasesNow(context.Background())
			}
		}
	}()

	// Background: reap runs whose owning agent's heartbeat went stale
	// (CRI-142). Pending/running runs of dead agents are stamped failed with
	// reason "agent heartbeat lost" so ListRuns stops accumulating zombies.
	if *runReapMultiplier > 0 {
		log.Info("run reaper enabled", "agent_heartbeat_interval", *agentHeartbeatInterval, "run_reap_multiplier", *runReapMultiplier, "staleness", runReapStaleness)
		go func() {
			t := time.NewTicker(15 * time.Second)
			defer t.Stop()
			for {
				select {
				case <-ctx.Done():
					return
				case <-t.C:
					reapStaleRunsOnce(context.Background(), st, log, runReapStaleness)
				}
			}
		}()
	}

	// Background: reap never-started runs (KB-233 rule 2). Pending/running
	// runs created more than the window ago whose started_at is still unset —
	// CreateRun without a RunStarted — are stamped failed with reason
	// "created_never_started" so unstartable and orphan-minted runs die by
	// timer instead of sitting pending forever. Independent of the heartbeat
	// reaper: the verdict is derivable from the run record alone.
	if *createdNeverStartedWindow > 0 {
		log.Info("created_never_started reaper enabled", "created_never_started_window", *createdNeverStartedWindow)
		go func() {
			t := time.NewTicker(15 * time.Second)
			defer t.Stop()
			for {
				select {
				case <-ctx.Done():
					return
				case <-t.C:
					reapNeverStartedRunsOnce(context.Background(), st, log, *createdNeverStartedWindow)
				}
			}
		}()
	}

	// Background: refire/classification policy passes (KB-321). Failed runs
	// requeue successor attempts under the policy's attempt/backoff limits
	// and designed-park endings reclassify to awaiting_human (the human gate
	// holds until acknowledged); this replaces the external arc-shepherd
	// cronjob. Every tick runs through the server's dispatch loop, so minted
	// successors reach connected agents the same way as first attempts.
	if *refireInterval > 0 {
		log.Info("refire policy engine enabled", "refire_interval", *refireInterval)
		go func() {
			t := time.NewTicker(*refireInterval)
			defer t.Stop()
			for {
				select {
				case <-ctx.Done():
					return
				case <-t.C:
					serverRPC.RunRefirePass(context.Background())
				}
			}
		}()
	}

	go func() {
		log.Info("castle listening", "addr", *addr, "db", *dbPath, "tls", tlsCfg != nil, "event_buffer_capacity", *eventBufferCapacity)
		var err error
		if tlsCfg != nil {
			err = srv.ListenAndServeTLS("", "")
		} else {
			err = srv.ListenAndServe()
		}
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Error("listen", "err", err)
			cancel()
		}
	}()

	<-ctx.Done()
	log.Info("shutting down")
	shutdownCtx, sc := context.WithTimeout(context.Background(), 5*time.Second)
	defer sc()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		// A shutdown timeout leaves in-flight handlers mid-request; their
		// scope mutations are dropped (logged by the coalescer), so surface
		// why instead of ignoring it.
		log.Warn("graceful shutdown timed out or failed", "err", err)
	}
	criteriaRPC.DrainBackgroundWriters()
}

// reapStaleRunsOnce performs one CRI-142 heartbeat-staleness reaping pass:
// runs in pending/running whose owning agent's heartbeat is older than
// staleness are classified from their own record and stamped (KB-226) — a
// run whose engine parked at a human gate is reclassified to awaiting_human
// with the reclassification reason logged, a run whose agent vanished
// mid-flight is stamped failed with reason "agent heartbeat lost". Split out
// of the ticker goroutine so the now/staleBefore derivation is unit-testable.
func reapStaleRunsOnce(ctx context.Context, st store.Store, log *slog.Logger, staleness time.Duration) {
	now := time.Now().UTC()
	outcomes, err := st.ReapStaleAgentRuns(ctx, now, now.Add(-staleness))
	if err != nil {
		log.Error("run reaper", "err", err)
		return
	}
	logReapOutcomes(log, "reaped run with stale agent heartbeat", outcomes)
}

// reapNeverStartedRunsOnce performs one KB-233 rule-2 reaping pass: runs in
// pending/running whose started_at is still unset and whose created_at is
// older than window are stamped failed with reason "created_never_started",
// unless their record ends at a human gate — those park as awaiting_human
// (KB-226) with the reclassification reason logged. Split out of the ticker
// goroutine so the now/createdBefore derivation is unit-testable.
func reapNeverStartedRunsOnce(ctx context.Context, st store.Store, log *slog.Logger, window time.Duration) {
	now := time.Now().UTC()
	outcomes, err := st.ReapNeverStartedRuns(ctx, now, now.Add(-window))
	if err != nil {
		log.Error("created_never_started reaper", "err", err)
		return
	}
	logReapOutcomes(log, "reaped run created but never started", outcomes)
}

// logReapOutcomes emits one log line per reaper verdict. Reaped runs carry
// the verdict reason; parked runs (KB-226) carry the reclassification reason
// and the status they were parked to, so an operator can trace why the reaper
// left a heartbeat-stale run open instead of failing it.
func logReapOutcomes(log *slog.Logger, reapedMsg string, outcomes []store.RunReapOutcome) {
	for _, o := range outcomes {
		if o.Parked {
			log.Info("reaper skipped run parked at human gate",
				"run_id", o.RunID, "reclassified_to", o.Status, "reason", o.Reason)
			continue
		}
		log.Info(reapedMsg, "run_id", o.RunID, "reason", o.Reason)
	}
}

// refireBootstrapOnce seeds the policy rows at boot (KB-321): the built-in
// default policy is inserted when the table has no default (”, ”) row, and
// the operator policy file (when configured) is upserted verbatim on top.
// A malformed file entry aborts boot with the parse error — deployment
// configuration must be decodable, not silently ignored.
func refireBootstrapOnce(ctx context.Context, st store.Store, log *slog.Logger, policyFile string) error {
	existing, err := st.ListRefirePolicies(ctx)
	if err != nil {
		return fmt.Errorf("list refire policies: %w", err)
	}
	hasDefault := false
	for _, r := range existing {
		hasDefault = hasDefault || (r.ProjectID == "" && r.Route == "")
	}
	if !hasDefault {
		if err := st.UpsertRefirePolicy(ctx, "", "",
			refire.MustMarshalPolicy(refire.DefaultPolicyDoc()), time.Now().UTC()); err != nil {
			return fmt.Errorf("seed default refire policy: %w", err)
		}
		log.Info("seeded default refire policy")
	}
	if policyFile == "" {
		return nil
	}
	specs, err := refire.LoadPolicyFile(policyFile)
	if err != nil {
		return fmt.Errorf("load refire policy file: %w", err)
	}
	for _, spec := range specs {
		payload, err := refire.MarshalPolicy(spec.Policy)
		if err != nil {
			return fmt.Errorf("marshal refire policy %s: %w", spec.Route, err)
		}
		if err := st.UpsertRefirePolicy(ctx, spec.ProjectID, spec.Route, payload, time.Now().UTC()); err != nil {
			return fmt.Errorf("upsert refire policy %s: %w", spec.Route, err)
		}
		log.Info("upserted refire policy", "project_id", spec.ProjectID, "route", spec.Route)
	}
	return nil
}
