// Package lease decides which instance an identity talks to, and for how long.
//
// The invariant everything else depends on (ADR-0002): one identity has at most one live lease, and a
// lease outlives individual TCP connections. A reconnect within the lease window must return the same
// instance, because the client is not going to re-sync what it already synced.
package lease

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	"byre/internal/backend"
	"byre/internal/metrics"
)

// defaultLivenessTimeout bounds the "does this instance still exist" read taken on the reuse path. It is
// short on purpose: an apiserver that cannot answer within this window produces an *inconclusive* probe,
// and an inconclusive probe keeps the lease. Waiting longer would turn an API blip into a slow connect
// for every client.
const defaultLivenessTimeout = 2 * time.Second

// entryState is the lifecycle of one table row. Each state says what a caller may do with the entry:
//
//	pending   an instance is being created; another connection for the same identity waits for the
//	          outcome instead of creating a second instance
//	leased    the instance is usable and can be referenced by another connection
//	draining  the backend is destroying the instance; the row is on its way out and must not be reused
//
// The distinction exists because "an instance exists" and "the table says who owns it" are two facts
// that a backend call separates in time.
type entryState uint8

const (
	statePending entryState = iota
	stateLeased
	stateDraining
)

func (s entryState) String() string {
	switch s {
	case statePending:
		return "pending"
	case stateLeased:
		return "leased"
	case stateDraining:
		return "draining"
	default:
		return "unknown"
	}
}

// Entry is one live lease.
type Entry struct {
	Identity string
	Backend  string
	Instance backend.Instance

	Created  time.Time
	LastSeen time.Time

	// refs counts open connections. A lease with refs > 0 is never reaped.
	refs int

	state entryState
	// done is closed when the entry leaves pending (it became leased, or creation failed) or leaves
	// draining (the instance is gone). Waiters block on this channel rather than on the manager mutex,
	// which is what keeps one slow backend call from stopping every other identity.
	done chan struct{}

	// adopted marks a lease rebuilt from an existing object at startup. Such a lease has refs == 0 by
	// definition (no connection has arrived yet), which is why it must be reported separately from the
	// ordinary idle count: an operator watching "active back to its old value" would otherwise compare
	// against a number that is structurally zero.
	adopted bool
}

// Refs reports the number of open connections.
func (e *Entry) Refs() int { return e.refs }

// State reports the lifecycle state of the row (draining and pending rows are not returned by Lookup).
func (e *Entry) State() string { return e.state.String() }

// Manager owns the identity → lease table.
type Manager struct {
	ttl   time.Duration
	grace time.Duration
	now   func() time.Time

	backends map[string]backend.Backend

	// livenessTimeout bounds the existence check on the reuse path; see defaultLivenessTimeout.
	livenessTimeout time.Duration

	// deadInstances counts leases dropped because their instance was gone. It is the number the incident
	// had no way to show: leases kept pointing at deleted pods and nothing in the process said so.
	deadInstances  atomic.Int64
	livenessErrors atomic.Int64

	mu         sync.Mutex
	byIdentity map[string]*Entry
	pools      map[string]*pool
	logger     *slog.Logger
	adopted    int
	discarded  int
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
		ttl:             ttl,
		grace:           grace,
		now:             time.Now,
		livenessTimeout: defaultLivenessTimeout,
		backends:        backends,
		byIdentity:      make(map[string]*Entry),
	}, nil
}

// Acquire returns a snapshot of the identity's live lease, creating one if there is none. The lease is
// marked as referenced by one connection; the caller must call ReleaseConn when that connection ends.
//
// The manager mutex is only ever held for map updates and field arithmetic. Everything that can block
// on the outside world — creating an instance, binding it to an identity, checking that it still
// exists, destroying it — happens outside the lock, so a backend that is slow for one identity does
// not delay Lookup, KeepAlive, Stats, ReleaseConn or another identity's Acquire.
//
// The snapshot is a copy on purpose: the manager keeps mutating refs/LastSeen under its own lock, and
// handing out the internal pointer would race with the reaper.
func (m *Manager) Acquire(ctx context.Context, identity, backendName string) (Entry, error) {
	for {
		m.mu.Lock()
		if e, ok := m.byIdentity[identity]; ok {
			switch e.state {
			case stateLeased:
				if e.Backend != backendName {
					// The mapping changed under a live lease: refuse rather than silently move the client.
					m.mu.Unlock()
					return Entry{}, fmt.Errorf("identity %s is already leased on backend %q, not %q", identity, e.Backend, backendName)
				}
				e.refs++
				e.LastSeen = m.now()
				snap := *e
				m.mu.Unlock()

				// A lease is worth reusing only while the object behind it still exists. This is checked
				// on every reuse, not on a timer: the window being closed is exactly one connection, and
				// a timer would reproduce the defect with a shorter delay. The probe is a backend round
				// trip and therefore runs with no lock held — a slow apiserver for one identity must not
				// stop the other identities — and it validates the snapshot's own instance ID, so a lease
				// that was replaced while the probe was in flight is never dropped by this call.
				gone, probeErr := m.instanceGone(ctx, &snap)
				if !gone {
					if probeErr != nil {
						// The backend could not answer. Keeping the lease is the conservative choice:
						// dropping it would destroy an instance that is probably alive, and its files
						// with it.
						m.livenessErrors.Add(1)
						if log := m.log(); log != nil {
							log.Warn("instance liveness unknown; keeping the lease",
								"identity", shortIdentity(identity), "backend", snap.Backend,
								"instance", snap.Instance.ID, "error", probeErr,
								"action", "lease kept; an inconclusive probe is not a deletion")
						}
					}
					metrics.ObserveAcquire(backendName, "reuse")
					return snap, nil
				}

				// The object is gone. Drop the row — only if the table still points at this very
				// instance — and loop: this connection provisions the replacement the client needs,
				// instead of being handed an error it cannot act on.
				m.dropStale(identity, snap.Instance.ID, "instance no longer exists", probeErr)
				continue
			case statePending, stateDraining:
				// Creation or teardown for this identity is already in flight. Wait for it and re-read
				// the table: we either join the new lease or create one ourselves.
				wait := e.done
				m.mu.Unlock()
				select {
				case <-wait:
					continue
				case <-ctx.Done():
					return Entry{}, fmt.Errorf("wait for identity %s: %w", identity, ctx.Err())
				}
			}
		}

		drv, ok := m.backends[backendName]
		if !ok {
			m.mu.Unlock()
			return Entry{}, fmt.Errorf("unknown backend %q", backendName)
		}

		// Claim the identity before talking to the backend. The placeholder is what a second connection
		// for this identity waits on, and it is the only thing that keeps two goroutines from creating
		// two instances for one identity — the lease exists to prevent exactly that.
		pending := &Entry{
			Identity: identity,
			Backend:  backendName,
			state:    statePending,
			done:     make(chan struct{}),
		}
		m.byIdentity[identity] = pending
		m.mu.Unlock()

		in, fromPool, err := m.provision(ctx, drv, backendName, identity)

		m.mu.Lock()
		if err != nil {
			if cur, ok := m.byIdentity[identity]; ok && cur == pending {
				delete(m.byIdentity, identity)
			}
			close(pending.done)
			m.mu.Unlock()
			return Entry{}, err
		}
		now := m.now()
		pending.Instance = in
		pending.Created = now
		pending.LastSeen = now
		pending.refs = 1
		pending.state = stateLeased
		close(pending.done)
		snap := *pending
		m.mu.Unlock()

		if fromPool {
			// Refill after the checkout, never during it.
			go m.refill(backendName)
		}
		return snap, nil
	}
}

// provision obtains an instance for identity: warm capacity first, then the driver, then the binding
// that makes a restart survivable. It is the whole of Acquire that talks to a backend and therefore
// runs with no manager lock held.
func (m *Manager) provision(ctx context.Context, drv backend.Backend, backendName, identity string) (backend.Instance, bool, error) {
	var in backend.Instance
	var fromPool bool
	if p := m.pool(backendName); p != nil {
		in, fromPool = p.take()
	}
	if !fromPool {
		var err error
		// Warm capacity first: an instance that already exists is the difference between a 300ms and a
		// 30s first connection. The pool is refilled after the checkout, never during it.
		in, err = drv.Acquire(ctx, identity)
		if err != nil {
			metrics.ObserveAcquire(backendName, "error")
			return backend.Instance{}, false, fmt.Errorf("acquire on backend %q: %w", backendName, err)
		}
	}
	if err := in.Validate(); err != nil {
		// The driver created something it cannot describe. Hand it back before returning: an instance
		// that no lease references and no driver tracks is an orphan only `kubectl get` can find.
		_ = drv.Release(ctx, in)
		metrics.ObserveAcquire(backendName, "error")
		return backend.Instance{}, false, fmt.Errorf("backend %q returned an unusable instance: %w", backendName, err)
	}

	// A pooled instance exists before the identity does, so the binding has to be written now. Without
	// it a restart cannot distinguish a live lease from stale warm capacity.
	if fromPool {
		if binder, ok := drv.(backend.Binder); ok {
			if err := binder.Bind(ctx, in, identity); err != nil {
				_ = drv.Release(ctx, in)
				metrics.ObserveAcquire(backendName, "error")
				return backend.Instance{}, false, fmt.Errorf("bind pooled instance on %q: %w", backendName, err)
			}
		}
		metrics.ObserveAcquire(backendName, "pool")
	} else {
		metrics.ObserveAcquire(backendName, "created")
	}
	return in, fromPool, nil
}

// instanceGone asks the backend whether the entry's instance still exists.
//
// Call it with no manager lock held: it is a bounded GET, not a map read, and a backend that hangs for
// one identity must not stall the table. The bound is livenessTimeout — past it the answer is
// "unknown", not "gone". Unknown keeps the lease, because destroying an instance that is probably
// alive is worse than one slow reconnect.
func (m *Manager) instanceGone(ctx context.Context, e *Entry) (bool, error) {
	drv := m.driver(e.Backend)
	if drv == nil {
		return false, nil
	}
	lv, ok := drv.(backend.Liveness)
	if !ok {
		// A driver with no object to ask (a static host list, a provisioning script) has no notion of
		// "gone" that the gateway can check, so the lease behaves as it did before.
		return false, nil
	}
	pctx, cancel := context.WithTimeout(ctx, m.livenessTimeout)
	defer cancel()
	err := lv.Alive(pctx, e.Instance)
	switch {
	case err == nil:
		return false, nil
	case errors.Is(err, backend.ErrInstanceGone):
		return true, err
	default:
		return false, err
	}
}

// dropStale forgets a lease whose instance no longer exists, and says so once.
//
// The instance is matched by ID before the row is removed, so a probe that finishes after the lease was
// replaced cannot delete the healthy row that took its place. The instance is not released: there is
// nothing left to release, and issuing a delete for an object that is already gone would turn one
// NotFound into a retry loop. The next Acquire provisions a replacement, which is what the client
// actually needs.
func (m *Manager) dropStale(identity, instanceID, reason string, cause error) bool {
	m.mu.Lock()
	e, ok := m.byIdentity[identity]
	if !ok || e.state != stateLeased || e.Instance.ID != instanceID {
		m.mu.Unlock()
		return false
	}
	delete(m.byIdentity, identity)
	m.deadInstances.Add(1)
	backendName, loggedID := e.Backend, e.Instance.ID
	logger := m.logger
	m.mu.Unlock()

	if logger != nil {
		logger.Warn("stale-binding",
			"identity", shortIdentity(identity), "backend", backendName, "instance", loggedID,
			"reason", reason, "cause", cause,
			"action", "lease dropped; the next connection provisions a replacement")
	}
	return true
}

// DiscardIfGone drops the lease when the backend confirms that its instance no longer exists, and
// reports whether it did.
//
// It exists for the dial path. A dial error on its own does not say what happened: the object may be gone
// (a deleted pod, whose address nothing answers on any more) or present but not answering (a wedged sshd,
// a refused port). Only the first justifies replacing the instance; the second must keep the lease,
// because replacing it would discard state the client is still entitled to.
//
// The instance is matched by ID before probing, so a connection that was holding an older object cannot
// discard a lease that has already been replaced under it.
func (m *Manager) DiscardIfGone(ctx context.Context, identity string, in backend.Instance) (bool, error) {
	m.mu.Lock()
	e, ok := m.byIdentity[identity]
	if !ok || e.state != stateLeased || e.Instance.ID != in.ID {
		m.mu.Unlock()
		return false, nil
	}
	snap := *e
	m.mu.Unlock()

	gone, probeErr := m.instanceGone(ctx, &snap)
	if !gone {
		return false, probeErr
	}
	return m.dropStale(identity, in.ID, "instance no longer exists (dial failed)", probeErr), probeErr
}

// shortIdentity truncates a fingerprint for logs: enough to correlate, not enough to reconstruct.
func shortIdentity(id string) string {
	if len(id) <= 16 {
		return id
	}
	return id[:16]
}

// DeadInstancesTotal reports leases dropped because the instance behind them disappeared.
func (m *Manager) DeadInstancesTotal() int64 { return m.deadInstances.Load() }

// LivenessErrorsTotal reports existence checks that could not answer; those leases were kept.
func (m *Manager) LivenessErrorsTotal() int64 { return m.livenessErrors.Load() }

// ReleaseConn records that one connection ended. The lease survives; only the idle clock moves.
func (m *Manager) ReleaseConn(identity string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	e, ok := m.byIdentity[identity]
	if !ok || e.state != stateLeased {
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
	if e, ok := m.byIdentity[identity]; ok && e.state == stateLeased {
		e.LastSeen = m.now()
	}
}

// Lookup returns a copy of the identity's live lease, or nil.
//
// Rows that are still being created or are already on their way out are not leases: handing one out
// would mean handing out an instance that is either not described yet or about to be destroyed.
func (m *Manager) Lookup(identity string) *Entry {
	m.mu.Lock()
	defer m.mu.Unlock()
	e, ok := m.byIdentity[identity]
	if !ok || e.state != stateLeased {
		return nil
	}
	cp := *e
	return &cp
}

// Invalidate drops the lease for identity because the instance behind it turned out to be unusable.
// It is what a failed dial calls: without it a dead instance is handed to every reconnect until
// ttl+grace expires, even though healthy capacity may be idle.
//
// instanceID narrows the call to one instance, so a caller holding a stale snapshot cannot evict the
// healthy lease that replaced it. Passing "" invalidates whatever the identity holds.
//
// It reports whether a lease was dropped.
func (m *Manager) Invalidate(ctx context.Context, identity, instanceID string) bool {
	m.mu.Lock()
	e, ok := m.byIdentity[identity]
	if !ok || e.state != stateLeased || (instanceID != "" && e.Instance.ID != instanceID) {
		m.mu.Unlock()
		return false
	}
	e.state = stateDraining
	e.done = make(chan struct{})
	m.mu.Unlock()

	_ = m.release(ctx, e, "invalidated")
	return true
}

// Reap releases leases idle for longer than ttl+grace and returns how many were reaped.
// Instances with open connections are never reaped.
//
// A release that keeps failing is reported and then *not* followed by a refill. The old object still
// exists and is no longer tracked, so creating a replacement would turn one leaked instance into a leak
// that grows every reap interval. The lease itself is dropped either way — its client is gone — and the
// failure surfaces as a log line plus byre_release_errors_total, which is the whole point: the previous
// version returned the error to a caller that discarded it and refilled anyway.
func (m *Manager) Reap(ctx context.Context) (int, error) {
	m.mu.Lock()
	now := m.now()
	horizon := m.ttl + m.grace
	var draining []*Entry
	for _, e := range m.byIdentity {
		if e.state != stateLeased || e.refs > 0 {
			continue
		}
		if now.Sub(e.LastSeen) >= horizon {
			// Mark, do not delete: between the marker and the backend's answer the instance still
			// exists, and a reconnect in that window must not be handed it (ADR-0008).
			e.state = stateDraining
			e.done = make(chan struct{})
			draining = append(draining, e)
		}
	}
	m.mu.Unlock()

	var firstErr error
	for _, e := range draining {
		if err := m.release(ctx, e, "expired"); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	return len(draining), firstErr
}

// release destroys the instance behind a draining entry and then removes the row. The backend call
// happens with no lock held; the row is removed only if the table still points at this very entry, so
// a release can never delete a lease that was created while it was in flight.
//
// A failed release is reported and then *not* followed by a refill: the old object still exists and is
// no longer tracked, and creating a replacement would turn one leaked instance into a leak that grows
// every reap interval.
func (m *Manager) release(ctx context.Context, e *Entry, reason string) error {
	var releaseErr error
	if drv := m.driver(e.Backend); drv != nil {
		releaseErr = m.releaseWithRetry(ctx, drv, e)
	}

	m.mu.Lock()
	if cur, ok := m.byIdentity[e.Identity]; ok && cur == e {
		delete(m.byIdentity, e.Identity)
	}
	close(e.done)
	logger := m.logger
	m.mu.Unlock()

	if logger != nil {
		if releaseErr != nil {
			logger.Warn("lease release failed; instance is no longer tracked",
				"identity", shortIdentity(e.Identity), "backend", e.Backend,
				"instance", e.Instance.ID, "reason", reason, "error", releaseErr,
				"action", "inspect the backend for an orphan; no replacement was created")
		} else {
			logger.Info("lease released",
				"identity", shortIdentity(e.Identity),
				"backend", e.Backend,
				"instance", e.Instance.ID,
				"reason", reason,
			)
		}
	}
	if releaseErr == nil {
		m.refill(e.Backend)
		return nil
	}
	return fmt.Errorf("release %s on %q: %w", e.Instance.ID, e.Backend, releaseErr)
}

// releaseAttempts bounds the retries of one release. A release is a delete; the failures worth retrying
// are a slow API server or a transient conflict, not a permanent rejection.
const releaseAttempts = 3

func (m *Manager) releaseWithRetry(ctx context.Context, drv backend.Backend, e *Entry) error {
	var err error
	for attempt := 1; attempt <= releaseAttempts; attempt++ {
		if err = drv.Release(ctx, e.Instance); err == nil {
			return nil
		}
		metrics.IncReleaseError()
		if attempt == releaseAttempts {
			break
		}
		select {
		case <-ctx.Done():
			return fmt.Errorf("release %s on %q: %w", e.Instance.ID, e.Backend, err)
		case <-time.After(time.Duration(attempt) * 250 * time.Millisecond):
		}
	}
	return fmt.Errorf("release %s on %q failed %d times: %w", e.Instance.ID, e.Backend, releaseAttempts, err)
}

func (m *Manager) driver(name string) backend.Backend {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.backends[name]
}

func (m *Manager) pool(name string) *pool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.pools[name]
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

		type discard struct {
			in     backend.Instance
			reason string
		}
		var doomed []discard
		var kept []backend.Instance
		now := m.now()

		// Decide under the lock, act outside it: Release is a backend call, and holding the table lock
		// across backend calls is what ADR-0008 forbids.
		m.mu.Lock()
		for _, in := range ins {
			id := in.Annotations[backend.LabelIdentity]
			if id == "" {
				if anonymousIsStale {
					// Warm capacity left over from a previous process.
					doomed = append(doomed, discard{in, "no identity annotation (stale warm capacity)"})
				} else {
					kept = append(kept, in)
				}
				continue
			}
			if verr := in.Validate(); verr != nil {
				doomed = append(doomed, discard{in, "unusable instance: " + verr.Error()})
				continue
			}
			if _, exists := m.byIdentity[id]; exists {
				doomed = append(doomed, discard{in, "duplicate identity annotation " + shortIdentity(id)})
				continue
			}
			m.byIdentity[id] = &Entry{
				Identity: id,
				Backend:  name,
				Instance: in,
				Created:  now,
				LastSeen: now,
				state:    stateLeased,
				adopted:  true,
			}
			m.adopted++
			adopted++
		}
		m.mu.Unlock()

		for _, d := range doomed {
			if err := drv.Release(ctx, d.in); err != nil {
				if firstErr == nil {
					firstErr = fmt.Errorf("release %s on %q: %w", d.in.ID, name, err)
				}
			}
			m.mu.Lock()
			m.discarded++
			logger := m.logger
			m.mu.Unlock()
			if logger != nil {
				logger.Warn("instance discarded at adoption",
					"backend", name, "instance", d.in.ID, "reason", d.reason)
			}
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

// DiscardedTotal reports how many instances adoption destroyed because nothing could own them
// (no identity annotation, a duplicate identity, or a description that could not be dialled).
func (m *Manager) DiscardedTotal() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.discarded
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

// Stats reports the number of active (refs > 0) and idle leases. Rows that are being created or
// destroyed are neither: they are transitions, not leases.
func (m *Manager) Stats() (active, idle int) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, e := range m.byIdentity {
		if e.state != stateLeased {
			continue
		}
		if e.refs > 0 {
			active++
		} else {
			idle++
		}
	}
	return active, idle
}

// AdoptedPending reports adopted leases that no connection has picked up yet.
//
// An adopted entry starts with refs == 0, so it is "idle" by definition even though its client is still
// entitled to it. Reporting it separately makes the documented post-restart check honest: the number to
// compare with "before the restart" is active + adopted_pending, not active alone.
func (m *Manager) AdoptedPending() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	n := 0
	for _, e := range m.byIdentity {
		if e.adopted && e.refs == 0 {
			n++
		}
	}
	return n
}

// OldestIdleAge reports how long the oldest lease has been without a connection; zero when none is idle.
// It is the leading indicator for "leases stop being reaped" — the reaper runs on a fixed interval, so a
// value that keeps growing past ttl+grace means the reaper is not finishing its work.
func (m *Manager) OldestIdleAge() time.Duration {
	m.mu.Lock()
	defer m.mu.Unlock()
	now := m.now()
	var oldest time.Duration
	for _, e := range m.byIdentity {
		if e.refs > 0 {
			continue
		}
		if age := now.Sub(e.LastSeen); age > oldest {
			oldest = age
		}
	}
	if oldest < 0 {
		return 0
	}
	return oldest
}

// SetClock replaces the clock; tests only.
func (m *Manager) SetClock(now func() time.Time) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.now = now
}
