package lease

import (
	"context"
	"testing"
	"time"

	"byre/internal/backend"
)

// The failure this test exists for, written down because it happened: a process sharing a namespace with
// another deployment sees that deployment's instances through List(), adopts them, and once their ttl
// expires the reaper destroys them. Nothing in the earlier code distinguished "an instance I can see"
// from "an identity I serve".
func TestAdoptIgnoresIdentitiesThisProcessIsNotConfiguredFor(t *testing.T) {
	ctx := context.Background()
	m, fb, _ := newManager(t)
	m.SetAllowedIdentities([]string{"SHA256:ours"})
	fb.list = []backend.Instance{
		annotatedIn("lab-1", "ours"),
		annotatedIn("lab-2", "theirs"),
	}

	n, err := m.Adopt(ctx)
	if err != nil {
		t.Fatalf("Adopt: %v", err)
	}
	if n != 1 {
		t.Errorf("adopted = %d, want 1 (only the configured identity)", n)
	}
	if rel := fb.Released(); len(rel) != 0 {
		t.Errorf("released = %v, want none: another deployment's instance is not ours to destroy", rel)
	}

	got, err := m.Acquire(ctx, "ours", "lab")
	if err != nil {
		t.Fatalf("Acquire(ours): %v", err)
	}
	if got.Instance.ID != "lab-1" {
		t.Errorf("ours -> %q, want lab-1", got.Instance.ID)
	}
	// Not asserted here: that Acquire(theirs) is refused. It is, but one layer up -- sshfront only
	// authenticates configured fingerprints, so an unconfigured identity never reaches the manager. What
	// this test pins is the property the manager owes: the foreign instance is not in the table, so
	// nothing can reuse it and nothing will release it.
	if fb.Acquires() != 0 {
		t.Errorf("driver created %d instances; adoption and the one Acquire should have created none", fb.Acquires())
	}
}

// The same rule on the recovery path, where it must also keep the foreign row in place rather than
// deleting it: that row belongs to a deployment which is still using it.
func TestRecoverIgnoresForeignIdentities(t *testing.T) {
	ctx := context.Background()
	m, fb, _ := newManager(t)
	st := &fakeStore{}
	m.SetStore(st)
	m.SetAllowedIdentities([]string{"SHA256:ours"})
	st.list = []StoredBinding{
		{Identity: "ours", Backend: "lab", Instance: "lab-1"},
		{Identity: "theirs", Backend: "lab", Instance: "lab-2"},
	}
	fb.list = []backend.Instance{
		annotatedIn("lab-1", "ours"),
		annotatedIn("lab-2", "theirs"),
	}

	if _, err := m.Recover(ctx); err != nil {
		t.Fatalf("Recover: %v", err)
	}
	if rel := fb.Released(); len(rel) != 0 {
		t.Errorf("released = %v, want none", rel)
	}
	if _, deletes := st.snapshot(); len(deletes) != 0 {
		t.Errorf("deletes = %v, want none: a row we do not own is not ours to delete either", deletes)
	}

	got, err := m.Acquire(ctx, "ours", "lab")
	if err != nil {
		t.Fatalf("Acquire(ours): %v", err)
	}
	if got.Instance.ID != "lab-1" {
		t.Errorf("ours -> %q, want lab-1 (recovered)", got.Instance.ID)
	}
}

// An empty list means "no restriction" and must keep the earlier behaviour, because embedders and tests
// build a Manager without telling it which identities it serves.
func TestUnrestrictedWhenNoIdentitiesAreDeclared(t *testing.T) {
	ctx := context.Background()
	m, fb, _ := newManager(t)
	fb.list = []backend.Instance{annotatedIn("lab-1", "anyone")}

	n, err := m.Adopt(ctx)
	if err != nil {
		t.Fatalf("Adopt: %v", err)
	}
	if n != 1 {
		t.Errorf("adopted = %d, want 1", n)
	}
}

// The reaper is what turns a wrong adoption into destruction, so prove the two are linked: an identity we
// do not serve never gets into the table, therefore it can never be released by expiry.
func TestForeignInstanceSurvivesATtlExpiry(t *testing.T) {
	ctx := context.Background()
	m, fb, clk := newManager(t)
	m.SetAllowedIdentities([]string{"ours"})
	fb.list = []backend.Instance{annotatedIn("lab-1", "theirs")}

	if _, err := m.Adopt(ctx); err != nil {
		t.Fatalf("Adopt: %v", err)
	}
	clk.advance(10 * time.Hour)
	if _, err := m.Reap(ctx); err != nil {
		t.Fatalf("Reap: %v", err)
	}
	if rel := fb.Released(); len(rel) != 0 {
		t.Errorf("released = %v after a full ttl, want none", rel)
	}
}
