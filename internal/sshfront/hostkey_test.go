package sshfront

import (
	"os"
	"path/filepath"
	"testing"
)

// TestHostKeyIsPersistedOnFirstStart is the first-install guarantee: a configured path that does not
// exist yet must be created and written down, not silently replaced by an in-memory key.
func TestHostKeyIsPersistedOnFirstStart(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state", "id_ed25519")

	first, err := loadOrCreateHostKey(path)
	if err != nil {
		t.Fatalf("first start: %v", err)
	}

	st, err := os.Stat(path)
	if err != nil {
		t.Fatalf("host key was not written: %v", err)
	}
	if perm := st.Mode().Perm(); perm != 0o600 {
		t.Fatalf("host key mode = %o, want 600", perm)
	}

	second, err := loadOrCreateHostKey(path)
	if err != nil {
		t.Fatalf("second start: %v", err)
	}
	if string(first.PublicKey().Marshal()) != string(second.PublicKey().Marshal()) {
		t.Fatal("restart produced a different host key: clients would see a changed fingerprint")
	}
}

// TestHostKeyExistingFileIsLoaded checks that a key supplied out of band (a Secret, a config-management
// file) is used as-is and never overwritten.
func TestHostKeyExistingFileIsLoaded(t *testing.T) {
	path := filepath.Join(t.TempDir(), "id_ed25519")

	original, err := loadOrCreateHostKey(path)
	if err != nil {
		t.Fatalf("seed: %v", err)
	}
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read seeded key: %v", err)
	}

	loaded, err := loadOrCreateHostKey(path)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	after, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("re-read: %v", err)
	}
	if string(before) != string(after) {
		t.Fatal("an existing host key file was rewritten")
	}
	if string(original.PublicKey().Marshal()) != string(loaded.PublicKey().Marshal()) {
		t.Fatal("loaded key does not match the stored one")
	}
}

// TestHostKeyEphemeralWhenUnset documents the third case: no path means an in-memory key. It is only
// acceptable for tests, and the constructor warns.
func TestHostKeyEphemeralWhenUnset(t *testing.T) {
	a, err := loadOrCreateHostKey("")
	if err != nil {
		t.Fatalf("ephemeral: %v", err)
	}
	b, err := loadOrCreateHostKey("")
	if err != nil {
		t.Fatalf("ephemeral again: %v", err)
	}
	if string(a.PublicKey().Marshal()) == string(b.PublicKey().Marshal()) {
		t.Fatal("two ephemeral keys should not be identical")
	}
}

// TestHostKeyUnreadablePathFails checks that a path we cannot read is an error, not a silent fallback.
func TestHostKeyUnreadablePathFails(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("root can read anything; the permission case is untestable as root")
	}
	dir := t.TempDir()
	path := filepath.Join(dir, "id_ed25519")
	if err := os.WriteFile(path, []byte("not a key"), 0o600); err != nil {
		t.Fatalf("seed: %v", err)
	}
	if _, err := loadOrCreateHostKey(path); err == nil {
		t.Fatal("a malformed host key must be an error")
	}
}
