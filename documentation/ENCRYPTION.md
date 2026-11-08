# Encrypted database

How KiChat's `kichat.db` is encrypted at rest, why this shape was chosen over the five
others that were considered, and how the build works. `AGENTS.md` is the index;
`CODE_MAP.md` says where the code lives. Before this work the database was **plaintext**,
with the file modes and `secure_delete` as the only protection (§1).

**Status: built.** The database is encrypted page by page in the SQLite file (§3), the
key lives in the OS keyring by default with a passphrase-wrapped copy beside the database
as the recovery path (§4), and an existing plaintext database is converted on the next
start (§5). Both decisions survived implementation; one mechanical detail did not — the
key cannot be supplied through a connection hook, and §3 records what the driver
actually required and how it was measured.

Where a number or a behaviour was not verified, the sentence says so. §10 keeps the
measurements, §8 lists what the tests assert, and the parts that are still open are in
§11.

## 1. The threat, and what it does and does not cover

The case this exists for: **a copy of the database gets out** — the file copied off a
laptop, a disk image, or `~/.config/kichat` synced to a cloud folder and taken from there.
Encryption at rest answers that case and only that case.

| scenario | encrypted file helps? |
| --- | --- |
| `kichat.db` copied, disk imaged, folder synced to a cloud drive | **yes** — the file alone is useless without the key |
| someone uses the running app on an unlocked machine | no |
| malware running as the user | no — it reads the keyring, and the memory |
| plaintext in swap, hibernation or a core dump | no — this design does not reach there |
| a cloud snapshot taken before a chat was deleted | no — the old snapshot still has it |
| an attacker who also has the login password **and** the copied home directory | no — see §4 on where the keyring's own key comes from |

Full-disk encryption and a real login password remain the foundation. This is the layer
that keeps a *copied file* from being a copied conversation.

## 2. Designs that were considered and rejected

**Keep `modernc.org/sqlite` and encrypt pages underneath it.** There is no hook. The
driver has no SQLCipher, SEE or `sqlite3mc` variant, and its only VFS package is
documented as read-only ("it has no write path and refuses to create a journal",
[`modernc.org/sqlite/vfs`](https://pkg.go.dev/modernc.org/sqlite/vfs)). A Go-side page
encryption needs a VFS with a write path, which this driver does not expose.

**cgo SQLCipher.** It would give page MACs and `PRAGMA rekey`, and it costs the build
matrix: `CGO_ENABLED=1` on all four targets, a Windows C toolchain plus OpenSSL for
`make windows`, and the end of `GOOS=windows/darwin go vet ./...` and `make vet-arm64`
(the Linux half of that gate runs with `CGO_ENABLED=0`). The Go drivers are also thin on
the ground: `mutecomm/go-sqlcipher` last shipped in 2020, upstream `mattn/go-sqlite3`
never merged encryption, and the maintained fork (`WissCore/go-sqlcipher` v4.15.0) needs
`libcrypto` headers and does not test Windows in CI. Not worth the release matrix.

**Keep the database in memory and put only ciphertext on disk** — the idea that started
this investigation. It works, and it is portable, but not the way it first looks: a
`memfd` is Linux-only, and SQLite cannot use one anyway, because WAL wants to create
`<path>-wal` beside the file. The portable form is `file::memory:` plus the driver's
`Serialize`/`Deserialize` on the connection (verified in the module's `conn.go`;
[modernc.org/sqlite](https://pkg.go.dev/modernc.org/sqlite)), measured in §10 at 25 ms
and 7 ms for a 100 MiB database. What it costs:

- **The crash window is the database.** Every kill −9, OOM or power cut loses everything
  since the last flush; SQLite's crash safety is gone by construction.
- **Every flush rewrites everything.** 100 MiB on a 30-second debounce is ~12 GB written
  per hour of active use — and the same 12 GB uploaded per hour by whichever cloud client
  watches the folder, as a new file version each time. A picture-heavy history (base64 in
  `documents`, so +33%) makes that seconds per flush.
- **RAM is the whole history**, plus a second copy during `Serialize`, plus the ciphertext:
  a ~3× spike on a 400 MiB history.
- **Two instances clobber each other.** Today SQLite serialises two copies of the app;
  with a memory database the second instance flushes a stale image over the first's work
  unless a lock file is added.

**Whole-file decrypt-on-open / encrypt-on-close.** Plaintext would exist on disk for the
whole session — and the threat is a cloud client that copies continuously, so this fails
exactly where it is needed.

**Field-level encryption on the current driver** fails on this schema: `chunks_fts` holds
the full text of every document, and the three caches hold second copies.

**Rejected for effort, recorded here so it is not re-litigated:** a passphrase on the
*archive* format is already built and separate (`internal/backup`); it does not protect
the live database and this plan does not change it.

## 3. The design: page-level encryption in the file

`internal/store` keeps its `*sql.DB` shape, so every query, the migrations, the bundle
code and the 34 `store.Open` call sites stay as they are. What changes is what is under
`database/sql`: the driver is `github.com/ncruces/go-sqlite3` (pure Go — a WASM build of
SQLite, so all four targets still build without a C toolchain), and an encrypted store
opens through a VFS wrapper that supplies the key:

```go
// internal/store/crypt.go — the "kichat" VFS wraps "adiantum".
func (k *keyVFS) OpenFilename(name *vfs.Filename, flags vfs.OpenFlag) (vfs.File, vfs.OpenFlag, error) {
	f, out, err := k.VFS.(vfs.VFSFilename).OpenFilename(name, flags)
	if err != nil || flags&vfs.OPEN_MAIN_DB == 0 { return f, out, err }
	if _, err := f.(vfs.FilePragma).Pragma("hexkey", hex.EncodeToString(k.key(name.String()))); err != nil {
		f.Close()
		return nil, out, err
	}
	return f, out, nil
}
```

Three details are load-bearing, and the first one contradicts this document's first draft:

- **The key cannot go through a connection hook.** Measured: `PRAGMA hexkey` executed from
  `driver.Open`'s callback creates a database, and that database then reopens only as
  *unable to open database file*. `sqlite3.Open` reads the first page before any statement
  runs, so an encrypting VFS with no key at that moment either answers "empty file" — and
  SQLite treats a populated database as new — or fails with CANTOPEN. The key has to be
  installed during `OpenFilename`, before the first read. It is still not in the DSN: a URI
  parameter is readable through `vfs.Filename.URIParameters` and can reach an error string,
  and this app keeps a log file. The VFS holds one key per database path and drops it in
  `Close`.
- **`PRAGMA temp_store = memory`** on keyed connections. The VFS encrypts temporary files
  with random keys, so spilling is not a leak — it is work for nothing.
- **FTS5 is a registration, not just an import.** `ext/fts5` registers the extension on
  each connection (`fts5.Register(c)`); without that call `CREATE VIRTUAL TABLE … fts5`
  fails with "no such module", and the failure is quiet: the probe in `Open` sets
  `FTS = false` and search degrades to nothing. A test asserts `FTS == true` and a real hit
  after a migration.

What the VFS covers: the database file, `-wal`, the rollback journal and temporary files.
One correction to the earlier draft: the `-shm` file is **not** encrypted. It is mapped
through `SharedMemory()` and holds only the WAL index — page numbers and checksums — never
page content, which is what the sidecar test asserts by writing a marker and finding it in
none of the three files.

The file is **not SQLCipher format** and not readable by the `sqlite3` CLI. There is no
export to a plaintext database: `VACUUM INTO` through the same VFS would produce another
encrypted file, and a read-through copy is the open question in §11. The encrypted backup
archive (`internal/backup`) is what leaves the app today.

The alternative in the same family is [`gosqlite.org/vfs/crypto`](https://gosqlite.com/docs/guides/encryption/),
which also offers Adiantum and AES-XTS over a pure-Go driver, with a vault module for
keyslots and tamper evidence. It is newer (v0.14.0, July 2026) and less exercised; ncruces
wraps a real SQLite build and is the safer of the two to move a working app onto.

## 4. The key

**32 random bytes**, generated once, never derived from anything except a passphrase when
the user chooses one.

| mode | where the key lives | launch |
| --- | --- | --- |
| **keyring** (default) | OS keyring, service `kichat`, entry `db-key` | silent |
| **keyring + recovery** (recommended default) | keyring, plus a passphrase-wrapped copy in `kichat.key` | silent, and the passphrase opens the database if the keyring is ever lost |
| **passphrase only** | nowhere; the wrapped copy is unwrapped at launch | a prompt before the database opens |

`kichat.key`, beside the database, 0600 inside the 0700 directory:

```
"KICHATKE"  magic (8)      version (1)     KDF id (1)     time/memory/lanes (3×4)
salt (16)                  nonce (12)      Seal(key, passphrase-derived KEK) (48)
```

Argon2id at 64 MiB / 3 passes / 4 lanes derives the KEK — measured at 23 ms (§10), so the
prompt is instant. The wrapped copy is what makes a keyring reset, an OS reinstall or a
new machine recoverable rather than fatal; without it, "the key lives in the keyring"
means "the key is lost with the keyring".

A passphrase unlock writes the recovered key into the OS keyring when one is available. A
restored database therefore asks once on the new machine and starts silently afterwards; a
failed keyring write is logged and does not fail the unlock, because the database is already
open and the next launch can simply ask again.

The keyring is read once per run, not once per question. The start asks for the key while
it opens the database and again for the status the unlock screen reads, and on macOS a read
is the `/usr/bin/security` process `go-keyring` spawns — so `dbkey` memoises the answer,
including a definitive "no entry". A *refusal* is not memoised: a locked or busy keyring
fails once and answers afterwards, and caching that would hide a key that really is stored.
The same rule applies to `internal/secret`, which reads every provider's key on the same
path.

Rules that follow from the threat:

- **No plaintext key fallback.** The existing `internal/secret` fallback writes an AES key
  next to its ciphertext, which is fine for a stray file and useless against a copied
  folder. It must not be reused for the database key: no keyring and no passphrase means
  a prompt, never an invented unprotected key.
- **Two different wrong answers.** A passphrase that fails to unwrap is *wrong passphrase*
  (AEAD tag). A key that opens the file but yields garbage is SQLite's `SQLITE_NOTADB`,
  which is reported as *wrong key or damaged database*. Neither is "corrupt".
- **The keyring's own key is the login.** On Linux the Secret Service keyring is encrypted
  with the login password; on macOS the Keychain and on Windows DPAPI are tied to the
  session. So the honest claim is "a copy of `kichat.db` alone is useless" — not "a stolen
  unlocked laptop is safe".

## 5. Migration of an existing plaintext database

Detection is unambiguous: an Adiantum file does not start with `SQLite format 3\0`, so the
magic is the test. The procedure, in order:

1. Ask the user to choose the key mode (§4) and create `kichat.key` if a passphrase is set.
2. Build `kichat.db.new` through a keyed connection, copying every table except the three
   caches (`search_cache`, `scrape_cache`, `digest_cache` — regenerable, and the bulk of
   the size), then rebuild `chunks_fts` from `chunks`.
3. `PRAGMA integrity_check` on the new file, and a row count per copied table compared
   against the source.
4. Rename into place only after both pass. The plaintext original is zero-overwritten,
   `fsync`ed and removed — with the honest note in the UI that this is best-effort on a
   copy-on-write or journaled filesystem, which is why §1 still starts with full-disk
   encryption.

A crash anywhere before step 4 leaves the plaintext database intact and the new file
discarded. The names are `kichat.db.new` for the copy and `kichat.db.old` for the replaced
file, and `store.RecoverMigration` runs before anything else is decided at startup: a
database that is missing comes back from `.old`, a migration that completed has its
leftover plaintext wiped, and an unfinished copy is discarded. Because the order is build,
verify, rename aside, rename in, wipe, there is no moment in which neither file exists.

It is **automatic on first launch**, not an opt-in setting: an opt-in leaves most
installations unprotected, which is the opposite of the point. With a keyring present the
whole conversion is silent — generate a key, store it, convert the file, open it. Without
one, the unlock screen asks for the passphrase that will become the key before anything is
converted.

Two details the build had to get right, and neither is visible in the step list above:

- **The `-wal` and `-shm` are named after the database, not the copy.** Renaming
  `kichat.db` to `kichat.db.old` leaves them where they were — beside the *encrypted*
  database, under its name, holding plaintext pages that SQLite would then try to replay
  into the new file. They are wiped and removed before the rename, and
  `TestEncryptRemovesTheOldSidecars` pins it.
- **The copy runs under a write lock.** The source is opened with `_txlock=immediate`, so
  the read happens inside a `BEGIN IMMEDIATE` transaction: a second instance starting at
  the same moment cannot append to the file while it is being copied. Its own attempt to
  write fails visibly against `busy_timeout` instead of being replayed into a file that is
  about to disappear.

## 6. What the window has to show

A key that is not in the keyring has to be entered **before the database exists**, and
`app.init` used to open the store before anything could ask. What exists now:

- `GetDatabaseStatus`, `UnlockDatabase`, `StartFreshDatabase` and `GetAppInfo` are the
  bindings that work while locked; every other one fails through `ensure()` with the
  `db.locked` code, which the window translates.
- `frontend/src/components/UnlockScreen.tsx` replaces the whole shell while locked. It
  renders without settings, so its language is English and its theme is the default until
  an unlock loads the real ones — a visible limitation, and the reason the strings are
  English sentences rather than keys waiting for a dictionary that is not readable yet.
  Setup mode has two fields and the 8-character rule; unlock mode has one, plus "move the
  database aside and start fresh", which says what it does to the old file and takes a
  second click.
- Settings → Data has a **Database** section: whether the file is encrypted, whether the
  keyring holds the key, whether a passphrase copy exists, the field that writes or
  replaces that copy, and the two buttons that remove one. Each removal is refused when it
  would leave no way in: the copy cannot go unless the keyring holds *the key this process
  opened the database with* — an entry that opens nothing else is exactly what the
  stale-keyring lock leaves behind, and deleting the last copy over it would strand the
  history — and the keyring key cannot go without a copy.

Every string in `de`, `en` and `zh`. The i18n test walks the Go tree for `uimsg` codes and
fails when one has no sentence in a language, which is what kept `err.db.*` complete.

## 7. Failure modes

| what happens | what the user sees |
| --- | --- |
| no keyring, no database yet (headless Linux, no Secret Service) | lock in setup mode, `db.reason.noKeyring`: the passphrase creates the key |
| keyring entry exists but does not open the file | `db.reason.staleKeyring`, and the passphrase is asked for; the entry is replaced only after an open succeeds |
| encrypted database, no key in the keyring, `kichat.key` present | `db.reason.needsPassphrase` — the unlock screen |
| encrypted database and neither place has the key | `db.reason.noKey`; the only way forward is moving it aside, which is offered and explains itself |
| wrong passphrase | `db.wrongPassphrase` — one sentence for unwrapping and for a database that is not that key's |
| disk full during a write | SQLite's own error, surfaced as it is today |
| two instances | unchanged: SQLite locking, both need the key |

## 8. Tests

- **Sidecars are ciphertext** (`TestKeyedSidecarsAreCiphertext`): a marker written into a
  message is absent from `kichat.db`, `-wal` and `-shm` while the store is open, and the
  main file does not start with the SQLite magic.
- **The passphrase copy** (`internal/dbkey`): round trip, wrong passphrase as its own
  error, a flipped salt, nonce or ciphertext byte, a foreign magic and a truncated file,
  the 0600 mode, and the short-passphrase refusal writing nothing.
- **The keyring path** against a fake, because a test must never reach the real keyring;
  and that an isolated store reports no keyring and refuses to save a key.
- **Migration** (`TestEncryptMigratesAndLeavesNothingReadable`): rows arrive, `FTS` is true
  and a `MATCH` hits, the caches stay behind, no `.old`/`.new`/`-wal` survives, and the
  ciphertext contains neither the marker nor the SQLite magic.
- **A wrong key is refused** (`TestOpenKeyedRejectsTheWrongKey`) and the failed attempt
  leaves the file openable by the right one; an already encrypted file is not rewritten;
  a foreign file is left byte-for-byte alone.
- **Interrupted migration** (`TestRecoverMigration`): a missing database comes back from
  `.old`, an unfinished `.new` is discarded.
- **The store suite twice**: `KICHAT_TEST_DBKEY=1 go test ./internal/store` opens every
  test store encrypted, so all queries go through the VFS. Run it before a release; it is
  the real safety net for the driver swap.
- **The launch paths** (`database_lock_test.go`): first launch without a keyring locks in
  setup mode and refuses everything else, a short passphrase writes nothing, a passphrase
  encrypts the existing history and a second launch with the wrong passphrase changes
  nothing; replacing a passphrase invalidates the old one; a database with no key can be
  moved aside without deleting anything.

Not covered yet: a subprocess killed with SIGKILL mid-write, reopened, data intact — the
property the memory design gives up and this one keeps. Adiantum's crash behaviour is
inherited from SQLite's WAL, but this app has not measured its own case.

## 9. Limits this design accepts

- **No tamper detection.** Adiantum is deterministic and carries no page MAC; an attacker
  with write access can alter or replay pages without the app noticing (they cannot read
  them). AES-XTS has the same property with FIPS-approved primitives only.
- **Deterministic encryption** means two snapshots reveal which blocks changed. `VACUUM`
  rewrites the file and undoes that, at the cost of a full rewrite.
- **Plaintext in memory.** Swap, hibernation and core dumps are outside the model;
  `mlock` needs privileges a desktop app does not have.
- **Old snapshots.** Cloud version history and backups taken before a deletion still hold
  the deleted chat, encrypted or not.
- **Not SQLCipher-compatible**, so no third-party tool opens the file.
- **An instance of the previous version that is already running** when the new one
  migrates keeps its handle to the file now called `kichat.db.old`. The write lock closes
  the window during the copy, not the window afterwards, so the correct habit is to quit
  KiChat before upgrading — which the installer and the package do on their own paths.

### What is still plaintext beside the database

Encryption covers `kichat.db`, not the directory around it. In the same owner-only
directory (`0700`) live `kichat.log` (the launch and why it ended, `0644`, rolled at
1 MiB), `kichat.key` (the passphrase-wrapped copy of the key, useless without the
passphrase, §4), and `kichat-trace.jsonl` (the diagnostic trace, `0600`, rolled at
2 MiB with one previous file). The keyring entry is outside the directory altogether.

The trace is the one that could have been a leak, so its schema is **closed rather than
redacted**: `internal/trace.Event` has 21 fields, every one an identifier, a count, a
duration, an enum or a public name, and `KindOf` maps an error to one of twelve words
instead of copying its sentence. There is no field that could hold a prompt, a reply, a
query, a URL, a file path, a chat title or a provider's error body. The guarantee is
pinned twice: `trace_test.go` compares the serialised field set against an allowlist,
and `TestTurnTraceKeepsTheShapeAndNotTheText` refuses a turn with an error quoting the
user and reads the file back to check the sentence is not in it. The chat and message
IDs that do appear point at rows that are ciphertext without the key, which is what
makes the file safe to attach to a bug report.

## 10. Measurements

Taken on this machine on 24 October 2026 (Linux x86-64, Go 1.26, `modernc.org/sqlite`
v1.60.1; `/` is a real filesystem, not tmpfs, so the write figures include disk). They
come from three throwaway programs, not from the app: they size the options, they do not
predict the app's performance.

| what | measured |
| --- | --- |
| Argon2id, 64 MiB / 3 passes / 4 lanes, 32-byte key | 23 ms (three runs, identical) |
| AES-256-GCM, 128 MiB, seal / open | 36 ms / 35 ms (~3.5 GiB/s) |
| the same 128 MiB in 4 KiB pages, the VFS shape | 28 ms — 0.8 µs per page |
| `Serialize` / `Deserialize`, 100 MiB in-memory database | 25 ms / 7 ms |
| writing 100 MiB | 241 ms into memory, 806 ms into a file |
| `VACUUM INTO` a 100 MiB database | 316 ms |
| `PRAGMA wal_checkpoint(TRUNCATE)` | 11 ms |
| a key supplied by `PRAGMA hexkey` after the connection opened | the database reopens only as *unable to open database file* — the key has to be installed before the first page is read (§3) |

What they decide: the passphrase prompt is free (23 ms), page encryption is noise next to
a write (0.8 µs), and the memory design's flush is ~60 ms of CPU plus the disk write — so
its cost is the policy (how often, how much data, how much churn), not the crypto.

## 11. Open questions

- **The recovery decision is not forced.** The plan wanted one screen on first encryption
  that made the user save a passphrase copy or accept the keyring-only risk; what exists is
  the state list and the field in Settings → Data, with no interruption. That is the honest
  gap: a keyring reset with no copy now ends at `db.reason.noKey` and a database moved
  aside. The argument for forcing it — the choice is free only once — still holds, and it
  is the first thing to add if the gap turns out to bite.
- **"Do not remember" lasts until the next unlock.** Removing the keyring key in Settings
  makes the next launch ask; once the passphrase has opened the database, the key is stored
  in the keyring again and the launch after that is silent. There is no checkbox on the
  unlock screen to skip the keyring for one session while keeping it.
- Does the encrypted file need an **export to plaintext** for a user who wants their data
  back without KiChat, or is the encrypted archive (`internal/backup`) enough? Unchanged;
  the backup archive is still the only export.

## 12. What was read

- [`vfs/adiantum`](https://github.com/ncruces/go-sqlite3/blob/main/vfs/adiantum/README.md)
  and [`vfs/xts`](https://github.com/ncruces/go-sqlite3/blob/main/vfs/xts/README.md):
  block sizes, the KDFs, what is encrypted (all files but super journals; temp files with
  random keys), the absence of `rekey`, and the no-MAC/no-forgery warnings.
- [`driver`](https://pkg.go.dev/github.com/ncruces/go-sqlite3/driver): `driver.Open`'s
  per-connection callback, `_pragma`, and the order the pragmas must be set in.
- [`adiantum` package docs](https://pkg.go.dev/github.com/ncruces/go-sqlite3/vfs/adiantum):
  the `key`/`hexkey`/`textkey` forms, the `PRAGMA` alternative, and `Wrap`.
- [`modernc.org/sqlite`](https://pkg.go.dev/modernc.org/sqlite) and its
  [`vfs`](https://pkg.go.dev/modernc.org/sqlite/vfs) package: the read-only VFS, the vtab
  package, and the pluggable page cache (which sits above the pager and cannot encrypt
  what is written).
- [`gosqlite` encryption guide](https://gosqlite.com/docs/guides/encryption/): the
  alternative VFS, its WAL/`-shm` caveat and its own no-MAC warning.
- The Go wiki on [Windows cross-compiling](https://go.dev/wiki/WindowsCrossCompiling) for
  what cgo costs a cross-build.
