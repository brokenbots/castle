package main

import (
	"context"
	"errors"
	"flag"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/go-chi/chi/v5"

	"github.com/brokenbots/overlord/castle/internal/httpapi"
	"github.com/brokenbots/overlord/castle/internal/hub"
	"github.com/brokenbots/overlord/castle/internal/store/sqlite"
	"github.com/brokenbots/overlord/castle/internal/wsapi"
)

func envOrDefault(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

func main() {
	addr := flag.String("addr", envOrDefault("CASTLE_ADDR", ":8080"), "listen address (or CASTLE_ADDR)")
	dbPath := flag.String("db", envOrDefault("CASTLE_DB_PATH", "./castle.db"), "sqlite database path (or CASTLE_DB_PATH)")
	flag.Parse()

	log := slog.New(slog.NewJSONHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo}))

	st, err := sqlite.Open(*dbPath)
	if err != nil {
		log.Error("open db", "err", err)
		os.Exit(1)
	}
	defer st.Close()

	h := hub.New()

	ws := &wsapi.Server{Store: st, Hub: h, Log: log}
	api := &httpapi.Server{Store: st, Log: log, Control: ws}

	root := chi.NewRouter()
	root.Mount("/", api.Router())
	ws.Mount(root)

	srv := &http.Server{
		Addr:              *addr,
		Handler:           root,
		ReadHeaderTimeout: 10 * time.Second,
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
		log.Info("castle listening", "addr", *addr, "db", *dbPath)
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
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
