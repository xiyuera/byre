package lease

import (
	"context"
	"errors"
	"testing"
	"time"

	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime"
	ktesting "k8s.io/client-go/testing"

	"byre/internal/backend"
	"byre/internal/backend/backendtest"
)

// TestAcquireReplacesAnInstanceThatWasDeleted is the acceptance test for the incident: a lease exists, the
// object behind it is deleted, and the next Acquire for the same identity must return a *new* instance
// instead of the address that no longer answers.
//
// The table assertion is the load-bearing one. A gateway that replaced the address but kept the entry
// would pass a naive "the address changed" check while still reporting two leases for one identity —
// which is what would leak the object and confuse the idle counters.
func TestAcquireReplacesAnInstanceThatWasDeleted(t *testing.T) {
	ctx := context.Background()
	drv, cs := backendtest.NewWorkloadDriver(t, "k8s", "instances")
	m, err := New(6*time.Minute, time.Minute, map[string]backend.Backend{"k8s": drv})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	first, err := m.Acquire(ctx, "alice", "k8s")
	if err != nil {
		t.Fatalf("first acquire: %v", err)
	}
	oldID, oldAddr := first.Instance.ID, first.Instance.Addr

	// The cluster deletes the pod. Nothing is left behind to compare against: the object is simply gone.
	backendtest.DeletePod(t, cs, "instances", oldID)

	deadBefore := m.DeadInstancesTotal()
	second, err := m.Acquire(ctx, "alice", "k8s")
	if err != nil {
		t.Fatalf("acquire after the instance was deleted: %v", err)
	}
	if second.Instance.ID == oldID {
		t.Fatalf("acquire returned the deleted instance %q again", oldID)
	}
	if second.Instance.Addr == oldAddr {
		t.Fatalf("acquire returned the deleted instance's address %q again", oldAddr)
	}

	// The old entry is gone from the table, not merely replaced in the value: exactly one lease for this
	// identity, and it is active.
	entry := m.Lookup("alice")
	if entry == nil {
		t.Fatal("no lease for alice after the replacement")
	}
	if entry.Instance.ID != second.Instance.ID {
		t.Fatalf("lease points at %q, want the replacement %q", entry.Instance.ID, second.Instance.ID)
	}
	active, idle := m.Stats()
	if active != 1 || idle != 0 {
		t.Fatalf("lease table = (%d active, %d idle), want (1, 0): the dead entry must be gone, not idle",
			active, idle)
	}
	if entry.Refs() != 1 {
		t.Fatalf("refs = %d, want 1: the stale reference went with the dropped entry", entry.Refs())
	}
	if delta := m.DeadInstancesTotal() - deadBefore; delta != 1 {
		t.Fatalf("byre_lease_dead_instance_total moved by %d, want 1", delta)
	}
}

// TestAcquireReusesALiveInstance is the control. Without it, a "check" that always reported the instance as
// gone would pass the test above while turning every reconnect into a new pod — the exact data loss the
// lease exists to prevent.
func TestAcquireReusesALiveInstance(t *testing.T) {
	ctx := context.Background()
	drv, _ := backendtest.NewWorkloadDriver(t, "k8s", "instances")
	m, err := New(6*time.Minute, time.Minute, map[string]backend.Backend{"k8s": drv})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	first, err := m.Acquire(ctx, "alice", "k8s")
	if err != nil {
		t.Fatalf("first acquire: %v", err)
	}
	deadBefore := m.DeadInstancesTotal()

	second, err := m.Acquire(ctx, "alice", "k8s")
	if err != nil {
		t.Fatalf("second acquire: %v", err)
	}
	if second.Instance.ID != first.Instance.ID || second.Instance.Addr != first.Instance.Addr {
		t.Fatalf("a live lease was replaced: %q/%q then %q/%q",
			first.Instance.ID, first.Instance.Addr, second.Instance.ID, second.Instance.Addr)
	}
	if second.Refs() != 2 {
		t.Fatalf("refs = %d, want 2", second.Refs())
	}
	if delta := m.DeadInstancesTotal() - deadBefore; delta != 0 {
		t.Fatalf("byre_lease_dead_instance_total moved by %d for a live instance", delta)
	}
}

// TestAcquireReusesADriverWithoutLiveness pins the behaviour for drivers that have no object to ask: a
// static host list or a provisioning script cannot answer "is it still there", so a lease over one behaves
// exactly as it did before this change. Making that explicit keeps the new check from silently becoming a
// requirement for every driver.
func TestAcquireReusesADriverWithoutLiveness(t *testing.T) {
	ctx := context.Background()
	fb := &fakeBackend{name: "lab"}
	m, err := New(6*time.Minute, time.Minute, map[string]backend.Backend{"lab": fb})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	first, err := m.Acquire(ctx, "alice", "lab")
	if err != nil {
		t.Fatalf("first acquire: %v", err)
	}
	second, err := m.Acquire(ctx, "alice", "lab")
	if err != nil {
		t.Fatalf("second acquire: %v", err)
	}
	if second.Instance.ID != first.Instance.ID {
		t.Fatalf("a driver without Liveness had its lease replaced: %q then %q", first.Instance.ID, second.Instance.ID)
	}
	if fb.acquires != 1 {
		t.Fatalf("backend acquired %d times, want 1", fb.acquires)
	}
}

// TestInconclusiveLivenessProbeKeepsTheLease pins the third outcome. "The apiserver did not answer" is not
// "the instance is gone": dropping the lease there would delete nothing (the object is fine) but would
// provision a second instance and strand the client's files on the first.
func TestInconclusiveLivenessProbeKeepsTheLease(t *testing.T) {
	ctx := context.Background()
	drv, cs := backendtest.NewWorkloadDriver(t, "k8s", "instances")
	m, err := New(6*time.Minute, time.Minute, map[string]backend.Backend{"k8s": drv})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	first, err := m.Acquire(ctx, "alice", "k8s")
	if err != nil {
		t.Fatalf("first acquire: %v", err)
	}

	// Installed after the first acquire, which needs a working Get to wait for the pod to be ready.
	cs.PrependReactor("get", "pods", func(ktesting.Action) (bool, runtime.Object, error) {
		return true, nil, apierrors.NewInternalError(errors.New("apiserver is unreachable"))
	})

	probeErrors := m.LivenessErrorsTotal()
	deadBefore := m.DeadInstancesTotal()

	second, err := m.Acquire(ctx, "alice", "k8s")
	if err != nil {
		t.Fatalf("acquire with an inconclusive probe: %v", err)
	}
	if second.Instance.ID != first.Instance.ID {
		t.Fatalf("an inconclusive probe replaced a live instance: %q then %q", first.Instance.ID, second.Instance.ID)
	}
	if delta := m.DeadInstancesTotal() - deadBefore; delta != 0 {
		t.Fatalf("byre_lease_dead_instance_total moved by %d on an inconclusive probe", delta)
	}
	if delta := m.LivenessErrorsTotal() - probeErrors; delta != 1 {
		t.Fatalf("byre_lease_liveness_errors_total moved by %d, want 1", delta)
	}
}
