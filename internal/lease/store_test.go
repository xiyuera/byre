package lease

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"
)

// fakeStore records what the manager asked of it. Deliberately not a map: the assertions are about
// which calls happen, not about storage semantics, which internal/lease/crd already covers.
type fakeStore struct {
	mu      sync.Mutex
	puts    []StoredBinding
	deletes []string
	list    []StoredBinding
	putErr  error
	delErr  error
	listErr error
}

func (f *fakeStore) Put(_ context.Context, b StoredBinding) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.putErr != nil {
		return f.putErr
	}
	f.puts = append(f.puts, b)
	return nil
}

func (f *fakeStore) Delete(_ context.Context, identity string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.delErr != nil {
		return f.delErr
	}
	f.deletes = append(f.deletes, identity)
	return nil
}

func (f *fakeStore) List(context.Context) ([]StoredBinding, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.listErr != nil {
		return nil, f.listErr
	}
	return append([]StoredBinding(nil), f.list...), nil
}

// setPutErr flips the store between failing and working, for the tests that exercise recovery.
func (f *fakeStore) setPutErr(err error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.putErr = err
}

func (f *fakeStore) snapshot() (puts []StoredBinding, deletes []string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]StoredBinding(nil), f.puts...), append([]string(nil), f.deletes...)
}

// A manager with no store is a supported mode, not a degraded one: `endpoints` and `command` have no
// cluster to put a CR in. Nothing must change for them.
func TestNoStoreMeansNoPersistence(t *testing.T) {
	m, drv, _ := newManager(t)
	if _, err := m.Acquire(context.Background(), "alice", "lab"); err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	if m.PersistErrors() != 0 {
		t.Errorf("PersistErrors = %d, want 0 (no store configured is not an error)", m.PersistErrors())
	}
	if drv.Acquires() != 1 {
		t.Errorf("acquires = %d, want 1", drv.Acquires())
	}
}

func TestAcquirePersistsTheBinding(t *testing.T) {
	m, _, _ := newManager(t)
	store := &fakeStore{}
	m.SetStore(store)
	m.SetIdentityNames(map[string]string{"alice": "alice-home"})

	if _, err := m.Acquire(context.Background(), "alice", "lab"); err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	puts, deletes := store.snapshot()
	if len(puts) != 1 {
		t.Fatalf("puts = %d, want 1", len(puts))
	}
	got := puts[0]
	if got.Identity != "alice" || got.Backend != "lab" {
		t.Errorf("binding carried %+v", got)
	}
	if got.Name != "alice-home" {
		t.Errorf("name = %q, want the configured stable name", got.Name)
	}
	if got.Instance == "" {
		t.Error("instance is empty; a binding without one is unrunnable after a restart")
	}
	if len(deletes) != 0 {
		t.Errorf("unexpected deletes: %v", deletes)
	}
}

// Reuse must not write. The hot path is "connect, find the lease, return"; an API write there is the
// cost that would make this design worse than the in-memory table it replaces.
func TestReAcquireDoesNotWriteAgain(t *testing.T) {
	m, _, _ := newManager(t)
	store := &fakeStore{}
	m.SetStore(store)

	ctx := context.Background()
	if _, err := m.Acquire(ctx, "alice", "lab"); err != nil {
		t.Fatalf("first Acquire: %v", err)
	}
	for i := 0; i < 5; i++ {
		if _, err := m.Acquire(ctx, "alice", "lab"); err != nil {
			t.Fatalf("reuse Acquire %d: %v", i, err)
		}
	}
	puts, _ := store.snapshot()
	if len(puts) != 1 {
		t.Errorf("puts = %d after 6 acquisitions, want 1 (reuse must not write)", len(puts))
	}
}

func TestReleaseDeletesTheBinding(t *testing.T) {
	m, _, clock := newManager(t)
	store := &fakeStore{}
	m.SetStore(store)

	ctx := context.Background()
	if _, err := m.Acquire(ctx, "alice", "lab"); err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	m.ReleaseConn("alice") // Reap skips leases with open connections, correctly
	clock.advance(10 * time.Minute)
	if _, err := m.Reap(ctx); err != nil {
		t.Fatalf("Reap: %v", err)
	}
	_, deletes := store.snapshot()
	if len(deletes) != 1 || deletes[0] != "alice" {
		t.Fatalf("deletes = %v, want [alice]", deletes)
	}
}

// ErrBindingExists means another replica won. Handing out a second instance for one identity is the
// exact defect the store exists to prevent, so the loser must release what it just created -- and it
// must stop, rather than spin, when the winner does not let go.
func TestLosingTheBindRaceReleasesTheInstance(t *testing.T) {
	m, drv, _ := newManager(t)
	m.SetStore(&fakeStore{putErr: ErrBindingExists})

	// Deliberately no deadline: this asserts Acquire terminates on its own. If it only stopped because
	// a context expired, a caller without one would hang -- which is what an unbounded retry looks
	// like from the outside.
	ctx := context.Background()
	_, err := m.Acquire(ctx, "alice", "lab")
	if !errors.Is(err, ErrBindingExists) {
		t.Fatalf("err = %v, want ErrBindingExists after %d retries", err, maxBindRetries)
	}
	if drv.Acquires() < 1 {
		t.Fatalf("acquires = %d, want at least 1", drv.Acquires())
	}
	// Every instance this attempt created must have gone back, or the retry loop leaks one per try.
	if len(drv.Released()) != drv.Acquires() {
		t.Errorf("acquired %d, released %d; a lost race must not orphan instances",
			drv.Acquires(), len(drv.Released()))
	}
	// And the table must be clean, so the next connection starts from scratch instead of picking up a
	// half-built row.
	if active, _ := m.Stats(); active != 0 {
		t.Errorf("active leases = %d after a lost race, want 0", active)
	}
}

// A store that is merely unreachable is different from a lost race: the instance works, and killing it
// over a bookkeeping error would take the client's files with it. Keep the lease, count the failure.
func TestPersistFailureKeepsTheLeaseAndCounts(t *testing.T) {
	m, drv, _ := newManager(t)
	m.SetStore(&fakeStore{putErr: errors.New("apiserver unreachable")})

	ctx := context.Background()
	got, err := m.Acquire(ctx, "alice", "lab")
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	if got.Instance.ID == "" {
		t.Error("lease has no instance")
	}
	if m.PersistErrors() != 1 {
		t.Errorf("PersistErrors = %d, want 1", m.PersistErrors())
	}
	if n := len(drv.Released()); n != 0 {
		t.Errorf("released %d instances; an unreachable store must not destroy a working lease", n)
	}
	// The lease is live but invisible to the store, and that has to be visible to an operator: with a
	// second replica running, this is the state that hands one identity two instances.
	if n := m.UnpersistedCount(); n != 1 {
		t.Errorf("UnpersistedCount = %d, want 1", n)
	}
	if _, err := m.Acquire(ctx, "alice", "lab"); err != nil {
		t.Fatalf("reuse after persist failure: %v", err)
	}
}

// The flag is not a permanent scar: the reuse path retries the write, so a store that comes back makes
// the binding appear without touching the client's instance.
func TestReuseRepairsAnUnpersistedLease(t *testing.T) {
	m, drv, _ := newManager(t)
	store := &fakeStore{putErr: errors.New("apiserver unreachable")}
	m.SetStore(store)

	ctx := context.Background()
	first, err := m.Acquire(ctx, "alice", "lab")
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	if n := m.UnpersistedCount(); n != 1 {
		t.Fatalf("UnpersistedCount = %d after a failed write, want 1", n)
	}

	store.setPutErr(nil) // the API server is back
	second, err := m.Acquire(ctx, "alice", "lab")
	if err != nil {
		t.Fatalf("reuse: %v", err)
	}
	if second.Instance.ID != first.Instance.ID {
		t.Errorf("reuse returned a different instance (%s vs %s); repairing the binding must not recreate the lease",
			second.Instance.ID, first.Instance.ID)
	}
	if n := m.UnpersistedCount(); n != 0 {
		t.Errorf("UnpersistedCount = %d after a successful retry, want 0", n)
	}
	puts, _ := store.snapshot()
	if len(puts) != 1 {
		t.Errorf("puts = %d, want exactly 1 (the repair)", len(puts))
	}
	// And the instance was never handed back to the backend.
	if n := len(drv.Released()); n != 0 {
		t.Errorf("released %d instances; repairing a binding must not disturb the lease", n)
	}
}

// A failed release leaves the object in place, and the CR pointing at it is the only record that lets
// a restart find it again.
func TestFailedReleaseKeepsTheBinding(t *testing.T) {
	m, _, _, clock := newFlakyManager(t, 99)
	store := &fakeStore{}
	m.SetStore(store)

	ctx := context.Background()
	if _, err := m.Acquire(ctx, "alice", "lab"); err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	m.ReleaseConn("alice")
	clock.advance(10 * time.Minute)
	// Reap is expected to report the failed release: that is how an operator learns an instance leaked.
	// What matters here is that the binding survived, because it is the only record of where that
	// instance is.
	if _, err := m.Reap(ctx); err == nil {
		t.Fatal("Reap returned no error even though the backend refused every release")
	}
	_, deletes := store.snapshot()
	if len(deletes) != 0 {
		t.Errorf("deletes = %v; a lease whose instance still exists must keep its binding", deletes)
	}
}

func TestDeleteFailureIsCounted(t *testing.T) {
	m, _, clock := newManager(t)
	m.SetStore(&fakeStore{delErr: errors.New("apiserver unreachable")})

	ctx := context.Background()
	if _, err := m.Acquire(ctx, "alice", "lab"); err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	m.ReleaseConn("alice")
	clock.advance(10 * time.Minute)
	if _, err := m.Reap(ctx); err != nil {
		t.Fatalf("Reap: %v", err)
	}
	if m.PersistErrors() < 1 {
		t.Error("PersistErrors did not count the failed delete")
	}
}
