// Package secret stores API keys. It prefers the OS keyring and falls back to a
// locally encrypted file when no keyring is available, so the app never writes a
// key to plaintext disk.
package secret

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/zalando/go-keyring"
)

const keyringService = "kichat"

// Store resolves secrets from the keyring when possible, else from an encrypted
// file.
type Store struct {
	dir      string
	keyFile  string
	dataFile string

	// The keyring calls are fields so the keyring path can be exercised without
	// reaching the real one. A test store must never touch the per-user keyring:
	// doing so once replaced a user's stored Brave key with a test fixture.
	keyringGet    func(service, user string) (string, error)
	keyringSet    func(service, user, password string) error
	keyringDelete func(service, user string) error

	mu       sync.Mutex
	keyring  bool
	fallback map[string]string

	// keyringCache and keyringMiss remember what the keyring answered for a name, so
	// one run asks it once per secret instead of once per read. A read is not free:
	// go-keyring reaches the macOS Keychain by spawning /usr/bin/security, and a start
	// reads the same handful of names several times over — the settings blob, the
	// provider set built from it, the image server's key — while a Linux D-Bus round
	// trip makes the repeats invisible.
	//
	// Only a definitive "not there" is remembered as a miss. A locked or busy keyring
	// fails once and works afterwards, so that answer is not cached: it would hide a
	// key that is really stored for the rest of the run.
	keyringCache map[string]string
	keyringMiss  map[string]bool
}

// New opens the secret store rooted at dir, using the OS keyring when one is
// available.
func New(dir string) (*Store, error) {
	return newStore(dir, probeKeyring())
}

// NewIsolated opens a store that keeps every secret inside dir and never touches
// the OS keyring.
//
// Tests must use this one. The keyring is a per-user global resource, so a test
// store built with New() writes its fixtures into the real keyring of whoever
// runs the suite: doing so once replaced a user's stored Brave key with the
// string "brave-key", and the app then presented that as a rejected API key.
func NewIsolated(dir string) (*Store, error) {
	return newStore(dir, false)
}

func newStore(dir string, useKeyring bool) (*Store, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("Secret-Verzeichnis: %w", err)
	}
	s := &Store{
		dir:           dir,
		keyFile:       filepath.Join(dir, "secret.key"),
		dataFile:      filepath.Join(dir, "secrets.enc"),
		fallback:      map[string]string{},
		keyringCache:  map[string]string{},
		keyringMiss:   map[string]bool{},
		keyringGet:    keyring.Get,
		keyringSet:    keyring.Set,
		keyringDelete: keyring.Delete,
	}
	s.keyring = useKeyring
	if err := s.loadFallback(); err != nil {
		return nil, err
	}
	return s, nil
}

// KeyringAvailable reports whether the OS keyring can be used.
func (s *Store) KeyringAvailable() bool { return s.keyring }

// Backend describes where secrets are currently kept, for display in settings.
func (s *Store) Backend() string {
	if s.keyring {
		return "keyring"
	}
	return "encrypted file"
}

func probeKeyring() bool {
	// A read of a key that does not exist distinguishes "no keyring" (error
	// mentioning D-Bus / libsecret) from "not set yet" (ErrNotFound).
	_, err := keyring.Get(keyringService, "probe")
	if err == nil || errors.Is(err, keyring.ErrNotFound) {
		return true
	}
	return false
}

// Set stores a secret. An empty value deletes it.
func (s *Store) Set(name, value string) error {
	name = strings.TrimSpace(name)
	value = strings.TrimSpace(value)
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.keyring {
		if value == "" {
			_ = s.keyringDelete(keyringService, name)
			// The cached answers go with it: a read after a removal must not be served
			// the value this call just deleted.
			delete(s.keyringCache, name)
			delete(s.keyringMiss, name)
			// Clear the file copy as well, and do not return early as this used to:
			// a keyring write that failed once left the value in the encrypted
			// fallback, and Get reads that whenever the keyring has nothing — so
			// deleting only from the keyring reported success while the secret
			// stayed readable.
			if _, had := s.fallback[name]; had {
				delete(s.fallback, name)
				return s.saveFallback()
			}
			return nil
		}
		if err := s.keyringSet(keyringService, name, value); err == nil {
			s.keyringCache[name] = value
			delete(s.keyringMiss, name)
			return nil
		}
		// One refusal is not proof the keyring is gone: a locked collection or a
		// busy D-Bus session fails once and works afterwards. Flipping the flag here
		// would hide every secret already in the keyring for the rest of the run,
		// because Get only consults it while the flag is set — so the write falls
		// through to the encrypted file and the next call tries the keyring again.
	}
	// The write fell through to the file, so whatever the keyring answered before is
	// no longer the newest answer this store holds.
	delete(s.keyringCache, name)
	if value == "" {
		delete(s.fallback, name)
	} else {
		s.fallback[name] = value
	}
	return s.saveFallback()
}

// Get reads a secret.
func (s *Store) Get(name string) (string, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.keyring {
		if v, ok := s.keyringCache[name]; ok && v != "" {
			return v, true
		}
		if !s.keyringMiss[name] {
			v, err := s.keyringGet(keyringService, name)
			switch {
			case err == nil && v != "":
				s.keyringCache[name] = v
				return v, true
			case errors.Is(err, keyring.ErrNotFound):
				s.keyringMiss[name] = true
			}
		}
	}
	v, ok := s.fallback[name]
	return v, ok && v != ""
}

// --- encrypted fallback ----------------------------------------------------

// loadFallback reads the encrypted blob. The key file is created on first use
// with owner-only permissions; this protects against accidental disclosure (a
// backup, a shared folder) rather than a determined local attacker.
func (s *Store) loadFallback() error {
	raw, err := os.ReadFile(s.dataFile)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("Secrets lesen: %w", err)
	}
	key, err := s.encryptionKey()
	if err != nil {
		return err
	}
	plain, err := decrypt(key, raw)
	if err != nil {
		// A corrupt or foreign blob should not brick the app.
		return nil
	}
	return json.Unmarshal(plain, &s.fallback)
}

func (s *Store) saveFallback() error {
	key, err := s.encryptionKey()
	if err != nil {
		return err
	}
	plain, err := json.Marshal(s.fallback)
	if err != nil {
		return err
	}
	blob, err := encrypt(key, plain)
	if err != nil {
		return err
	}
	return os.WriteFile(s.dataFile, blob, 0o600)
}

func (s *Store) encryptionKey() ([]byte, error) {
	if raw, err := os.ReadFile(s.keyFile); err == nil && len(raw) == 32 {
		return raw, nil
	}
	key := make([]byte, 32)
	if _, err := io.ReadFull(rand.Reader, key); err != nil {
		return nil, err
	}
	if err := os.WriteFile(s.keyFile, key, 0o600); err != nil {
		return nil, fmt.Errorf("Schlüssel schreiben: %w", err)
	}
	return key, nil
}

func encrypt(key, plain []byte) ([]byte, error) {
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	nonce := make([]byte, gcm.NonceSize())
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return nil, err
	}
	sealed := gcm.Seal(nonce, nonce, plain, nil)
	return []byte(base64.StdEncoding.EncodeToString(sealed)), nil
}

func decrypt(key, blob []byte) ([]byte, error) {
	decoded, err := base64.StdEncoding.DecodeString(string(blob))
	if err != nil {
		return nil, err
	}
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	if len(decoded) < gcm.NonceSize() {
		return nil, errors.New("geheimer Datensatz zu kurz")
	}
	nonce, ciphertext := decoded[:gcm.NonceSize()], decoded[gcm.NonceSize():]
	return gcm.Open(nil, nonce, ciphertext, nil)
}

// Redact removes any occurrence of the given secrets from a string, so an error
// message can never leak a key into a log or the UI.
func Redact(text string, secrets ...string) string {
	for _, s := range secrets {
		if len(strings.TrimSpace(s)) < 8 {
			continue
		}
		text = strings.ReplaceAll(text, s, "[redacted]")
	}
	return text
}
