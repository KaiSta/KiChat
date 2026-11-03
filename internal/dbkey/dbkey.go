// Package dbkey holds the key that encrypts the chat database, and the two places it
// may be kept: the OS keyring, and a passphrase-wrapped copy of the file
// (kichat.key) beside the database.
//
// It does something internal/secret deliberately does not: when the keyring is
// unavailable it refuses to invent a fallback. secret's encrypted file is protected
// by a key stored next to it, which is enough for a stray API key and useless against
// a copied directory — and a copied directory is the threat the database key exists
// for. A key either lives in the keyring, or is wrapped under a passphrase the user
// knows, or there is none.
package dbkey

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/base64"
	"encoding/binary"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/zalando/go-keyring"
	"golang.org/x/crypto/argon2"
)

// KeySize is the length of the key the encrypting VFS accepts.
const KeySize = 32

// MinPassphrase is the shortest passphrase a recovery copy is written with. Short
// enough to be typed, long enough that the Argon2id cost below is not the only thing
// standing between a stolen file and a guessing attack.
const MinPassphrase = 8

const (
	keyringService = "kichat"
	keyringEntry   = "db-key"
	// FileName is the wrapped copy, beside the database.
	FileName = "kichat.key"
	// magic marks the file, so a stray file of the same name is rejected as such
	// rather than parsed as gibberish.
	magic = "KICHATKE"
	// version is the format version, and kdf is 1 for Argon2id with the parameters
	// stored in the header. Both travel in the file so a change is detected instead
	// of silently producing a wrong key.
	version = 1
	kdfID   = 1
)

// The Argon2id cost. 64 MiB, three passes and four lanes measured 23 ms on the
// development machine, which is what makes a launch prompt feel immediate.
const (
	argonTime   uint32 = 3
	argonMemory uint32 = 64 * 1024 // KiB
	argonLanes  uint32 = 4
)

var (
	// ErrWrongPassphrase means the file is there and intact but the passphrase does
	// not unwrap it. Distinct from damage on purpose: the two need different advice.
	ErrWrongPassphrase = errors.New("dbkey: wrong passphrase")
	// ErrNoRecovery means there is no wrapped copy beside the database.
	ErrNoRecovery = errors.New("dbkey: no recovery key")
	// ErrNoKeyring means the OS keyring cannot be used on this machine.
	ErrNoKeyring = errors.New("dbkey: no OS keyring")
	// ErrNoKey means neither place holds a key.
	ErrNoKey = errors.New("dbkey: no key for this database")
	// ErrDamaged means the file is not a wrapped key this build understands: too
	// short, another magic, another version. Different from a wrong passphrase,
	// which is an intact file that this passphrase does not open.
	ErrDamaged = errors.New("dbkey: recovery key is damaged or from another version")
	// ErrShortPassphrase is returned instead of writing a copy guarded by a
	// passphrase too short to be worth the KDF.
	ErrShortPassphrase = fmt.Errorf("dbkey: passphrase must be at least %d characters", MinPassphrase)
)

// Store owns the key material for one data directory.
type Store struct {
	dir string
	// The keyring calls are fields so a test can exercise both paths without touching
	// the real keyring, which is a per-user global resource.
	get           func(service, user string) (string, error)
	set           func(service, user, password string) error
	delete        func(service, user string) error
	keyringUsable bool

	// keyText, keyRead and keyMiss memoise what the keyring answered, so one start
	// reads the key once. It is asked for while the database is opened and again when
	// the status the unlock screen reads is built, and on macOS a read means spawning
	// /usr/bin/security for the Keychain (go-keyring has no other way in).
	//
	// Only a definitive "not there" is remembered. A keyring that is locked or busy
	// refuses once and answers afterwards, so that refusal is asked about again.
	keyText string
	keyRead bool
	keyMiss bool
}

// New returns a store for dir, using the OS keyring when one is available.
func New(dir string) *Store {
	return newStore(dir, true)
}

// NewIsolated returns a store that never touches the OS keyring. Tests use this one.
func NewIsolated(dir string) *Store {
	return newStore(dir, false)
}

func newStore(dir string, probe bool) *Store {
	s := &Store{
		dir:    dir,
		get:    keyring.Get,
		set:    keyring.Set,
		delete: keyring.Delete,
	}
	if probe {
		// A read of an entry that may not exist separates "no keyring" (D-Bus or
		// libsecret missing) from "not stored yet" (ErrNotFound).
		_, err := keyring.Get(keyringService, keyringEntry)
		s.keyringUsable = err == nil || errors.Is(err, keyring.ErrNotFound)
	}
	return s
}

// KeyringAvailable reports whether the OS keyring can be used here.
func (s *Store) KeyringAvailable() bool { return s.keyringUsable }

// NewKey returns a fresh random key.
func NewKey() ([]byte, error) {
	key := make([]byte, KeySize)
	if _, err := rand.Read(key); err != nil {
		return nil, fmt.Errorf("generate database key: %w", err)
	}
	return key, nil
}

// KeyringKey returns the key held in the keyring.
//
// A missing entry, an unusable keyring and an entry that does not decode all answer
// false. The last one is treated as absent rather than as an error so a stale entry
// cannot stop the recovery passphrase from opening the database.
func (s *Store) KeyringKey() ([]byte, bool) {
	if !s.keyringUsable || s.get == nil {
		return nil, false
	}
	if !s.keyRead && !s.keyMiss {
		text, err := s.get(keyringService, keyringEntry)
		switch {
		case err == nil:
			s.keyText, s.keyRead = text, true
		case errors.Is(err, keyring.ErrNotFound):
			s.keyMiss = true
		default:
			return nil, false
		}
	}
	if !s.keyRead {
		return nil, false
	}
	key, err := base64.StdEncoding.DecodeString(s.keyText)
	if err != nil || len(key) != KeySize {
		return nil, false
	}
	return key, true
}

// SaveKeyring stores key in the keyring.
func (s *Store) SaveKeyring(key []byte) error {
	if len(key) != KeySize {
		return fmt.Errorf("dbkey: key must be %d bytes", KeySize)
	}
	if !s.keyringUsable || s.set == nil {
		return ErrNoKeyring
	}
	text := base64.StdEncoding.EncodeToString(key)
	if err := s.set(keyringService, keyringEntry, text); err != nil {
		return fmt.Errorf("store database key in the keyring: %w", err)
	}
	s.keyText, s.keyRead, s.keyMiss = text, true, false
	return nil
}

// RemoveKeyring forgets the keyring entry. The database stays readable through the
// wrapped copy, and the next launch asks for its passphrase.
func (s *Store) RemoveKeyring() error {
	if !s.keyringUsable || s.delete == nil {
		return ErrNoKeyring
	}
	if err := s.delete(keyringService, keyringEntry); err != nil && !errors.Is(err, keyring.ErrNotFound) {
		return fmt.Errorf("remove database key from the keyring: %w", err)
	}
	// Anything read before this call is the key that was just removed.
	s.keyText, s.keyRead, s.keyMiss = "", false, false
	return nil
}

// Path is where the wrapped copy lives.
func (s *Store) Path() string { return filepath.Join(s.dir, FileName) }

// HasRecovery reports whether a wrapped copy exists.
func (s *Store) HasRecovery() bool {
	_, err := os.Stat(s.Path())
	return err == nil
}

// SaveRecovery writes key wrapped under passphrase.
//
// The header is authenticated along with the key, so a flipped version byte, KDF
// parameter, salt or nonce fails the open instead of steering the reader at a key it
// can never match. The file is written to a temporary name and renamed, so a crash
// leaves the previous copy rather than half of a new one.
func (s *Store) SaveRecovery(passphrase string, key []byte) error {
	if len(key) != KeySize {
		return fmt.Errorf("dbkey: key must be %d bytes", KeySize)
	}
	if len(passphrase) < MinPassphrase {
		return ErrShortPassphrase
	}
	if err := os.MkdirAll(s.dir, 0o700); err != nil {
		return fmt.Errorf("create data directory: %w", err)
	}
	salt := make([]byte, 16)
	if _, err := rand.Read(salt); err != nil {
		return fmt.Errorf("generate salt: %w", err)
	}
	header := make([]byte, 0, 8+1+1+12+16)
	header = append(header, magic...)
	header = append(header, version, kdfID)
	for _, v := range []uint32{currentKDF.time, currentKDF.memory, uint32(currentKDF.lanes)} {
		header = binary.BigEndian.AppendUint32(header, v)
	}
	header = append(header, salt...)

	aead, err := newAEAD(passphrase, salt, currentKDF)
	if err != nil {
		return err
	}
	nonce := make([]byte, aead.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return fmt.Errorf("generate nonce: %w", err)
	}
	out := append(header, nonce...)
	out = aead.Seal(out, nonce, key, header)

	tmp := s.Path() + ".tmp"
	// 0600 before anything is written: the wrapped key is not a secret the file
	// mode may relax, even for the moment before the rename.
	f, err := os.OpenFile(tmp, os.O_WRONLY|os.O_CREATE|os.O_TRUNC, 0o600)
	if err != nil {
		return fmt.Errorf("write recovery key: %w", err)
	}
	if _, err := f.Write(out); err != nil {
		f.Close()
		os.Remove(tmp)
		return fmt.Errorf("write recovery key: %w", err)
	}
	if err := f.Sync(); err != nil {
		f.Close()
		os.Remove(tmp)
		return fmt.Errorf("sync recovery key: %w", err)
	}
	if err := f.Close(); err != nil {
		os.Remove(tmp)
		return err
	}
	if err := os.Rename(tmp, s.Path()); err != nil {
		// Windows cannot rename onto a file that already exists, and replacing an
		// existing copy does exactly that. Removing it first is the only order that
		// works there; on Unix the rename above has already succeeded.
		if removeErr := os.Remove(s.Path()); removeErr == nil {
			err = os.Rename(tmp, s.Path())
		}
		if err != nil {
			os.Remove(tmp)
			return fmt.Errorf("install recovery key: %w", err)
		}
	}
	return nil
}

// OpenRecovery unwraps the key with passphrase.
func (s *Store) OpenRecovery(passphrase string) ([]byte, error) {
	blob, err := os.ReadFile(s.Path())
	if errors.Is(err, os.ErrNotExist) {
		return nil, ErrNoRecovery
	}
	if err != nil {
		return nil, fmt.Errorf("read recovery key: %w", err)
	}
	const headerLen = 8 + 1 + 1 + 12 + 16
	if len(blob) < headerLen+12+KeySize {
		return nil, ErrDamaged
	}
	if string(blob[:8]) != magic || blob[8] != version || blob[9] != kdfID {
		return nil, ErrDamaged
	}
	salt := blob[22:38]
	header := blob[:headerLen]
	nonce := blob[headerLen : headerLen+12]
	sealed := blob[headerLen+12:]

	// The cost travels with the file, so an older copy still opens after the
	// constants change — and a header that names an impossible cost is damage rather
	// than a passphrase to argue with.
	// The three parameters are 32-bit each; the lane count is narrowed to the uint8
	// argon2 takes, after checking it fits — 260 would otherwise truncate to 4 and
	// slip past the bound check below.
	lanes := binary.BigEndian.Uint32(blob[18:22])
	if lanes > 255 {
		return nil, ErrDamaged
	}
	written := kdf{
		time:   binary.BigEndian.Uint32(blob[10:14]),
		memory: binary.BigEndian.Uint32(blob[14:18]),
		lanes:  uint8(lanes),
	}
	if !written.sane() {
		return nil, ErrDamaged
	}

	aead, err := newAEAD(passphrase, salt, written)
	if err != nil {
		return nil, err
	}
	key, err := aead.Open(nil, nonce, sealed, header)
	if err != nil || len(key) != KeySize {
		return nil, ErrWrongPassphrase
	}
	return key, nil
}

// RemoveRecovery deletes the wrapped copy. The keyring becomes the only way in, which
// is why callers must have one before doing this.
func (s *Store) RemoveRecovery() error {
	if err := os.Remove(s.Path()); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("remove recovery key: %w", err)
	}
	return nil
}

// kdf is the Argon2id cost a wrapped key was written with.
type kdf struct {
	time   uint32
	memory uint32 // KiB
	lanes  uint8
}

// currentKDF is what a new copy is written with: 64 MiB, three passes, four lanes,
// measured at 23 ms.
var currentKDF = kdf{time: argonTime, memory: argonMemory, lanes: uint8(argonLanes)}

// sane rejects a cost that would panic or hang the launch.
//
// argon2.IDKey panics when memory is below 8 KiB per lane, so an unchecked header is a
// crash rather than an error message. The upper bounds do the same job for a header
// that asks for gigabytes.
func (k kdf) sane() bool {
	if k.time < 1 || k.time > 100 {
		return false
	}
	if k.lanes < 1 || k.lanes > 64 {
		return false
	}
	if k.memory < 8*uint32(k.lanes) || k.memory > 1<<20 {
		return false
	}
	return true
}

// newAEAD derives the wrapping key from passphrase and salt.
func newAEAD(passphrase string, salt []byte, cost kdf) (cipher.AEAD, error) {
	kek := argon2.IDKey([]byte(passphrase), salt, cost.time, cost.memory, cost.lanes, KeySize)
	block, err := aes.NewCipher(kek)
	if err != nil {
		return nil, err
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	return aead, nil
}
