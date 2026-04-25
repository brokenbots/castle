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

	"github.com/brokenbots/overlord/castle/internal/auth"
	"github.com/brokenbots/overlord/castle/internal/hub"
	"github.com/brokenbots/overlord/castle/internal/rpc"
	"github.com/brokenbots/overlord/castle/internal/store/sqlite"
	"github.com/brokenbots/overlord/shared/pb/overlord/v1/overlordv1connect"
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

func main() {
	addr := flag.String("addr", envOrDefault("CASTLE_ADDR", ":8080"), "listen address (or CASTLE_ADDR)")
	dbPath := flag.String("db", envOrDefault("CASTLE_DB_PATH", "./castle.db"), "sqlite database path (or CASTLE_DB_PATH)")
	tlsCert := flag.String("tls-cert", envOrDefault("CASTLE_TLS_CERT", ""), "TLS certificate path (or CASTLE_TLS_CERT)")
	tlsKey := flag.String("tls-key", envOrDefault("CASTLE_TLS_KEY", ""), "TLS key path (or CASTLE_TLS_KEY)")
	tlsCA := flag.String("tls-ca", envOrDefault("CASTLE_TLS_CA", ""), "TLS CA bundle path (or CASTLE_TLS_CA)")
	tlsClientCA := flag.String("tls-client-ca", envOrDefault("CASTLE_TLS_CLIENT_CA", ""), "mTLS client CA path (or CASTLE_TLS_CLIENT_CA)")
	tlsDefault := *tlsCert != "" || *tlsKey != ""
	grpcReflection := flag.Bool("grpc-reflection", envOrDefaultBool("CASTLE_GRPC_REFLECTION", !tlsDefault), "enable gRPC reflection")
	allowAnonReads := flag.Bool("allow-anon-reads", envOrDefaultBool("CASTLE_ALLOW_ANON_READS", !tlsDefault), "allow anonymous CastleService read RPCs")
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

	st, err := sqlite.Open(*dbPath)
	if err != nil {
		log.Error("open db", "err", err)
		os.Exit(1)
	}
	defer st.Close()

	h := hub.NewWithBuffer(*eventBufferCapacity, log)
	controls := rpc.NewControlRegistry()

	overseerRPC := rpc.NewOverseerServer(st, h, log, controls)
	castleRPC := rpc.NewCastleServer(st, h, log, controls)

	interceptors := []connect.Interceptor{
		auth.NewLoggingInterceptor(log),
		auth.NewInterceptor(st, *allowAnonReads),
	}

	mux := http.NewServeMux()
	ovPath, ovHandler := overlordv1connect.NewOverseerServiceHandler(overseerRPC, connect.WithInterceptors(interceptors...))
	csPath, csHandler := overlordv1connect.NewCastleServiceHandler(castleRPC, connect.WithInterceptors(interceptors...))
	healthPath, healthHandler := grpchealth.NewHandler(grpchealth.NewStaticChecker(
		overlordv1connect.OverseerServiceName,
		overlordv1connect.CastleServiceName,
	))
	mux.Handle(ovPath, ovHandler)
	mux.Handle(csPath, csHandler)
	mux.Handle(healthPath, healthHandler)

	if *grpcReflection {
		reflector := grpcreflect.NewStaticReflector(
			overlordv1connect.OverseerServiceName,
			overlordv1connect.CastleServiceName,
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
