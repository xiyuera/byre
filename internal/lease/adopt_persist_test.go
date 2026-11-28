package lease

import (
	"context"
	"errors"
	"testing"

	"byre/internal/backend"
)

// TestAdoptPersistsEveryLeaseItTakesOver is the acceptance assertion for the second defect: adoption used
// to fill the in-memory table and stop there, so an operator saw two leases in memory, zero ByreLease
// objects, and byre_leases_unpersisted reporting zero — the gauge only knew about writes that failed,
// never about rows that were never attempted.
//
// The assertion is the equality the card asks for: one row per lease.
func TestAdoptPersistsEveryLeaseItTakesOver(t *testing.T) {
	ctx := context.Background()
	m, fb, _ := newManager(t)
	st := &fakeStore{}
	m.SetStore(st)
	fb.list = []backend.Instance{
		annotatedIn("lab-1", "alice"),
		annotatedIn("lab-2", "bob"),
	}

	n, err := m.Adopt(ctx)
	if err != nil {
		t.Fatalf("Adopt: %v", err)
	}
	if n != 2 {
		t.Fatalf("adopted = %d, want 2", n)
	}
	puts, _ := st.snapshot()
	if len(puts) != leaseCount(m) {
		t.Fatalf("rows = %d, leases = %d: every adopted lease must have a row", len(puts), leaseCount(m))
	}
	if len(puts) != 2 {
		t.Fatalf("rows = %d, want 2", len(puts))
	}
	bound := map[string]string{}
	for _, b := range puts {
		bound[b.Identity] = b.Instance
	}
	if bound["alice"] != "lab-1" || bound["bob"] != "lab-2" {
		t.Fatalf("rows = %+v, want alice->lab-1 and bob->lab-2", puts)
	}
	if n := m.UnpersistedCount(); n != 0 {
		t.Fatalf("byre_leases_unpersisted = %d after successful rows, want 0", n)
	}
}

// A write that fails must move the gauge, not just the counter: only the flag makes Reap and the reuse path
// retry, and a lease whose row was never written is otherwise invisible for the rest of the process's life.
func TestAdoptPersistFailureIsVisibleInTheGauge(t *testing.T) {
	ctx := context.Background()
	m, fb, _ := newManager(t)
	st := &fakeStore{}
	st.setPutErr(errors.New("apiserver unreachable"))
	m.SetStore(st)
	fb.list = []backend.Instance{annotatedIn("lab-1", "alice")}

	if _, err := m.Adopt(ctx); err == nil {
		t.Fatal("Adopt must report a row it could not write")
	}
	if n := m.UnpersistedCount(); n != leaseCount(m) {
		t.Fatalf("byre_leases_unpersisted = %d, leases = %d: the gauge must equal the leases the store does not know",
			n, leaseCount(m))
	}
	if m.PersistErrors() < 1 {
		t.Error("PersistErrors did not count the failed write")
	}
}

// Losing a write race to another replica that recorded the *same* instance is not a failure: the row it
// wrote is the binding this process adopted, so nothing is left to record and the lease must not be
// flagged as untracked.
func TestAdoptAcceptsARowAnotherReplicaWrote(t *testing.T) {
	ctx := context.Background()
	m, fb, _ := newManager(t)
	st := &fakeStore{putErr: ErrBindingExists}
	st.list = []StoredBinding{{Identity: "alice", Backend: "lab", Instance: "lab-1"}}
	m.SetStore(st)
	fb.list = []backend.Instance{annotatedIn("lab-1", "alice")}

	if _, err := m.Adopt(ctx); err != nil {
		t.Fatalf("Adopt: %v", err)
	}
	if n := m.UnpersistedCount(); n != 0 {
		t.Fatalf("byre_leases_unpersisted = %d; the row names the instance we adopted", n)
	}
}

// The other half of the race: the row that exists binds this identity to a *different* instance. The store
// wins (ADR-0013), so the adopted instance is the leftover — and saying so is the point. Marking it
// persisted would claim a durable binding that points somewhere else.
func TestAdoptReportsAConflictWithADifferentInstance(t *testing.T) {
	ctx := context.Background()
	m, fb, _ := newManager(t)
	st := &fakeStore{putErr: ErrBindingExists}
	st.list = []StoredBinding{{Identity: "alice", Backend: "lab", Instance: "lab-9"}}
	m.SetStore(st)
	fb.list = []backend.Instance{annotatedIn("lab-1", "alice")}

	if _, err := m.Adopt(ctx); err == nil {
		t.Fatal("Adopt must report that the store binds this identity elsewhere")
	}
	if n := m.UnpersistedCount(); n != 1 {
		t.Fatalf("byre_leases_unpersisted = %d, want 1", n)
	}
}

// A manager with no store is a supported mode: adoption must stay write-free there, not start counting
// failures for a table that has nowhere to be written.
func TestAdoptWithoutAStoreWritesNothing(t *testing.T) {
	ctx := context.Background()
	m, fb, _ := newManager(t)
	fb.list = []backend.Instance{annotatedIn("lab-1", "alice")}

	if _, err := m.Adopt(ctx); err != nil {
		t.Fatalf("Adopt: %v", err)
	}
	if m.PersistErrors() != 0 {
		t.Fatalf("PersistErrors = %d with no store configured", m.PersistErrors())
	}
	if n := m.UnpersistedCount(); n != 0 {
		t.Fatalf("byre_leases_unpersisted = %d with no store configured", n)
	}
}
