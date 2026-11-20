package lease

import (
	"context"
	"log/slog"
	"time"

	"byre/internal/backend"
)

// WatchDeletions subscribes to every driver that can report an instance disappearing, and drops the
// matching lease as it happens instead of waiting for the idle clock (ADR-0013, part 4).
//
// It is best-effort by construction and says so: a driver without the capability is skipped, and a
// subscription that fails is logged rather than fatal. Alive() remains the backstop for the paths
// that can afford a round trip, so a deployment that never gets a watch still works -- it is just
// slower to notice.
func (m *Manager) WatchDeletions(ctx context.Context) {
	m.mu.Lock()
	drivers := make(map[string]backend.Backend, len(m.backends))
	for name, drv := range m.backends {
		drivers[name] = drv
	}
	logger := m.logger
	m.mu.Unlock()

	for name, drv := range drivers {
		if _, ok := drv.(backend.DeletionWatcher); !ok {
			continue
		}
		// One goroutine per driver, and each owns its subscription for the life of ctx.
		go m.consumeDeletions(ctx, name, drv, logger)
	}
}

// A deletion watch ending is normal, not exceptional: the apiserver closes watches on a control-plane
// restart, on etcd compaction, and on "resourceVersion too old". Re-subscribing is what keeps "a deleted
// instance stops being handed out immediately" true for a process that has been up for weeks -- which is
// exactly when the guarantee matters and exactly when a one-shot subscription has quietly stopped
// providing it.
const (
	watchRetryMin = time.Second
	watchRetryMax = 30 * time.Second
)

func (m *Manager) consumeDeletions(ctx context.Context, backendName string, drv backend.Backend, logger *slog.Logger) {
	watcher, ok := drv.(backend.DeletionWatcher)
	if !ok {
		return
	}
	backoff := watchRetryMin
	for {
		if ctx.Err() != nil {
			return
		}
		events, err := watcher.WatchDeletions(ctx)
		switch {
		case err != nil:
			if logger != nil {
				logger.Warn("cannot watch for instance deletions; leases rely on Alive() meanwhile",
					"backend", backendName, "error", err, "retry_in", backoff)
			}
		default:
			if logger != nil {
				logger.Info("watching for instance deletions", "backend", backendName)
			}
			// A successful subscription resets the backoff: the next failure is a new event, not a
			// continuation of the previous streak.
			backoff = watchRetryMin
			if !m.drainDeletions(ctx, backendName, events) {
				return
			}
			if logger != nil {
				logger.Warn("instance deletion watch ended; re-subscribing",
					"backend", backendName, "retry_in", backoff)
			}
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(backoff):
		}
		if backoff < watchRetryMax {
			backoff *= 2
		}
	}
}

// drainDeletions consumes one subscription until it ends, and reports whether ctx is still live so the
// caller can tell "the watch ended" from "we are shutting down".
func (m *Manager) drainDeletions(ctx context.Context, backendName string, ids <-chan string) bool {
	for {
		select {
		case <-ctx.Done():
			return false
		case id, ok := <-ids:
			if !ok {
				return true
			}
			m.dropByInstance(ctx, backendName, id)
		}
	}
}

// dropByInstance removes the lease held on an instance that no longer exists.
//
// The row is marked Draining before the backend call, the same mark-then-release order Reap uses: a
// connection arriving in that window waits on the row instead of being handed an address that just
// disappeared. Rows in any other state are left alone -- a pending create is still in flight and the
// code that owns it will observe the same thing through Alive().
func (m *Manager) dropByInstance(ctx context.Context, backendName, instanceID string) {
	m.mu.Lock()
	var victim *Entry
	for _, e := range m.byIdentity {
		if e.Backend == backendName && e.Instance.ID == instanceID {
			victim = e
			break
		}
	}
	if victim == nil || victim.state != stateLeased {
		m.mu.Unlock()
		return
	}
	victim.state = stateDraining
	victim.done = make(chan struct{})
	logger := m.logger
	m.mu.Unlock()

	if logger != nil {
		logger.Info("instance deleted; dropping its lease",
			"identity", shortIdentity(victim.Identity), "backend", backendName,
			"instance", instanceID)
	}
	_ = m.release(ctx, victim, "instance deleted")
}
