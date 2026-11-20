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
	"strings"
	"sync/atomic"
	"syscall"
	"time"

	"byre/internal/backend"
	"byre/internal/config"
	"byre/internal/lease"
	"byre/internal/lease/crd"
	"byre/internal/metrics"
	"byre/internal/sshfront"
	"byre/internal/version"

	"k8s.io/client-go/dynamic"
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

	// Which identities this process serves, told unconditionally: adoption reads it whether or not a store
	// is configured, because claiming an instance that belongs to another deployment is the same mistake
	// in both modes. Without it, a process pointed at a shared namespace adopts its neighbours' leases and
	// the reaper eventually releases their instances.
	fingerprints := identityFingerprints(cfg)
	if len(fingerprints) == 0 {
		// A config file with no usable fingerprints yields an unrestricted manager, and the failure mode
		// is the one the boundary exists to prevent. The manager keeps the permissive behaviour because
		// embedders rely on it, so the process that read the config has to be the one that says so.
		logger.Warn("no identities are configured, so adoption is unrestricted: any annotated instance this process can see may be taken over",
			"action", "check the identities block -- an empty list, or entries without a fingerprint, disable the ownership boundary")
	}
	mgr.SetAllowedIdentities(fingerprints)

	// The lease table moves to a CRD when configured (ADR-0013). This has to happen before Adopt: a
	// store attached later would leave the first restart restoring from annotations, which is the
	// defect the store exists to fix.
	if cfg.Lease.Store == "crd" {
		st, err := newLeaseStore(ctx, cfg, backends)
		if err != nil {
			logger.Error("lease store", "error", err)
			os.Exit(1)
		}
		mgr.SetStore(lease.NewCRDStore(st))
		// A table in a different namespace from the instances is legitimate -- one table, instances
		// spread across namespaces -- but it is also a configuration where an operator looking in the
		// obvious namespace sees nothing. Say it once, at startup, rather than let it be discovered.
		if explicit := strings.TrimSpace(cfg.Lease.Namespace); explicit != "" {
			for name, drv := range backends {
				if k, ok := drv.(*backend.Kubernetes); ok && k.Namespace() != explicit {
					logger.Warn("lease table is in a different namespace than the backend that provisions its instances",
						"backend", name, "lease_namespace", explicit, "backend_namespace", k.Namespace())
				}
			}
		}
		names := identityNames(cfg)
		mgr.SetIdentityNames(names)
		logger.Info("lease store", "kind", "crd", "namespace", st.Namespace(), "identity_names", len(names))
	}

	// Recovery must happen before warming: an unannotated instance found at startup is stale pool
	// capacity for drivers that can tell (see ADR-0009), while warming would immediately create more.
	// The outcome is always logged, including "nothing adopted": a restart that quietly found nothing
	// and a restart that quietly destroyed something used to look the same.
	//
	// Recover rather than Adopt: with a store configured the durable table is read first, so a restart
	// hands back the instance each identity was using instead of cold-starting all of them. Without one,
	// or when the store cannot be read, Recover delegates to Adopt and behaviour is exactly as before.
	if n, err := mgr.Recover(ctx); err != nil {
		logger.Warn("recovery incomplete", "adopted", n, "error", err)
	} else {
		logger.Info("adoption finished", "adopted", n)
	}

	go mgr.RunReaper(ctx, 30*time.Second)
	go reportOrphanHomes(ctx, cfg, backends, logger, 5*time.Minute)
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
	// Serve must not be cancelled by the same signal that starts the shutdown sequence: the delay below is
	// the window in which the Service stops routing here, and a listener that closes at the first SIGTERM
	// has no window at all (measured before this fix: port refused at 1.0s of a 2s delay). The listener is
	// closed by the closeSessions step, after the delay has been waited.
	serveCtx, stopServing := context.WithCancel(context.Background())
	go func() {
		if err := front.Serve(serveCtx); err != nil {
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
	}, func() {
		stopServing()
		_ = front.Close()
	})
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
	fmt.Fprintf(w, "# HELP byre_home_claims_orphan Home claims with no configured identity behind them; reported only, never deleted.\n# TYPE byre_home_claims_orphan gauge\nbyre_home_claims_orphan %d\n", metrics.HomeClaimsOrphan())
	fmt.Fprintf(w, "# HELP byre_instances_discarded Instances destroyed because nothing could own them, at startup or by the sweep.\n# TYPE byre_instances_discarded counter\nbyre_instances_discarded %d\n", mgr.DiscardedTotal())
	fmt.Fprintf(w, `# HELP byre_instances_reclaimed Instances destroyed by the running sweep for having no identity and no lease.
# TYPE byre_instances_reclaimed counter
byre_instances_reclaimed %d
`, mgr.ReclaimedTotal())
	fmt.Fprintf(w, "# HELP byre_leases_adopted_pending Adopted leases no connection has picked up yet; add it to leases_active when comparing across a restart.\n# TYPE byre_leases_adopted_pending gauge\nbyre_leases_adopted_pending %d\n", mgr.AdoptedPending())
	// Non-zero means these leases exist only in this process's memory: the store does not have them, so
	// a second replica could hand the same identity a different instance. A gauge, not a counter,
	// because the condition heals as soon as a retry succeeds on the next reuse.
	fmt.Fprintf(w, "# HELP byre_leases_unpersisted Live leases the lease store does not know about; non-zero breaks one-instance-per-identity while more than one replica runs.\n# TYPE byre_leases_unpersisted gauge\nbyre_leases_unpersisted %d\n", mgr.UnpersistedCount())
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

// persistentHome converts the config block into the driver's own type. Nil stays nil, which is what
// keeps a backend without volumes on the warm path.
func persistentHome(v *config.Volumes) *backend.PersistentHome {
	if v == nil {
		return nil
	}
	return &backend.PersistentHome{
		MountPath:    v.MountPath,
		ClaimName:    v.ClaimName,
		StorageClass: v.StorageClass,
		Size:         v.Size,
		AccessMode:   v.AccessMode,
	}
}

// identityNamer resolves a fingerprint to the stable name its claim is derived from.
//
// It returns an error rather than a hash fallback. A hash would change with the key, so a rotation
// would silently hand the identity an empty directory — indistinguishable from a fresh home, which is
// the worst possible failure mode for something whose purpose is to keep data.
func identityNamer(cfg *config.Config, backendName string) func(string) (string, error) {
	byFingerprint := make(map[string]string, len(cfg.Identities))
	for _, id := range cfg.Identities {
		if id.Backend != backendName {
			continue
		}
		fp, err := config.NormalizeFingerprint(id.Fingerprint)
		if err != nil {
			continue
		}
		byFingerprint[fp] = id.Name
	}
	return func(fingerprint string) (string, error) {
		fp, err := config.NormalizeFingerprint(fingerprint)
		if err != nil {
			return "", err
		}
		name, ok := byFingerprint[fp]
		if !ok || name == "" {
			return "", fmt.Errorf("identity %s has no name on backend %q", fp, backendName)
		}
		return name, nil
	}
}

// newLeaseStore builds the ByreLease store, reusing the dynamic client a Kubernetes backend already
// created: reading the kubeconfig a second time would work but doubles the credential surface for no
// gain, and two clients that could disagree about which cluster they point at is not a state worth
// having.
func newLeaseStore(ctx context.Context, cfg *config.Config, backends map[string]backend.Backend) (*crd.Store, error) {
	namespace := strings.TrimSpace(cfg.Lease.Namespace)
	var borrowed dynamic.Interface
	for _, drv := range backends {
		k, ok := drv.(*backend.Kubernetes)
		if !ok {
			continue
		}
		if namespace == "" {
			namespace = k.Namespace()
		}
		// A dry-run backend has no cluster client: -check-config must work on a machine with no
		// credentials, so its client is nil by design.
		borrowed = k.Dynamic()
		break
	}

	var st *crd.Store
	switch {
	case cfg.Lease.Kubeconfig != "":
		// An explicit kubeconfig wins over borrowing. It is how the table is pointed at a cluster the
		// backends do not use, and on a machine whose backends rely on in-cluster credentials it is the
		// only way to give the table the permissions the backend's identity may not have.
		if namespace == "" {
			return nil, fmt.Errorf("lease.store is %q with lease.kubeconfig set but no namespace is known", cfg.Lease.Store)
		}
		var err error
		st, err = crd.NewFromKubeconfig(cfg.Lease.Kubeconfig, namespace)
		if err != nil {
			return nil, err
		}
	case borrowed != nil:
		st = crd.New(borrowed, namespace)
	case namespace != "":
		// No Kubernetes backend to borrow from: a `command` or `endpoints` backend can still want the
		// table in a cluster, because where instances come from and where the binding is recorded are
		// independent questions.
		var err error
		st, err = crd.NewFromKubeconfig("", namespace)
		if err != nil {
			return nil, err
		}
	default:
		return nil, fmt.Errorf("lease.store is %q but no namespace is known: set lease.namespace", cfg.Lease.Store)
	}

	// Checked at startup so a missing CRD is a sentence naming the manifest to apply, not a bare 404 on
	// somebody's first connection.
	if err := st.Ready(ctx); err != nil {
		return nil, err
	}
	return st, nil
}

// identityNames maps a fingerprint to the configured stable name, for spec.name of the lease objects.
// Identities without a name are omitted rather than given a derived one: a name that changes when the
// key rotates is worse than no name at all.
//
// The key is the fingerprint *as the SSH layer reports it*, which omits the "SHA256:" label that the
// configuration file uses. Keying this map on the config spelling would never match — the lookup would
// fail silently and every lease would be written without a name.
func identityNames(cfg *config.Config) map[string]string {
	out := make(map[string]string, len(cfg.Identities))
	for _, id := range cfg.Identities {
		if id.Name == "" {
			continue
		}
		out[strings.TrimPrefix(id.Fingerprint, "SHA256:")] = id.Name
	}
	return out
}

// identityFingerprints lists every configured identity, named or not. It feeds SetAllowedIdentities,
// which is what stops this process from adopting -- and later releasing -- instances that belong to a
// different deployment sharing the same namespace.
func identityFingerprints(cfg *config.Config) []string {
	out := make([]string, 0, len(cfg.Identities))
	for _, id := range cfg.Identities {
		if id.Fingerprint != "" {
			out = append(out, id.Fingerprint)
		}
	}
	return out
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
					Home:          persistentHome(b.Provider.Volumes),
				},
				IdentityName:   identityNamer(cfg, b.Name),
				SSHUser:        b.SSH.User,
				SSHPrivateKey:  b.SSH.PrivateKey,
				HostKeyPolicy:  b.SSH.HostKeyPolicy,
				IdentityLabels: identityLabels(cfg),
				Logger:         logger,
				DryRun:         dryRun,
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

// identityLabels turns the operator-declared per-identity labels into the lookup the drivers use. It is
// built once so the label a claim carries and the label the config names cannot drift apart, and it
// returns a copy because the driver stores what it gets.
func identityLabels(cfg *config.Config) func(string) map[string]string {
	byFingerprint := make(map[string]map[string]string, len(cfg.Identities))
	for _, id := range cfg.Identities {
		if len(id.Labels) == 0 {
			continue
		}
		fp, err := config.NormalizeFingerprint(id.Fingerprint)
		if err != nil {
			continue // Load already rejected this fingerprint; there is nothing to label.
		}
		copied := make(map[string]string, len(id.Labels))
		for k, v := range id.Labels {
			copied[k] = v
		}
		byFingerprint[fp] = copied
	}
	return func(fingerprint string) map[string]string {
		labels := byFingerprint[fingerprint]
		if len(labels) == 0 {
			return nil
		}
		out := make(map[string]string, len(labels))
		for k, v := range labels {
			out[k] = v
		}
		return out
	}
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

// reportOrphanHomes periodically counts home claims that no configured identity maps to.
//
// It reports and never deletes. A claim holds data, and this layer cannot distinguish three very
// different situations: the identity was removed, the identity is between connections, or the key was
// rotated and a new name is about to be configured. Automating the delete would turn all three into
// data loss, so the number exists to make the question askable rather than to answer it.
func reportOrphanHomes(ctx context.Context, cfg *config.Config, drvs map[string]backend.Backend, log *slog.Logger, every time.Duration) {
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			scanOrphanHomes(ctx, cfg, drvs, log)
		}
	}
}

// homeClaimLister is implemented by drivers that create claims for identities' homes.
type homeClaimLister interface {
	HomeClaimNames(context.Context) ([]string, error)
}

func scanOrphanHomes(ctx context.Context, cfg *config.Config, drvs map[string]backend.Backend, log *slog.Logger) {
	// What the configuration expects. Built from the same template the driver renders, so the two
	// cannot drift into disagreeing about a name.
	want := map[string]bool{}
	for _, b := range cfg.Backends {
		if b.Provider.Volumes == nil {
			continue
		}
		for _, id := range cfg.Identities {
			if id.Backend != b.Name || id.Name == "" {
				continue
			}
			want[strings.ReplaceAll(b.Provider.Volumes.ClaimName, "{{.Name}}", id.Name)] = true
		}
	}

	orphans := 0
	for name, drv := range drvs {
		lister, ok := drv.(homeClaimLister)
		if !ok {
			continue
		}
		claims, err := lister.HomeClaimNames(ctx)
		if err != nil {
			log.Warn("orphan claim scan failed", "backend", name, "error", err)
			continue
		}
		for _, c := range claims {
			if !want[c] {
				orphans++
				log.Info("home claim has no configured identity", "backend", name, "claim", c)
			}
		}
	}
	metrics.SetHomeClaimsOrphan(orphans)
}
