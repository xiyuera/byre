package lease

import (
	"context"
	"log/slog"
	"sync"
	"time"

	"byre/internal/backend"
	"byre/internal/metrics"
)

// Pooler is implemented by backends that can create an instance *before* an identity asks for it.
//
// The distinction matters: a static host list has no concept of warm capacity (the host exists or it
// does not), while a container/claim backend would rather pay the cold-start cost ahead of time. A
// backend that does not implement Pooler simply gets no pool — acquire stays on demand.
type Pooler interface {
	Create(ctx context.Context) (backend.Instance, error)
}

// pool is the warm capacity for one backend.
type pool struct {
	driver Pooler
	size   int

	mu   sync.Mutex
	warm []backend.Instance
	// inflight counts creations in progress, so refills do not overshoot the target size.
	inflight int

	// The outcome of the most recent fill, kept so /readyz can answer "is this gateway able to
	// provision at all" instead of "is the process alive".
	lastAttempt time.Time
	lastSuccess time.Time
	lastErr     error
}

// SetPool enables warm capacity for a backend. size == 0 disables it.
func (m *Manager) SetPool(backendName string, size int, driver Pooler) {
	if size <= 0 || driver == nil {
		return
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.pools == nil {
		m.pools = map[string]*pool{}
	}
	m.pools[backendName] = &pool{driver: driver, size: size}
}

// Warm fills every pool to its target size.
func (m *Manager) Warm(ctx context.Context) error {
	m.mu.Lock()
	pools := make([]*pool, 0, len(m.pools))
	for _, p := range m.pools {
		pools = append(pools, p)
	}
	m.mu.Unlock()

	var firstErr error
	for _, p := range pools {
		if err := p.fill(ctx, m.Logger()); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	return firstErr
}

// RunWarmer keeps pools full until ctx is done.
//
// A failed fill is logged every tick, not once at startup: a pool that cannot provision is the failure
// that used to be invisible (process healthy, /healthz 200, pool empty), and one line at boot followed
// by 48 minutes of silence reads exactly like a working gateway.
func (m *Manager) RunWarmer(ctx context.Context, interval time.Duration) {
	if interval <= 0 {
		interval = 15 * time.Second
	}
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			if err := m.Warm(ctx); err != nil {
				log := m.Logger()
				if log != nil {
					log.Warn("pool warm failed", "error", err,
						"note", "warm capacity is short; every acquire pays the cold start")
				}
			}
		}
	}
}

// refill asks the pool for one more instance after a checkout.
func (m *Manager) refill(backendName string) {
	m.mu.Lock()
	p := m.pools[backendName]
	m.mu.Unlock()
	if p == nil {
		return
	}
	go func() {
		if err := p.fill(context.Background(), m.Logger()); err != nil {
			if log := m.Logger(); log != nil {
				log.Warn("pool refill failed", "backend", backendName, "error", err)
			}
		}
	}()
}

func (p *pool) take() (backend.Instance, bool) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if len(p.warm) == 0 {
		return backend.Instance{}, false
	}
	in := p.warm[len(p.warm)-1]
	p.warm = p.warm[:len(p.warm)-1]
	return in, true
}

// fill creates instances until warm+inflight reaches the target size.
func (p *pool) fill(ctx context.Context, log *slog.Logger) error {
	p.mu.Lock()
	need := p.size - len(p.warm) - p.inflight
	if need <= 0 {
		// Nothing to do — and never subtract: a negative need would permanently shrink the counter.
		p.mu.Unlock()
		return nil
	}
	p.inflight += need
	p.mu.Unlock()

	start := time.Now()
	var created []backend.Instance
	var firstErr error
	var failures int64
	for i := 0; i < need; i++ {
		in, err := p.driver.Create(ctx)
		if err != nil {
			failures++
			if firstErr == nil {
				firstErr = err
			}
			continue
		}
		created = append(created, in)
	}
	elapsed := time.Since(start)
	metrics.ObserveFill(elapsed)
	if failures > 0 {
		metrics.IncPoolFillErrors(failures)
	}

	p.mu.Lock()
	p.inflight -= need
	p.warm = append(p.warm, created...)
	size := p.size
	warm := len(p.warm)
	p.lastAttempt = time.Now()
	if len(created) > 0 {
		p.lastSuccess = p.lastAttempt
	}
	p.lastErr = firstErr
	p.mu.Unlock()

	if log != nil {
		log.Debug("pool filled", "created", len(created), "failed", failures,
			"warm", warm, "size", size, "elapsed_ms", elapsed.Milliseconds())
	}
	return firstErr
}

// WarmSize reports the number of idle instances held for a backend (tests and metrics).
func (m *Manager) WarmSize(backendName string) int {
	m.mu.Lock()
	p := m.pools[backendName]
	m.mu.Unlock()
	if p == nil {
		return 0
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	return len(p.warm)
}

// Logger returns the manager's logger; nil until SetLogger is called.
func (m *Manager) Logger() *slog.Logger {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.logger
}

// FillState is the aggregate view of all pools: what they are trying to hold, what they hold, and
// whether the last attempt could create anything.
type FillState struct {
	Targets     map[string]int
	Warm        map[string]int
	Inflight    map[string]int
	LastAttempt time.Time
	LastSuccess time.Time
	LastErr     error
}

// CanProvision answers the readiness question: a gateway whose pool target is zero needs nothing, one
// that holds warm capacity can serve, and one whose most recent fill produced at least one instance is
// able to provision even if the target is temporarily short.
//
// It deliberately does not ask "is the pool full": a pool one instance short is a latency problem, while
// a pool that cannot create anything is an outage, and readiness must not page for the first.
func (fs FillState) CanProvision() bool {
	total := 0
	for _, t := range fs.Targets {
		total += t
	}
	if total == 0 {
		return true
	}
	for _, w := range fs.Warm {
		if w > 0 {
			return true
		}
	}
	return !fs.LastSuccess.IsZero() && !fs.LastSuccess.Before(fs.LastAttempt)
}

// FillState snapshots every pool.
func (m *Manager) FillState() FillState {
	m.mu.Lock()
	pools := make(map[string]*pool, len(m.pools))
	for name, p := range m.pools {
		pools[name] = p
	}
	m.mu.Unlock()

	fs := FillState{
		Targets:  make(map[string]int, len(pools)),
		Warm:     make(map[string]int, len(pools)),
		Inflight: make(map[string]int, len(pools)),
	}
	for name, p := range pools {
		p.mu.Lock()
		fs.Targets[name] = p.size
		fs.Warm[name] = len(p.warm)
		fs.Inflight[name] = p.inflight
		if p.lastAttempt.After(fs.LastAttempt) {
			fs.LastAttempt = p.lastAttempt
			fs.LastSuccess = p.lastSuccess
			fs.LastErr = p.lastErr
		}
		p.mu.Unlock()
	}
	return fs
}
