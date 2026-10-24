package store

import (
	"encoding/hex"
	"path/filepath"
	"sync"

	sqlite3 "github.com/ncruces/go-sqlite3"
	"github.com/ncruces/go-sqlite3/vfs"
	_ "github.com/ncruces/go-sqlite3/vfs/adiantum"
)

// cryptVFSName is the VFS every encrypted store opens through. It wraps the
// "adiantum" VFS that the blank import above registers.
const cryptVFSName = "kichat"

// KeySize is the length of the key the encrypting VFS accepts: Adiantum's default
// construction is XChaCha12 over AES-256, and the VFS refuses any other length.
const KeySize = 32

// keyVFS supplies each database's key at the moment SQLite opens it.
//
// The alternative the VFS offers — a key pragma on every new connection — cannot
// work for a database that already exists. `sqlite3.Open` reads the first page
// before any statement runs, and an encrypting VFS has no key yet at that point, so
// it either answers "empty file" (and SQLite treats a populated database as new) or
// fails with CANTOPEN. Measured: the pragma path creates a database that reopens
// only as "unable to open database file"; the key-in-URI path works, and this is the
// same thing without the key ever entering a string that outlives the open.
type keyVFS struct {
	vfs.VFS
	mu   sync.RWMutex
	keys map[string][]byte
}

var encryption = &keyVFS{VFS: vfs.Find("adiantum"), keys: map[string][]byte{}}

func init() { vfs.Register(cryptVFSName, encryption) }

func (k *keyVFS) install(path string, key []byte) {
	k.mu.Lock()
	k.keys[filepath.Clean(path)] = key
	k.mu.Unlock()
}

func (k *keyVFS) forget(path string) {
	k.mu.Lock()
	delete(k.keys, filepath.Clean(path))
	k.mu.Unlock()
}

func (k *keyVFS) key(path string) []byte {
	k.mu.RLock()
	defer k.mu.RUnlock()
	if key, ok := k.keys[filepath.Clean(path)]; ok {
		return key
	}
	// One database per process is the normal case and the path SQLite resolved may
	// differ from the one handed in (a symlinked temporary directory, a volume with
	// its own spelling). Falling back to the only installed key cannot open the
	// wrong database: a key that is not that file's key fails to decrypt it.
	if len(k.keys) == 1 {
		for _, key := range k.keys {
			return key
		}
	}
	return nil
}

// OpenFilename hands the key to the encrypting file before SQLite reads a page.
//
// Only the main database is keyed: the -wal, the rollback journal and temporary
// files find their key through the database file they belong to, which is how the
// VFS keeps them encrypted as well.
func (k *keyVFS) OpenFilename(name *vfs.Filename, flags vfs.OpenFlag) (vfs.File, vfs.OpenFlag, error) {
	f, out, err := k.VFS.(vfs.VFSFilename).OpenFilename(name, flags)
	if err != nil || f == nil || flags&vfs.OPEN_MAIN_DB == 0 || name == nil {
		return f, out, err
	}
	// Fail closed. Returning the file without a key lets SQLite treat an existing
	// database as empty and, on a new path, write a plaintext one that the caller then
	// believes is encrypted. A missing key is an error, never a quieter fallback.
	key := k.key(name.String())
	if len(key) != KeySize {
		f.Close()
		return nil, out, sqlite3.IOERR_BADKEY
	}
	p, ok := f.(vfs.FilePragma)
	if !ok {
		f.Close()
		return nil, out, sqlite3.IOERR_BADKEY
	}
	if _, err := p.Pragma("hexkey", hex.EncodeToString(key)); err != nil {
		f.Close()
		return nil, out, err
	}
	return f, out, nil
}
