package main

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"time"

	"github.com/wailsapp/wails/v2/pkg/runtime"

	"kichat/internal/config"
	"kichat/internal/dbkey"
	"kichat/internal/store"
	"kichat/internal/uimsg"
)

// DatabaseStatus is what the window needs to decide whether it can show the history
// or has to ask for a passphrase first.
type DatabaseStatus struct {
	// Locked means no key could be resolved, so every binding except the ones in this
	// file answers with the db.locked code.
	Locked bool `json:"locked"`
	// Setup means there is no key yet — no keyring and no wrapped copy — so the
	// passphrase the user types becomes the key's wrapper rather than unwrapping one.
	Setup bool `json:"setup"`
	// Encrypted reports whether the file on disk is ciphertext.
	Encrypted bool `json:"encrypted"`
	// Recovery reports a wrapped copy beside the database.
	Recovery bool `json:"recovery"`
	// Keyring reports an OS keyring on this machine.
	Keyring bool `json:"keyring"`
	// Remembered reports the key currently held in the keyring.
	Remembered bool `json:"remembered"`
	// Reason is an i18n key explaining why the window is locked, empty when it is not.
	Reason string `json:"reason"`
}

// GetDatabaseStatus answers without requiring the database, like GetAppInfo: its
// whole purpose is the case where opening it failed.
func (a *App) GetDatabaseStatus() DatabaseStatus {
	if err := a.ensureInit(); err != nil {
		return DatabaseStatus{}
	}
	a.dbMu.Lock()
	defer a.dbMu.Unlock()
	status := DatabaseStatus{
		Locked:   a.locked,
		Setup:    a.locked && a.lockSetup,
		Recovery: a.keys.HasRecovery(),
		Keyring:  a.keys.KeyringAvailable(),
		Reason:   a.lockReason,
	}
	if encrypted, err := store.IsEncrypted(a.dbPath); err == nil {
		status.Encrypted = encrypted
	}
	if _, held := a.keys.KeyringKey(); held {
		status.Remembered = true
	}
	return status
}

// openDatabase resolves the key and opens the store, or records why it cannot.
//
// The three answers are deliberate: open silently when the keyring holds the key;
// lock with a passphrase prompt when only the wrapped copy can open it; and lock in
// setup mode when no key exists at all, which is the one case where a passphrase is
// creating the key rather than unwrapping it. A plaintext database is encrypted here,
// on the way in, so no start can run one.
func (a *App) openDatabase() error {
	encrypted, err := store.IsEncrypted(a.dbPath)
	if err != nil {
		return err
	}
	if encrypted {
		if key, held := a.keys.KeyringKey(); held {
			st, err := store.OpenKeyed(a.dbPath, key)
			switch {
			case err == nil:
				a.store, a.dbKey = st, key
				return nil
			case errors.Is(err, store.ErrWrongKey):
				// The entry is left alone: only an open that succeeds may replace it.
				a.staleKeyring = true
				a.lock("db.reason.staleKeyring")
				return nil
			default:
				return err
			}
		}
		reason := "db.reason.needsPassphrase"
		if !a.keys.HasRecovery() {
			reason = "db.reason.noKey"
		}
		a.lock(reason)
		return nil
	}

	// Plaintext or absent. Opening it plainly would defeat the point, so a key is
	// resolved first and the file is converted before it is ever opened.
	if key, held := a.keys.KeyringKey(); held {
		return a.adoptKey(key)
	}
	if !a.keys.KeyringAvailable() {
		// No keyring and no key: the user's passphrase is the only place a key can
		// come from, and that is a decision the unlock screen has to explain.
		a.lock("db.reason.noKeyring")
		a.lockSetup = true
		return nil
	}
	key, err := dbkey.NewKey()
	if err != nil {
		return err
	}
	if err := a.keys.SaveKeyring(key); err != nil {
		return err
	}
	return a.adoptKey(key)
}

// adoptKey encrypts a plaintext database if one is there and opens the store with key.
func (a *App) adoptKey(key []byte) error {
	if _, err := os.Stat(a.dbPath); err == nil {
		if err := store.Encrypt(a.dbPath, key); err != nil {
			return fmt.Errorf("encrypt database: %w", err)
		}
	}
	st, err := store.OpenKeyed(a.dbPath, key)
	if err != nil {
		return err
	}
	a.store, a.dbKey = st, key
	return nil
}

func (a *App) lock(reason string) {
	a.locked = true
	a.lockReason = reason
}

// UnlockDatabase opens the database with a passphrase.
//
// In setup mode the passphrase creates the key and wraps it; otherwise it unwraps the
// copy beside the database. A wrong passphrase and a stale keyring entry both answer
// with the same sentence, because the user's next step is the same in either case.
func (a *App) UnlockDatabase(passphrase string) error {
	if err := a.ensureInit(); err != nil {
		return err
	}
	opened, err := a.unlock(passphrase)
	if err != nil || !opened {
		return err
	}
	a.startAfterUnlock()
	return nil
}

// unlock holds dbMu for the whole switch from locked to open, and reports whether the
// app ended up usable. Split from UnlockDatabase because what runs afterwards must not
// hold the lock; see startAfterUnlock.
func (a *App) unlock(passphrase string) (bool, error) {
	a.dbMu.Lock()
	defer a.dbMu.Unlock()
	if !a.locked {
		return false, nil
	}
	if a.lockSetup {
		if len(passphrase) < dbkey.MinPassphrase {
			return false, uimsg.Errorf("db.shortPassphrase", dbkey.MinPassphrase)
		}
		key, err := dbkey.NewKey()
		if err != nil {
			return false, err
		}
		if err := a.keys.SaveRecovery(passphrase, key); err != nil {
			return false, err
		}
		if err := a.adoptKey(key); err != nil {
			// The unwrapped copy names a key that opened nothing; leaving it would
			// keep asking for a passphrase that cannot work.
			a.keys.RemoveRecovery()
			return false, err
		}
		// The key exists from here on. A retry after a later failure has to unwrap this
		// one, never run the setup branch again and mint a second key over the copy.
		a.lockSetup = false
		return true, a.completeUnlock()
	}

	key, err := a.keys.OpenRecovery(passphrase)
	if errors.Is(err, dbkey.ErrWrongPassphrase) || errors.Is(err, dbkey.ErrNoRecovery) {
		return false, uimsg.Errorf("db.wrongPassphrase")
	}
	if err != nil {
		return false, err
	}
	st, err := store.OpenKeyed(a.dbPath, key)
	if errors.Is(err, store.ErrWrongKey) {
		return false, uimsg.Errorf("db.wrongPassphrase")
	}
	if err != nil {
		return false, err
	}
	a.store, a.dbKey = st, key
	// A stale entry may be replaced now that an open has proved this key is the right
	// one. A failure to write it is not a failed unlock: the database is open, and the
	// only consequence is that the next launch asks for the passphrase again — which is
	// worth a line in the log, not a password prompt the user cannot get past.
	if a.staleKeyring && a.keys.KeyringAvailable() {
		if err := a.keys.SaveKeyring(key); err != nil {
			runtime.LogWarningf(a.ctx, "Datenbankschlüssel im Schlüsselbund ablegen: %v", err)
		}
	}
	a.staleKeyring = false
	return true, a.completeUnlock()
}

// startAfterUnlock starts what the settings ask for once the database is open.
//
// Deliberately outside dbMu: every one of these paths goes through ensure (a managed
// model through startManagedModel, the picture server through startImageServer), and
// ensure takes dbMu itself. A sync.Mutex is not reentrant, so running them from inside
// the unlock — which is what this file did first — wedged the window on a machine that
// had left a managed model switched on, with no error and nothing to click.
func (a *App) startAfterUnlock() {
	a.startEnabledModels()
	a.startEnabledImages()
}

// completeUnlock builds the services the open database makes possible.
//
// Wails runs startup() once, and it returned while the database was locked, so this is
// the first half of what startup would have done; the servers the settings ask for are
// started afterwards by the caller, with dbMu released. Building the services inside
// finishInit would run it during migration, before the window could say anything about
// the wait.
func (a *App) completeUnlock() error {
	if err := a.finishInit(); err != nil {
		// Close what was opened and stay locked. Leaving the store open while the app
		// still reports itself locked would make the retry open a second handle; and
		// treating the unlock as done would let bindings reach services that were never
		// built. The key material is already in place, so the retry is a normal unlock.
		if a.store != nil {
			a.store.Close()
			a.store = nil
		}
		a.dbKey = nil
		a.locked = true
		if a.lockReason == "" {
			a.lockReason = "db.reason.needsPassphrase"
		}
		return err
	}
	return nil
}

// StartFreshDatabase moves an unreadable database aside and starts an empty one.
//
// It is offered where a passphrase failed and no recovery copy exists, because the
// alternative is an app that can never open again. Nothing is deleted: the database,
// its logs and the wrapped key are renamed with a timestamp, so the history can still
// be recovered by someone who finds the key later.
func (a *App) StartFreshDatabase() error {
	if err := a.ensureInit(); err != nil {
		return err
	}
	opened, err := a.startFresh()
	if err != nil || !opened {
		return err
	}
	a.startAfterUnlock()
	return nil
}

// startFresh moves the unreadable database aside under dbMu and reports whether the
// result is a database this process can use. A fresh start on a machine with no
// keyring lands back in setup mode, which is a locked answer and not a failure.
func (a *App) startFresh() (bool, error) {
	a.dbMu.Lock()
	defer a.dbMu.Unlock()
	if !a.locked {
		return false, uimsg.Errorf("db.notLocked")
	}
	stamp := time.Now().UTC().Format("20060102-150405")
	for _, path := range []string{a.dbPath, a.dbPath + "-wal", a.dbPath + "-shm", a.keys.Path(), a.keys.Path() + ".tmp"} {
		if _, err := os.Stat(path); err != nil {
			continue
		}
		if err := os.Rename(path, path+"."+stamp+".orphaned"); err != nil {
			return false, fmt.Errorf("move the unreadable database aside: %w", err)
		}
	}
	// locked stays true until openDatabase succeeds or re-locks: if it returns an
	// error, every binding must keep refusing rather than reaching a nil store.
	a.lockSetup, a.staleKeyring, a.lockReason, a.dbKey = false, false, "", nil
	a.store = nil
	if err := a.openDatabase(); err != nil {
		return false, err
	}
	if a.locked {
		a.settingsMu.Lock()
		a.settings = config.Default()
		a.settingsMu.Unlock()
		return false, nil
	}
	if err := a.completeUnlock(); err != nil {
		return false, err
	}
	return true, nil
}

// SetDatabasePassphrase writes (or replaces) the wrapped copy of the key.
//
// This is the recovery path: if the keyring is lost, this passphrase is what opens the
// database on a new machine.
func (a *App) SetDatabasePassphrase(passphrase string) error {
	if err := a.ensure(); err != nil {
		return err
	}
	a.dbMu.Lock()
	defer a.dbMu.Unlock()
	if len(passphrase) < dbkey.MinPassphrase {
		return uimsg.Errorf("db.shortPassphrase", dbkey.MinPassphrase)
	}
	return a.keys.SaveRecovery(passphrase, a.dbKey)
}

// RemoveDatabasePassphrase deletes the wrapped copy. Refused unless the keyring holds
// the key this process opened the database with, because otherwise the next launch could
// not open the database at all — and an entry that does not open *this* database is
// exactly the state the stale-keyring lock leaves behind.
func (a *App) RemoveDatabasePassphrase() error {
	if err := a.ensure(); err != nil {
		return err
	}
	a.dbMu.Lock()
	defer a.dbMu.Unlock()
	if held, ok := a.keys.KeyringKey(); !ok || !bytes.Equal(held, a.dbKey) {
		return uimsg.Errorf("db.needKeyring")
	}
	return a.keys.RemoveRecovery()
}

// RemoveDatabaseKeyring forgets the key in the OS keyring, so the next launch asks for
// the passphrase. Refused unless a wrapped copy exists.
func (a *App) RemoveDatabaseKeyring() error {
	if err := a.ensure(); err != nil {
		return err
	}
	a.dbMu.Lock()
	defer a.dbMu.Unlock()
	if !a.keys.KeyringAvailable() {
		return uimsg.Errorf("db.noKeyring")
	}
	if !a.keys.HasRecovery() {
		return uimsg.Errorf("db.needRecovery")
	}
	return a.keys.RemoveKeyring()
}
