package lease

import (
	"context"
	"errors"
	"testing"

	"byre/internal/backend"
	"byre/internal/lease/crd"
)

// annotated is one instance as a backend lists it, carrying the identity annotation the previous process
// wrote. Addr/User/Key are filled because Entry.Validate needs them to be adoptable.
func annotatedIn(id, identity string) backend.Instance {
	return backend.Instance{
		ID:            id,
		Addr:          "127.0.0.1:22",
		User:          "sandbox",
		PrivateKey:    "/keys/pool",
		HostKeyPolicy: "accept-new",
		Annotations:   map[string]string{backend.LabelIdentity: identity},
	}
}

// The point of the whole change: a restart hands back the instance each identity was using, instead of
// cold-starting everyone while the old instances are still running.
func TestRecoverRestoresBindingsFromTheStore(t *testing.T) {
	ctx := context.Background()
	m, fb, _ := newManager(t)
	st := &fakeStore{}
	m.SetStore(st)
	st.list = []StoredBinding{{
		Identity: "alice", Backend: "lab", Instance: "lab-7", Phase: crd.PhaseLeased,
	}}
	fb.list = []backend.Instance{annotatedIn("lab-7", "alice")}

	n, err := m.Recover(ctx)
	if err != nil {
		t.Fatalf("Recover: %v", err)
	}
	if n != 1 {
		t.Errorf("adopted = %d, want 1", n)
	}

	got, err := m.Acquire(ctx, "alice", "lab")
	if err != nil {
		t.Fatalf("Acquire after recovery: %v", err)
	}
	if got.Instance.ID != "lab-7" {
		t.Errorf("instance = %q, want lab-7 (the one the store recorded)", got.Instance.ID)
	}
	if fb.Acquires() != 0 {
		t.Errorf("driver created %d instances; recovery was supposed to avoid all of them", fb.Acquires())
	}
	// A row that was already correct must not be rewritten on every start.
	if puts, _ := st.snapshot(); len(puts) != 0 {
		t.Errorf("puts = %+v, want none: the row was already there", puts)
	}
}

// A binding whose object is gone cannot be handed out, and leaving it would make that identity
// unrecoverable forever.
func TestRecoverDropsBindingsWhoseInstanceIsGone(t *testing.T) {
	ctx := context.Background()
	m, fb, _ := newManager(t)
	st := &fakeStore{}
	m.SetStore(st)
	st.list = []StoredBinding{{Identity: "alice", Backend: "lab", Instance: "lab-99", Phase: crd.PhaseLeased}}
	fb.list = nil

	n, err := m.Recover(ctx)
	if err != nil {
		t.Fatalf("Recover: %v", err)
	}
	if n != 0 {
		t.Errorf("adopted = %d, want 0", n)
	}
	_, deletes := st.snapshot()
	if len(deletes) != 1 || deletes[0] != "alice" {
		t.Errorf("deletes = %v, want [alice]", deletes)
	}
}

// Draining means the previous process was mid-release. Re-adopting would hand out something that is on
// its way out, so the release is finished instead.
func TestRecoverFinishesAReleaseThatWasInterrupted(t *testing.T) {
	ctx := context.Background()
	m, fb, _ := newManager(t)
	st := &fakeStore{}
	m.SetStore(st)
	st.list = []StoredBinding{{Identity: "alice", Backend: "lab", Instance: "lab-9", Phase: crd.PhaseDraining}}
	fb.list = nil

	if _, err := m.Recover(ctx); err != nil {
		t.Fatalf("Recover: %v", err)
	}
	if rel := fb.Released(); len(rel) != 1 || rel[0] != "lab-9" {
		t.Errorf("released = %v, want [lab-9]", rel)
	}
	if _, deletes := st.snapshot(); len(deletes) != 1 {
		t.Errorf("deletes = %v, want the binding removed", deletes)
	}
}

// An instance recovered from its annotation alone has no row, so one is written: otherwise every restart
// re-derives it and a second replica never sees it.
func TestRecoverRecordsInstancesTheStoreDidNotKnowAbout(t *testing.T) {
	ctx := context.Background()
	m, fb, _ := newManager(t)
	st := &fakeStore{}
	m.SetStore(st)
	st.list = nil // the store is empty: this instance predates it
	fb.list = []backend.Instance{annotatedIn("lab-4", "alice")}

	if _, err := m.Recover(ctx); err != nil {
		t.Fatalf("Recover: %v", err)
	}
	puts, _ := st.snapshot()
	if len(puts) != 1 || puts[0].Identity != "alice" || puts[0].Instance != "lab-4" {
		t.Fatalf("puts = %+v, want one row binding alice to lab-4", puts)
	}
}

// Where the store and an annotation disagree, the store wins: the instance is the leftover, not the
// binding. Adopting both would put two instances behind one identity.
func TestRecoverPrefersTheStoreOverAConflictingAnnotation(t *testing.T) {
	ctx := context.Background()
	m, fb, _ := newManager(t)
	st := &fakeStore{}
	m.SetStore(st)
	st.list = []StoredBinding{{Identity: "alice", Backend: "lab", Instance: "lab-2", Phase: crd.PhaseLeased}}
	fb.list = []backend.Instance{
		annotatedIn("lab-1", "alice"), // stale: created before the binding moved
		annotatedIn("lab-2", "alice"), // what the store says
	}

	if _, err := m.Recover(ctx); err != nil {
		t.Fatalf("Recover: %v", err)
	}

	got, err := m.Acquire(ctx, "alice", "lab")
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	if got.Instance.ID != "lab-2" {
		t.Errorf("instance = %q, want lab-2", got.Instance.ID)
	}
	rel := fb.Released()
	if len(rel) != 1 || rel[0] != "lab-1" {
		t.Errorf("released = %v, want exactly [lab-1]", rel)
	}
}

// An unreadable store is the pre-ADR world, not a new failure. Refusing to start would turn a store
// outage into an outage of every session.
func TestRecoverFallsBackToAnnotationsWhenTheStoreCannotBeRead(t *testing.T) {
	ctx := context.Background()
	m, fb, _ := newManager(t)
	st := &fakeStore{}
	m.SetStore(st)
	st.listErr = errors.New("apiserver unreachable")
	fb.list = []backend.Instance{annotatedIn("lab-3", "alice")}

	n, err := m.Recover(ctx)
	if err != nil {
		t.Fatalf("Recover must degrade, not fail: %v", err)
	}
	if n != 1 {
		t.Errorf("adopted = %d, want 1 from the annotation fallback", n)
	}
	got, err := m.Acquire(ctx, "alice", "lab")
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	if got.Instance.ID != "lab-3" {
		t.Errorf("instance = %q, want lab-3", got.Instance.ID)
	}
}

// With no store at all Recover must behave exactly like Adopt, so the in-memory mode is untouched.
func TestRecoverWithoutAStoreIsAdopt(t *testing.T) {
	ctx := context.Background()
	m, fb, _ := newManager(t)
	fb.list = []backend.Instance{annotatedIn("lab-5", "alice")}

	if _, err := m.Recover(ctx); err != nil {
		t.Fatalf("Recover: %v", err)
	}
	got, err := m.Acquire(ctx, "alice", "lab")
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	if got.Instance.ID != "lab-5" {
		t.Errorf("instance = %q, want lab-5", got.Instance.ID)
	}
}
