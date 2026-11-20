package lease

import (
	"context"
	"sync"
	"testing"
	"time"

	"byre/internal/backend"
)

// watchingBackend is a fakeBackend that also reports deletions, so the manager's watch path can be
// exercised without a cluster.
type watchingBackend struct {
	*fakeBackend
	mu     sync.Mutex
	out    chan string
	closed bool
}

func (w *watchingBackend) WatchDeletions(ctx context.Context) (<-chan string, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.out == nil {
		w.out = make(chan string, 8)
	}
	return w.out, nil
}

func (w *watchingBackend) delete(id string) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.out == nil || w.closed {
		return
	}
	w.out <- id
}

func (w *watchingBackend) endWatch() {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.out != nil && !w.closed {
		close(w.out)
		w.closed = true
	}
}

func newWatchingManager(t *testing.T) (*Manager, *watchingBackend, *clock, context.CancelFunc) {
	t.Helper()
	fb := &fakeBackend{name: "lab"}
	wb := &watchingBackend{fakeBackend: fb}
	m, err := New(360*time.Second, 60*time.Second, map[string]backend.Backend{"lab": wb})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	c := &clock{t: time.Date(2026, 11, 17, 0, 0, 0, 0, time.UTC)}
	m.SetClock(c.now)
	ctx, cancel := context.WithCancel(context.Background())
	m.WatchDeletions(ctx)
	return m, wb, c, cancel
}

// The whole point of part 4: a deleted instance stops being handed out without waiting for the ttl.
func TestDeletionDropsTheLeaseImmediately(t *testing.T) {
	m, wb, _, cancel := newWatchingManager(t)
	defer cancel()
	ctx := context.Background()

	lease, err := m.Acquire(ctx, "alice", "lab")
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	if m.Lookup("alice") == nil {
		t.Fatal("lease should exist before the deletion")
	}

	wb.delete(lease.Instance.ID)

	// The watch goroutine and the drop both run asynchronously; poll rather than sleep a fixed time.
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if m.Lookup("alice") == nil {
			return // dropped, as intended
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("lease survived a deleted instance; the next reconnect would dial a dead address")
}

// A deletion for an instance this manager never leased must not remove anything.
func TestDeletionForAnUnknownInstanceIsIgnored(t *testing.T) {
	m, wb, _, cancel := newWatchingManager(t)
	defer cancel()
	ctx := context.Background()

	if _, err := m.Acquire(ctx, "alice", "lab"); err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	wb.delete("some-other-instance")
	time.Sleep(100 * time.Millisecond)

	if m.Lookup("alice") == nil {
		t.Fatal("lease was dropped by an unrelated deletion")
	}
}

// The instance ID is only unique within a backend. A deletion reported by one driver must not touch a
// lease held on a same-named instance from another.
func TestDeletionIsScopedToItsBackend(t *testing.T) {
	lab := &watchingBackend{fakeBackend: &fakeBackend{name: "lab"}}
	edge := &watchingBackend{fakeBackend: &fakeBackend{name: "edge"}}
	m, err := New(360*time.Second, 60*time.Second, map[string]backend.Backend{"lab": lab, "edge": edge})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	m.SetClock((&clock{t: time.Date(2026, 11, 17, 0, 0, 0, 0, time.UTC)}).now)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	m.WatchDeletions(ctx)

	lease, err := m.Acquire(ctx, "alice", "edge")
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	// Same instance name, reported by the *other* backend.
	lab.delete(lease.Instance.ID)
	time.Sleep(100 * time.Millisecond)

	if m.Lookup("alice") == nil {
		t.Fatal("a deletion from backend lab removed a lease held on backend edge")
	}
}

// A watch that ends must not take the process with it, and the lease stays until something else
// notices -- Alive() is still the backstop.
func TestWatchEndingIsNotFatal(t *testing.T) {
	m, wb, _, cancel := newWatchingManager(t)
	defer cancel()
	ctx := context.Background()

	if _, err := m.Acquire(ctx, "alice", "lab"); err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	wb.endWatch()
	time.Sleep(100 * time.Millisecond)

	if m.Lookup("alice") == nil {
		t.Fatal("ending the watch should not drop leases")
	}
}

// A driver that cannot watch is skipped silently: `endpoints` and `command` backends have nothing to
// watch, and that is a supported mode rather than a degraded one.
func TestDriverWithoutWatchIsSkipped(t *testing.T) {
	m, _, _ := newManager(t) // fakeBackend does not implement DeletionWatcher
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	m.WatchDeletions(ctx) // must not panic or block

	if _, err := m.Acquire(context.Background(), "alice", "lab"); err != nil {
		t.Fatalf("Acquire: %v", err)
	}
}

// Cancelling the context must stop the consumer goroutine; otherwise every restart leaks one per
// backend.
func TestWatchStopsOnContextCancel(t *testing.T) {
	m, wb, _, cancel := newWatchingManager(t)
	if _, err := m.Acquire(context.Background(), "alice", "lab"); err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	cancel()
	// A deletion after cancellation must be a no-op rather than a panic on a closed consumer.
	wb.delete("whatever")
	time.Sleep(50 * time.Millisecond)
	if m.Lookup("alice") == nil {
		t.Fatal("lease vanished after the context was cancelled")
	}
}
