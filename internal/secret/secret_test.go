package secret

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/zalando/go-keyring"
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

// TestClearingASecretAlsoClearsTheFallbackCopy pins the state a transient keyring
// failure produces. Set writes to the encrypted file when keyring.Set refuses
// once — a locked collection, a busy D-Bus — while the store still reports a
// keyring, and Get then reads that file copy whenever the keyring has nothing.
// Clearing only deleted from the keyring, so it reported success while the old
// key stayed readable through the fallback.
//
// The keyring calls are stubbed rather than exercised: a test must never reach
// the real keyring, and that is also what makes "the keyring refuses" testable at
// all.
func TestClearingASecretAlsoClearsTheFallbackCopy(t *testing.T) {
	s := newFallbackStore(t)
	s.keyring = true
	s.keyringGet = func(string, string) (string, error) { return "", keyring.ErrNotFound }
	s.keyringSet = func(string, string, string) error { return errors.New("keyring gesperrt") }
	s.keyringDelete = func(string, string) error { return nil }

	const key = "BSA-transient-keyring-value"
	if err := s.Set("brave", key); err != nil {
		t.Fatalf("Set: %v", err)
	}
	if got, ok := s.Get("brave"); !ok || got != key {
		t.Fatalf("Get = %q, %v; want the value written to the fallback", got, ok)
	}

	if err := s.Set("brave", ""); err != nil {
		t.Fatalf("Set(\"\"): %v", err)
	}
	if got, ok := s.Get("brave"); ok {
		t.Fatalf("the cleared secret is still readable through the fallback: %q", got)
	}

	// The deletion has to reach the file, not just the in-memory map: a restart
	// must not bring the key back.
	reopened, err := NewIsolated(filepath.Dir(s.dataFile))
	if err != nil {
		t.Fatalf("reopening: %v", err)
	}
	if got, ok := reopened.Get("brave"); ok {
		t.Fatalf("the cleared secret came back after a restart: %q", got)
	}
}

// TestAKeyringReadIsAskedForOnce pins what the memo is for. A read is not free: go-keyring
// reaches the macOS Keychain by spawning /usr/bin/security, and one start reads the same
// names repeatedly — the settings blob, the provider set built from it, the image server's
// credential, then every probe. Nothing else writes these entries while the app runs, so a
// second answer would be the same one at the cost of another process.
func TestAKeyringReadIsAskedForOnce(t *testing.T) {
	s := newFallbackStore(t)
	s.keyring = true
	reads := 0
	s.keyringGet = func(_, user string) (string, error) {
		reads++
		if user == "brave" {
			return "stored-in-keyring", nil
		}
		return "", keyring.ErrNotFound
	}

	for i := 0; i < 3; i++ {
		if got, ok := s.Get("brave"); !ok || got != "stored-in-keyring" {
			t.Fatalf("Get on read %d = %q, %v; want the keyring value", i, got, ok)
		}
	}
	if reads != 1 {
		t.Errorf("the keyring was read %d times for one name, want one", reads)
	}

	// A name the keyring does not hold is a fact about this run too: repeating it would
	// be one spawn per probe for a secret that does not exist.
	for i := 0; i < 3; i++ {
		if _, ok := s.Get("groq"); ok {
			t.Fatalf("an unset secret was reported set on read %d", i)
		}
	}
	if reads != 2 {
		t.Errorf("%d keyring reads for one stored and one missing name, want 2", reads)
	}
}

// TestATransientKeyringFailureIsNotRemembered is the other half of the memo: a locked or
// busy keyring refuses once and answers afterwards, so that refusal is asked about again
// rather than cached as "this secret does not exist" for the rest of the run.
func TestATransientKeyringFailureIsNotRemembered(t *testing.T) {
	s := newFallbackStore(t)
	s.keyring = true
	locked := true
	reads := 0
	s.keyringGet = func(_, _ string) (string, error) {
		reads++
		if locked {
			return "", errors.New("keyring gesperrt")
		}
		return "recovered-value", nil
	}

	if _, ok := s.Get("brave"); ok {
		t.Fatal("a locked keyring yielded a secret")
	}
	locked = false
	got, ok := s.Get("brave")
	if !ok || got != "recovered-value" {
		t.Fatalf("Get after the keyring came back = %q, %v; want the stored value", got, ok)
	}
	if reads != 2 {
		t.Errorf("the keyring was read %d times, want the refusal asked about again", reads)
	}
}

// TestASetIsVisibleToTheNextGet pins the memo against the writes that pass through the
// same store: a value just stored must not be hidden by a miss remembered from before,
// and a removal must not leave the old answer behind.
func TestASetIsVisibleToTheNextGet(t *testing.T) {
	s := newFallbackStore(t)
	s.keyring = true
	entries := map[string]string{}
	s.keyringGet = func(_, user string) (string, error) {
		v, ok := entries[user]
		if !ok {
			return "", keyring.ErrNotFound
		}
		return v, nil
	}
	s.keyringSet = func(_, user, value string) error { entries[user] = value; return nil }
	s.keyringDelete = func(_, user string) error { delete(entries, user); return nil }

	if _, ok := s.Get("brave"); ok {
		t.Fatal("a fresh store holds a secret")
	}
	if err := s.Set("brave", "BSA-new-value"); err != nil {
		t.Fatalf("Set: %v", err)
	}
	if got, ok := s.Get("brave"); !ok || got != "BSA-new-value" {
		t.Fatalf("Get after Set = %q, %v; want the value just stored", got, ok)
	}
	if err := s.Set("brave", ""); err != nil {
		t.Fatalf("Set(\"\"): %v", err)
	}
	if got, ok := s.Get("brave"); ok {
		t.Fatalf("the cleared secret is still readable: %q", got)
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
