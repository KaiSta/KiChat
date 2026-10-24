// Package backup writes and reads KiChat's portable archive of chats.
//
// One file holds a selection of chats with everything needed to continue, search and
// re-read them elsewhere, encrypted with a passphrase the user chooses. That is the
// format's whole reason to exist: the live database stays where it is, and this is the
// copy that travels — to another machine, or into a cloud folder the user already has.
//
// The reason it is a passphrase rather than a key from the OS keyring is the same:
// a keyring key does not exist on the machine receiving the file. A passphrase travels
// inside the user's head and nowhere else, and it is never written by this package.
package backup

import (
	"bytes"
	"compress/gzip"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"time"

	"golang.org/x/crypto/argon2"

	"kichat/internal/store"
)

// Format is the payload version. It is written twice on purpose: once in the clear, as
// the container's version byte, so a future reader can tell v1 apart before deriving a
// key; and once inside the encrypted JSON, so a payload that does not match the header
// is detected rather than decoded into a half-filled struct.
const Format = 1

// container layout, all written before the ciphertext:
//
//	magic   8 bytes  "KICHATBK"
//	version 1 byte   1
//	salt   16 bytes  Argon2id salt
//	nonce  12 bytes  AES-GCM nonce
//
// Everything after that is one GCM-sealed blob, so a truncated or edited file fails to
// open rather than decrypting into something almost right.
const (
	magic        = "KICHATBK"
	headerLength = len(magic) + 1 + 16 + 12

	// Key derivation: Argon2id at 64 MiB, three passes, four lanes. 64 MiB is the
	// number the SQLite encryption VFS considered when it was evaluated (see
	// AGENTS.md), and it is what makes a stolen archive inconvenient to attack
	// offline. The cost is paid twice per backup — once to write, once to open.
	kdfTime    = 3
	kdfMemory  = 64 * 1024
	kdfThreads = 4

	// MinPassphrase is the shortest passphrase an archive is written with. Decoding
	// accepts anything, because an archive made elsewhere must open.
	MinPassphrase = 8

	// maxPayload bounds a decoded archive. A gzip stream can claim to expand without
	// limit, and the file came from outside the app, so the reader stops rather than
	// filling memory.
	maxPayload = 2 << 30
)

// Errors the caller turns into a message: each one is a different thing to tell the
// user, and none of them is a programming mistake.
var (
	ErrPassphrase = errors.New("backup: wrong passphrase or damaged archive")
	ErrFormat     = errors.New("backup: not a KiChat archive")
	ErrVersion    = errors.New("backup: archive was written by a newer KiChat")
	ErrEmpty      = errors.New("backup: the archive contains no chats")
	ErrTooLarge   = errors.New("backup: archive expands beyond the size limit")
)

// Archive is the plaintext payload of one archive file.
type Archive struct {
	Format int `json:"format"`
	// App is the version that wrote it, so a bug report about an import names both
	// ends of the journey.
	App      string             `json:"app"`
	Exported time.Time          `json:"exported"`
	Chats    []store.ChatBundle `json:"chats"`
}

// Encode seals an archive with a passphrase.
func Encode(a Archive, passphrase string) ([]byte, error) {
	if len([]rune(passphrase)) < MinPassphrase {
		return nil, fmt.Errorf("backup: passphrase must be at least %d characters", MinPassphrase)
	}
	if len(a.Chats) == 0 {
		return nil, ErrEmpty
	}
	a.Format = Format

	plain, err := json.Marshal(a)
	if err != nil {
		return nil, fmt.Errorf("backup: encode: %w", err)
	}
	// Compressed before encryption, not after: the text of a chat is where the size is
	// (documents, transcripts, tool results), and ciphertext does not compress.
	var packed bytes.Buffer
	gz := gzip.NewWriter(&packed)
	if _, err := gz.Write(plain); err != nil {
		return nil, fmt.Errorf("backup: compress: %w", err)
	}
	if err := gz.Close(); err != nil {
		return nil, fmt.Errorf("backup: compress: %w", err)
	}

	salt := make([]byte, 16)
	nonce := make([]byte, 12)
	if _, err := io.ReadFull(rand.Reader, salt); err != nil {
		return nil, err
	}
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return nil, err
	}
	key := argon2.IDKey([]byte(passphrase), salt, kdfTime, kdfMemory, kdfThreads, 32)
	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}

	out := make([]byte, 0, headerLength+packed.Len()+aead.Overhead())
	out = append(out, magic...)
	out = append(out, byte(Format))
	out = append(out, salt...)
	out = append(out, nonce...)
	// The header is the additional data, so a version byte or salt edited in the file
	// fails authentication instead of steering the reader.
	out = aead.Seal(out, nonce, packed.Bytes(), out[:len(magic)+1])
	return out, nil
}

// Decode opens an archive, returning ErrPassphrase for anything whose key does not fit
// — which is what a wrong passphrase and a damaged file look like from here.
func Decode(raw []byte, passphrase string) (Archive, error) {
	if len(raw) < headerLength || string(raw[:len(magic)]) != magic {
		return Archive{}, ErrFormat
	}
	if version := raw[len(magic)]; version != Format {
		return Archive{}, ErrVersion
	}
	salt := raw[len(magic)+1 : len(magic)+1+16]
	nonce := raw[len(magic)+1+16 : headerLength]
	sealed := raw[headerLength:]

	key := argon2.IDKey([]byte(passphrase), salt, kdfTime, kdfMemory, kdfThreads, 32)
	block, err := aes.NewCipher(key)
	if err != nil {
		return Archive{}, err
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return Archive{}, err
	}
	packed, err := aead.Open(nil, nonce, sealed, raw[:len(magic)+1])
	if err != nil {
		// GCM cannot tell a wrong key from a flipped bit, and saying which would be a
		// guess: one message covers both.
		return Archive{}, ErrPassphrase
	}

	gz, err := gzip.NewReader(bytes.NewReader(packed))
	if err != nil {
		return Archive{}, ErrPassphrase
	}
	defer gz.Close()
	plain, err := io.ReadAll(io.LimitReader(gz, maxPayload+1))
	if err != nil {
		return Archive{}, ErrPassphrase
	}
	if int64(len(plain)) > maxPayload {
		return Archive{}, ErrTooLarge
	}

	var a Archive
	if err := json.Unmarshal(plain, &a); err != nil {
		return Archive{}, fmt.Errorf("backup: decode: %w", err)
	}
	if a.Format != Format {
		return Archive{}, ErrVersion
	}
	return a, nil
}

// Manifest is what can be said about an archive before anything is imported.
//
// It is returned from the decrypted payload rather than kept in the clear: a list of
// chat titles in a header would be readable by anyone who finds the file in a shared
// folder, which is exactly what the encryption is for.
type Manifest struct {
	App      string                `json:"app"`
	Exported time.Time             `json:"exported"`
	Chats    []store.BundleSummary `json:"chats"`
	Bytes    int64                 `json:"bytes"`
}

// ManifestOf summarises an archive without touching a database.
func ManifestOf(a Archive) Manifest {
	m := Manifest{App: a.App, Exported: a.Exported, Chats: make([]store.BundleSummary, 0, len(a.Chats))}
	for _, chat := range a.Chats {
		summary := chat.Summary()
		m.Chats = append(m.Chats, summary)
		m.Bytes += summary.Bytes
	}
	return m
}
