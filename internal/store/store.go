// Package store persists chats, messages, documents and caches in a single
// SQLite file, encrypted page by page when it is opened with a key.
//
// The driver is github.com/ncruces/go-sqlite3: pure Go (a WASM build of SQLite), so
// no C toolchain is needed for any of the four targets, and its VFS layer is
// writable, which is what makes encryption at rest possible at all. The previous
// driver had no writable VFS.
package store

import (
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	sqlite3 "github.com/ncruces/go-sqlite3"
	"github.com/ncruces/go-sqlite3/driver"
	"github.com/ncruces/go-sqlite3/ext/fts5"
)

// Store wraps the database handle.
type Store struct {
	db   *sql.DB
	path string
	// encrypted records whether this handle was opened with a key, so Close can
	// drop the key from the VFS table instead of leaving it installed.
	encrypted bool
	// FTS reports whether full-text search is available. It is probed at open
	// time so a build without FTS5 degrades to "documents are still readable,
	// just not searchable" instead of failing outright.
	FTS bool
}

// Open opens (creating if needed) the plaintext database at path and runs migrations.
//
// Tests use this one. The app opens every database through OpenKeyed, and the migration
// reads its source through the immediate-transaction variant of this function.
func Open(path string) (*Store, error) { return open(path, nil) }

// OpenKeyed opens (creating if needed) the database at path with every page
// encrypted under key.
//
// Without the key the file is noise: not a SQLite database to any other program,
// including the sqlite3 shell. A plaintext file is not converted here — OpenKeyed on
// one fails with ErrWrongKey — because converting has to copy, verify and replace, and
// Encrypt is what does that.
func OpenKeyed(path string, key []byte) (*Store, error) {
	if len(key) != KeySize {
		return nil, fmt.Errorf("database key must be %d bytes, got %d", KeySize, len(key))
	}
	return open(path, key)
}

// ErrWrongKey reports a file that did not decrypt: the key is not the one the
// database was written with, or the file is damaged. SQLite cannot tell the two
// apart, and calling a merely wrong key "corrupt" sends a user looking for a backup
// they do not need.
var ErrWrongKey = errors.New("store: wrong key or damaged database")

func open(path string, key []byte) (*Store, error) { return openWith(path, key, false) }

// openWith is open with one option more: immediate transactions.
//
// Encrypt uses it for its source so the copy runs under SQLite's write lock, which is
// what keeps a second instance from writing to the file while it is being read.
func openWith(path string, key []byte, immediate bool) (st *Store, err error) {
	if dir := filepath.Dir(path); dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return nil, fmt.Errorf("create data dir: %w", err)
		}
		if err := os.Chmod(dir, 0o700); err != nil {
			return nil, fmt.Errorf("restrict data dir: %w", err)
		}
	}
	if err := createOwnerOnly(path); err != nil {
		return nil, err
	}
	// The key goes to the VFS for this path, not into the DSN: a URI parameter is
	// readable through vfs.Filename.URIParameters and can reach an error string,
	// and this app keeps a log file.
	dsn := "file:" + filepath.ToSlash(path)
	var params []string
	if key != nil {
		params = append(params, "vfs="+cryptVFSName)
		encryption.install(path, key)
		defer func() {
			if st == nil {
				encryption.forget(path)
			}
		}()
	}
	if immediate {
		params = append(params, "_txlock=immediate")
	}
	if len(params) > 0 {
		dsn += "?" + strings.Join(params, "&")
	}

	db, err := driver.Open(dsn, func(c *sqlite3.Conn) error {
		// FTS5 is an extension in this driver rather than part of the build, and a
		// missing registration does not fail loudly: the probe below then reports
		// FTS = false and search degrades to nothing. A test asserts it is true.
		if err := fts5.Register(c); err != nil {
			return err
		}
		for _, pragma := range pragmas {
			if err := c.Exec(pragma); err != nil {
				return fmt.Errorf("apply %s: %w", pragma, err)
			}
		}
		if key != nil {
			// The VFS encrypts temporary files with random keys, so spilling is not
			// a leak — only work for nothing.
			if err := c.Exec("PRAGMA temp_store = memory"); err != nil {
				return fmt.Errorf("apply temp_store: %w", err)
			}
		}
		return nil
	})
	if err != nil {
		return nil, fmt.Errorf("open database: %w", err)
	}
	db.SetMaxOpenConns(1) // SQLite writers serialise; one conn avoids lock churn.
	db.SetMaxIdleConns(1)
	db.SetConnMaxLifetime(0)

	s := &Store{db: db, path: path, encrypted: key != nil}
	if err := s.migrate(); err != nil {
		db.Close()
		if key != nil {
			return nil, wrongKey(err)
		}
		return nil, err
	}
	// Housekeeping that has to happen on every start: nothing else ever removes a
	// row from the two web caches. A failure is reported rather than swallowed —
	// being unable to delete from its own database is not something to paper over,
	// and every later write would fail the same way.
	if _, err := s.PruneExpiredCaches(); err != nil {
		db.Close()
		return nil, err
	}
	return s, nil
}

// createOwnerOnly creates the database file if it is absent and makes sure it is not
// group- or world-readable.
//
// The file is created before SQLite opens it, rather than left to the driver: a driver
// creates it with 0666&^umask, which is 0644 on a default account, and a mode cannot be
// set by opening a file that already exists. An empty file is a database SQLite starts
// from scratch, so pre-creating it changes nothing else.
func createOwnerOnly(path string) error {
	f, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		return fmt.Errorf("create database file: %w", err)
	}
	// The mode above only applied if this call created the file; every database that
	// already exists carries whatever mode it was made with.
	chmodErr := f.Chmod(0o600)
	closeErr := f.Close()
	if chmodErr != nil {
		return fmt.Errorf("restrict database file: %w", chmodErr)
	}
	if closeErr != nil {
		return fmt.Errorf("close new database file: %w", closeErr)
	}
	return nil
}

// pragmas are applied to every new connection, in this order.
//
// secure_delete is the nearest thing to a delete this app has: nothing vacuums, so
// without it a removed chat and every pruned cache row stay legible in the file.
var pragmas = []string{
	"PRAGMA busy_timeout = 10000",
	"PRAGMA journal_mode = WAL",
	"PRAGMA synchronous = NORMAL",
	"PRAGMA foreign_keys = ON",
	"PRAGMA secure_delete = ON",
}

// wrongKey turns the driver's complaint about a file it cannot read into ErrWrongKey.
//
// A wrong key and a damaged file produce the same complaint from SQLite, and both
// arrive here as "file is not a database". CANTOPEN is included because that is what
// an encrypting VFS answers when it has no key at all.
func wrongKey(err error) error {
	msg := err.Error()
	for _, want := range []string{"not a database", "unable to open database file", "encrypted"} {
		if strings.Contains(msg, want) {
			return fmt.Errorf("%w: %v", ErrWrongKey, err)
		}
	}
	return err
}

// Close releases the database and drops its key from the process.
func (s *Store) Close() error {
	err := s.db.Close()
	if s.encrypted {
		encryption.forget(s.path)
	}
	return err
}

// Checkpoint copies the write-ahead log into the database file and empties it.
//
// secure_delete zeroes a deleted row's page, but the page it wrote the row to still
// lives in the `-wal` until SQLite checkpoints, and with WAL enabled that can be hours
// later — the file is emptied on a clean close, which for a desktop app is whenever the
// window is closed. So a chat the user deleted stayed readable in `kichat.db-wal` for
// the rest of the session. TRUNCATE rather than PASSIVE is the point: PASSIVE copies
// the log and leaves it as long as before.
func (s *Store) Checkpoint() error {
	if _, err := s.db.Exec("PRAGMA wal_checkpoint(TRUNCATE)"); err != nil {
		return fmt.Errorf("checkpoint the write-ahead log: %w", err)
	}
	return nil
}

// commitAndCheckpoint is how a deletion ends: the rows go first, then the log that held
// the pages they came from.
//
// A failed checkpoint is reported rather than swallowed. The deletion itself is
// committed by then, so the caller's data is still gone — what failed is the promise
// that it is also gone from the file, and that is exactly what this store now claims.
func (s *Store) commitAndCheckpoint(tx *sql.Tx) error {
	if err := tx.Commit(); err != nil {
		return err
	}
	return s.Checkpoint()
}

// DB exposes the handle for packages that need custom queries.
func (s *Store) DB() *sql.DB { return s.db }

// migrations are applied in order and tracked via PRAGMA user_version.
var migrations = []string{
	// 1: core chat tables.
	`
CREATE TABLE IF NOT EXISTS chats (
  id TEXT PRIMARY KEY,
  title TEXT NOT NULL DEFAULT '',
  created_at INTEGER NOT NULL,
  updated_at INTEGER NOT NULL,
  backend TEXT NOT NULL DEFAULT '',
  model TEXT NOT NULL DEFAULT '',
  system_prompt TEXT NOT NULL DEFAULT '',
  system_prompt_preset TEXT NOT NULL DEFAULT '',
  temperature REAL NOT NULL DEFAULT 0.7,
  top_p REAL NOT NULL DEFAULT 0.9,
  num_ctx INTEGER NOT NULL DEFAULT 0,
  archived INTEGER NOT NULL DEFAULT 0,
  auto_compact INTEGER NOT NULL DEFAULT 1,
  compact_threshold REAL NOT NULL DEFAULT 0.80,
  keep_recent_turns INTEGER NOT NULL DEFAULT 6,
  search_first INTEGER NOT NULL DEFAULT 0
);

CREATE TABLE IF NOT EXISTS messages (
  id TEXT PRIMARY KEY,
  chat_id TEXT NOT NULL REFERENCES chats(id) ON DELETE CASCADE,
  seq INTEGER NOT NULL,
  role TEXT NOT NULL,
  content TEXT NOT NULL DEFAULT '',
  reasoning TEXT NOT NULL DEFAULT '',
  name TEXT NOT NULL DEFAULT '',
  tool_calls_json TEXT NOT NULL DEFAULT '',
  tool_call_id TEXT NOT NULL DEFAULT '',
  status TEXT NOT NULL DEFAULT 'complete',
  created_at INTEGER NOT NULL,
  backend TEXT NOT NULL DEFAULT '',
  model TEXT NOT NULL DEFAULT '',
  prompt_tokens INTEGER NOT NULL DEFAULT 0,
  completion_tokens INTEGER NOT NULL DEFAULT 0,
  tokens_per_second REAL NOT NULL DEFAULT 0,
  ttft_ms INTEGER NOT NULL DEFAULT 0,
  generation_ms INTEGER NOT NULL DEFAULT 0,
  stop_reason TEXT NOT NULL DEFAULT '',
  stats_source TEXT NOT NULL DEFAULT ''
);
CREATE INDEX IF NOT EXISTS idx_messages_chat ON messages(chat_id, seq);

CREATE TABLE IF NOT EXISTS compactions (
  id TEXT PRIMARY KEY,
  chat_id TEXT NOT NULL REFERENCES chats(id) ON DELETE CASCADE,
  trigger TEXT NOT NULL DEFAULT 'auto',
  from_seq INTEGER NOT NULL DEFAULT 0,
  to_seq INTEGER NOT NULL DEFAULT 0,
  summary TEXT NOT NULL DEFAULT '',
  focus TEXT NOT NULL DEFAULT '',
  tokens_before INTEGER NOT NULL DEFAULT 0,
  tokens_after INTEGER NOT NULL DEFAULT 0,
  model TEXT NOT NULL DEFAULT '',
  backend TEXT NOT NULL DEFAULT '',
  created_at INTEGER NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_compactions_chat ON compactions(chat_id, to_seq);

CREATE TABLE IF NOT EXISTS tool_calls (
  id TEXT PRIMARY KEY,
  message_id TEXT NOT NULL DEFAULT '',
  chat_id TEXT NOT NULL DEFAULT '',
  name TEXT NOT NULL DEFAULT '',
  args_json TEXT NOT NULL DEFAULT '',
  result_json TEXT NOT NULL DEFAULT '',
  status TEXT NOT NULL DEFAULT 'pending',
  duration_ms INTEGER NOT NULL DEFAULT 0,
  error TEXT NOT NULL DEFAULT '',
  created_at INTEGER NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_tool_calls_chat ON tool_calls(chat_id, created_at);
`,
	// 2: attachments and documents.
	`
CREATE TABLE IF NOT EXISTS attachments (
  id TEXT PRIMARY KEY,
  chat_id TEXT NOT NULL REFERENCES chats(id) ON DELETE CASCADE,
  message_id TEXT NOT NULL DEFAULT '',
  kind TEXT NOT NULL DEFAULT 'file',
  filename TEXT NOT NULL DEFAULT '',
  mime TEXT NOT NULL DEFAULT '',
  size_bytes INTEGER NOT NULL DEFAULT 0,
  sha256 TEXT NOT NULL DEFAULT '',
  pages INTEGER NOT NULL DEFAULT 0,
  token_estimate INTEGER NOT NULL DEFAULT 0,
  placement TEXT NOT NULL DEFAULT 'inline',
  document_id TEXT NOT NULL DEFAULT '',
  warning TEXT NOT NULL DEFAULT '',
  created_at INTEGER NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_attachments_chat ON attachments(chat_id, created_at);

CREATE TABLE IF NOT EXISTS documents (
  id TEXT PRIMARY KEY,
  attachment_id TEXT NOT NULL DEFAULT '',
  chat_id TEXT NOT NULL DEFAULT '',
  title TEXT NOT NULL DEFAULT '',
  text TEXT NOT NULL DEFAULT '',
  token_count INTEGER NOT NULL DEFAULT 0,
  page_count INTEGER NOT NULL DEFAULT 0,
  sha256 TEXT NOT NULL DEFAULT '',
  source_url TEXT NOT NULL DEFAULT '',
  created_at INTEGER NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_documents_chat ON documents(chat_id);

CREATE TABLE IF NOT EXISTS chunks (
  id INTEGER PRIMARY KEY AUTOINCREMENT,
  document_id TEXT NOT NULL REFERENCES documents(id) ON DELETE CASCADE,
  chat_id TEXT NOT NULL DEFAULT '',
  ordinal INTEGER NOT NULL,
  heading TEXT NOT NULL DEFAULT '',
  page INTEGER NOT NULL DEFAULT 0,
  text TEXT NOT NULL,
  tokens INTEGER NOT NULL DEFAULT 0
);
CREATE INDEX IF NOT EXISTS idx_chunks_doc ON chunks(document_id, ordinal);
CREATE INDEX IF NOT EXISTS idx_chunks_chat ON chunks(chat_id);
`,
	// 3: caches, settings, counters.
	`
CREATE TABLE IF NOT EXISTS search_cache (
  query_hash TEXT PRIMARY KEY,
  provider TEXT NOT NULL DEFAULT '',
  query TEXT NOT NULL DEFAULT '',
  response_json TEXT NOT NULL DEFAULT '',
  created_at INTEGER NOT NULL,
  expires_at INTEGER NOT NULL
);

CREATE TABLE IF NOT EXISTS scrape_cache (
  url_hash TEXT PRIMARY KEY,
  url TEXT NOT NULL DEFAULT '',
  title TEXT NOT NULL DEFAULT '',
  content_md TEXT NOT NULL DEFAULT '',
  tokens INTEGER NOT NULL DEFAULT 0,
  fetched_at INTEGER NOT NULL,
  expires_at INTEGER NOT NULL
);

CREATE TABLE IF NOT EXISTS settings (
  key TEXT PRIMARY KEY,
  value_json TEXT NOT NULL
);

CREATE TABLE IF NOT EXISTS model_overrides (
  backend TEXT NOT NULL,
  model TEXT NOT NULL,
  settings_json TEXT NOT NULL,
  PRIMARY KEY (backend, model)
);

CREATE TABLE IF NOT EXISTS counters (
  key TEXT PRIMARY KEY,
  value INTEGER NOT NULL DEFAULT 0,
  period TEXT NOT NULL DEFAULT ''
);
`,
	// 4: the recipe behind a generated picture, so it can be drawn again.
	//
	// It is its own migration and it is appended rather than folded into migration 2,
	// because of how the runner works: it resumes at the version this database is
	// already at, so an installation created before this feature runs this entry and
	// nothing earlier. Adding the column to migration 2's CREATE TABLE would leave
	// every existing database without it, and every attachment read would then fail.
	`ALTER TABLE attachments ADD COLUMN generation_json TEXT NOT NULL DEFAULT ''`,
	// 5: where a picture was exported to.
	//
	// It exists so the pictures panel can tell a picture its owner already has a file of
	// from one that lives only in this database. An empty path is "no copy outside KiChat",
	// which is also what every row written before this migration is, and that is the
	// honest reading: nothing recorded a copy, so the app cannot claim there is one. It is
	// appended here for the same reason migration 4 is — the runner resumes at the version
	// a database is already at, so an existing installation runs this entry and nothing
	// earlier.
	`ALTER TABLE attachments ADD COLUMN saved_path TEXT NOT NULL DEFAULT ''`,
	// 6: how an enlarged picture was derived from another one.
	//
	// A separate column rather than a field in generation_json, because the two answer
	// different questions: generation_json is a prompt a redraw replays, and an upscale has
	// no prompt of its own. Its own migration for the same reason 4 and 5 are: an existing
	// database resumes here and only runs this entry.
	`ALTER TABLE attachments ADD COLUMN derivation_json TEXT NOT NULL DEFAULT ''`,
	// 7: condensed tool results, keyed by the content they were made from.
	//
	// The page itself is already cached in scrape_cache, so this is the other half of
	// the same economy: reading the same page twice should cost one condensation, not
	// two — and the key is the content hash rather than the URL, because a page that
	// changed is a different page and must not inherit the old summary. The model is
	// part of the key for the same reason: two models write different condensations,
	// and swapping the model is how a user asks for a different one.
	`
CREATE TABLE IF NOT EXISTS digest_cache (
  key TEXT PRIMARY KEY,
  model TEXT NOT NULL DEFAULT '',
  source_tokens INTEGER NOT NULL DEFAULT 0,
  summary TEXT NOT NULL DEFAULT '',
  digest_tokens INTEGER NOT NULL DEFAULT 0,
  created_at INTEGER NOT NULL,
  expires_at INTEGER NOT NULL
);
CREATE INDEX IF NOT EXISTS idx_digest_cache_expiry ON digest_cache(expires_at);
`,
	// 8: what a tool result cost, and what of it the model read.
	//
	// The two numbers are what makes a condensation visible in the transcript: the
	// tool card can say "12,345 → 640 tokens" only if the record kept both, since the
	// only other copy of the page is the result itself — and a client-side estimate of
	// it would be a second tokenizer disagreeing with the one the budget charged.
	// `condensed` is the cause, because the same two numbers also arise from the
	// answer's tool budget, and a reader cannot tell those apart from the sizes alone.
	// Appended here for the reason 4–7 are: the runner resumes at the version a
	// database is already at, so an existing installation runs this entry and nothing
	// earlier.
	`
ALTER TABLE tool_calls ADD COLUMN source_tokens INTEGER NOT NULL DEFAULT 0;
ALTER TABLE tool_calls ADD COLUMN context_tokens INTEGER NOT NULL DEFAULT 0;
ALTER TABLE tool_calls ADD COLUMN condensed INTEGER NOT NULL DEFAULT 0;
`,
	// 9: the result the budget refused entirely.
	//
	// Its own migration rather than a fourth column in 8: 8 has already run on the
	// installations that built between the two, so folding this in would leave them
	// without the column while the runner reports the schema up to date — the exact
	// silent failure §4 of the migration comment describes.
	`ALTER TABLE tool_calls ADD COLUMN withheld INTEGER NOT NULL DEFAULT 0`,
	// 10: the numbered sources an answer cites.
	//
	// A column on the message rather than a derivation: the numbers come from the
	// turn's searches, which are over by the time the answer is stored, and a client
	// that rebuilt them from the tool texts would be re-deriving the numbering from
	// text this app no longer keeps in that shape. An empty string is every row
	// written before citations existed, and renders no list.
	`ALTER TABLE messages ADD COLUMN sources_json TEXT NOT NULL DEFAULT ''`,
}

// ftsMigration creates the search index. It is attempted separately because a
// SQLite build without FTS5 must not prevent the app from starting.
const ftsMigration = `
CREATE VIRTUAL TABLE IF NOT EXISTS chunks_fts USING fts5(
  text,
  heading,
  chat_id UNINDEXED,
  doc_id UNINDEXED,
  tokenize='unicode61 remove_diacritics 2'
);
`

func (s *Store) migrate() error {
	var version int
	if err := s.db.QueryRow("PRAGMA user_version").Scan(&version); err != nil {
		return fmt.Errorf("read schema version: %w", err)
	}
	for i := version; i < len(migrations); i++ {
		tx, err := s.db.Begin()
		if err != nil {
			return err
		}
		if _, err := tx.Exec(migrations[i]); err != nil {
			tx.Rollback()
			return fmt.Errorf("migration %d: %w", i+1, err)
		}
		if err := tx.Commit(); err != nil {
			return err
		}
		if _, err := s.db.Exec(fmt.Sprintf("PRAGMA user_version = %d", i+1)); err != nil {
			return err
		}
	}

	// Probe FTS5 independently of the versioned migrations.
	if _, err := s.db.Exec(ftsMigration); err != nil {
		s.FTS = false
		return nil
	}
	s.FTS = true
	return nil
}

func millis(t time.Time) int64 { return t.UnixNano() / int64(time.Millisecond) }

func fromMillis(ms int64) time.Time { return time.Unix(0, ms*int64(time.Millisecond)).UTC() }

func boolToInt(b bool) int {
	if b {
		return 1
	}
	return 0
}
