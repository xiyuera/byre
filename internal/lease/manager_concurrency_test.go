package lease

import (
	"context"
	"sync"
	"testing"
	"time"

	"byre/internal/backend"
)

// These tests exist because the repository had none: -race passed on a code path that no test ever
// drove from two goroutines, which is exactly where the two blockers lived (F1, F2).

// TestConcurrentAcquireSharesOneInstance: eight connections for one identity at the same moment must
// produce one instance and eight references. The lease exists to prevent the second instance.
func TestConcurrentAcquireSharesOneInstance(t *testing.T) {
	m, fb, _ := newManager(t)
	ctx := context.Background()

	const n = 8
	ids := make([]string, n)
	errs := make([]error, n)
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			e, err := m.Acquire(ctx, "id-a", "lab")
			errs[i], ids[i] = err, e.Instance.ID
		}(i)
	}
	wg.Wait()

	for i := range errs {
		if errs[i] != nil {
			t.Fatalf("acquire #%d: %v", i, errs[i])
		}
		if ids[i] != ids[0] {
			t.Fatalf("acquire #%d got %q, #0 got %q: one identity must resolve to one instance", i, ids[i], ids[0])
		}
	}
	if got := fb.Acquires(); got != 1 {
		t.Fatalf("backend acquired %d instances for one identity, want 1", got)
	}
	e := m.Lookup("id-a")
	if e == nil {
		t.Fatal("lease disappeared")
	}
	if e.Refs() != n {
		t.Fatalf("refs = %d, want %d", e.Refs(), n)
	}
	if active, idle := m.Stats(); active != 1 || idle != 0 {
		t.Fatalf("stats = (%d active, %d idle), want (1, 0)", active, idle)
	}
}

// gateBackend blocks inside Acquire until the test opens the gate: it is the slow backend call that
// used to freeze every other lease in the process.
type gateBackend struct {
	inner   *fakeBackend
	entered chan struct{}
	gate    chan struct{}
	once    sync.Once
}

func (g *gateBackend) Name() string { return g.inner.Name() }

func (g *gateBackend) Acquire(ctx context.Context, identity string) (backend.Instance, error) {
	g.once.Do(func() { close(g.entered) })
	select {
	case <-g.gate:
	case <-ctx.Done():
		return backend.Instance{}, ctx.Err()
	}
	return g.inner.Acquire(ctx, identity)
}

func (g *gateBackend) Release(ctx context.Context, in backend.Instance) error {
	return g.inner.Release(ctx, in)
}

func (g *gateBackend) List(ctx context.Context) ([]backend.Instance, error) { return g.inner.List(ctx) }

func (g *gateBackend) Adopt(ctx context.Context, ins []backend.Instance) error {
	return g.inner.Adopt(ctx, ins)
}

// TestSlowBackendDoesNotBlockOtherLeases is the F1 regression: while one identity's instance is being
// created, Lookup, KeepAlive, ReleaseConn and Stats for other identities must still answer. Before the
// fix they waited behind the same mutex for the whole backend round trip (measured at 379ms in the
// review, with the gateway unavailable for the duration of a cold start).
func TestSlowBackendDoesNotBlockOtherLeases(t *testing.T) {
	slow := &gateBackend{
		inner:   &fakeBackend{name: "slow"},
		entered: make(chan struct{}),
		gate:    make(chan struct{}),
	}
	lab := &fakeBackend{name: "lab"}
	m, err := New(360*time.Second, 60*time.Second, map[string]backend.Backend{"slow": slow, "lab": lab})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	ctx := context.Background()
	release := sync.OnceFunc(func() { close(slow.gate) })
	defer release()

	if _, err := m.Acquire(ctx, "id-existing", "lab"); err != nil {
		t.Fatalf("seed acquire: %v", err)
	}

	done := make(chan struct{})
	go func() {
		defer close(done)
		if _, err := m.Acquire(ctx, "id-slow", "slow"); err != nil {
			t.Errorf("slow acquire: %v", err)
		}
	}()
	<-slow.entered // the backend call is in flight: the manager is "in the middle of an Acquire"

	elapsed := make(chan time.Duration, 1)
	go func() {
		start := time.Now()
		if e := m.Lookup("id-existing"); e == nil {
			t.Error("Lookup lost an existing lease during another identity's acquire")
		}
		m.KeepAlive("id-existing")
		m.Stats()
		m.Lookup("id-unknown")
		elapsed <- time.Since(start)
	}()

	select {
	case d := <-elapsed:
		if d > 100*time.Millisecond {
			t.Fatalf("Lookup/KeepAlive/Stats took %s while another identity's backend call was in flight", d)
		}
	case <-time.After(2 * time.Second):
		release()
		t.Fatal("Lookup/KeepAlive/Stats blocked behind another identity's backend call")
	}

	release()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("the slow acquire never finished after the gate opened")
	}
}

// slowRelease wraps a driver so that destroying an instance blocks: it widens the window between
// "the lease is gone" and "the instance is gone" that F2 was about.
type slowRelease struct {
	backend.Backend
	entered chan struct{}
	gate    chan struct{}
	once    sync.Once
}

func (s *slowRelease) Release(ctx context.Context, in backend.Instance) error {
	s.once.Do(func() { close(s.entered) })
	select {
	case <-s.gate:
	case <-ctx.Done():
	}
	return s.Backend.Release(ctx, in)
}

// TestReapDoesNotHandOutTheInstanceItIsDestroying is the F2 regression, the E7 probe as a test.
//
// A static driver keeps handing an endpoint to the identity that owns it, so in the window where the
// reaper has deleted the table row but the backend has not been told yet, a reconnect is given the
// very instance that is about to be destroyed — and once it is released, a second identity gets it
// too. Two identities then share one machine.
func TestReapDoesNotHandOutTheInstanceBeingDestroyed(t *testing.T) {
	st, err := backend.NewStatic(backend.StaticConfig{
		Name:          "lab",
		Endpoints:     []string{"ep-1:22", "ep-2:22"},
		User:          "sandbox",
		PrivateKey:    "/keys/pool",
		HostKeyPolicy: "insecure",
	})
	if err != nil {
		t.Fatalf("static backend: %v", err)
	}
	slow := &slowRelease{Backend: st, entered: make(chan struct{}), gate: make(chan struct{})}
	open := sync.OnceFunc(func() { close(slow.gate) })
	defer open()

	m, err := New(360*time.Second, 60*time.Second, map[string]backend.Backend{"lab": slow})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	c := &clock{t: time.Date(2026, 11, 17, 0, 0, 0, 0, time.UTC)}
	m.SetClock(c.now)
	ctx := context.Background()

	first, err := m.Acquire(ctx, "id-a", "lab")
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}
	m.ReleaseConn("id-a")
	c.advance(420 * time.Second) // ttl 360 + grace 60

	reaped := make(chan int, 1)
	go func() {
		n, _ := m.Reap(ctx)
		reaped <- n
	}()
	<-slow.entered // the row is draining; the backend has not destroyed the instance yet

	type result struct {
		entry Entry
		err   error
	}
	acquired := make(chan result, 1)
	go func() {
		e, err := m.Acquire(ctx, "id-a", "lab")
		acquired <- result{e, err}
	}()

	// Either the reconnect waits for the teardown (current design) or it gets a different instance;
	// what it must never do is get the one being destroyed.
	select {
	case r := <-acquired:
		if r.err != nil {
			open()
			t.Fatalf("reconnect during reap: %v", r.err)
		}
		if r.entry.Instance.ID == first.Instance.ID {
			open()
			t.Fatalf("reconnect during reap was handed %q, the instance being destroyed", first.Instance.ID)
		}
	case <-time.After(300 * time.Millisecond):
		// Waiting is the expected outcome.
	}

	open()
	select {
	case r := <-acquired:
		if r.err != nil {
			t.Fatalf("reconnect after reap: %v", r.err)
		}
		if r.entry.Instance.ID == first.Instance.ID {
			t.Fatalf("reconnect after reap was handed %q again", first.Instance.ID)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("reconnect never completed after the reap finished")
	}
	select {
	case n := <-reaped:
		if n != 1 {
			t.Fatalf("reaped %d, want 1", n)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Reap did not return")
	}

	// The destroyed instance must not be owned by anyone any more: a second identity may now have it,
	// which is correct, but the first identity must not.
	if e := m.Lookup("id-a"); e == nil || e.Instance.ID == first.Instance.ID {
		t.Fatalf("id-a still points at %v after the reap", e)
	}
}
