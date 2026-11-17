// Package lease decides which instance an identity talks to, and for how long.
//
// The invariant everything else depends on (ADR-0002): one identity has at most one live lease, and a
// lease outlives individual TCP connections. A reconnect within the lease window must return the same
// instance, because the client is not going to re-sync what it already synced.
package lease

import (
	"context"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"byre/internal/backend"
)

// Entry is one live lease.
type Entry struct {
	Identity string
	Backend  string
	Instance backend.Instance

	Created  time.Time
	LastSeen time.Time

	// refs counts open connections. A lease with refs > 0 is never reaped.
	refs int
}

// Refs reports the number of open connections.
func (e *Entry) Refs() int { return e.refs }

// Manager owns the identity → lease table.
type Manager struct {
	ttl   time.Duration
	grace time.Duration
	now   func() time.Time

	backends map[string]backend.Backend

	mu         sync.Mutex
	byIdentity map[string]*Entry
	pools      map[string]*pool
	logger     *slog.Logger
	adopted    int
}

// SetLogger attaches a logger (optional).
func (m *Manager) SetLogger(l *slog.Logger) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.logger = l
}

// New builds a manager over the given drivers. Keys are backend names.
func New(ttl, grace time.Duration, backends map[string]backend.Backend) (*Manager, error) {
	if ttl <= 0 {
		return nil, fmt.Errorf("lease: ttl must be > 0, got %s", ttl)
	}
	if grace < 0 {
		return nil, fmt.Errorf("lease: grace must be >= 0, got %s", grace)
	}
	if len(backends) == 0 {
		return nil, fmt.Errorf("lease: at least one backend is required")
	}
	return &Manager{
		ttl:        ttl,
		grace:      grace,
		now:        time.Now,
		backends:   backends,
		byIdentity: make(map[string]*Entry),
	}, nil
}

// Acquire returns a snapshot of the identity's live lease, creating one if there is none. The lease is
// marked as referenced by one connection; the caller must call ReleaseConn when that connection ends.
//
// The snapshot is a copy on purpose: the manager keeps mutating refs/LastSeen under its own lock, and
// handing out the internal pointer would race with the reaper.
func (m *Manager) Acquire(ctx context.Context, identity, backendName string) (Entry, error) {
	m.mu.Lock()
	defer m.mu.Unlock()

	now := m.now()
	if e, ok := m.byIdentity[identity]; ok {
		if e.Backend != backendName {
			// The mapping changed under a live lease: refuse rather than silently move the client.
			return Entry{}, fmt.Errorf("identity %s is already leased on backend %q, not %q", identity, e.Backend, backendName)
		}
		e.refs++
		e.LastSeen = now
		return *e, nil
	}

	drv, ok := m.backends[backendName]
	if !ok {
		return Entry{}, fmt.Errorf("unknown backend %q", backendName)
	}

	// Warm capacity first: an instance that already exists is the difference between a 300ms and a
	// 30s first connection. The pool is refilled after the checkout, never during it.
	var in backend.Instance
	var fromPool bool
	if p := m.pools[backendName]; p != nil {
		in, fromPool = p.take()
	}
	if !fromPool {
		var err error
		in, err = drv.Acquire(ctx, identity)
		if err != nil {
			return Entry{}, fmt.Errorf("acquire on backend %q: %w", backendName, err)
		}
	}
	if err := in.Validate(); err != nil {
		return Entry{}, fmt.Errorf("backend %q returned an unusable instance: %w", backendName, err)
	}

	// A pooled instance exists before the identity does, so the binding has to be written now. Without
	// it a restart cannot distinguish a live lease from stale warm capacity.
	if fromPool {
		if binder, ok := drv.(backend.Binder); ok {
			if err := binder.Bind(ctx, in, identity); err != nil {
				_ = drv.Release(ctx, in)
				return Entry{}, fmt.Errorf("bind pooled instance on %q: %w", backendName, err)
			}
		}
	}

	e := &Entry{
		Identity: identity,
		Backend:  backendName,
		Instance: in,
		Created:  now,
		LastSeen: now,
		refs:     1,
	}
	m.byIdentity[identity] = e
	if fromPool {
		// Cannot refill while holding the table lock: the refill path takes it again.
		go m.refill(backendName)
	}
	return *e, nil
}

// ReleaseConn records that one connection ended. The lease survives; only the idle clock moves.
func (m *Manager) ReleaseConn(identity string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	e, ok := m.byIdentity[identity]
	if !ok {
		return
	}
	if e.refs > 0 {
		e.refs--
	}
	e.LastSeen = m.now()
}

// KeepAlive refreshes the idle clock for an active session.
func (m *Manager) KeepAlive(identity string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if e, ok := m.byIdentity[identity]; ok {
		e.LastSeen = m.now()
	}
}

// Lookup returns a copy of the identity's entry, or nil.
func (m *Manager) Lookup(identity string) *Entry {
	m.mu.Lock()
	defer m.mu.Unlock()
	e, ok := m.byIdentity[identity]
	if !ok {
		return nil
	}
	cp := *e
	return &cp
}

// Reap releases leases idle for longer than ttl+grace and returns how many were reaped.
// Instances with open connections are never reaped.
func (m *Manager) Reap(ctx context.Context) (int, error) {
	m.mu.Lock()
	var expired []*Entry
	now := m.now()
	horizon := m.ttl + m.grace
	for id, e := range m.byIdentity {
		if e.refs > 0 {
			continue
		}
		if now.Sub(e.LastSeen) >= horizon {
			delete(m.byIdentity, id)
			expired = append(expired, e)
		}
	}
	m.mu.Unlock()

	var firstErr error
	for _, e := range expired {
		drv := m.backends[e.Backend]
		if drv == nil {
			continue
		}
		if err := drv.Release(ctx, e.Instance); err != nil && firstErr == nil {
			firstErr = fmt.Errorf("release %s on %q: %w", e.Instance.ID, e.Backend, err)
		}
		m.refill(e.Backend)
	}
	return len(expired), firstErr
}

// Adopt rebuilds the identity table from instances that already exist, so a restarted process hands
// the same instance back to the same client instead of orphaning it and cold-starting a new one.
//
// An instance that carries LabelIdentity is adopted. An instance that carries no annotation is *not*
// automatically destroyed: whether "no identity" means "stale warm capacity" or "a lister that predates
// the annotation contract" depends on the driver, and only a driver that promises the former
// (backend.StaleCapacity) gets its anonymous instances released. Everything else is kept and reported,
// because a gateway restart that silently kills a live lease is the worst possible outcome and it is
// indistinguishable from a tidy cleanup in the log (ADR-0009).
//
// Adoption stays forgiving in the other direction too: a driver that cannot list is reported and does
// not abort the others.
func (m *Manager) Adopt(ctx context.Context) (int, error) {
	m.mu.Lock()
	drivers := make(map[string]backend.Backend, len(m.backends))
	for name, drv := range m.backends {
		drivers[name] = drv
	}
	m.mu.Unlock()

	adopted := 0
	var firstErr error
	for name, drv := range drivers {
		ins, err := drv.List(ctx)
		if err != nil {
			if firstErr == nil {
				firstErr = fmt.Errorf("list on %q: %w", name, err)
			}
			continue
		}
		if err := drv.Adopt(ctx, ins); err != nil && firstErr == nil {
			firstErr = fmt.Errorf("adopt on %q: %w", name, err)
		}

		// Only a driver that can promise "every instance I lease out carries an identity" may have its
		// anonymous instances reclaimed. The command driver cannot, so its leftovers survive a restart.
		anonymousIsStale := false
		if sc, ok := drv.(backend.StaleCapacity); ok {
			anonymousIsStale = sc.AnonymousInstancesAreStale()
		}

		now := m.now()
		var toRelease, kept []backend.Instance
		m.mu.Lock()
		for _, in := range ins {
			id := in.Annotations[backend.LabelIdentity]
			if id == "" {
				if anonymousIsStale {
					toRelease = append(toRelease, in)
				} else {
					kept = append(kept, in)
				}
				continue
			}
			if _, exists := m.byIdentity[id]; exists {
				continue
			}
			m.byIdentity[id] = &Entry{
				Identity: id,
				Backend:  name,
				Instance: in,
				Created:  now,
				LastSeen: now,
			}
			m.adopted++
			adopted++
		}
		m.mu.Unlock()

		// Release outside the table lock: it is a network call, and one slow backend must not stall every
		// session that is looking up its lease.
		for _, in := range toRelease {
			if err := drv.Release(ctx, in); err != nil {
				if firstErr == nil {
					firstErr = fmt.Errorf("release anonymous instance %s on %q: %w", in.ID, name, err)
				}
				m.log().Warn("released instance at startup", "instance_id", in.ID, "backend", name,
					"reason", "no "+backend.LabelIdentity+" annotation and the driver reports anonymous instances as stale capacity",
					"error", err)
				continue
			}
			m.log().Info("released instance at startup", "instance_id", in.ID, "backend", name,
				"reason", "no "+backend.LabelIdentity+" annotation and the driver reports anonymous instances as stale capacity")
		}
		for _, in := range kept {
			m.log().Warn("instance kept at startup", "instance_id", in.ID, "backend", name,
				"reason", "no "+backend.LabelIdentity+" annotation and the driver cannot promise that anonymous instances are stale; destroying it could kill a live lease")
		}
	}
	return adopted, firstErr
}

// log returns the manager's logger, or the default one. Call it without holding the table lock.
func (m *Manager) log() *slog.Logger {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.logger == nil {
		return slog.Default()
	}
	return m.logger
}

// AdoptedTotal reports how many leases were recovered from existing instances.
func (m *Manager) AdoptedTotal() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.adopted
}

// RunReaper reaps expired leases every interval until ctx is done.
func (m *Manager) RunReaper(ctx context.Context, interval time.Duration) {
	if interval <= 0 {
		interval = 30 * time.Second
	}
	t := time.NewTicker(interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			_, _ = m.Reap(ctx)
		}
	}
}

// Stats reports the number of active (refs > 0) and idle leases.
func (m *Manager) Stats() (active, idle int) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, e := range m.byIdentity {
		if e.refs > 0 {
			active++
		} else {
			idle++
		}
	}
	return active, idle
}

// SetClock replaces the clock; tests only.
func (m *Manager) SetClock(now func() time.Time) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.now = now
}
