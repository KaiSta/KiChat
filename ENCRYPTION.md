# Encrypted database

How KiChat's `kichat.db` gets encrypted at rest, why this shape was chosen over the five
others that were considered, and what will have to be built. `AGENTS.md` is the index;
`CODE_MAP.md` says where the code lives. Today the database is **plaintext**: the only
things protecting it are the file modes and `secure_delete` (§1).

**Status: a plan, nothing built yet.** Two decisions are taken — page-level encryption in
the SQLite file (§3), and a keyring-held key with a passphrase-wrapped copy for recovery
(§4). Everything below is either a measurement taken on this machine (§10), a fact read
from the driver's documentation (§12), or work still to do. Where a number or a behaviour
was not verified, the sentence says so.

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
`database/sql`:

```go
import (
    _ "github.com/ncruces/go-sqlite3/driver"
    _ "github.com/ncruces/go-sqlite3/ext/fts5"      // separate module since v0.35.0
    _ "github.com/ncruces/go-sqlite3/vfs/adiantum"  // registers the "adiantum" VFS
)

db, err := driver.Open(
    "file:kichat.db?vfs=adiantum&_pragma=busy_timeout(10000)&_pragma=journal_mode(WAL)…",
    func(c *sqlite3.Conn) error { return c.Exec("PRAGMA hexkey='" + hex + "'") },
)
```

Three details are load-bearing:

- **The key goes through the connect hook, not the DSN.** The VFS documentation warns
  that a `key=`/`hexkey=` URI parameter is readable through `vfs.Filename.URIParameters`;
  `driver.Open`'s callback runs on every new connection, which is also where the pragmas
  already run. A key in a DSN is a key that can reach an error string, and this app keeps
  a log file.
- **`PRAGMA temp_store = memory`.** The VFS encrypts temporary files with random keys, so
  spilling is not a leak — it is work for nothing.
- **FTS5 is an import, not a built-in.** Forgetting `ext/fts5` does not fail loudly: the
  probe in `Open` sets `FTS = false` and search silently degrades. A test asserts
  `FTS == true` and a real hit after an import.

What the VFS covers: the database file, `-wal`, `-shm`, the rollback journal and temp
files. That is the whole reason to prefer this to the memory design — the sidecars are
where the last change always is, and they are encrypted in place.

The file is **not SQLCipher format** and not readable by the `sqlite3` CLI. An export
path is `VACUUM INTO` through the same VFS (which produces another encrypted file), or a
read-through-copy into a plaintext database for a user who explicitly asks for one.

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
discarded; the migration is retried on the next start. It is **automatic on first launch
with a notice**, not an opt-in setting: an opt-in leaves most installations unprotected,
which is the opposite of the point.

## 6. What the window has to show

A key that is not in the keyring must be entered **before the database exists**, and today
`app.init` opens the store before anything can ask. So:

- `GetLockState` and `UnlockDatabase(passphrase)` are the only bindings that work while
  locked; every other one keeps failing through `ensure()` as it does now.
- An unlock screen that renders without settings — default language and theme — with
  "forgot the passphrase" offering to move the database aside and start fresh, never a
  silent delete.
- Settings → Data gains **Change passphrase**, **Remove keyring copy** (and then it asks
  every launch), and the one-time recovery step on first encryption: show the passphrase
  requirement or confirm that the keyring only is accepted, because that is the last
  moment the choice is free.

Every string in `de`, `en` and `zh`, as always.

## 7. Failure modes

| what happens | what the user sees |
| --- | --- |
| no keyring (headless Linux, no Secret Service) | passphrase mode is forced; the reason is stated once |
| keyring entry exists but is stale/wrong | *wrong key or damaged database*, with the passphrase path offered; the entry is not overwritten until an open succeeds |
| database copied without `kichat.key` and without the keyring | *this database needs its key*, with the recovery explanation |
| wrong passphrase | *wrong passphrase* — distinct from damage |
| disk full during a write | SQLite's own error, surfaced as it is today |
| two instances | unchanged: SQLite locking, both need the key |

## 8. Tests

- **Sidecars are ciphertext**: write a marker, then grep `kichat.db`, `-wal` and `-shm`
  for it while the store is open. This is the test that proves the claim rather than the
  intent, and it is the first one to write.
- **Keyring mode leaves no key material in the data directory**: open, close, grep every
  file for the hex key.
- **Passphrase mode**: `kichat.key` unwraps; a wrong passphrase is its own error; the
  header is authenticated (a flipped salt or ciphertext byte fails).
- **Migration**: a fixture plaintext database arrives with identical row counts, a working
  FTS hit, and no plaintext file left behind.
- **`FTS == true`** on every store the app opens, so a missing `ext/fts5` import is a test
  failure rather than a silent downgrade.
- **The whole existing suite runs twice** — plaintext and keyed — which is the real safety
  net for the driver swap, since almost every package opens a store.
- Optional but worthwhile: a subprocess killed with SIGKILL mid-write, reopened, data
  intact — the property the memory design gives up and this one keeps.

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

What they decide: the passphrase prompt is free (23 ms), page encryption is noise next to
a write (0.8 µs), and the memory design's flush is ~60 ms of CPU plus the disk write — so
its cost is the policy (how often, how much data, how much churn), not the crypto.

## 11. Open questions

- Does the first launch after migration **force** the recovery decision — save a
  passphrase-wrapped copy or explicitly accept keyring-only — or is it a quiet default?
  Current preference: force it once, because it is the only moment the choice is free.
- Should unlock offer **"do not remember"** per launch (type the passphrase even when the
  keyring holds the key)?
- Does the encrypted file need an **export to plaintext** for a user who wants their data
  back without KiChat, or is the encrypted archive (`internal/backup`) enough?

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
