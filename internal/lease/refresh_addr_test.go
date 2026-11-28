package lease

import (
	"context"
	"testing"
	"time"

	"k8s.io/client-go/dynamic"

	"byre/internal/backend"
	"byre/internal/backend/backendtest"
)

// newClaimManager builds a manager over the claim-backed fake: one claim per instance, and a status the
// test can rewrite the way the claim controller would.
func newClaimManager(t *testing.T) (*Manager, dynamic.Interface) {
	t.Helper()
	drv, dyn := backendtest.NewClaimDriver(t, "claims", "instances")
	m, err := New(6*time.Minute, time.Minute, map[string]backend.Backend{"claims": drv})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return m, dyn
}

// TestAcquireRefreshesAClaimAddressWhenTheSandboxIsReplaced is the acceptance test for the incident: a
// claim survives while the object it selected is deleted and rebuilt under a new name, so the address the
// lease cached stops resolving. The next Acquire for the same identity must hand out the address the
// claim publishes *now*.
//
// The table assertions are the load-bearing ones. The claim is the stable handle, so the lease must be
// updated in place: replacing the row would lose the client's object, and dropping it would count a dead
// instance for something that never died. Both are asserted, not just "the address changed".
func TestAcquireRefreshesAClaimAddressWhenTheSandboxIsReplaced(t *testing.T) {
	ctx := context.Background()
	m, dyn := newClaimManager(t)

	first, err := m.Acquire(ctx, "alice", "claims")
	if err != nil {
		t.Fatalf("first acquire: %v", err)
	}
	const oldAddr = "sandbox-1.instances.svc:22"
	if first.Instance.Addr != oldAddr {
		t.Fatalf("first addr = %q, want %q", first.Instance.Addr, oldAddr)
	}

	// The Sandbox behind the claim is deleted; the controller creates a replacement under a new name.
	// The claim keeps its name and UID, which is why Alive() still says "exists".
	backendtest.MoveSandbox(t, dyn, "instances", first.Instance.ID, "sandbox-7")

	deadBefore := m.DeadInstancesTotal()
	errorsBefore := m.LivenessErrorsTotal()

	second, err := m.Acquire(ctx, "alice", "claims")
	if err != nil {
		t.Fatalf("acquire after the sandbox was replaced: %v", err)
	}
	const newAddr = "sandbox-7.instances.svc:22"
	if second.Instance.Addr != newAddr {
		t.Fatalf("addr = %q, want the replacement's %q: the lease still hands out the deleted sandbox",
			second.Instance.Addr, newAddr)
	}
	if second.Instance.ID != first.Instance.ID {
		t.Fatalf("instance = %q, want the same claim %q: the claim is the stable handle",
			second.Instance.ID, first.Instance.ID)
	}
	if n := leaseCount(m); n != 1 {
		t.Fatalf("leases = %d, want 1: refreshing an address must not add a lease", n)
	}
	entry := m.Lookup("alice")
	if entry == nil {
		t.Fatal("no lease for alice after the refresh")
	}
	if entry.Instance.Addr != newAddr {
		t.Fatalf("the table still holds %q, want %q", entry.Instance.Addr, newAddr)
	}
	if entry.Refs() != 2 {
		t.Fatalf("refs = %d, want 2: the second connection joined the same lease", entry.Refs())
	}
	if delta := m.DeadInstancesTotal() - deadBefore; delta != 0 {
		t.Fatalf("byre_lease_dead_instance_total moved by %d; the instance was replaced, not lost", delta)
	}
	if delta := m.LivenessErrorsTotal() - errorsBefore; delta != 0 {
		t.Fatalf("byre_lease_liveness_errors_total moved by %d on a successful refresh", delta)
	}
}

// The claim between a deleted sandbox and its replacement advertises nothing. That is not "gone" and not
// a fresh address: the cached one is kept, the lease is kept, and the inconclusive answer is counted, so
// the next reconnect asks again instead of dialling "".
func TestAcquireKeepsTheCachedAddressWhenTheClaimAdvertisesNone(t *testing.T) {
	ctx := context.Background()
	m, dyn := newClaimManager(t)

	first, err := m.Acquire(ctx, "alice", "claims")
	if err != nil {
		t.Fatalf("first acquire: %v", err)
	}
	backendtest.ClearSandboxEndpoint(t, dyn, "instances", first.Instance.ID)

	deadBefore := m.DeadInstancesTotal()
	errorsBefore := m.LivenessErrorsTotal()

	second, err := m.Acquire(ctx, "alice", "claims")
	if err != nil {
		t.Fatalf("acquire with no advertised address: %v", err)
	}
	if second.Instance.Addr != first.Instance.Addr {
		t.Fatalf("addr = %q, want the cached %q: an unpublished replacement is not a reason to dial nothing",
			second.Instance.Addr, first.Instance.Addr)
	}
	if n := leaseCount(m); n != 1 {
		t.Fatalf("leases = %d, want 1", n)
	}
	if delta := m.DeadInstancesTotal() - deadBefore; delta != 0 {
		t.Fatalf("byre_lease_dead_instance_total moved by %d for a live claim", delta)
	}
	if delta := m.LivenessErrorsTotal() - errorsBefore; delta != 1 {
		t.Fatalf("byre_lease_liveness_errors_total moved by %d, want 1", delta)
	}
}

// The complement: a claim that really is deleted drops the lease and provisions a replacement, exactly as
// a deleted pod does. Without this, the refresh path could be "keep everything forever" and the test
// above would still pass.
func TestAcquireReplacesADeletedClaim(t *testing.T) {
	ctx := context.Background()
	m, dyn := newClaimManager(t)

	first, err := m.Acquire(ctx, "alice", "claims")
	if err != nil {
		t.Fatalf("first acquire: %v", err)
	}
	backendtest.DeleteClaim(t, dyn, "instances", first.Instance.ID)

	deadBefore := m.DeadInstancesTotal()
	second, err := m.Acquire(ctx, "alice", "claims")
	if err != nil {
		t.Fatalf("acquire after the claim was deleted: %v", err)
	}
	if second.Instance.ID == first.Instance.ID {
		t.Fatalf("acquire returned the deleted claim %q again", first.Instance.ID)
	}
	if second.Instance.Addr == first.Instance.Addr {
		t.Fatalf("acquire returned the deleted claim's address %q again", first.Instance.Addr)
	}
	if n := leaseCount(m); n != 1 {
		t.Fatalf("leases = %d, want 1: the dead row must be gone, not idle", n)
	}
	if delta := m.DeadInstancesTotal() - deadBefore; delta != 1 {
		t.Fatalf("byre_lease_dead_instance_total moved by %d, want 1", delta)
	}
}

// leaseCount is the table size the acceptance criteria talk about, read the same way the manager does.
func leaseCount(m *Manager) int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.byIdentity)
}
