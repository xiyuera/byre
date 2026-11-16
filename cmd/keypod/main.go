// Command keypod is a lease-based SSH gateway: a client authenticates with a public key and receives
// one instance for the whole session, held exclusively until the lease expires.
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

	"keypod/internal/backend"
	"keypod/internal/config"
	"keypod/internal/lease"
	"keypod/internal/sshfront"
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

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	backends, err := buildBackends(cfg, logger)
	if err != nil {
		logger.Error("configuration rejected", "error", err)
		os.Exit(2)
	}
	identities, err := buildIdentities(cfg, backends)
	if err != nil {
		logger.Error("configuration rejected", "error", err)
		os.Exit(2)
	}
	logger.Info("configuration accepted",
		"listen", cfg.Listen,
		"backends", len(backends),
		"identities", len(identities),
		"ttl", cfg.Lease.TTL.String(),
		"grace", cfg.Lease.Grace.String(),
		"pool_size", cfg.Lease.PoolSize,
		"version", version.String(),
	)
	if *checkConfig {
		return
	}

	mgr, err := lease.New(cfg.Lease.TTL.Std(), cfg.Lease.Grace.Std(), backends)
	if err != nil {
		logger.Error("lease manager", "error", err)
		os.Exit(1)
	}

	mgr.SetLogger(logger)
	for name, drv := range backends {
		if p, ok := drv.(lease.Pooler); ok {
			mgr.SetPool(name, cfg.Lease.PoolSize, p)
		}
	}

	go mgr.RunReaper(ctx, 30*time.Second)
	if cfg.Lease.PoolSize > 0 {
		go mgr.RunWarmer(ctx, 15*time.Second)
		// Warming is best effort: a cold cluster must not stop the gateway from accepting sessions.
		go func() {
			if err := mgr.Warm(ctx); err != nil {
				logger.Warn("initial pool warm failed", "error", err)
			}
		}()
	}

	front, err := sshfront.New(sshfront.Config{
		Listen:     cfg.Listen,
		Identities: identities,
		Leases:     mgr,
		Logger:     logger,
	})
	if err != nil {
		logger.Error("ssh front end", "error", err)
		os.Exit(1)
	}
	if n, err := mgr.Adopt(ctx); err != nil {
		logger.Warn("adoption incomplete", "adopted", n, "error", err)
	} else if n > 0 {
		logger.Info("adopted existing leases", "count", n)
	}

	if err := front.Listen(); err != nil {
		logger.Error("ssh front end", "error", err)
		os.Exit(1)
	}
	go func() {
		if err := front.Serve(ctx); err != nil {
			logger.Error("ssh front end stopped", "error", err)
			stop()
		}
	}()

	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		fmt.Fprintf(w, "ok %s\n", version.String())
	})
	mux.HandleFunc("/metrics", func(w http.ResponseWriter, r *http.Request) {
		active, idle := mgr.Stats()
		w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
		fmt.Fprintf(w, "# HELP keypod_up Whether the process is running.\n# TYPE keypod_up gauge\nkeypod_up 1\n")
		fmt.Fprintf(w, "# HELP keypod_leases_active Leases with at least one open connection.\n# TYPE keypod_leases_active gauge\nkeypod_leases_active %d\n", active)
		fmt.Fprintf(w, "# HELP keypod_leases_idle Leases without connections, waiting to be reaped.\n# TYPE keypod_leases_idle gauge\nkeypod_leases_idle %d\n", idle)
		fmt.Fprintf(w, "# HELP keypod_leases_adopted Leases recovered from existing instances at startup.\n# TYPE keypod_leases_adopted counter\nkeypod_leases_adopted %d\n", mgr.AdoptedTotal())
		for name := range backends {
			fmt.Fprintf(w, "keypod_pool_warm{backend=%q} %d\n", name, mgr.WarmSize(name))
		}
	})
	health := &http.Server{Addr: ":8099", Handler: mux, ReadHeaderTimeout: 5 * time.Second}
	go func() {
		logger.Info("health endpoint listening", "addr", health.Addr)
		if err := health.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			logger.Error("health endpoint failed", "error", err)
			stop()
		}
	}()

	<-ctx.Done()
	logger.Info("shutting down")
	shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	_ = health.Shutdown(shutdownCtx)
	_ = front.Close()
}

// buildBackends instantiates one driver per configured backend.
func buildBackends(cfg *config.Config, logger *slog.Logger) (map[string]backend.Backend, error) {
	out := make(map[string]backend.Backend, len(cfg.Backends))
	for _, b := range cfg.Backends {
		switch b.Type {
		case config.TypeStatic:
			drv, err := backend.NewStatic(backend.StaticConfig{
				Name:          b.Name,
				Endpoints:     b.Endpoints,
				User:          b.SSH.User,
				PrivateKey:    b.SSH.PrivateKey,
				HostKeyPolicy: b.SSH.HostKeyPolicy,
			})
			if err != nil {
				return nil, err
			}
			out[b.Name] = drv
		case config.TypeKubernetes:
			drv, err := backend.NewKubernetes(backend.KubernetesConfig{
				Name:       b.Name,
				Namespace:  b.Namespace,
				Kubeconfig: b.Kubeconfig,
				Provider: backend.ProviderSpec{
					Kind:          b.Provider.Kind,
					Template:      b.Provider.Template,
					WarmPool:      b.Provider.WarmPool,
					Image:         b.Provider.Image,
					StartCommand:  b.Provider.StartCommand,
					ReadyTimeout:  b.Provider.ReadyTimeout.Std(),
					APIGroup:      b.Provider.APIGroup,
					APIVersion:    b.Provider.APIVersion,
					ClaimResource: b.Provider.ClaimResource,
					EndpointField: b.Provider.EndpointField,
				},
				SSHUser:       b.SSH.User,
				SSHPrivateKey: b.SSH.PrivateKey,
				HostKeyPolicy: b.SSH.HostKeyPolicy,
				Logger:        logger,
			})
			if err != nil {
				return nil, err
			}
			out[b.Name] = drv
		default:
			return nil, fmt.Errorf("backend %q: unknown type %q", b.Name, b.Type)
		}
	}
	return out, nil
}

// buildIdentities normalises fingerprints so two spellings of one key cannot become two identities,
// and checks that every identity names a driver that exists.
func buildIdentities(cfg *config.Config, backends map[string]backend.Backend) (map[string]sshfront.Identity, error) {
	out := make(map[string]sshfront.Identity, len(cfg.Identities))
	for _, id := range cfg.Identities {
		fp, err := config.NormalizeFingerprint(id.Fingerprint)
		if err != nil {
			return nil, err
		}
		if _, ok := backends[id.Backend]; !ok {
			return nil, fmt.Errorf("identity %s: unknown backend %q", fp, id.Backend)
		}
		if _, dup := out[fp]; dup {
			return nil, fmt.Errorf("identity %s: duplicate fingerprint", fp)
		}
		out[fp] = sshfront.Identity{Fingerprint: fp, Backend: id.Backend, Aliases: id.Aliases}
	}
	return out, nil
}
