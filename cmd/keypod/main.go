// Command keypod is a lease-based SSH gateway: a client authenticates with a public key and receives
// one instance for the whole session, held exclusively until the lease expires.
//
// M0 scope: configuration loading, strict validation, and the health endpoint. The SSH front end and
// the lease manager are wired in M1/M2.
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
	"syscall"
	"time"

	"keypod/internal/config"
	"keypod/internal/version"
)

func main() {
	var (
		cfgPath     = flag.String("config", "config.yaml", "path to configuration file")
		listen      = flag.String("listen", "", "override listen address (default from config)")
		checkConfig = flag.Bool("check-config", false, "validate configuration and exit")
		showVersion = flag.Bool("version", false, "print version and exit")
	)
	flag.Parse()

	if *showVersion {
		fmt.Println(version.String())
		return
	}

	logger := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo}))
	slog.SetDefault(logger)

	cfg, err := config.Load(*cfgPath)
	if err != nil {
		logger.Error("configuration rejected", "error", err)
		os.Exit(2)
	}
	if *listen != "" {
		cfg.Listen = *listen
	}
	logger.Info("configuration accepted",
		"listen", cfg.Listen,
		"backends", len(cfg.Backends),
		"identities", len(cfg.Identities),
		"ttl", cfg.Lease.TTL.String(),
		"pool_size", cfg.Lease.PoolSize,
		"version", version.String(),
	)
	if *checkConfig {
		return
	}

	// TODO(M1): start the SSH front end on cfg.Listen.
	// TODO(M2): start the lease manager and warm the pool.
	// TODO(M3): expose real metrics; /metrics below is a placeholder.

	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		fmt.Fprintf(w, "ok %s\n", version.String())
	})
	mux.HandleFunc("/metrics", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
		fmt.Fprintf(w, "# HELP keypod_up Whether the process is running.\n# TYPE keypod_up gauge\nkeypod_up 1\n")
	})

	srv := &http.Server{
		Addr:              ":8099",
		Handler:           mux,
		ReadHeaderTimeout: 5 * time.Second,
	}
	go func() {
		logger.Info("health endpoint listening", "addr", srv.Addr)
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			logger.Error("health endpoint failed", "error", err)
			os.Exit(1)
		}
	}()

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()
	<-ctx.Done()

	logger.Info("shutting down")
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		logger.Warn("health endpoint shutdown", "error", err)
	}
}
