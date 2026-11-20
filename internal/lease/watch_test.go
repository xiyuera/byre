package lease

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"byre/internal/backend"
)

// watchingBackend is a fakeBackend that also reports deletions, so the manager's watch path can be
// exercised without a cluster.
type watchingBackend struct {
	*fakeBackend
	mu            sync.Mutex
	out           chan string
	closed        bool
	subscriptions int
	// failures makes the next N subscriptions fail. An apiserver that is briefly unavailable is the
	// common case at startup, not an exotic one, so the retry path deserves the same coverage as the
	// ordinary one.
	failures int
}

func (w *watchingBackend) WatchDeletions(ctx context.Context) (<-chan string, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.subscriptions++
	if w.failures > 0 {
		w.failures--
		return nil, errors.New("apiserver unavailable")
	}
	// A fresh channel per subscription, because that is what a real watch is: a new stream every time. A
	// reused channel would make the re-subscription path untestable, and it is the path that keeps a
	// long-lived process from silently watching nothing.
	w.out = make(chan string, 8)
	w.closed = false
	return w.out, nil
}

// current reports the channel of the most recent successful subscription, or nil when none is live. A
// test needs this to tell "a watch is established" from "a subscription was attempted and failed".
func (w *watchingBackend) current() chan string {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.out
}

// subscriptionCount reports how many times the manager has subscribed, so a test can tell "subscribed
// again" from "still waiting".
func (w *watchingBackend) subscriptionCount() int {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.subscriptions
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
	// Subscription happens in its own goroutine by design; wait for it so a test can send a deletion
	// immediately afterwards instead of racing the goroutine that is about to start watching.
	waitFor(t, "the initial deletion subscription", func() bool { return wb.subscriptionCount() >= 1 })
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

// A watch ending must lead to a new subscription, not to a process that has been running for weeks with
// nothing watching and a log line promising coverage. The apiserver closes watches routinely, so this is
// the normal path rather than an error path.
func TestWatchResubscribesAfterTheSubscriptionEnds(t *testing.T) {
	m, wb, _, cancel := newWatchingManager(t)
	defer cancel()
	ctx := context.Background()

	// Subscription happens in its own goroutine now, so wait for it rather than assuming it has run.
	waitFor(t, "the first subscription", func() bool {
		return wb.subscriptionCount() >= 1
	})
	wb.endWatch()

	// The backoff starts at a second; this only needs "it happened", not "it happened quickly".
	waitFor(t, "the manager subscribed again after the watch ended", func() bool {
		return wb.subscriptionCount() >= 2
	})

	// And the new subscription has to be wired to the table, not merely counted.
	lease, err := m.Acquire(ctx, "alice", "lab")
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	m.ReleaseConn("alice")
	wb.delete(lease.Instance.ID)
	waitFor(t, "the re-subscribed watch dropped the lease", func() bool {
		return m.Lookup("alice") == nil
	})
}

// A subscription that fails must be retried. The common case is an apiserver that is briefly unavailable
// while the gateway itself is starting; the wrong behaviour is to spend the rest of the process's life
// relying on Alive() because of a blip that lasted a second.
func TestWatchRetriesAFailedSubscription(t *testing.T) {
	fb := &fakeBackend{name: "lab"}
	// Exactly one failure, so the retry lands inside waitFor's two-second window. The backoff schedule is
	// not what is under test here; that it is retried at all is.
	wb := &watchingBackend{fakeBackend: fb, failures: 1}
	m, err := New(360*time.Second, 60*time.Second, map[string]backend.Backend{"lab": wb})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	m.SetClock(func() time.Time { return time.Date(2026, 11, 17, 0, 0, 0, 0, time.UTC) })
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	m.WatchDeletions(ctx)
	waitFor(t, "a successful subscription after the first one failed", func() bool {
		return wb.current() != nil
	})

	// And the subscription that succeeded has to be wired to the table, not merely opened.
	lease, err := m.Acquire(ctx, "alice", "lab")
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	m.ReleaseConn("alice")
	wb.delete(lease.Instance.ID)
	waitFor(t, "the watch that succeeded dropped the lease", func() bool {
		return m.Lookup("alice") == nil
	})
}
