package lease

import (
	"context"
	"errors"
	"testing"
	"time"

	"byre/internal/backend"
)

// anonymous is an instance nobody holds: the shape a claim gets when its process died between Create and
// the annotate step, or when a release failed and the object was simply forgotten.
func anonymous(id string, created time.Time) backend.Instance {
	return backend.Instance{
		ID: id, Addr: "10.0.0.7:22", User: "sandbox", PrivateKey: "/k", HostKeyPolicy: "insecure",
		Created: created,
	}
}

// TestAdoptReleasesAnonymousInstanceWithoutAnAddress is the regression test for the live leak. Two claims
// with no identity annotation and no address in the configured status field stayed Running for half an
// hour across a restart of the gateway: the driver never returned them, so adoption never saw them. Here
// the driver returns them (as it must) and adoption has to destroy them.
func TestAdoptReleasesAnonymousInstanceWithoutAnAddress(t *testing.T) {
	fb := &staleBackend{&fakeBackend{name: "lab"}}
	noAddr := anonymous("byre-b11f7e8c", time.Time{})
	noAddr.Addr = ""
	fb.list = []backend.Instance{noAddr}

	m, err := New(360*time.Second, 60*time.Second, map[string]backend.Backend{"lab": fb})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if _, err := m.Adopt(context.Background()); err != nil {
		t.Fatalf("Adopt: %v", err)
	}
	if got := fb.Released(); len(got) != 1 || got[0] != "byre-b11f7e8c" {
		t.Fatalf("released = %v, want the addressless anonymous claim destroyed", got)
	}
}

// TestReclaimDestroysAnAnonymousInstanceThatOutlivedTheCreateWindow: adoption only runs at startup, so an
// object orphaned while the process runs needs the sweep. The age guard is what separates "abandoned"
// from "a create that has not finished yet".
func TestReclaimDestroysAnAnonymousInstanceThatOutlivedTheCreateWindow(t *testing.T) {
	fb := &staleBackend{&fakeBackend{name: "lab"}}
	fb.list = []backend.Instance{anonymous("byre-0eb4523e", time.Time{})}

	m, _, clock := newManager(t)
	m.backends["lab"] = fb
	fb.list[0].Created = clock.t.Add(-time.Hour)

	if _, err := m.Reap(context.Background()); err != nil {
		t.Fatalf("Reap: %v", err)
	}
	if got := fb.Released(); len(got) != 1 || got[0] != "byre-0eb4523e" {
		t.Fatalf("released = %v, want the abandoned claim destroyed by the sweep", got)
	}
	if n := m.ReclaimedTotal(); n != 1 {
		t.Fatalf("reclaimed = %d, want 1", n)
	}
	if n := m.DiscardedTotal(); n != 1 {
		t.Fatalf("discarded = %d, want 1 (the sweep counts as a discard)", n)
	}
}

// TestReclaimSparesFreshAndHeldInstances is the safety half: the sweep must not touch a create that is
// still in flight (too young), an instance a lease still points at, or warm capacity the pool is holding.
func TestReclaimSparesFreshAndHeldInstances(t *testing.T) {
	fb := &staleBackend{&fakeBackend{name: "lab"}}
	m, _, clock := newManager(t)
	m.backends["lab"] = fb
	ctx := context.Background()

	// A lease on lab-1, which the driver then also reports as anonymous: the mismatch is what a release
	// that failed after the row was dropped looks like. The ID is still held, so the sweep must skip it.
	if _, err := m.Acquire(ctx, "fp-1", "lab"); err != nil {
		t.Fatalf("acquire: %v", err)
	}
	held := anonymous("lab-1", clock.t.Add(-time.Hour))
	fresh := anonymous("byre-fresh", clock.t.Add(-time.Minute))

	fb.list = []backend.Instance{held, fresh}

	if _, err := m.Reap(ctx); err != nil {
		t.Fatalf("Reap: %v", err)
	}
	if got := fb.Released(); len(got) != 0 {
		t.Fatalf("released = %v, want nothing: lab-1 is leased and byre-fresh is younger than the "+
			"reclaim age", got)
	}

	// Warm capacity is anonymous by construction; the sweep must never empty the pool.
	fb.list = nil
	m.SetPool("lab", 1, fb)
	in := anonymous("lab-warm-1", clock.t.Add(-time.Hour))
	p := m.pool("lab")
	p.mu.Lock()
	p.warm = append(p.warm, in)
	p.mu.Unlock()

	clock.advance(defaultReclaimEvery)
	fb.list = []backend.Instance{in}
	if _, err := m.Reap(ctx); err != nil {
		t.Fatalf("Reap: %v", err)
	}
	if got := fb.Released(); len(got) != 0 {
		t.Fatalf("released = %v, want the warm instance kept", got)
	}
}

// TestReclaimRunsAtMostOncePerInterval keeps the sweep from turning every reap into an API list: the
// numbers are a policy, and a policy that only exists in a comment is not a policy.
func TestReclaimRunsAtMostOncePerInterval(t *testing.T) {
	fb := &staleBackend{&fakeBackend{name: "lab"}}
	m, _, clock := newManager(t)
	m.backends["lab"] = fb
	fb.list = []backend.Instance{anonymous("byre-old", clock.t.Add(-time.Hour))}

	if _, err := m.Reap(context.Background()); err != nil {
		t.Fatalf("Reap: %v", err)
	}
	if got := fb.Released(); len(got) != 1 {
		t.Fatalf("first reap released %v, want the abandoned instance", got)
	}

	clock.advance(time.Minute)
	fb.list = []backend.Instance{anonymous("byre-old-2", clock.t.Add(-2*time.Hour))}
	fb.released = nil
	if _, err := m.Reap(context.Background()); err != nil {
		t.Fatalf("Reap: %v", err)
	}
	if got := fb.Released(); len(got) != 0 {
		t.Fatalf("second reap released %v, want nothing inside the reclaim interval", got)
	}

	clock.advance(defaultReclaimEvery)
	if _, err := m.Reap(context.Background()); err != nil {
		t.Fatalf("Reap: %v", err)
	}
	if got := fb.Released(); len(got) != 1 || got[0] != "byre-old-2" {
		t.Fatalf("released = %v after the interval, want [byre-old-2]", got)
	}
}

// TestReclaimSkipsDriversThatCannotJudgeOwnership: ADR-0009's rule applies to the sweep too. A driver
// whose lister predates the annotation contract cannot say that an anonymous instance is stale, so the
// sweep must not destroy anything there.
func TestReclaimSkipsDriversThatCannotJudgeOwnership(t *testing.T) {
	fb := &fakeBackend{name: "cmd"}
	m, _, clock := newManager(t)
	m.backends["lab"] = fb
	fb.list = []backend.Instance{anonymous("vm-1", clock.t.Add(-24*time.Hour))}

	if _, err := m.Reap(context.Background()); err != nil {
		t.Fatalf("Reap: %v", err)
	}
	if got := fb.Released(); len(got) != 0 {
		t.Fatalf("released = %v, want nothing: this driver makes no ownership promise", got)
	}
}

// TestReclaimReportsListFailureAndKeepsGoing: a driver that cannot list must not stop another driver from
// being cleaned up, and the error must survive to the caller instead of being swallowed.
func TestReclaimReportsListFailureAndKeepsGoing(t *testing.T) {
	broken := &staleBackend{&fakeBackend{name: "broken", listErr: errors.New("forbidden")}}
	healthy := &staleBackend{&fakeBackend{name: "healthy"}}

	m, _, clock := newManager(t)
	m.backends = map[string]backend.Backend{"broken": broken, "healthy": healthy}
	healthy.list = []backend.Instance{anonymous("byre-1", clock.t.Add(-time.Hour))}

	_, err := m.Reap(context.Background())
	if err == nil {
		t.Fatalf("Reap returned no error although one driver could not list")
	}
	if got := healthy.Released(); len(got) != 1 {
		t.Fatalf("healthy driver released %v, want the abandoned instance cleaned up anyway", got)
	}
}
