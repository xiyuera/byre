package sshfront

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"golang.org/x/crypto/ssh"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"byre/internal/backend"
	"byre/internal/backend/backendtest"
	"byre/internal/lease"
)

// TestDialFailureOnDeletedInstanceReplacesTheLease reproduces the production failure end to end: the lease
// is bound to a pod the cluster has deleted, and the client's next connection is dialled at the address
// that went with it. One attempt must be enough to leave the identity on a different instance.
//
// Nothing here waits: the reaper never runs in this test and ttl is six minutes, so a lease that changed
// changed inside the connection attempt rather than because time passed.
func TestDialFailureOnDeletedInstanceReplacesTheLease(t *testing.T) {
	ctx := context.Background()
	drv, cs := backendtest.NewWorkloadDriver(t, "lab", "instances")
	var dials atomic.Int64
	var mu sync.Mutex
	var dialled []string
	h := startGatewayDial(t, "", drv, func(_ context.Context, in backend.Instance) (*ssh.Client, error) {
		dials.Add(1)
		mu.Lock()
		dialled = append(dialled, in.Addr)
		mu.Unlock()
		return nil, fmt.Errorf("dial %s: connect: connection refused", in.Addr)
	})

	first, err := h.manager.Acquire(ctx, h.identity, "lab")
	if err != nil {
		t.Fatalf("bind the first instance: %v", err)
	}
	// The test is not a connection: give the reference back so the baseline is "idle lease, no client".
	h.manager.ReleaseConn(h.identity)
	backendtest.DeletePod(t, cs, "instances", first.Instance.ID)

	deadBefore := h.manager.DeadInstancesTotal()

	c, err := h.dial(t, h.signer)
	if err != nil {
		t.Fatalf("authentication should succeed: %v", err)
	}
	defer c.Close()
	if _, err := c.NewSession(); err == nil {
		t.Fatal("a session was opened against a deleted instance")
	}

	newID, newAddr := waitForReplacement(t, h, first.Instance.ID)
	if newAddr == first.Instance.Addr {
		t.Fatalf("the replacement carries the deleted instance's address %q", newAddr)
	}
	if delta := h.manager.DeadInstancesTotal() - deadBefore; delta != 1 {
		t.Fatalf("byre_lease_dead_instance_total moved by %d, want 1", delta)
	}
	// The dead address must not even be attempted: the reuse check answers before the dial, so the client
	// is not handed to an address that has been proved empty. The one dial that remains is the replacement's,
	// which fails here because nothing listens on it.
	mu.Lock()
	tried := append([]string(nil), dialled...)
	mu.Unlock()
	for _, addr := range tried {
		if addr == first.Instance.Addr {
			t.Fatalf("the deleted instance's address %q was dialled after it was proved gone (attempts: %v)",
				addr, tried)
		}
	}
	if n := dials.Load(); n != 1 {
		t.Fatalf("dialled %d time(s) (%v), want 1: the replacement only", n, tried)
	}
	active, idle := h.manager.Stats()
	if active != 0 || idle != 1 {
		t.Fatalf("lease table = (%d active, %d idle), want (0, 1): the replacement lease is idle after the failed dial",
			active, idle)
	}
	t.Logf("dead instance %s -> replacement %s in one connection attempt", first.Instance.ID, newID)
}

// TestClientSessionContinuesOnTheReplacementInstance is the half of the incident the operator could see.
//
// The client authenticated, then requested a session. Its instance had been deleted. With the replacement
// dialled inside the same connection attempt, that session request is answered by the new instance: the
// client does not see a closed connection at all, it sees a session. Before the fix the connection was
// closed before any session existed, which is the "closed by remote host" in the report.
func TestClientSessionContinuesOnTheReplacementInstance(t *testing.T) {
	ctx := context.Background()
	drv, cs := backendtest.NewWorkloadDriver(t, "lab", "instances")

	target := startFakeTarget(t, "replacement-1")
	_, poolPriv := newKeyPair(t)
	poolKey := writeKeyFile(t, poolPriv)

	var deadAddr atomic.Value
	h := startGatewayDial(t, "", drv, func(ctx context.Context, in backend.Instance) (*ssh.Client, error) {
		if a, ok := deadAddr.Load().(string); ok && a == in.Addr {
			return nil, fmt.Errorf("dial %s: connect: connection refused", in.Addr)
		}
		// Every other instance is reachable — it is the same fake sshd, which is enough to prove the
		// client's session landed somewhere that answers.
		return DialInstance(ctx, backend.Instance{
			ID:            in.ID,
			Addr:          target,
			User:          "sandbox",
			PrivateKey:    poolKey,
			HostKeyPolicy: "insecure",
		})
	})

	first, err := h.manager.Acquire(ctx, h.identity, "lab")
	if err != nil {
		t.Fatalf("bind the first instance: %v", err)
	}
	h.manager.ReleaseConn(h.identity)
	deadAddr.Store(first.Instance.Addr)
	backendtest.DeletePod(t, cs, "instances", first.Instance.ID)

	c, err := h.dial(t, h.signer)
	if err != nil {
		t.Fatalf("authentication should succeed: %v", err)
	}
	defer c.Close()

	// The fake sshd answers every exec with its own name, so the output says which instance served the
	// command — the replacement, not the deleted pod.
	if out := runCommand(t, c, "echo alive"); out != "replacement-1\n" {
		t.Fatalf("the session was not served by the replacement instance: %q", out)
	}

	entry := h.manager.Lookup(h.identity)
	if entry == nil {
		t.Fatal("no lease after the session ended")
	}
	if entry.Instance.ID == first.Instance.ID {
		t.Fatalf("the session ran while the lease still named the deleted instance %q", first.Instance.ID)
	}
}

// TestInstanceDeletedBetweenLeaseAndDialIsReplaced covers the race the check at lease time cannot close: the
// object was there when the lease was validated and gone by the time the dial happened. The dial path has to
// ask again, and act on the answer.
//
// The fixture deletes the pod from inside the dialler, which is the only place where that ordering is exact
// rather than likely.
func TestInstanceDeletedBetweenLeaseAndDialIsReplaced(t *testing.T) {
	ctx := context.Background()
	drv, cs := backendtest.NewWorkloadDriver(t, "lab", "instances")

	var dials atomic.Int64
	var once sync.Once
	h := startGatewayDial(t, "", drv, func(ctx context.Context, in backend.Instance) (*ssh.Client, error) {
		dials.Add(1)
		// The first dial is the one the lease validated; the cluster deletes that pod just before the
		// connection is refused, which is exactly the window between the check and the dial.
		once.Do(func() {
			_ = cs.CoreV1().Pods("instances").Delete(ctx, in.ID, metav1.DeleteOptions{})
		})
		return nil, fmt.Errorf("dial %s: connect: connection refused", in.Addr)
	})

	first, err := h.manager.Acquire(ctx, h.identity, "lab")
	if err != nil {
		t.Fatalf("bind the first instance: %v", err)
	}
	h.manager.ReleaseConn(h.identity)

	deadBefore := h.manager.DeadInstancesTotal()

	c, err := h.dial(t, h.signer)
	if err != nil {
		t.Fatalf("authentication should succeed: %v", err)
	}
	defer c.Close()
	if _, err := c.NewSession(); err == nil {
		t.Fatal("a session was opened against an instance that was deleted mid-dial")
	}

	entry := h.manager.Lookup(h.identity)
	if entry == nil {
		t.Fatal("no lease after a dial failure: the replacement was not provisioned")
	}
	if entry.Instance.ID == first.Instance.ID {
		t.Fatalf("the instance deleted during the dial is still leased (%q): the dial path did not ask", first.Instance.ID)
	}
	if entry.Instance.Addr == first.Instance.Addr {
		t.Fatalf("the lease still carries the deleted address %q", first.Instance.Addr)
	}
	if _, err := cs.CoreV1().Pods("instances").Get(ctx, entry.Instance.ID, metav1.GetOptions{}); err != nil {
		t.Fatalf("the replacement instance %q does not exist: %v", entry.Instance.ID, err)
	}
	if delta := h.manager.DeadInstancesTotal() - deadBefore; delta != 1 {
		t.Fatalf("byre_lease_dead_instance_total moved by %d, want 1", delta)
	}
	if n := dials.Load(); n != 2 {
		t.Fatalf("dialled %d time(s), want 2: the dead instance, then the replacement", n)
	}
	if active, idle := h.manager.Stats(); active != 0 || idle != 1 {
		t.Fatalf("lease table = (%d active, %d idle), want (0, 1)", active, idle)
	}
}

// TestDialFailureOnLiveInstanceKeepsTheLease is the other half of the classification. Same dial error, an
// instance that still exists: the lease must survive, because the object is intact and replacing it would
// throw away the work the client is entitled to find there. The attempt is counted as unreachable so the
// two cases stay distinguishable in /metrics.
func TestDialFailureOnLiveInstanceKeepsTheLease(t *testing.T) {
	ctx := context.Background()
	drv, _ := backendtest.NewWorkloadDriver(t, "lab", "instances")
	var dials atomic.Int64
	h := startGatewayDial(t, "", drv, func(_ context.Context, in backend.Instance) (*ssh.Client, error) {
		dials.Add(1)
		return nil, fmt.Errorf("dial %s: connect: connection refused", in.Addr)
	})

	first, err := h.manager.Acquire(ctx, h.identity, "lab")
	if err != nil {
		t.Fatalf("bind the first instance: %v", err)
	}
	h.manager.ReleaseConn(h.identity)

	deadBefore := h.manager.DeadInstancesTotal()

	c, err := h.dial(t, h.signer)
	if err != nil {
		t.Fatalf("authentication should succeed: %v", err)
	}
	defer c.Close()
	if _, err := c.NewSession(); err == nil {
		t.Fatal("a session was opened against an unreachable instance")
	}

	entry := waitForIdleLease(t, h)
	if entry.Instance.ID != first.Instance.ID {
		t.Fatalf("a live instance was replaced: %q then %q", first.Instance.ID, entry.Instance.ID)
	}
	if entry.Instance.Addr != first.Instance.Addr {
		t.Fatalf("the lease address changed for a live instance: %q then %q", first.Instance.Addr, entry.Instance.Addr)
	}
	if n := dials.Load(); n != 1 {
		t.Fatalf("dialled %d times, want 1: a live instance must not be retried behind the client's back", n)
	}
	if delta := h.manager.DeadInstancesTotal() - deadBefore; delta != 0 {
		t.Fatalf("byre_lease_dead_instance_total moved by %d for a live instance", delta)
	}
}

// waitForReplacement waits for the identity to be leased to something other than oldID with no open
// connection. The front end completes the swap before it closes the client, so this covers scheduling only;
// a lease that is still the old one at the deadline is a failure, not a slow test.
func waitForReplacement(t *testing.T, h *harness, oldID string) (string, string) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if e := h.manager.Lookup(h.identity); e != nil && e.Instance.ID != oldID && e.Refs() == 0 {
			return e.Instance.ID, e.Instance.Addr
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("the deleted instance %q is still leased after one connection attempt: %+v",
		oldID, h.manager.Lookup(h.identity))
	return "", ""
}

// waitForIdleLease waits for the connection reference to be returned, and reports the entry.
func waitForIdleLease(t *testing.T, h *harness) *lease.Entry {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if e := h.manager.Lookup(h.identity); e != nil && e.Refs() == 0 {
			return e
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("the connection reference was never returned: %+v", h.manager.Lookup(h.identity))
	return nil
}
