package lease

import (
	"context"
	"strings"
	"testing"
	"time"

	"byre/internal/backend"
)

// unusableBackend creates an instance the driver cannot describe: an Acquire that succeeds and a
// Validate that fails. The review measured the result as `backend.Release calls=[]` — an instance in
// the cluster that no lease and no driver bookkeeping refers to.
type unusableBackend struct {
	fakeBackend
	releases int
}

func (u *unusableBackend) Acquire(context.Context, string) (backend.Instance, error) {
	return backend.Instance{ID: "bad-1", User: "sandbox", PrivateKey: "/k", HostKeyPolicy: "insecure"}, nil
}

func (u *unusableBackend) Release(context.Context, backend.Instance) error {
	u.releases++
	return nil
}

// TestUnusableInstanceIsReleased is the F3 regression.
func TestUnusableInstanceIsReleased(t *testing.T) {
	ub := &unusableBackend{fakeBackend: fakeBackend{name: "lab"}}
	m, err := New(360*time.Second, 60*time.Second, map[string]backend.Backend{"lab": ub})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	_, err = m.Acquire(context.Background(), "id-a", "lab")
	if err == nil || !strings.Contains(err.Error(), "unusable instance") {
		t.Fatalf("Acquire error = %v, want an unusable-instance error", err)
	}
	if ub.releases != 1 {
		t.Fatalf("driver.Release called %d times, want 1: the instance it created must be given back", ub.releases)
	}
	if e := m.Lookup("id-a"); e != nil {
		t.Fatalf("a failed acquire left a lease behind: %+v", e)
	}
}

// TestAdoptReleasesWhatItCannotOwn is the F4 regression: instances that carry a duplicate identity
// annotation, or cannot be dialled, used to be skipped with `continue` and stayed in the cluster
// forever, invisible to the lease table, the reaper and the metrics.
func TestAdoptReleasesWhatItCannotOwn(t *testing.T) {
	// A driver that promises its List result is authoritative about identity (kubernetes, static), so an
	// unannotated instance really is capacity nobody holds. For a driver that cannot promise it, ADR-0009
	// keeps the leftover instead — that case is TestAdoptKeepsAnonymousInstancesOfADriverThatCannotJudgeThem.
	fb := &staleBackend{&fakeBackend{name: "lab"}}
	fb.list = []backend.Instance{
		{
			ID: "keep-1", Addr: "192.0.2.1:22", User: "sandbox", PrivateKey: "/k", HostKeyPolicy: "insecure",
			Annotations: map[string]string{backend.LabelIdentity: "id-a"},
		},
		{
			ID: "dup-2", Addr: "192.0.2.2:22", User: "sandbox", PrivateKey: "/k", HostKeyPolicy: "insecure",
			Annotations: map[string]string{backend.LabelIdentity: "id-a"},
		},
		{
			ID: "orphan-warm", Addr: "192.0.2.3:22", User: "sandbox", PrivateKey: "/k", HostKeyPolicy: "insecure",
		},
		{
			ID: "no-addr", User: "sandbox", PrivateKey: "/k", HostKeyPolicy: "insecure",
			Annotations: map[string]string{backend.LabelIdentity: "id-b"},
		},
	}
	m, err := New(360*time.Second, 60*time.Second, map[string]backend.Backend{"lab": fb})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	adopted, err := m.Adopt(context.Background())
	if err != nil {
		t.Fatalf("Adopt: %v", err)
	}
	if adopted != 1 {
		t.Fatalf("adopted = %d, want 1", adopted)
	}
	if e := m.Lookup("id-a"); e == nil || e.Instance.ID != "keep-1" {
		t.Fatalf("id-a lease = %+v, want keep-1", e)
	}
	if e := m.Lookup("id-b"); e != nil {
		t.Fatalf("an instance that cannot be dialled was adopted: %+v", e)
	}

	got := fb.Released()
	want := map[string]bool{"dup-2": true, "orphan-warm": true, "no-addr": true}
	if len(got) != len(want) {
		t.Fatalf("released = %v, want %v", got, want)
	}
	for _, id := range got {
		if !want[id] {
			t.Fatalf("released %q, which adoption should have kept or never seen (%v)", id, got)
		}
	}
	if d := m.DiscardedTotal(); d != 3 {
		t.Fatalf("discarded = %d, want 3", d)
	}
}

// TestInvalidateDropsTheLease is the F5 unit test: a lease whose instance cannot be dialled has to go,
// or the next connection is handed the same dead address until ttl+grace expires.
func TestInvalidateDropsTheLease(t *testing.T) {
	m, fb, _ := newManager(t)
	ctx := context.Background()

	first, err := m.Acquire(ctx, "id-a", "lab")
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}
	if !m.Invalidate(ctx, "id-a", first.Instance.ID) {
		t.Fatal("Invalidate reported false for a live lease")
	}
	if e := m.Lookup("id-a"); e != nil {
		t.Fatalf("lease survived invalidation: %+v", e)
	}
	if got := fb.Released(); len(got) != 1 || got[0] != first.Instance.ID {
		t.Fatalf("released = %v, want [%s]", got, first.Instance.ID)
	}
	if m.Invalidate(ctx, "id-a", first.Instance.ID) {
		t.Fatal("a second invalidation of the same lease reported true")
	}

	// The next connection gets a fresh instance rather than the dead one.
	second, err := m.Acquire(ctx, "id-a", "lab")
	if err != nil {
		t.Fatalf("reacquire: %v", err)
	}
	if second.Instance.ID == first.Instance.ID {
		t.Fatalf("reconnect after invalidation got %q again", first.Instance.ID)
	}
}

// TestInvalidateIgnoresAStaleInstance: a caller holding an old snapshot must not evict the lease that
// replaced it (the dial failure may be reported after the identity already moved on).
func TestInvalidateIgnoresAStaleInstance(t *testing.T) {
	m, fb, _ := newManager(t)
	ctx := context.Background()

	first, err := m.Acquire(ctx, "id-a", "lab")
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}
	if m.Invalidate(ctx, "id-a", "lab-not-this-one") {
		t.Fatal("Invalidate dropped a lease for an instance the caller never held")
	}
	if e := m.Lookup("id-a"); e == nil || e.Instance.ID != first.Instance.ID {
		t.Fatalf("lease = %+v, want %s untouched", e, first.Instance.ID)
	}
	if got := fb.Released(); len(got) != 0 {
		t.Fatalf("released = %v, want nothing", got)
	}
}

// TestAdoptDestroysStaleWarmCapacity: the unannotated instance a previous process left behind must be
// destroyed, not merely ignored (the existing test never asserted the release happened).
func TestAdoptDestroysStaleWarmCapacity(t *testing.T) {
	fb := &staleBackend{&fakeBackend{name: "lab"}}
	fb.list = []backend.Instance{{
		ID: "warm-1", Addr: "192.0.2.1:22", User: "sandbox", PrivateKey: "/k", HostKeyPolicy: "insecure",
		Annotations: map[string]string{},
	}}
	m, err := New(360*time.Second, 60*time.Second, map[string]backend.Backend{"lab": fb})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if _, err := m.Adopt(context.Background()); err != nil {
		t.Fatalf("Adopt: %v", err)
	}
	if got := fb.Released(); len(got) != 1 || got[0] != "warm-1" {
		t.Fatalf("released = %v, want [warm-1]", got)
	}
}
