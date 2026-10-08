package secret

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// newFallbackStore builds a store with the keyring switched off: the path a
// machine without libsecret uses, and the path a keyring that refuses mid-run
// falls back to. It uses the isolated constructor so a test run cannot reach the
// real keyring of whoever runs the suite.
func newFallbackStore(t *testing.T) *Store {
	t.Helper()
	s, err := NewIsolated(t.TempDir())
	if err != nil {
		t.Fatalf("NewIsolated: %v", err)
	}
	return s
}

func TestFallbackRoundTripAndRemoval(t *testing.T) {
	s := newFallbackStore(t)
	if _, ok := s.Get("brave"); ok {
		t.Fatal("a fresh store should hold nothing")
	}

	const key = "BSA-very-secret-key-value"
	if err := s.Set("brave", key); err != nil {
		t.Fatalf("Set: %v", err)
	}
	got, ok := s.Get("brave")
	if !ok || got != key {
		t.Fatalf("Get = %q, %v; want the value that was set", got, ok)
	}
	if s.Backend() != "encrypted file" {
		t.Errorf("Backend = %q, want encrypted file", s.Backend())
	}

	// The blob on disk must not carry the secret in the clear.
	raw, err := os.ReadFile(s.dataFile)
	if err != nil {
		t.Fatalf("reading the blob: %v", err)
	}
	if strings.Contains(string(raw), key) {
		t.Fatal("the secret was written in the clear")
	}
	info, err := os.Stat(s.keyFile)
	if err != nil {
		t.Fatalf("key file: %v", err)
	}
	if perm := info.Mode().Perm(); perm != 0o600 {
		t.Errorf("key file mode = %o, want 600", perm)
	}

	// An empty value deletes.
	if err := s.Set("brave", ""); err != nil {
		t.Fatalf("Set(\"\"): %v", err)
	}
	if _, ok := s.Get("brave"); ok {
		t.Error("the secret survived being cleared")
	}

	// A second store over the same directory reads back what the first wrote,
	// which is what makes the file a working fallback rather than a write-only
	// box.
	if err := s.Set("brave", key); err != nil {
		t.Fatal(err)
	}
	reopened, err := NewIsolated(filepath.Dir(s.dataFile))
	if err != nil {
		t.Fatalf("reopening: %v", err)
	}
	if got, ok := reopened.Get("brave"); !ok || got != key {
		t.Errorf("reopened Get = %q, %v; want the stored key", got, ok)
	}

	// A corrupt blob must not brick the app: the store comes up empty instead.
	if err := os.WriteFile(s.dataFile, []byte("not a sealed record"), 0o600); err != nil {
		t.Fatal(err)
	}
	corrupt, err := NewIsolated(filepath.Dir(s.dataFile))
	if err != nil {
		t.Fatalf("opening with a corrupt blob: %v", err)
	}
	if _, ok := corrupt.Get("brave"); ok {
		t.Error("a corrupt blob should not yield a secret")
	}
}

// TestIsolatedStoreNeverTouchesTheKeyring is the guard for a bug that reached a
// user: a test store built over the real keyring wrote its fixture string into
// it, so the stored Brave key the app read back at startup was a test value and
// every search came back as a rejected key. A test store must not be able to
// reach the keyring at all, whatever the machine offers.
func TestIsolatedStoreNeverTouchesTheKeyring(t *testing.T) {
	s, err := NewIsolated(t.TempDir())
	if err != nil {
		t.Fatalf("NewIsolated: %v", err)
	}
	if s.KeyringAvailable() {
		t.Error("an isolated store reports a keyring")
	}
	if s.Backend() != "encrypted file" {
		t.Errorf("Backend = %q, want the file fallback", s.Backend())
	}
	if err := s.Set("brave", "fixture-value"); err != nil {
		t.Fatalf("Set: %v", err)
	}
	if got, ok := s.Get("brave"); !ok || got != "fixture-value" {
		t.Errorf("Get = %q, %v; want the value written to the file", got, ok)
	}
	// The value has to be in the file, which is what "isolated" means: a store
	// whose secrets went to the keyring would still read them back here.
	raw, err := os.ReadFile(s.dataFile)
	if err != nil {
		t.Fatalf("reading the blob: %v", err)
	}
	if len(raw) == 0 {
		t.Error("nothing was written to the store's own directory")
	}
}
