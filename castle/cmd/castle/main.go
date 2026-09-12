package main

import (
	"context"
	"errors"
	"flag"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strconv"
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
	"github.com/brokenbots/castle/castle/internal/store/sqlite"
	criteria "github.com/brokenbots/criteria/sdk"
	"github.com/brokenbots/criteria/sdk/pb/criteria/v1/criteriav1connect"
)

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

func flagWasSet(name string) bool {
	set := false
	flag.Visit(func(f *flag.Flag) {
		if f.Name == name {
			set = true
		}
	})
	return set
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
	devAllowAnonRegister := flag.Bool("dev-allow-anon-register", false, "dev mode: allow Register without bootstrap token (unsafe in production)")
	tlsDefault := *tlsCert != "" || *tlsKey != ""
	grpcReflection := flag.Bool("grpc-reflection", envOrDefaultBool("CASTLE_GRPC_REFLECTION", !tlsDefault), "enable gRPC reflection")
	allowAnonReads := flag.Bool("allow-anon-reads", envOrDefaultBool("CASTLE_ALLOW_ANON_READS", !tlsDefault), "allow anonymous ServerService read RPCs")
	eventBufferCapacity := flag.Int("event-buffer-capacity", envOrDefaultInt("CASTLE_EVENT_BUFFER_CAPACITY", hub.DefaultEventBufferCapacity), "in-memory events retained per run for WatchRun replay")
	flag.Parse()

	tlsEnabled := *tlsCert != "" || *tlsKey != ""
	if !flagWasSet("grpc-reflection") {
		*grpcReflection = !tlsEnabled
	}
	if !flagWasSet("allow-anon-reads") {
		*allowAnonReads = !tlsEnabled
	}

	log := slog.New(slog.NewJSONHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo}))
	if *eventBufferCapacity <= 0 {
		log.Error("invalid event buffer capacity", "event_buffer_capacity", *eventBufferCapacity)
		os.Exit(1)
	}

	// Deprecation notice: --allow-anon-reads and --grpc-reflection default to
	// true in non-TLS (dev) mode. These defaults will be flipped to false in the
	// next overlord phase (post-split cleanup). Operators relying on these
	// defaults should set the flags explicitly before upgrading.
	if *allowAnonReads && !tlsEnabled {
		log.Warn("dev-mode default: --allow-anon-reads=true; this default will be flipped to false in a future release")
	}
	if *grpcReflection && !tlsEnabled {
		log.Warn("dev-mode default: --grpc-reflection=true; this default will be flipped to false in a future release")
	}

	st, err := sqlite.Open(*dbPath)
	if err != nil {
		log.Error("open db", "err", err)
		os.Exit(1)
	}
	defer st.Close()

	h := hub.NewWithBuffer(*eventBufferCapacity, log)
	controls := rpc.NewControlRegistry()

	criteriaRPC := rpc.NewCriteriaServer(st, h, log, controls)
	serverRPC := rpc.NewServerServer(st, h, log, controls)
	orchestratorRPC := rpc.NewOrchestratorServer(st, log)

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

	interceptors := []connect.Interceptor{
		auth.NewLoggingInterceptor(log),
		auth.NewInterceptor(st, *allowAnonReads, buildInterceptorOpts(*bootstrapToken, *devAllowAnonRegister)...),
	}

	mux := http.NewServeMux()
	critPath, critHandler := criteria.NewServiceHandler(criteriaRPC, connect.WithInterceptors(interceptors...))
	serverPath, serverHandler := criteriav1connect.NewServerServiceHandler(serverRPC, connect.WithInterceptors(interceptors...))
	orchestratorPath, orchestratorHandler := criteriav1connect.NewOrchestratorServiceHandler(orchestratorRPC, connect.WithInterceptors(interceptors...))
	healthPath, healthHandler := grpchealth.NewHandler(grpchealth.NewStaticChecker(
		criteria.ServiceName,
		criteriav1connect.ServerServiceName,
		criteriav1connect.OrchestratorServiceName,
	))
	mux.Handle(critPath, critHandler)
	mux.Handle(serverPath, serverHandler)
	mux.Handle(orchestratorPath, orchestratorHandler)
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
	_ = srv.Shutdown(shutdownCtx)
}
