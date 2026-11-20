package lease

import (
	"context"
	"log/slog"

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
		watcher, ok := drv.(backend.DeletionWatcher)
		if !ok {
			continue
		}
		events, err := watcher.WatchDeletions(ctx)
		if err != nil {
			if logger != nil {
				logger.Warn("cannot watch for instance deletions; leases will rely on Alive() alone",
					"backend", name, "error", err)
			}
			continue
		}
		if logger != nil {
			logger.Info("watching for instance deletions", "backend", name)
		}
		go m.consumeDeletions(ctx, name, events, logger)
	}
}

func (m *Manager) consumeDeletions(ctx context.Context, backendName string, ids <-chan string, logger *slog.Logger) {
	for {
		select {
		case <-ctx.Done():
			return
		case id, ok := <-ids:
			if !ok {
				// The watch ended. Saying so matters: silence here would look identical to "no
				// instance was ever deleted".
				if logger != nil {
					logger.Warn("instance deletion watch ended; leases rely on Alive() until it is re-established",
						"backend", backendName)
				}
				return
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
