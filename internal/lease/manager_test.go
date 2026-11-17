package lease

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"testing"
	"time"

	"byre/internal/backend"
	"byre/internal/metrics"
)

type fakeBackend struct {
	name      string
	acquires  int
	creates   int
	released  []string
	lastIdent string
	failWith  error
	list      []backend.Instance
	listErr   error
}

func (f *fakeBackend) Name() string { return f.name }

func (f *fakeBackend) Acquire(_ context.Context, identity string) (backend.Instance, error) {
	if f.failWith != nil {
		return backend.Instance{}, f.failWith
	}
	f.acquires++
	f.lastIdent = identity
	return backend.Instance{
		ID:            fmt.Sprintf("%s-%d", f.name, f.acquires),
		Addr:          "127.0.0.1:22",
		User:          "sandbox",
		PrivateKey:    "/keys/pool",
		HostKeyPolicy: "accept-new",
		Annotations:   map[string]string{"identity": identity},
	}, nil
}

// Create makes fakeBackend a Pooler, so pool tests can use the same double.
func (f *fakeBackend) Create(_ context.Context) (backend.Instance, error) {
	if f.failWith != nil {
		return backend.Instance{}, f.failWith
	}
	f.creates++
	return backend.Instance{
		ID:            fmt.Sprintf("%s-warm-%d", f.name, f.creates),
		Addr:          "127.0.0.1:22",
		User:          "sandbox",
		PrivateKey:    "/keys/pool",
		HostKeyPolicy: "accept-new",
		Annotations:   map[string]string{},
	}, nil
}

func (f *fakeBackend) Release(_ context.Context, in backend.Instance) error {
	f.released = append(f.released, in.ID)
	return nil
}

func (f *fakeBackend) List(context.Context) ([]backend.Instance, error) {
	return f.list, f.listErr
}
func (f *fakeBackend) Adopt(context.Context, []backend.Instance) error { return nil }

type clock struct{ t time.Time }

func (c *clock) now() time.Time          { return c.t }
func (c *clock) advance(d time.Duration) { c.t = c.t.Add(d) }

func newManager(t *testing.T) (*Manager, *fakeBackend, *clock) {
	t.Helper()
	fb := &fakeBackend{name: "lab"}
	m, err := New(360*time.Second, 60*time.Second, map[string]backend.Backend{"lab": fb})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	c := &clock{t: time.Date(2026, 11, 17, 0, 0, 0, 0, time.UTC)}
	m.SetClock(c.now)
	return m, fb, c
}

func TestAcquireIsStablePerIdentity(t *testing.T) {
	m, fb, _ := newManager(t)
	ctx := context.Background()

	a1, err := m.Acquire(ctx, "id-a", "lab")
	if err != nil {
		t.Fatalf("acquire: %v", err)
	}
	a2, err := m.Acquire(ctx, "id-a", "lab")
	if err != nil {
		t.Fatalf("acquire again: %v", err)
	}
	if a1.Instance.ID != a2.Instance.ID {
		t.Fatalf("same identity got %q then %q", a1.Instance.ID, a2.Instance.ID)
	}
	if fb.acquires != 1 {
		t.Fatalf("backend acquired %d times, want 1", fb.acquires)
	}
	if a2.Refs() != 2 {
		t.Fatalf("refs = %d, want 2", a2.Refs())
	}
}

func TestLeaseSurvivesDisconnectWithinWindow(t *testing.T) {
	m, fb, c := newManager(t)
	ctx := context.Background()

	if _, err := m.Acquire(ctx, "id-a", "lab"); err != nil {
		t.Fatalf("acquire: %v", err)
	}
	m.ReleaseConn("id-a")

	c.advance(359 * time.Second)
	if n, err := m.Reap(ctx); err != nil || n != 0 {
		t.Fatalf("reap early: n=%d err=%v", n, err)
	}
	// A reconnect must return the very same instance.
	again, err := m.Acquire(ctx, "id-a", "lab")
	if err != nil {
		t.Fatalf("reacquire: %v", err)
	}
	if again.Instance.ID != "lab-1" {
		t.Fatalf("reconnect got %q, want lab-1", again.Instance.ID)
	}
	if fb.acquires != 1 {
		t.Fatalf("backend acquired %d times, want 1", fb.acquires)
	}
}

func TestReapReleasesAfterWindow(t *testing.T) {
	m, fb, c := newManager(t)
	ctx := context.Background()

	if _, err := m.Acquire(ctx, "id-a", "lab"); err != nil {
		t.Fatalf("acquire: %v", err)
	}
	m.ReleaseConn("id-a")
	c.advance(420 * time.Second) // ttl 360 + grace 60

	n, err := m.Reap(ctx)
	if err != nil {
		t.Fatalf("reap: %v", err)
	}
	if n != 1 {
		t.Fatalf("reaped %d, want 1", n)
	}
	if len(fb.released) != 1 || fb.released[0] != "lab-1" {
		t.Fatalf("released = %v, want [lab-1]", fb.released)
	}
	if e := m.Lookup("id-a"); e != nil {
		t.Fatalf("lease still present after reap: %+v", e)
	}
}

func TestReapSkipsActiveLeases(t *testing.T) {
	m, fb, c := newManager(t)
	ctx := context.Background()

	if _, err := m.Acquire(ctx, "id-a", "lab"); err != nil {
		t.Fatalf("acquire: %v", err)
	}
	c.advance(10 * time.Minute)
	if n, _ := m.Reap(ctx); n != 0 {
		t.Fatalf("reaped an active lease (n=%d)", n)
	}
	if len(fb.released) != 0 {
		t.Fatalf("released an active instance: %v", fb.released)
	}
}

func TestUnknownBackendAndMismatch(t *testing.T) {
	m, _, _ := newManager(t)
	ctx := context.Background()

	if _, err := m.Acquire(ctx, "id-a", "nope"); err == nil || !strings.Contains(err.Error(), "unknown backend") {
		t.Fatalf("err = %v, want unknown backend", err)
	}
	if _, err := m.Acquire(ctx, "id-a", "lab"); err != nil {
		t.Fatalf("acquire: %v", err)
	}
	if _, err := m.Acquire(ctx, "id-a", "other"); err == nil || !strings.Contains(err.Error(), "already leased") {
		t.Fatalf("err = %v, want already-leased refusal", err)
	}
}

func TestStatsCountsActiveAndIdle(t *testing.T) {
	m, _, _ := newManager(t)
	ctx := context.Background()

	if _, err := m.Acquire(ctx, "id-a", "lab"); err != nil {
		t.Fatalf("acquire a: %v", err)
	}
	if _, err := m.Acquire(ctx, "id-b", "lab"); err != nil {
		t.Fatalf("acquire b: %v", err)
	}
	m.ReleaseConn("id-b")

	active, idle := m.Stats()
	if active != 1 || idle != 1 {
		t.Fatalf("stats = (%d active, %d idle), want (1, 1)", active, idle)
	}
}

// staleBackend is fakeBackend plus the StaleCapacity promise: the honest statement for a driver that
// creates every instance itself (kubernetes, static), where an unannotated instance really is capacity
// nobody holds.
type staleBackend struct{ *fakeBackend }

func (s *staleBackend) AnonymousInstancesAreStale() bool { return true }

// adoptionList is the restart from the bug report: vm-live carries byre.io/identity (an agent is using
// it), vm-adopted carries no annotation at all because the lister predates the annotation contract.
func adoptionList() []backend.Instance {
	return []backend.Instance{
		{
			ID: "vm-live", Addr: "127.0.0.1:22022", User: "u", PrivateKey: "/k", HostKeyPolicy: "accept-new",
			Annotations: map[string]string{backend.LabelIdentity: "fp-abc"},
		},
		{
			ID: "vm-adopted", Addr: "127.0.0.1:22023", User: "u", PrivateKey: "/k", HostKeyPolicy: "accept-new",
		},
	}
}

// TestAdoptKeepsAnonymousInstancesOfADriverThatCannotJudgeThem is the regression test for the blocker:
// a command-backend lister that reports no annotation used to have every instance destroyed at startup,
// including the one an agent was still connected to. A restart must not destroy what it cannot identify.
func TestAdoptKeepsAnonymousInstancesOfADriverThatCannotJudgeThem(t *testing.T) {
	fb := &fakeBackend{name: "lab", list: adoptionList()}
	m, err := New(360*time.Second, 60*time.Second, map[string]backend.Backend{"lab": fb})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	n, err := m.Adopt(context.Background())
	if err != nil {
		t.Fatalf("adopt: %v", err)
	}
	if n != 1 {
		t.Fatalf("adopted %d, want 1 (the annotated instance)", n)
	}
	if len(fb.released) != 0 {
		t.Fatalf("startup destroyed %v; an instance without an annotation is not provably stale", fb.released)
	}
	if e := m.Lookup("fp-abc"); e == nil || e.Instance.ID != "vm-live" {
		t.Fatalf("annotated instance was not adopted: %+v", e)
	}
	if e := m.Lookup(""); e != nil {
		t.Fatalf("an anonymous instance was filed under an empty identity: %+v", e)
	}
}

// TestAdoptReleasesAnonymousInstancesWhenTheDriverSaysTheyAreStale keeps the old cleanup for the drivers
// that earned it: kubernetes labels everything it creates, so an unannotated object really is leftover
// warm capacity and the pool is refilled right after.
func TestAdoptReleasesAnonymousInstancesWhenTheDriverSaysTheyAreStale(t *testing.T) {
	fb := &fakeBackend{name: "lab", list: adoptionList()}
	m, err := New(360*time.Second, 60*time.Second, map[string]backend.Backend{"lab": &staleBackend{fakeBackend: fb}})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	n, err := m.Adopt(context.Background())
	if err != nil {
		t.Fatalf("adopt: %v", err)
	}
	if n != 1 {
		t.Fatalf("adopted %d, want 1", n)
	}
	if len(fb.released) != 1 || fb.released[0] != "vm-adopted" {
		t.Fatalf("released = %v, want [vm-adopted] (and never the annotated instance)", fb.released)
	}
	if e := m.Lookup("fp-abc"); e == nil || e.Instance.ID != "vm-live" {
		t.Fatalf("annotated instance was not adopted: %+v", e)
	}
}

// flakyRelease fails the first failures release attempts, then hands over to the fake.
type flakyRelease struct {
	*fakeBackend
	failures int
	attempts int
}

func (f *flakyRelease) Release(ctx context.Context, in backend.Instance) error {
	f.attempts++
	if f.attempts <= f.failures {
		return errors.New("api server unavailable")
	}
	return f.fakeBackend.Release(ctx, in)
}

func newFlakyManager(t *testing.T, failures int) (*Manager, *flakyRelease, *fakeBackend, *clock) {
	t.Helper()
	fb := &fakeBackend{name: "lab"}
	flaky := &flakyRelease{fakeBackend: fb, failures: failures}
	m, err := New(360*time.Second, 60*time.Second, map[string]backend.Backend{"lab": flaky})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	c := &clock{t: time.Date(2026, 11, 17, 0, 0, 0, 0, time.UTC)}
	m.SetClock(c.now)
	return m, flaky, fb, c
}

// TestReapRetriesFailedRelease: a delete can fail transiently, and the previous version did not retry,
// did not log and did not count — the instance simply stayed behind while a replacement was created.
func TestReapRetriesFailedRelease(t *testing.T) {
	m, flaky, fb, c := newFlakyManager(t, 2)
	ctx := context.Background()

	if _, err := m.Acquire(ctx, "id-a", "lab"); err != nil {
		t.Fatalf("acquire: %v", err)
	}
	m.ReleaseConn("id-a")
	c.advance(420 * time.Second)

	if _, err := m.Reap(ctx); err != nil {
		t.Fatalf("reap: %v", err)
	}
	if flaky.attempts != 3 {
		t.Fatalf("release attempts = %d, want 3 (two failures then success)", flaky.attempts)
	}
	if len(fb.released) != 1 || fb.released[0] != "lab-1" {
		t.Fatalf("released = %v, want [lab-1]", fb.released)
	}
}

// TestReapReportsPermanentReleaseFailure: when every attempt fails the error must surface and be counted,
// because that is the only trace an orphan instance leaves.
func TestReapReportsPermanentReleaseFailure(t *testing.T) {
	m, flaky, fb, c := newFlakyManager(t, 99)
	ctx := context.Background()

	if _, err := m.Acquire(ctx, "id-a", "lab"); err != nil {
		t.Fatalf("acquire: %v", err)
	}
	m.ReleaseConn("id-a")
	c.advance(420 * time.Second)

	before := metrics.ReleaseErrorsTotal()
	n, err := m.Reap(ctx)
	if n != 1 {
		t.Fatalf("reaped = %d, want 1", n)
	}
	if err == nil {
		t.Fatal("reap returned no error although every release attempt failed")
	}
	if flaky.attempts != releaseAttempts {
		t.Fatalf("release attempts = %d, want %d", flaky.attempts, releaseAttempts)
	}
	if got := metrics.ReleaseErrorsTotal() - before; got != int64(releaseAttempts) {
		t.Fatalf("release_errors_total delta = %d, want %d", got, releaseAttempts)
	}
	if len(fb.released) != 0 {
		t.Fatalf("a failing release must not be followed by a refill path that reports success: %v", fb.released)
	}
}

// TestAdoptedPendingIsReportedSeparately: an adopted lease has refs == 0, so leases_active alone can never
// show it. The post-restart check in docs/operations.md compares active + adopted_pending.
func TestAdoptedPendingIsReportedSeparately(t *testing.T) {
	fb := &fakeBackend{name: "lab"}
	fb.list = []backend.Instance{{
		ID: "lab-existing", Addr: "127.0.0.1:22", User: "sandbox",
		PrivateKey: "/keys/pool", HostKeyPolicy: "accept-new",
		Annotations: map[string]string{backend.LabelIdentity: "id-adopted"},
	}}
	m, err := New(360*time.Second, 60*time.Second, map[string]backend.Backend{"lab": fb})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	c := &clock{t: time.Date(2026, 11, 17, 0, 0, 0, 0, time.UTC)}
	m.SetClock(c.now)

	if n, err := m.Adopt(context.Background()); err != nil || n != 1 {
		t.Fatalf("adopt: n=%d err=%v", n, err)
	}
	active, idle := m.Stats()
	if active != 0 || idle != 1 {
		t.Fatalf("stats = (%d active, %d idle), want (0, 1) for an adopted lease", active, idle)
	}
	if got := m.AdoptedPending(); got != 1 {
		t.Fatalf("adopted_pending = %d, want 1", got)
	}
	// Once the client reconnects it is an ordinary active lease.
	if _, err := m.Acquire(context.Background(), "id-adopted", "lab"); err != nil {
		t.Fatalf("reacquire: %v", err)
	}
	if got := m.AdoptedPending(); got != 0 {
		t.Fatalf("adopted_pending = %d after a connection, want 0", got)
	}
}

// TestFillStateDecidesReadiness: /readyz must be red only when the gateway cannot provision at all.
func TestFillStateDecidesReadiness(t *testing.T) {
	fb := &fakeBackend{name: "lab"}
	m, err := New(360*time.Second, 60*time.Second, map[string]backend.Backend{"lab": fb})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if !m.FillState().CanProvision() {
		t.Fatal("a gateway with no pool configured must be ready: nothing to warm")
	}

	m.SetPool("lab", 2, fb)
	if m.FillState().CanProvision() {
		t.Fatal("a pool that has never been filled must not report ready")
	}

	// A backend that cannot create anything must stay red, and say so in the counter.
	fb.failWith = errors.New("quota exceeded")
	before := metrics.PoolFillErrorsTotal()
	if err := m.Warm(context.Background()); err == nil {
		t.Fatal("warm reported success while every create failed")
	}
	if got := metrics.PoolFillErrorsTotal() - before; got != 2 {
		t.Fatalf("pool_fill_errors_total delta = %d, want 2", got)
	}
	if m.FillState().CanProvision() {
		t.Fatal("an empty pool that cannot create must not report ready")
	}

	// Recovery: the very next successful fill makes the gateway ready again.
	fb.failWith = nil
	if err := m.Warm(context.Background()); err != nil {
		t.Fatalf("warm after recovery: %v", err)
	}
	if !m.FillState().CanProvision() {
		t.Fatal("a warm pool must report ready")
	}
}
