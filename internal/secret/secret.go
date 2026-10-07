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

	mu       sync.Mutex
	keyring  bool
	fallback map[string]string
}

// New opens the secret store rooted at dir.
func New(dir string) (*Store, error) {
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("Secret-Verzeichnis: %w", err)
	}
	s := &Store{
		dir:      dir,
		keyFile:  filepath.Join(dir, "secret.key"),
		dataFile: filepath.Join(dir, "secrets.enc"),
		fallback: map[string]string{},
	}
	s.keyring = probeKeyring()
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
			_ = keyring.Delete(keyringService, name)
			return nil
		}
		if err := keyring.Set(keyringService, name, value); err == nil {
			return nil
		}
		// One refusal is not proof the keyring is gone: a locked collection or a
		// busy D-Bus session fails once and works afterwards. Flipping the flag here
		// would hide every secret already in the keyring for the rest of the run,
		// because Get only consults it while the flag is set — so the write falls
		// through to the encrypted file and the next call tries the keyring again.
	}
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
		if v, err := keyring.Get(keyringService, name); err == nil && v != "" {
			return v, true
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
