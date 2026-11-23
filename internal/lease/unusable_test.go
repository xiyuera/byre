package lease

import (
	"context"
	"testing"
)

// The failure Alive() cannot see: the instance answers the handshake and then refuses every session. One
// refusal is a blip -- a restarting sshd does it for a moment -- and a run of them is an instance that will
// keep refusing. Without this the client reconnects to the same instance forever.
func TestRecordUnusableDiscardsAfterTheThreshold(t *testing.T) {
	ctx := context.Background()
	m, fb, _ := newManager(t)

	e, err := m.Acquire(ctx, "alice", "lab")
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}

	for i := 1; i < UnusableThreshold; i++ {
		if m.RecordUnusable(ctx, "alice", e.Instance) {
			t.Fatalf("discarded after %d refusal(s); the threshold is %d", i, UnusableThreshold)
		}
		if rel := fb.Released(); len(rel) != 0 {
			t.Fatalf("released %v below the threshold", rel)
		}
	}

	if !m.RecordUnusable(ctx, "alice", e.Instance) {
		t.Fatalf("not discarded at the threshold (%d refusals)", UnusableThreshold)
	}
	if rel := fb.Released(); len(rel) != 1 || rel[0] != e.Instance.ID {
		t.Errorf("released = %v, want exactly [%s]", rel, e.Instance.ID)
	}
	if m.Lookup("alice") != nil {
		t.Error("the lease survived its instance being discarded")
	}
}

// A session that opens proves the streak was a blip. Leaving the count standing would replace a working
// instance on the next two refusals, which is churn nobody asked for.
func TestNoteHealthyClearsTheStreak(t *testing.T) {
	ctx := context.Background()
	m, fb, _ := newManager(t)

	e, err := m.Acquire(ctx, "alice", "lab")
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	for i := 1; i < UnusableThreshold; i++ {
		m.RecordUnusable(ctx, "alice", e.Instance)
	}
	m.NoteHealthy(e.Instance)
	for i := 1; i < UnusableThreshold; i++ {
		if m.RecordUnusable(ctx, "alice", e.Instance) {
			t.Fatalf("discarded after the streak was cleared (%d refusals since)", i)
		}
	}
	if rel := fb.Released(); len(rel) != 0 {
		t.Errorf("released = %v, want none", rel)
	}
}

// A refusal reported for an instance this identity no longer holds must not count against the one it does:
// the count is keyed by instance precisely so a replacement starts at zero.
func TestRecordUnusableIgnoresAStaleInstance(t *testing.T) {
	ctx := context.Background()
	m, _, _ := newManager(t)

	e, err := m.Acquire(ctx, "alice", "lab")
	if err != nil {
		t.Fatalf("Acquire: %v", err)
	}
	stale := e.Instance
	stale.ID = "lab-999"
	for i := 0; i < UnusableThreshold*2; i++ {
		if m.RecordUnusable(ctx, "alice", stale) {
			t.Fatal("discarded a lease because of a refusal reported for a different instance")
		}
	}
	if m.Lookup("alice") == nil {
		t.Error("the lease was dropped for a stale instance")
	}
}
