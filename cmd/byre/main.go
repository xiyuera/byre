// Command byre is a lease-based SSH gateway: a client authenticates with a public key and receives
// one instance for the whole session, held exclusively until the lease expires.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"sort"
	"strconv"
	"sync/atomic"
	"syscall"
	"time"

	"byre/internal/backend"
	"byre/internal/config"
	"byre/internal/lease"
	"byre/internal/metrics"
	"byre/internal/sshfront"
	"byre/internal/version"
)

func main() {
	var (
		cfgPath     = flag.String("config", "config.yaml", "path to configuration file")
		listen      = flag.String("listen", "", "override listen address (default from config)")
		checkConfig = flag.Bool("check-config", false, "validate configuration and exit")
		showVersion = flag.Bool("version", false, "print version and exit")
		// Without a stable host key every restart changes the fingerprint clients pin. Tests and lab
		// runs need that escape hatch; a deployment does not, so it is opt-in and never a config key.
		allowEphemeral = flag.Bool("allow-ephemeral-hostkey", false,
			"run without host_key (tests only: the fingerprint changes on every restart)")
		// The delay lives here, not in a preStop hook: the prebuilt image is `FROM scratch` and has no
		// shell to run `sleep` with.
		shutdownDelay = flag.Duration("shutdown-delay", 2*time.Second,
			"wait after SIGTERM before dropping sessions, so the Service stops routing here first")
	)
	flag.Parse()

	if *showVersion {
		fmt.Println(version.String())
		return
	}

	logger := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo}))
	slog.SetDefault(logger)

	cfg, err := config.Load(*cfgPath, config.AllowEphemeralHostKey(*allowEphemeral))
	if err != nil {
		logger.Error("configuration rejected", "error", err)
		os.Exit(2)
	}
	if *listen != "" {
		cfg.Listen = *listen
	}

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	backends, err := buildBackends(cfg, logger, *checkConfig)
	if err != nil {
		logger.Error("configuration rejected", "error", err)
		os.Exit(2)
	}
	identities, err := buildIdentities(cfg, backends)
	if err != nil {
		logger.Error("configuration rejected", "error", err)
		os.Exit(2)
	}
	poolSize := cfg.Lease.PoolSizeValue()
	logger.Info("configuration accepted",
		"listen", cfg.Listen,
		"host_key", cfg.HostKey,
		"backends", len(backends),
		"identities", len(identities),
		"ttl", cfg.Lease.TTL.String(),
		"grace", cfg.Lease.Grace.String(),
		"pool_size", poolSize,
		"version", version.String(),
	)
	if *checkConfig {
		// Warnings are printed and do not fail the check: an ephemeral host key is legal (ADR-0006),
		// it is just wrong for a deployment, and silence is how it reached one.
		for _, w := range cfg.Warnings() {
			fmt.Fprintf(os.Stderr, "warning: %s\n", w)
		}
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
			mgr.SetPool(name, poolSize, p)
		}
	}

	// Adoption must happen before warming: an unannotated instance found at startup is stale pool
	// capacity for drivers that can tell (see ADR-0009), while warming would immediately create more.
	// The outcome is always logged, including "nothing adopted": a restart that quietly found nothing
	// and a restart that quietly destroyed something used to look the same.
	if n, err := mgr.Adopt(ctx); err != nil {
		logger.Warn("adoption incomplete", "adopted", n, "error", err)
	} else {
		logger.Info("adoption finished", "adopted", n)
	}

	go mgr.RunReaper(ctx, 30*time.Second)
	if poolSize > 0 {
		go mgr.RunWarmer(ctx, 15*time.Second)
		// Warming is best effort: a cold cluster must not stop the gateway from accepting sessions.
		go func() {
			if err := mgr.Warm(ctx); err != nil {
				logger.Warn("initial pool warm failed", "error", err)
			}
		}()
	} else {
		logger.Info("warm pool disabled by configuration", "pool_size", 0)
	}

	front, err := sshfront.New(sshfront.Config{
		Listen:      cfg.Listen,
		HostKeyPath: cfg.HostKey,
		Identities:  identities,
		Leases:      mgr,
		Logger:      logger,
	})
	if err != nil {
		logger.Error("ssh front end", "error", err)
		os.Exit(1)
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

	// shuttingDown flips the moment SIGTERM arrives: readiness goes red before the delay, so a Service
	// that is still deciding whether this pod is an endpoint stops choosing it.
	var shuttingDown atomic.Bool

	mux := http.NewServeMux()
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		fmt.Fprintf(w, "ok %s\n", version.String())
	})
	// /healthz answers "the process is alive"; it stays 200 while the gateway cannot provision anything,
	// which is exactly the failure an operator needs to see. /readyz is the one that goes red: a gateway
	// with warm capacity configured but no way to create instances must not receive traffic.
	mux.HandleFunc("/readyz", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		if shuttingDown.Load() {
			w.WriteHeader(http.StatusServiceUnavailable)
			fmt.Fprint(w, "not ready: shutting down\n")
			return
		}
		fs := mgr.FillState()
		if !fs.CanProvision() {
			w.WriteHeader(http.StatusServiceUnavailable)
			fmt.Fprintf(w, "not ready: pool target %v, warm %v, last error %v\n", fs.Targets, fs.Warm, fs.LastErr)
			return
		}
		fmt.Fprintf(w, "ready: pool target %v, warm %v\n", fs.Targets, fs.Warm)
	})
	mux.Handle("/metrics", metricsHandler(mgr, backends, front))
	health := &http.Server{Addr: ":8099", Handler: mux, ReadHeaderTimeout: 5 * time.Second}
	go func() {
		logger.Info("health endpoint listening", "addr", health.Addr)
		if err := health.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			logger.Error("health endpoint failed", "error", err)
			stop()
		}
	}()

	<-ctx.Done()
	logger.Info("shutting down", "delay", shutdownDelay.String())
	shutdownSequence(*shutdownDelay, func() { shuttingDown.Store(true) }, func() {
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = health.Shutdown(shutdownCtx)
	}, func() { _ = front.Close() })
}

// shutdownSequence is everything that happens after the termination signal, in the order that matters:
// readiness goes red first, then the process keeps accepting for `delay` (the window in which the
// Service and the dataplane stop routing here), and only then are the health endpoint and the sessions
// dropped. Closing the listener first would cut clients that are still being routed to it; waiting
// forever is what the previous version did, and every rolling update had to be killed.
//
// It is a function so that the order — and the fact that the delay is actually waited — has a test. The
// value (2s by default) is an operational judgement: it is longer than the endpoint removal this cluster
// measures and shorter than any client's reconnect budget, so a rollout costs a reconnect, not a lease.
func shutdownSequence(delay time.Duration, notReady, stopHealth, closeSessions func()) {
	if delay < 0 {
		delay = 0
	}
	if notReady != nil {
		notReady()
	}
	time.Sleep(delay)
	if stopHealth != nil {
		stopHealth()
	}
	if closeSessions != nil {
		closeSessions()
	}
}

// metricsHandler serves the exposition. It is a named handler so that a test can read /metrics through an
// HTTP response, exactly as an operator does, instead of trusting that the text inside main() is right.
func metricsHandler(mgr *lease.Manager, backends map[string]backend.Backend, front unreachableCounter) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
		writeMetrics(w, mgr, backends, front)
	})
}

// unreachableCounter is the front end's part of the exposition.
//
// An interface rather than *sshfront.Server so the rendering can be read in a test without an SSH listener,
// a host key and a lease table — the exposition is hand-written, and a formatting mistake in it is invisible
// until someone greps it.
type unreachableCounter interface {
	UnreachableTotal() int64
}

// writeMetrics renders the Prometheus text exposition.
//
// The last three series are the ones that make a disappeared instance visible. Before them, a lease that
// pointed at a deleted pod produced no number anywhere: the gateway kept forwarding, the client saw a closed
// connection, and the only trace was a log line nobody was tailing.
func writeMetrics(w io.Writer, mgr *lease.Manager, backends map[string]backend.Backend, front unreachableCounter) {
	active, idle := mgr.Stats()
	fs := mgr.FillState()
	names := make([]string, 0, len(backends))
	for name := range backends {
		names = append(names, name)
	}
	sort.Strings(names)

	fmt.Fprint(w, "# HELP byre_up Whether the process is running.\n# TYPE byre_up gauge\nbyre_up 1\n")
	fmt.Fprintf(w, "# HELP byre_build_info Build metadata of this binary.\n# TYPE byre_build_info gauge\n"+
		"byre_build_info{version=%q,commit=%q,build_date=%q} 1\n",
		version.Version, version.Commit, version.BuildDate)

	fmt.Fprintf(w, "# HELP byre_leases_active Leases with at least one open connection.\n# TYPE byre_leases_active gauge\nbyre_leases_active %d\n", active)
	fmt.Fprintf(w, "# HELP byre_leases_idle Leases without connections, waiting to be reaped.\n# TYPE byre_leases_idle gauge\nbyre_leases_idle %d\n", idle)
	fmt.Fprintf(w, "# HELP byre_leases_adopted Leases recovered from existing instances at startup.\n# TYPE byre_leases_adopted counter\nbyre_leases_adopted %d\n", mgr.AdoptedTotal())
	fmt.Fprintf(w, "# HELP byre_instances_discarded Instances destroyed at startup because nothing could own them.\n# TYPE byre_instances_discarded counter\nbyre_instances_discarded %d\n", mgr.DiscardedTotal())
	fmt.Fprintf(w, "# HELP byre_leases_adopted_pending Adopted leases no connection has picked up yet; add it to leases_active when comparing across a restart.\n# TYPE byre_leases_adopted_pending gauge\nbyre_leases_adopted_pending %d\n", mgr.AdoptedPending())
	fmt.Fprintf(w, "# HELP byre_oldest_idle_lease_age_seconds Age of the oldest lease without a connection.\n# TYPE byre_oldest_idle_lease_age_seconds gauge\nbyre_oldest_idle_lease_age_seconds %g\n", mgr.OldestIdleAge().Seconds())
	fmt.Fprintf(w, "# HELP byre_lease_dead_instance_total Leases dropped because the instance behind them no longer exists.\n# TYPE byre_lease_dead_instance_total counter\nbyre_lease_dead_instance_total %d\n", mgr.DeadInstancesTotal())
	fmt.Fprintf(w, "# HELP byre_lease_liveness_errors_total Existence checks that could not answer; those leases were kept.\n# TYPE byre_lease_liveness_errors_total counter\nbyre_lease_liveness_errors_total %d\n", mgr.LivenessErrorsTotal())
	if front != nil {
		fmt.Fprintf(w, "# HELP byre_lease_unreachable_total Dial failures against instances that still exist.\n# TYPE byre_lease_unreachable_total counter\nbyre_lease_unreachable_total %d\n", front.UnreachableTotal())
	}

	fmt.Fprint(w, "# HELP byre_pool_target Warm instances this backend is trying to hold; 0 means no pool.\n# TYPE byre_pool_target gauge\n")
	for _, name := range names {
		fmt.Fprintf(w, "byre_pool_target{backend=%q} %d\n", name, fs.Targets[name])
	}
	fmt.Fprint(w, "# HELP byre_pool_warm Idle instances held for a backend.\n# TYPE byre_pool_warm gauge\n")
	for _, name := range names {
		fmt.Fprintf(w, "byre_pool_warm{backend=%q} %d\n", name, fs.Warm[name])
	}
	fmt.Fprint(w, "# HELP byre_pool_inflight Instance creations in progress.\n# TYPE byre_pool_inflight gauge\n")
	for _, name := range names {
		fmt.Fprintf(w, "byre_pool_inflight{backend=%q} %d\n", name, fs.Inflight[name])
	}

	fill := metrics.FillDurations()
	fmt.Fprint(w, "# HELP byre_pool_fill_seconds Duration of one pool fill, including failed creations.\n# TYPE byre_pool_fill_seconds histogram\n")
	for _, b := range fill.Buckets {
		fmt.Fprintf(w, "byre_pool_fill_seconds_bucket{le=%q} %d\n", formatFloat(b.LE), b.Count)
	}
	fmt.Fprintf(w, "byre_pool_fill_seconds_sum %g\nbyre_pool_fill_seconds_count %d\n", fill.Sum, fill.Count)

	fmt.Fprintf(w, "# HELP byre_pool_fill_errors_total Instance creations that failed during a fill.\n# TYPE byre_pool_fill_errors_total counter\nbyre_pool_fill_errors_total %d\n", metrics.PoolFillErrorsTotal())
	fmt.Fprintf(w, "# HELP byre_handshake_fail_total Readiness probes where the port answered but SSH did not.\n# TYPE byre_handshake_fail_total counter\nbyre_handshake_fail_total %d\n", metrics.HandshakeFailTotal())
	fmt.Fprintf(w, "# HELP byre_release_errors_total Release attempts that failed (one release may count several times).\n# TYPE byre_release_errors_total counter\nbyre_release_errors_total %d\n", metrics.ReleaseErrorsTotal())

	fmt.Fprint(w, "# HELP byre_acquire_total Acquisitions by backend and outcome: reuse, pool, created or error.\n# TYPE byre_acquire_total counter\n")
	for _, t := range metrics.AcquireTotals() {
		fmt.Fprintf(w, "byre_acquire_total{backend=%q,result=%q} %d\n", t.Backend, t.Result, t.Count)
	}

	if rss, ok := residentMemory(); ok {
		fmt.Fprintf(w, "# HELP byre_process_resident_memory_bytes Resident set size, from /proc/self/statm.\n# TYPE byre_process_resident_memory_bytes gauge\nbyre_process_resident_memory_bytes %d\n", rss)
	}
}

// formatFloat renders a bucket bound the way Prometheus expects: +Inf, not +Infinity.
func formatFloat(f float64) string {
	if f > 1e300 {
		return "+Inf"
	}
	return strconv.FormatFloat(f, 'g', -1, 64)
}

// residentMemory reads the RSS from /proc. The memory limit in the manifests is judged by this number and
// by nothing else, so it has to be exported rather than inferred from a container runtime.
func residentMemory() (int64, bool) {
	b, err := os.ReadFile("/proc/self/statm")
	if err != nil {
		return 0, false
	}
	var size, resident int64
	if _, err := fmt.Sscanf(string(b), "%d %d", &size, &resident); err != nil {
		return 0, false
	}
	return resident * int64(os.Getpagesize()), true
}

// buildBackends instantiates one driver per configured backend. dryRun skips cluster clients so that
// -check-config works on a machine that has no credentials; the configuration is still what is being
// checked, not the cluster.
func buildBackends(cfg *config.Config, logger *slog.Logger, dryRun bool) (map[string]backend.Backend, error) {
	out := make(map[string]backend.Backend, len(cfg.Backends))
	for _, b := range cfg.Backends {
		switch b.Provision {
		case config.ProvisionEndpoints:
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
		case config.ProvisionClaims, config.ProvisionWorkloads:
			drv, err := backend.NewKubernetes(backend.KubernetesConfig{
				Name:       b.Name,
				Namespace:  b.Namespace,
				Kubeconfig: b.Kubeconfig,
				Provider: backend.ProviderSpec{
					Kind:          b.Provision,
					Template:      b.Provider.Template,
					WarmPool:      b.Provider.WarmPool,
					Image:         b.Provider.Image,
					StartCommand:  b.Provider.StartCommand,
					ReadyTimeout:  b.Provider.ReadyTimeout.Std(),
					APIGroup:      b.Provider.APIGroup,
					APIVersion:    b.Provider.APIVersion,
					Resource:      b.Provider.Resource,
					ObjectKind:    b.Provider.ObjectKind,
					EndpointField: b.Provider.EndpointField,
					AllowRoot:     b.Provider.RunAsRoot,
				},
				SSHUser:       b.SSH.User,
				SSHPrivateKey: b.SSH.PrivateKey,
				HostKeyPolicy: b.SSH.HostKeyPolicy,
				Logger:        logger,
				DryRun:        dryRun,
			})
			if err != nil {
				return nil, err
			}
			out[b.Name] = drv
		case config.ProvisionCommand:
			drv, err := backend.NewCommand(backend.CommandConfig{
				Name:          b.Name,
				Acquire:       b.Command.Acquire,
				Release:       b.Command.Release,
				List:          b.Command.List,
				Env:           b.Command.Env,
				Timeout:       b.Command.Timeout.Std(),
				User:          b.SSH.User,
				PrivateKey:    b.SSH.PrivateKey,
				HostKeyPolicy: b.SSH.HostKeyPolicy,
				Logger:        logger,
			})
			if err != nil {
				return nil, err
			}
			out[b.Name] = drv
		default:
			return nil, fmt.Errorf("backend %q: unknown provision %q", b.Name, b.Provision)
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
