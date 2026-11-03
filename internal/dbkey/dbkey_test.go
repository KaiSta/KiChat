package dbkey

import (
	"bytes"
	"encoding/binary"
	"errors"
	"os"
	"runtime"
	"testing"

	"github.com/zalando/go-keyring"
)

func newTestStore(t *testing.T) *Store {
	t.Helper()
	// NewIsolated never probes or writes the per-user keyring.
	return NewIsolated(t.TempDir())
}

func TestRecoveryRoundTrip(t *testing.T) {
	s := newTestStore(t)
	key, err := NewKey()
	if err != nil {
		t.Fatal(err)
	}
	if err := s.SaveRecovery("correct horse battery", key); err != nil {
		t.Fatalf("SaveRecovery: %v", err)
	}
	if !s.HasRecovery() {
		t.Fatal("HasRecovery = false after writing")
	}
	got, err := s.OpenRecovery("correct horse battery")
	if err != nil {
		t.Fatalf("OpenRecovery: %v", err)
	}
	if !bytes.Equal(got, key) {
		t.Fatalf("unwrapped key differs from the stored one")
	}
	// The file must not contain the key, and must be the size the header layout
	// predicts: magic, version, kdf, 3 parameters, salt, nonce, sealed key.
	blob, err := os.ReadFile(s.Path())
	if err != nil {
		t.Fatal(err)
	}
	if want := 8 + 1 + 1 + 12 + 16 + 12 + KeySize + 16; len(blob) != want {
		t.Errorf("recovery file is %d bytes, want %d", len(blob), want)
	}
	if bytes.Contains(blob, key) {
		t.Error("the key appears in the recovery file in the clear")
	}
	if string(blob[:8]) != magic {
		t.Errorf("magic = %q, want %q", blob[:8], magic)
	}
}

func TestWrongPassphraseIsItsOwnError(t *testing.T) {
	s := newTestStore(t)
	key, _ := NewKey()
	if err := s.SaveRecovery("one long passphrase", key); err != nil {
		t.Fatal(err)
	}
	if _, err := s.OpenRecovery("another long passphrase"); !errors.Is(err, ErrWrongPassphrase) {
		t.Fatalf("OpenRecovery with a wrong passphrase = %v, want ErrWrongPassphrase", err)
	}
}

func TestTamperingIsDetected(t *testing.T) {
	cases := map[string]func([]byte) []byte{
		"flipped salt": func(b []byte) []byte { b[25] ^= 0xff; return b },
		"flipped key":  func(b []byte) []byte { b[len(b)-1] ^= 0xff; return b },
		"flipped nonce": func(b []byte) []byte {
			b[56] ^= 0xff
			return b
		},
		"foreign magic": func(b []byte) []byte { copy(b[:8], "NOTAKEY!"); return b },
		"truncated":     func(b []byte) []byte { return b[:40] },
	}
	for name, damage := range cases {
		t.Run(name, func(t *testing.T) {
			s := newTestStore(t)
			key, _ := NewKey()
			if err := s.SaveRecovery("a long enough phrase", key); err != nil {
				t.Fatal(err)
			}
			blob, err := os.ReadFile(s.Path())
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(s.Path(), damage(blob), 0o600); err != nil {
				t.Fatal(err)
			}
			if _, err := s.OpenRecovery("a long enough phrase"); err == nil {
				t.Fatal("a damaged file opened")
			}
		})
	}
}

// TestImpossibleHeaderCostIsDamageNotAPanic pins the bound check in kdf.sane.
//
// argon2.IDKey panics when memory is below 8 KiB per lane, so a header that names such
// a cost would take the whole app down with a corrupted file rather than report it.
func TestImpossibleHeaderCostIsDamageNotAPanic(t *testing.T) {
	s := newTestStore(t)
	key, _ := NewKey()
	if err := s.SaveRecovery("a long enough phrase", key); err != nil {
		t.Fatal(err)
	}
	blob, err := os.ReadFile(s.Path())
	if err != nil {
		t.Fatal(err)
	}
	binary.BigEndian.PutUint32(blob[14:18], 1) // memory: 1 KiB
	if err := os.WriteFile(s.Path(), blob, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := s.OpenRecovery("a long enough phrase"); !errors.Is(err, ErrDamaged) {
		t.Fatalf("OpenRecovery with an impossible cost = %v, want ErrDamaged", err)
	}
}

func TestShortPassphraseIsRefused(t *testing.T) {
	s := newTestStore(t)
	key, _ := NewKey()
	if err := s.SaveRecovery("short", key); !errors.Is(err, ErrShortPassphrase) {
		t.Fatalf("SaveRecovery with a short passphrase = %v, want ErrShortPassphrase", err)
	}
	if s.HasRecovery() {
		t.Fatal("a refused passphrase left a file behind")
	}
}

func TestRecoveryFileIsOwnerOnly(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Windows has no POSIX mode bits")
	}
	s := newTestStore(t)
	key, _ := NewKey()
	if err := s.SaveRecovery("a long enough phrase", key); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(s.Path())
	if err != nil {
		t.Fatal(err)
	}
	if got := info.Mode().Perm(); got != 0o600 {
		t.Errorf("recovery file mode = %#o, want 0600", got)
	}
}

func TestRemoveRecovery(t *testing.T) {
	s := newTestStore(t)
	key, _ := NewKey()
	if err := s.SaveRecovery("a long enough phrase", key); err != nil {
		t.Fatal(err)
	}
	if err := s.RemoveRecovery(); err != nil {
		t.Fatalf("RemoveRecovery: %v", err)
	}
	if s.HasRecovery() {
		t.Fatal("the file survived RemoveRecovery")
	}
	if _, err := s.OpenRecovery("a long enough phrase"); !errors.Is(err, ErrNoRecovery) {
		t.Fatalf("OpenRecovery after removal = %v, want ErrNoRecovery", err)
	}
}

// TestKeyringPath exercises the keyring branch against a fake. The real keyring is a
// per-user global resource and a test must never reach it.
func TestKeyringPath(t *testing.T) {
	entries := map[string]string{}
	s := NewIsolated(t.TempDir())
	s.keyringUsable = true
	s.get = func(_, user string) (string, error) {
		v, ok := entries[user]
		if !ok {
			return "", errors.New("not found")
		}
		return v, nil
	}
	s.set = func(_, user, value string) error { entries[user] = value; return nil }
	s.delete = func(_, user string) error { delete(entries, user); return nil }

	key, _ := NewKey()
	if err := s.SaveKeyring(key); err != nil {
		t.Fatalf("SaveKeyring: %v", err)
	}
	got, ok := s.KeyringKey()
	if !ok || !bytes.Equal(got, key) {
		t.Fatalf("KeyringKey = %x, %v", got, ok)
	}
	if bytes.Contains([]byte(entries[keyringEntry]), key) {
		t.Error("the key is stored in the keyring in raw form")
	}
	if err := s.RemoveKeyring(); err != nil {
		t.Fatalf("RemoveKeyring: %v", err)
	}
	if _, ok := s.KeyringKey(); ok {
		t.Fatal("the key survived RemoveKeyring")
	}
}

// TestTheKeyringKeyIsReadOnce pins the memo the launch depends on: the key is asked for
// while the database is opened and again when the status the unlock screen reads is built,
// and on macOS a read means spawning /usr/bin/security for the Keychain.
func TestTheKeyringKeyIsReadOnce(t *testing.T) {
	entries := map[string]string{}
	reads := 0
	s := NewIsolated(t.TempDir())
	s.keyringUsable = true
	s.get = func(_, user string) (string, error) {
		reads++
		v, ok := entries[user]
		if !ok {
			return "", keyring.ErrNotFound
		}
		return v, nil
	}
	s.set = func(_, user, value string) error { entries[user] = value; return nil }
	s.delete = func(_, user string) error { delete(entries, user); return nil }

	key, _ := NewKey()
	for i := 0; i < 2; i++ {
		if _, ok := s.KeyringKey(); ok {
			t.Fatalf("an empty keyring yielded a key on read %d", i)
		}
	}
	if reads != 1 {
		t.Errorf("an empty keyring was read %d times, want one", reads)
	}

	if err := s.SaveKeyring(key); err != nil {
		t.Fatalf("SaveKeyring: %v", err)
	}
	for i := 0; i < 2; i++ {
		got, ok := s.KeyringKey()
		if !ok || !bytes.Equal(got, key) {
			t.Fatalf("KeyringKey on read %d = %x, %v; want the saved key", i, got, ok)
		}
	}
	if reads != 1 {
		t.Errorf("%d keyring reads after a save, want the saved value to answer", reads)
	}
}

func TestNoKeyringMeansNoKey(t *testing.T) {
	s := newTestStore(t)
	if s.KeyringAvailable() {
		t.Fatal("NewIsolated reports a usable keyring")
	}
	key, _ := NewKey()
	if err := s.SaveKeyring(key); !errors.Is(err, ErrNoKeyring) {
		t.Fatalf("SaveKeyring without a keyring = %v, want ErrNoKeyring", err)
	}
	if _, ok := s.KeyringKey(); ok {
		t.Fatal("KeyringKey returned a key without a keyring")
	}
}
