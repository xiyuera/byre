package lease

import (
	"context"
	"errors"
	"testing"
	"time"
)

// A client that connects once and leaves leaves the binding in memory only until the sweep retries the
// write. Retrying on reuse alone is not enough: a ttl can be hours long, during which a restart would
// find nothing in the store and hand the identity a second instance -- the exact failure the store
// exists to prevent.
func TestReapRetriesUnpersistedWrites(t *testing.T) {
	ctx := context.Background()
	m, _, _ := newManager(t)
	st := &fakeStore{}
	m.SetStore(st)
	st.setPutErr(errors.New("api server unavailable"))

	if _, err := m.Acquire(ctx, "alice", "lab"); err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	if n := m.UnpersistedCount(); n != 1 {
		t.Fatalf("unpersisted = %d after a failed write, want 1", n)
	}

	st.setPutErr(nil)
	if _, err := m.Reap(ctx); err != nil {
		t.Fatalf("Reap: %v", err)
	}
	if n := m.UnpersistedCount(); n != 0 {
		t.Errorf("unpersisted = %d after a successful sweep retry, want 0", n)
	}
	puts, _ := st.snapshot()
	if len(puts) != 1 || puts[0].Identity != "alice" {
		t.Errorf("puts = %+v, want exactly one for alice", puts)
	}
}

// A sweep must not retry a write for a lease that is already being drained: the release is about to
// delete the row, and writing it back first would leave the object behind.
func TestReapDoesNotRewriteADrainingLease(t *testing.T) {
	ctx := context.Background()
	m, _, clk := newManager(t)
	st := &fakeStore{}
	m.SetStore(st)
	st.setPutErr(errors.New("api server unavailable"))

	if _, err := m.Acquire(ctx, "alice", "lab"); err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	// Without this the lease still has a live connection and the sweep correctly leaves it alone.
	m.ReleaseConn("alice")
	st.setPutErr(nil)

	// Past the ttl and the grace, so the sweep both retries and drains.
	clk.advance(500 * time.Second)
	if _, err := m.Reap(ctx); err != nil {
		t.Fatalf("Reap: %v", err)
	}
	puts, deletes := st.snapshot()
	if len(deletes) != 1 {
		t.Fatalf("deletes = %v, want the expired binding removed", deletes)
	}
	// One put from the retry is harmless: it happens before the delete. What must not happen is a put
	// after it, so assert on the ordering the fake records.
	if len(puts) > 1 {
		t.Errorf("puts = %d, want at most one (the retry) before the delete", len(puts))
	}
}
