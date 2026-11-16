package lease

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"keypod/internal/backend"
)

func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

func TestWarmFillsPoolToSize(t *testing.T) {
	m, fb, _ := newManager(t)
	m.SetPool("lab", 2, fb)

	if err := m.Warm(context.Background()); err != nil {
		t.Fatalf("warm: %v", err)
	}
	if got := m.WarmSize("lab"); got != 2 {
		t.Fatalf("warm size = %d, want 2", got)
	}
	if fb.creates != 2 {
		t.Fatalf("Create called %d times, want 2", fb.creates)
	}
	if fb.acquires != 0 {
		t.Fatalf("pool warming used Acquire (%d calls); it must use Create", fb.acquires)
	}
}

func TestAcquireTakesFromPoolAndRefills(t *testing.T) {
	m, fb, _ := newManager(t)
	m.SetPool("lab", 1, fb)
	if err := m.Warm(context.Background()); err != nil {
		t.Fatalf("warm: %v", err)
	}

	e, err := m.Acquire(context.Background(), "id-a", "lab")
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}
	if fb.acquires != 0 {
		t.Fatalf("a warm instance was available but Acquire was used (%d times)", fb.acquires)
	}
	if e.Instance.ID != "lab-warm-1" {
		t.Fatalf("instance = %q, want the warm one (lab-warm-1)", e.Instance.ID)
	}

	// The pool must refill in the background after the checkout.
	waitFor(t, "pool refill", func() bool { return m.WarmSize("lab") == 1 })
	if fb.creates != 2 {
		t.Fatalf("Create called %d times, want 2 (initial + refill)", fb.creates)
	}
}

func TestAcquireFallsBackToDriverWhenPoolEmpty(t *testing.T) {
	m, fb, _ := newManager(t)
	m.SetPool("lab", 1, fb)
	// No Warm call: the pool starts empty.
	if _, err := m.Acquire(context.Background(), "id-a", "lab"); err != nil {
		t.Fatalf("acquire: %v", err)
	}
	if fb.acquires != 1 {
		t.Fatalf("Acquire called %d times, want 1", fb.acquires)
	}
}

func TestPoolDisabledWhenSizeZero(t *testing.T) {
	m, fb, _ := newManager(t)
	m.SetPool("lab", 0, fb)
	if err := m.Warm(context.Background()); err != nil {
		t.Fatalf("warm: %v", err)
	}
	if got := m.WarmSize("lab"); got != 0 {
		t.Fatalf("warm size = %d, want 0", got)
	}
	if fb.creates != 0 {
		t.Fatalf("Create called %d times with a disabled pool", fb.creates)
	}
}

func TestPoolNeverOvershootsSize(t *testing.T) {
	m, fb, _ := newManager(t)
	m.SetPool("lab", 2, fb)
	ctx := context.Background()
	for i := 0; i < 5; i++ {
		if err := m.Warm(ctx); err != nil {
			t.Fatalf("warm #%d: %v", i, err)
		}
	}
	if got := m.WarmSize("lab"); got != 2 {
		t.Fatalf("warm size = %d after repeated Warm, want 2", got)
	}
	if fb.creates != 2 {
		t.Fatalf("Create called %d times, want 2", fb.creates)
	}
}

func TestReapReleasesAndRefills(t *testing.T) {
	m, fb, c := newManager(t)
	m.SetPool("lab", 1, fb)
	ctx := context.Background()
	if err := m.Warm(ctx); err != nil {
		t.Fatalf("warm: %v", err)
	}

	if _, err := m.Acquire(ctx, "id-a", "lab"); err != nil {
		t.Fatalf("acquire: %v", err)
	}
	m.ReleaseConn("id-a")

	c.advance(420 * time.Second)
	if n, err := m.Reap(ctx); err != nil || n != 1 {
		t.Fatalf("reap: n=%d err=%v", n, err)
	}
	if len(fb.released) != 1 {
		t.Fatalf("released = %v, want one instance", fb.released)
	}
	waitFor(t, "pool refill after reap", func() bool { return m.WarmSize("lab") == 1 })
}

func TestAdoptRebuildsIdentityTable(t *testing.T) {
	m, fb, _ := newManager(t)
	ctx := context.Background()

	fb.list = []backend.Instance{
		{
			ID: "lab-1", Addr: "127.0.0.1:22", User: "sandbox", PrivateKey: "/keys/pool",
			HostKeyPolicy: "accept-new",
			Annotations:   map[string]string{backend.LabelIdentity: "id-a"},
		},
		{
			// A warm instance with no identity must not become a lease.
			ID: "lab-2", Addr: "127.0.0.1:22", User: "sandbox", PrivateKey: "/keys/pool",
			HostKeyPolicy: "accept-new",
			Annotations:   map[string]string{},
		},
	}

	n, err := m.Adopt(ctx)
	if err != nil {
		t.Fatalf("adopt: %v", err)
	}
	if n != 1 {
		t.Fatalf("adopted %d leases, want 1", n)
	}
	e := m.Lookup("id-a")
	if e == nil || e.Instance.ID != "lab-1" {
		t.Fatalf("adopted entry = %+v", e)
	}
	if m.AdoptedTotal() != 1 {
		t.Fatalf("AdoptedTotal = %d, want 1", m.AdoptedTotal())
	}

	// The identity keeps using the adopted instance, and the driver is not asked for a new one.
	again, err := m.Acquire(ctx, "id-a", "lab")
	if err != nil {
		t.Fatalf("acquire after adopt: %v", err)
	}
	if again.Instance.ID != "lab-1" {
		t.Fatalf("instance after restart = %q, want lab-1", again.Instance.ID)
	}
	if fb.acquires != 0 {
		t.Fatalf("driver Acquire called %d times after adoption", fb.acquires)
	}
}

func TestAdoptReportsListFailureButKeepsGoing(t *testing.T) {
	m, fb, _ := newManager(t)
	fb.listErr = errors.New("api down")
	n, err := m.Adopt(context.Background())
	if err == nil || !strings.Contains(err.Error(), "api down") {
		t.Fatalf("err = %v, want the list failure surfaced", err)
	}
	if n != 0 {
		t.Fatalf("adopted %d with a failing driver", n)
	}
}
