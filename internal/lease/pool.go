package lease

import (
	"context"
	"log/slog"
	"sync"
	"time"

	"keypod/internal/backend"
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
		if err := p.fill(ctx, m.logger); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	return firstErr
}

// RunWarmer keeps pools full until ctx is done.
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
			_ = m.Warm(ctx)
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
		if err := p.fill(context.Background(), m.logger); err != nil && m.logger != nil {
			m.logger.Warn("pool refill failed", "backend", backendName, "error", err)
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
	p.inflight += need
	p.mu.Unlock()
	if need <= 0 {
		return nil
	}

	var created []backend.Instance
	var firstErr error
	for i := 0; i < need; i++ {
		in, err := p.driver.Create(ctx)
		if err != nil {
			if firstErr == nil {
				firstErr = err
			}
			continue
		}
		created = append(created, in)
	}

	p.mu.Lock()
	p.inflight -= need
	p.warm = append(p.warm, created...)
	size := p.size
	warm := len(p.warm)
	p.mu.Unlock()

	if log != nil {
		log.Debug("pool filled", "created", len(created), "warm", warm, "size", size)
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
