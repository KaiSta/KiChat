package main

import (
	"bytes"
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/zalando/go-keyring"

	"kichat/internal/config"
	"kichat/internal/dbkey"
	"kichat/internal/secret"
	"kichat/internal/store"
)

// databaseTestEnv points config.DataDir at a temporary directory and returns a
// constructor for Apps rooted there. The key store is isolated and the secret store is
// isolated, because both real ones touch the per-user keyring.
func databaseTestEnv(t *testing.T) func() *App {
	t.Helper()
	root := t.TempDir()
	// os.UserConfigDir reads a different variable per platform, and getting this wrong
	// on Windows would run these tests against the real ~/.config equivalent — the one
	// thing a test must never touch.
	t.Setenv("XDG_CONFIG_HOME", root)
	t.Setenv("AppData", root)
	t.Setenv("HOME", root)
	return func() *App {
		a := &App{ctx: context.Background()}
		a.newKeys = dbkey.NewIsolated
		a.newSecrets = func(dir string) (*secret.Store, error) { return secret.NewIsolated(dir) }
		return a
	}
}

// TestFirstLaunchWithoutAKeyringAsksForAPassphrase walks the one path that creates a
// key: no keyring, a plaintext database already on disk, and a passphrase that has to
// do two jobs — become the key's only home, and carry the history across.
func TestFirstLaunchWithoutAKeyringAsksForAPassphrase(t *testing.T) {
	newApp := databaseTestEnv(t)
	dir, err := config.DataDir()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	dbPath := filepath.Join(dir, "kichat.db")
	plain, err := store.Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UnixMilli()
	if _, err := plain.DB().Exec(`INSERT INTO chats(id,title,created_at,updated_at) VALUES('c1','Kept',?,?)`, now, now); err != nil {
		t.Fatal(err)
	}
	if err := plain.Close(); err != nil {
		t.Fatal(err)
	}

	a := newApp()
	if err := a.init(); err != nil {
		t.Fatalf("init: %v", err)
	}
	status := a.GetDatabaseStatus()
	if !status.Locked || !status.Setup {
		t.Fatalf("status = %+v, want locked in setup mode", status)
	}
	if status.Keyring {
		t.Fatal("the isolated key store reports a keyring")
	}
	if status.Reason != "db.reason.noKeyring" {
		t.Errorf("reason = %q", status.Reason)
	}
	// Everything that needs the history refuses, and says so with a code the window
	// can translate.
	if err := a.ensure(); err == nil || !strings.Contains(err.Error(), "db.locked") {
		t.Fatalf("ensure while locked = %v, want the db.locked code", err)
	}
	if _, err := a.ListChats(false); err == nil {
		t.Fatal("ListChats worked while locked")
	}
	// A passphrase too short to be worth the KDF is refused before anything is
	// written.
	if err := a.UnlockDatabase("short"); err == nil {
		t.Fatal("a short passphrase was accepted")
	}
	if a.GetDatabaseStatus().Recovery {
		t.Fatal("a refused passphrase wrote a recovery file")
	}

	const passphrase = "a passphrase long enough"
	if err := a.UnlockDatabase(passphrase); err != nil {
		t.Fatalf("UnlockDatabase: %v", err)
	}
	status = a.GetDatabaseStatus()
	if status.Locked || status.Setup || !status.Encrypted || !status.Recovery {
		t.Fatalf("status after unlocking = %+v", status)
	}
	chats, err := a.ListChats(false)
	if err != nil {
		t.Fatalf("ListChats after unlocking: %v", err)
	}
	if len(chats) != 1 || chats[0].Title != "Kept" {
		t.Fatalf("chats after migration = %+v, want the chat that was already there", chats)
	}

	// The file is ciphertext now, and the plaintext copy is gone.
	if encrypted, err := store.IsEncrypted(dbPath); err != nil || !encrypted {
		t.Fatalf("IsEncrypted = %v, %v", encrypted, err)
	}
	if _, err := store.Open(dbPath); err == nil {
		t.Error("the database still opens as plaintext")
	}

	// A second launch starts locked and needs the same passphrase. The wrong one is
	// refused without producing a new key, and the right one reopens the history.
	b := newApp()
	if err := b.init(); err != nil {
		t.Fatalf("second init: %v", err)
	}
	if got := b.GetDatabaseStatus(); !got.Locked || got.Setup {
		t.Fatalf("second status = %+v, want locked with a recovery key", got)
	}
	if err := b.UnlockDatabase("the wrong passphrase"); err == nil {
		t.Fatal("a wrong passphrase opened the database")
	}
	if err := b.UnlockDatabase(passphrase); err != nil {
		t.Fatalf("second UnlockDatabase: %v", err)
	}
	chats, err = b.ListChats(false)
	if err != nil || len(chats) != 1 {
		t.Fatalf("chats after the second unlock = %+v, %v", chats, err)
	}
}

// TestAFailedUnlockKeepsTheKeyItMade covers the failure after the hard part: the key
// has been generated, wrapped and used, and then building the services fails. The app
// has to go back to a locked state that retries the *unlock* — not the setup path,
// which would mint a second key and overwrite the only recovery copy.
func TestAFailedUnlockKeepsTheKeyItMade(t *testing.T) {
	newApp := databaseTestEnv(t)
	a := newApp()
	a.newSecrets = func(string) (*secret.Store, error) {
		return nil, errors.New("no secret store")
	}
	if err := a.init(); err != nil {
		t.Fatalf("init: %v", err)
	}
	const passphrase = "a passphrase long enough"
	if err := a.UnlockDatabase(passphrase); err == nil {
		t.Fatal("the unlock reported success although the services could not be built")
	}
	status := a.GetDatabaseStatus()
	if !status.Locked || status.Setup {
		t.Fatalf("status after a failed unlock = %+v, want locked and not in setup mode", status)
	}
	if !status.Encrypted || !status.Recovery {
		t.Fatalf("status after a failed unlock = %+v, want the encrypted file and its copy", status)
	}
	dir, err := config.DataDir()
	if err != nil {
		t.Fatal(err)
	}
	wrapped, err := os.ReadFile(filepath.Join(dir, dbkey.FileName))
	if err != nil {
		t.Fatalf("read the recovery key: %v", err)
	}

	a.newSecrets = func(dir string) (*secret.Store, error) { return secret.NewIsolated(dir) }
	if err := a.UnlockDatabase(passphrase); err != nil {
		t.Fatalf("retry with the same passphrase: %v", err)
	}
	if a.GetDatabaseStatus().Locked {
		t.Fatal("still locked after a successful retry")
	}
	again, err := os.ReadFile(filepath.Join(dir, dbkey.FileName))
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(wrapped, again) {
		t.Error("the recovery key was rewritten, so the retry created a second key")
	}
	// And the database it opens is the one the first attempt encrypted.
	if _, err := a.ListChats(false); err != nil {
		t.Fatalf("ListChats after the retry: %v", err)
	}
}

// TestPassphraseActionsRefuseToStrandTheDatabase pins the two refusals that keep a
// user from deleting the only way into their history.
func TestPassphraseActionsRefuseToStrandTheDatabase(t *testing.T) {
	newApp := databaseTestEnv(t)
	a := newApp()
	if err := a.init(); err != nil {
		t.Fatal(err)
	}
	if err := a.UnlockDatabase("a passphrase long enough"); err != nil {
		t.Fatal(err)
	}

	// There is no keyring here, so removing the recovery copy would leave no key at
	// all on the next launch.
	if err := a.RemoveDatabasePassphrase(); err == nil || !strings.Contains(err.Error(), "db.needKeyring") {
		t.Fatalf("RemoveDatabasePassphrase without a keyring = %v", err)
	}
	if err := a.RemoveDatabaseKeyring(); err == nil || !strings.Contains(err.Error(), "db.noKeyring") {
		t.Fatalf("RemoveDatabaseKeyring without a keyring = %v", err)
	}
	// Replacing the passphrase keeps the same history readable.
	if err := a.SetDatabasePassphrase("another passphrase here"); err != nil {
		t.Fatalf("SetDatabasePassphrase: %v", err)
	}
	b := newApp()
	if err := b.init(); err != nil {
		t.Fatal(err)
	}
	if err := b.UnlockDatabase("a passphrase long enough"); err == nil {
		t.Fatal("the replaced passphrase still opens the database")
	}
	if err := b.UnlockDatabase("another passphrase here"); err != nil {
		t.Fatalf("the new passphrase does not open the database: %v", err)
	}
}

// TestAPassphraseUnlockRemembersTheKey: a database restored on a new machine has its
// key only in kichat.key, so the first launch asks for the passphrase. Once that
// passphrase has proved the key, it belongs in the keyring, and the next launch is
// silent instead of asking again.
func TestAPassphraseUnlockRemembersTheKey(t *testing.T) {
	newApp := databaseTestEnv(t)
	entries := map[string]string{}
	newKeys := func(dir string) *dbkey.Store {
		return dbkey.NewWithKeyring(dir,
			func(_, user string) (string, error) {
				value, ok := entries[user]
				if !ok {
					return "", keyring.ErrNotFound
				}
				return value, nil
			},
			func(_, user, value string) error { entries[user] = value; return nil },
			func(_, user string) error { delete(entries, user); return nil },
		)
	}

	// The source installation: a database with a recovery copy, as a user who is about
	// to copy the data directory has.
	source := newApp()
	source.newKeys = newKeys
	if err := source.init(); err != nil {
		t.Fatalf("source init: %v", err)
	}
	const passphrase = "a passphrase long enough"
	if err := source.SetDatabasePassphrase(passphrase); err != nil {
		t.Fatalf("SetDatabasePassphrase: %v", err)
	}
	now := time.Now().UnixMilli()
	if _, err := source.store.DB().Exec(
		`INSERT INTO chats(id,title,created_at,updated_at) VALUES('restored','Restored',?,?)`, now, now); err != nil {
		t.Fatalf("insert chat: %v", err)
	}
	if err := source.store.Close(); err != nil {
		t.Fatal(err)
	}

	// The copy lands on a machine whose keyring does not know this database.
	clear(entries)
	target := newApp()
	target.newKeys = newKeys
	if err := target.init(); err != nil {
		t.Fatalf("target init: %v", err)
	}
	status := target.GetDatabaseStatus()
	if !status.Locked || status.Reason != "db.reason.needsPassphrase" || status.Remembered {
		t.Fatalf("restored database status = %+v, want locked with a passphrase and no remembered key", status)
	}
	if err := target.UnlockDatabase(passphrase); err != nil {
		t.Fatalf("UnlockDatabase: %v", err)
	}
	if got := target.GetDatabaseStatus(); !got.Remembered {
		t.Errorf("status after the passphrase unlock = %+v, want the key remembered", got)
	}
	if err := target.store.Close(); err != nil {
		t.Fatal(err)
	}

	// The launch after that finds the key in the keyring and needs no passphrase.
	next := newApp()
	next.newKeys = newKeys
	if err := next.init(); err != nil {
		t.Fatalf("next init: %v", err)
	}
	if got := next.GetDatabaseStatus(); got.Locked {
		t.Fatalf("next launch status = %+v, want it opened from the keyring", got)
	}
	chats, err := next.ListChats(false)
	if err != nil {
		t.Fatalf("ListChats: %v", err)
	}
	if len(chats) != 1 || chats[0].Title != "Restored" {
		t.Fatalf("chats after the silent launch = %+v, want the restored chat", chats)
	}
}

// restoreFixture writes an encrypted database and its matching wrapped key, as a user
// who copies both files to another machine has them.
func restoreFixture(t *testing.T, title string) (dbPath, keyPath, passphrase string) {
	t.Helper()
	dir := t.TempDir()
	dbPath = filepath.Join(dir, "kichat.db")
	plain, err := store.Open(dbPath)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UnixMilli()
	if _, err := plain.DB().Exec(
		`INSERT INTO chats(id,title,created_at,updated_at) VALUES('restored',?,?,?)`, title, now, now); err != nil {
		t.Fatal(err)
	}
	if err := plain.Close(); err != nil {
		t.Fatal(err)
	}
	key, err := dbkey.NewKey()
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Encrypt(dbPath, key); err != nil {
		t.Fatal(err)
	}
	passphrase = "a passphrase long enough"
	if err := dbkey.NewIsolated(dir).SaveRecovery(passphrase, key); err != nil {
		t.Fatal(err)
	}
	return dbPath, filepath.Join(dir, dbkey.FileName), passphrase
}

// TestRestoreInstallsACopiedDatabaseAndUnlocksIt: a fresh installation with no key at
// all can take a copied pair and end up open on the copied history, not in setup mode.
func TestRestoreInstallsACopiedDatabaseAndUnlocksIt(t *testing.T) {
	copiedDB, copiedKey, passphrase := restoreFixture(t, "Restored")
	newApp := databaseTestEnv(t)
	a := newApp()
	if err := a.init(); err != nil {
		t.Fatal(err)
	}
	if !a.GetDatabaseStatus().Setup {
		t.Fatalf("status before restore = %+v, want a blank setup-state installation", a.GetDatabaseStatus())
	}
	if err := a.restoreDatabase(copiedDB, copiedKey, passphrase); err != nil {
		t.Fatalf("restoreDatabase: %v", err)
	}
	status := a.GetDatabaseStatus()
	if status.Locked || status.Setup || !status.Recovery || !status.Encrypted {
		t.Fatalf("status after restore = %+v, want an open encrypted database with its recovery key", status)
	}
	chats, err := a.ListChats(false)
	if err != nil {
		t.Fatalf("ListChats after restore: %v", err)
	}
	if len(chats) != 1 || chats[0].Title != "Restored" {
		t.Fatalf("chats after restore = %+v, want the copied chat", chats)
	}
}

// TestRestoreRefusesAWrongPassphraseWithoutReplacingTheCurrentDatabase makes the
// validation step visible: the copied pair is opened in a temporary directory first, so
// its failure leaves the database that was already here readable with its own key.
func TestRestoreRefusesAWrongPassphraseWithoutReplacingTheCurrentDatabase(t *testing.T) {
	copiedDB, copiedKey, _ := restoreFixture(t, "Restored")
	newApp := databaseTestEnv(t)
	a := newApp()
	if err := a.init(); err != nil {
		t.Fatal(err)
	}
	const oldPassphrase = "the old passphrase"
	if err := a.UnlockDatabase(oldPassphrase); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UnixMilli()
	if _, err := a.store.DB().Exec(
		`INSERT INTO chats(id,title,created_at,updated_at) VALUES('existing','Existing',?,?)`, now, now); err != nil {
		t.Fatal(err)
	}
	if err := a.store.Close(); err != nil {
		t.Fatal(err)
	}

	b := newApp()
	if err := b.init(); err != nil {
		t.Fatal(err)
	}
	if err := b.restoreDatabase(copiedDB, copiedKey, "the wrong passphrase"); err == nil || !strings.Contains(err.Error(), "db.wrongPassphrase") {
		t.Fatalf("restore with a wrong passphrase = %v, want the db.wrongPassphrase code", err)
	}
	if !b.GetDatabaseStatus().Locked {
		t.Fatal("a refused restore left the database open")
	}
	if err := b.UnlockDatabase(oldPassphrase); err != nil {
		t.Fatalf("the existing database no longer unlocks: %v", err)
	}
	chats, err := b.ListChats(false)
	if err != nil {
		t.Fatalf("ListChats after the refused restore: %v", err)
	}
	if len(chats) != 1 || chats[0].Title != "Existing" {
		t.Fatalf("chats after the refused restore = %+v, want the existing history", chats)
	}
}

// TestRestoreReplacesTheCurrentDatabaseAndKeepsItAside is the successful case with
// history already here: the restored pair takes over, and the previous database and
// key are kept beside it rather than deleted.
func TestRestoreReplacesTheCurrentDatabaseAndKeepsItAside(t *testing.T) {
	copiedDB, copiedKey, passphrase := restoreFixture(t, "Restored")
	newApp := databaseTestEnv(t)
	a := newApp()
	if err := a.init(); err != nil {
		t.Fatal(err)
	}
	const oldPassphrase = "the old passphrase"
	if err := a.UnlockDatabase(oldPassphrase); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UnixMilli()
	if _, err := a.store.DB().Exec(
		`INSERT INTO chats(id,title,created_at,updated_at) VALUES('existing','Existing',?,?)`, now, now); err != nil {
		t.Fatal(err)
	}
	if err := a.store.Close(); err != nil {
		t.Fatal(err)
	}

	b := newApp()
	if err := b.init(); err != nil {
		t.Fatal(err)
	}
	if err := b.restoreDatabase(copiedDB, copiedKey, passphrase); err != nil {
		t.Fatalf("restoreDatabase: %v", err)
	}
	chats, err := b.ListChats(false)
	if err != nil {
		t.Fatalf("ListChats after restore: %v", err)
	}
	if len(chats) != 1 || chats[0].Title != "Restored" {
		t.Fatalf("chats after restore = %+v, want only the copied chat", chats)
	}
	for _, pattern := range []string{"kichat.db.*.orphaned", "kichat.key.*.orphaned"} {
		matches, err := filepath.Glob(filepath.Join(b.dataDir, pattern))
		if err != nil || len(matches) == 0 {
			t.Errorf("restore left no %s: %v, %v", pattern, matches, err)
		}
	}
}

// TestStartFreshMovesAnUnreadableDatabaseAside is the last resort: an encrypted
// database with no recovery copy and no keyring can never be opened, and the app has
// to be usable again without deleting anything.
func TestStartFreshMovesAnUnreadableDatabaseAside(t *testing.T) {
	newApp := databaseTestEnv(t)
	a := newApp()
	if err := a.init(); err != nil {
		t.Fatal(err)
	}
	if err := a.UnlockDatabase("a passphrase long enough"); err != nil {
		t.Fatal(err)
	}
	// Written through the store rather than CreateChat: the bound method emits an
	// event, and a Wails emit needs the real lifecycle context.
	now := time.Now().UnixMilli()
	if _, err := a.store.DB().Exec(`INSERT INTO chats(id,title,created_at,updated_at) VALUES('gone','Gone',?,?)`, now, now); err != nil {
		t.Fatal(err)
	}
	dir := a.dataDir
	if err := a.store.Close(); err != nil {
		t.Fatal(err)
	}
	// Neither place has the key any more, which is the state a user reaches after a
	// keyring reset with no passphrase.
	if err := os.Remove(filepath.Join(dir, dbkey.FileName)); err != nil {
		t.Fatal(err)
	}
	a.store = nil

	b := newApp()
	if err := b.init(); err != nil {
		t.Fatal(err)
	}
	status := b.GetDatabaseStatus()
	if !status.Locked || status.Reason != "db.reason.noKey" {
		t.Fatalf("status = %+v, want locked with no key", status)
	}
	if status.Recovery {
		t.Fatal("a recovery key is reported for a database that has none")
	}
	if err := b.UnlockDatabase("a passphrase long enough"); err == nil {
		t.Fatal("a passphrase opened a database whose key is gone")
	}
	if err := b.StartFreshDatabase(); err != nil {
		t.Fatalf("StartFreshDatabase: %v", err)
	}
	if got := b.GetDatabaseStatus(); !got.Locked || !got.Setup {
		t.Fatalf("status after a fresh start = %+v, want setup mode", got)
	}
	if err := b.UnlockDatabase("a fresh passphrase"); err != nil {
		t.Fatalf("UnlockDatabase after a fresh start: %v", err)
	}
	chats, err := b.ListChats(false)
	if err != nil {
		t.Fatalf("ListChats: %v", err)
	}
	if len(chats) != 0 {
		t.Fatalf("a fresh database has %d chats", len(chats))
	}
	// Nothing was deleted: the old database and its key are beside the new one.
	orphans, err := filepath.Glob(filepath.Join(dir, "kichat.db.*.orphaned"))
	if err != nil || len(orphans) == 0 {
		t.Fatalf("orphaned database not found: %v, %v", orphans, err)
	}
	// A recovery key that does exist is moved aside with the database, so a later
	// attempt with that passphrase can still open the old file by hand.
	c := newApp()
	if err := c.init(); err != nil {
		t.Fatal(err)
	}
	if got := c.GetDatabaseStatus(); !got.Locked || !got.Recovery {
		t.Fatalf("status before the second fresh start = %+v", got)
	}
	if err := c.StartFreshDatabase(); err != nil {
		t.Fatalf("second StartFreshDatabase: %v", err)
	}
	if keys, _ := filepath.Glob(filepath.Join(dir, "kichat.key.*.orphaned")); len(keys) == 0 {
		t.Error("the recovery key was not kept beside the orphaned database")
	}
}
