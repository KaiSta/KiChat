package store

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/ncruces/go-sqlite3/driver"
)

// testKey is the key the opt-in encrypted run of the suite uses. Fixed rather than
// random so a failure can be reproduced.
var testKey = bytes.Repeat([]byte{0x5a}, KeySize)

// seed writes one chat, one message and one document chunk — enough rows to see that
// a migration carries the history across and that the search index still answers.
func seed(t *testing.T, st *Store) (chatID, text string) {
	t.Helper()
	now := millis(time.Now())
	chatID = "chat-1"
	text = "the migration marker about herons"
	if _, err := st.db.Exec(`INSERT INTO chats(id,title,created_at,updated_at) VALUES(?,?,?,?)`,
		chatID, "A chat", now, now); err != nil {
		t.Fatalf("insert chat: %v", err)
	}
	if _, err := st.db.Exec(`INSERT INTO messages(id,chat_id,seq,role,content,created_at) VALUES(?,?,?,?,?,?)`,
		"msg-1", chatID, 1, "user", text, now); err != nil {
		t.Fatalf("insert message: %v", err)
	}
	if _, err := st.db.Exec(`INSERT INTO documents(id,chat_id,title,text,created_at) VALUES(?,?,?,?,?)`,
		"doc-1", chatID, "Doc", text, now); err != nil {
		t.Fatalf("insert document: %v", err)
	}
	if err := st.InsertChunks([]Chunk{{DocumentID: "doc-1", ChatID: chatID, Ordinal: 0, Text: text}}); err != nil {
		t.Fatalf("insert chunk: %v", err)
	}
	if _, err := st.db.Exec(`INSERT INTO search_cache(query_hash,query,response_json,created_at,expires_at) VALUES(?,?,?,?,?)`,
		"h1", "herons", "{}", now, now+1_000_000); err != nil {
		t.Fatalf("insert cache: %v", err)
	}
	return chatID, text
}

func TestEncryptMigratesAndLeavesNothingReadable(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "kichat.db")
	src, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	chatID, text := seed(t, src)
	if err := src.Close(); err != nil {
		t.Fatal(err)
	}

	if enc, err := IsEncrypted(path); err != nil || enc {
		t.Fatalf("IsEncrypted before migration = %v, %v", enc, err)
	}
	if err := Encrypt(path, testKey); err != nil {
		t.Fatalf("Encrypt: %v", err)
	}
	if enc, err := IsEncrypted(path); err != nil || !enc {
		t.Fatalf("IsEncrypted after migration = %v, %v", enc, err)
	}

	// Nothing plaintext survives: the replaced copy and the crashed-copy name are
	// gone, and the write-ahead log the old store was appending to is gone with it.
	for _, name := range []string{"kichat.db.old", "kichat.db.new", "kichat.db-wal"} {
		if _, err := os.Stat(filepath.Join(dir, name)); err == nil {
			t.Errorf("%s still exists after the migration", name)
		}
	}
	// The ciphertext does not contain the conversation, including the write-ahead log
	// the old store had been appending to.
	for _, name := range []string{"kichat.db", "kichat.db-wal"} {
		if b, err := os.ReadFile(filepath.Join(dir, name)); err == nil && bytes.Contains(b, []byte(text)) {
			t.Errorf("%s contains the plaintext conversation", name)
		}
	}

	st, err := OpenKeyed(path, testKey)
	if err != nil {
		t.Fatalf("OpenKeyed: %v", err)
	}
	defer st.Close()
	var got string
	if err := st.db.QueryRow(`SELECT content FROM messages WHERE chat_id=?`, chatID).Scan(&got); err != nil {
		t.Fatalf("read message from the encrypted database: %v", err)
	}
	if got != text {
		t.Errorf("message = %q, want %q", got, text)
	}
	if !st.FTS {
		t.Fatal("FTS is false on a migrated database: the ext/fts5 registration is missing")
	}
	var hit string
	if err := st.db.QueryRow(`SELECT text FROM chunks_fts WHERE chunks_fts MATCH 'herons'`).Scan(&hit); err != nil {
		t.Fatalf("search the migrated index: %v", err)
	}
	// The caches are regenerable and deliberately left behind.
	var n int
	if err := st.db.QueryRow(`SELECT COUNT(*) FROM search_cache`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Errorf("search_cache has %d rows after migration, want 0", n)
	}
}

// TestKeyedSidecarsAreCiphertext is the claim the whole feature rests on, asserted
// against the files themselves rather than through the API: the write-ahead log is
// where the newest change lives, and a database whose main file is encrypted but
// whose -wal is not has not protected the conversation at all.
func TestKeyedSidecarsAreCiphertext(t *testing.T) {
	path := filepath.Join(t.TempDir(), "kichat.db")
	st, err := OpenKeyed(path, testKey)
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	const marker = "a marker no ciphertext should contain"
	now := millis(time.Now())
	if _, err := st.db.Exec(`INSERT INTO chats(id,title,created_at,updated_at) VALUES('c','t',?,?)`, now, now); err != nil {
		t.Fatal(err)
	}
	if _, err := st.db.Exec(`INSERT INTO messages(id,chat_id,seq,role,content,created_at) VALUES('m','c',1,'user',?,?)`, marker, now); err != nil {
		t.Fatal(err)
	}
	for _, suffix := range []string{"", "-wal", "-shm"} {
		b, err := os.ReadFile(path + suffix)
		if err != nil {
			if errors.Is(err, os.ErrNotExist) {
				continue
			}
			t.Fatalf("read %s: %v", path+suffix, err)
		}
		if bytes.Contains(b, []byte(marker)) {
			t.Errorf("%s contains the plaintext message", suffix)
		}
	}
	// The database file does not even look like a database.
	head, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.HasPrefix(head, []byte(sqliteMagic)) {
		t.Error("the encrypted file starts with the SQLite magic")
	}
}

// TestEncryptRemovesTheOldSidecars pins a mistake that only shows up on the second
// launch: the -wal and -shm are named after the database, so renaming the database does
// not rename them. Left behind, they would sit beside the encrypted file under its own
// name, holding plaintext pages of the old one, and SQLite would try to recover them
// into the new database.
func TestEncryptRemovesTheOldSidecars(t *testing.T) {
	path := filepath.Join(t.TempDir(), "kichat.db")
	st, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	seed(t, st)
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}
	// A clean close removes both; recreating them models an installation that did not
	// get one, which is exactly the case the migration has to clean up.
	for _, suffix := range []string{"-wal", "-shm"} {
		if err := os.WriteFile(path+suffix, []byte("pages from the plaintext file"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := Encrypt(path, testKey); err != nil {
		t.Fatalf("Encrypt: %v", err)
	}
	for _, suffix := range []string{"-wal", "-shm"} {
		if _, err := os.Stat(path + suffix); err == nil {
			t.Errorf("%s survived the migration beside the encrypted database", suffix)
		}
	}
	enc, err := OpenKeyed(path, testKey)
	if err != nil {
		t.Fatalf("OpenKeyed after the migration: %v", err)
	}
	defer enc.Close()
	var n int
	if err := enc.db.QueryRow(`SELECT COUNT(*) FROM messages`).Scan(&n); err != nil || n != 1 {
		t.Fatalf("messages = %d, %v; want the seeded message", n, err)
	}
}

// TestKeyedVFSWithoutAKeyFailsClosed is the safety property behind the key table: a
// connection through the encrypting VFS that has no key must fail, not fall back to
// writing an unencrypted file that the caller believes is encrypted.
func TestKeyedVFSWithoutAKeyFailsClosed(t *testing.T) {
	path := filepath.Join(t.TempDir(), "x.db")
	db, err := driver.Open("file:" + filepath.ToSlash(path) + "?vfs=" + cryptVFSName)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec("CREATE TABLE t(x)"); err == nil {
		t.Fatal("a database was created through the encrypting VFS with no key installed")
	}
	if b, err := os.ReadFile(path); err == nil && bytes.HasPrefix(b, []byte(sqliteMagic)) {
		t.Error("a plaintext database was written where ciphertext was promised")
	}
}

// TestEncryptKeepsTheAutoincrementSequence pins a property the row copy alone loses:
// chunks.id is AUTOINCREMENT so a deleted id is never reused, and the counter lives in
// sqlite_sequence rather than in any row.
func TestEncryptKeepsTheAutoincrementSequence(t *testing.T) {
	path := filepath.Join(t.TempDir(), "kichat.db")
	st, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	now := millis(time.Now())
	if _, err := st.db.Exec(`INSERT INTO documents(id,chat_id,title,text,created_at) VALUES('d','c','D','text',?)`, now); err != nil {
		t.Fatal(err)
	}
	// One chunk, then deleted: the counter is 1 while the table is empty.
	if err := st.InsertChunks([]Chunk{{DocumentID: "d", ChatID: "c", Ordinal: 0, Text: "gone"}}); err != nil {
		t.Fatal(err)
	}
	if _, err := st.db.Exec(`DELETE FROM chunks`); err != nil {
		t.Fatal(err)
	}
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}

	if err := Encrypt(path, testKey); err != nil {
		t.Fatalf("Encrypt: %v", err)
	}
	enc, err := OpenKeyed(path, testKey)
	if err != nil {
		t.Fatalf("OpenKeyed: %v", err)
	}
	defer enc.Close()
	if err := enc.InsertChunks([]Chunk{{DocumentID: "d", ChatID: "c", Ordinal: 1, Text: "new"}}); err != nil {
		t.Fatalf("insert a chunk after the migration: %v", err)
	}
	var id int64
	if err := enc.db.QueryRow(`SELECT id FROM chunks`).Scan(&id); err != nil {
		t.Fatal(err)
	}
	if id != 2 {
		t.Errorf("the chunk written after the migration got id %d, want 2: the AUTOINCREMENT sequence restarted", id)
	}
}

func TestOpenKeyedRejectsTheWrongKey(t *testing.T) {
	path := filepath.Join(t.TempDir(), "kichat.db")
	st, err := OpenKeyed(path, testKey)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.db.Exec(`CREATE TABLE probe(x)`); err != nil {
		t.Fatal(err)
	}
	st.Close()

	wrong := bytes.Repeat([]byte{0xa5}, KeySize)
	if _, err := OpenKeyed(path, wrong); !errors.Is(err, ErrWrongKey) {
		t.Fatalf("OpenKeyed with another key = %v, want ErrWrongKey", err)
	}
	// The file is untouched by the failed attempt.
	st, err = OpenKeyed(path, testKey)
	if err != nil {
		t.Fatalf("OpenKeyed with the right key after a failed attempt: %v", err)
	}
	st.Close()
}

func TestEncryptIsIdempotentAndRefusesMissingFiles(t *testing.T) {
	path := filepath.Join(t.TempDir(), "kichat.db")
	st, err := OpenKeyed(path, testKey)
	if err != nil {
		t.Fatal(err)
	}
	st.Close()
	before, _ := os.Stat(path)
	if err := Encrypt(path, testKey); err != nil {
		t.Fatalf("Encrypt on an encrypted database: %v", err)
	}
	after, _ := os.Stat(path)
	if before.Size() != after.Size() {
		t.Errorf("an already encrypted database was rewritten: %d -> %d bytes", before.Size(), after.Size())
	}

	if err := Encrypt(filepath.Join(t.TempDir(), "absent.db"), testKey); err == nil {
		t.Fatal("Encrypt of a missing database returned nil")
	}
}

func TestEncryptLeavesAnUnreadableSourceAlone(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "kichat.db")
	original := []byte("this is not a database, and pretending otherwise would lose it")
	if err := os.WriteFile(path, original, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := Encrypt(path, testKey); err == nil {
		t.Fatal("Encrypt of a foreign file returned nil")
	}
	got, err := os.ReadFile(path)
	if err != nil || !bytes.Equal(got, original) {
		t.Fatalf("the source file was modified or removed: %v", err)
	}
	if _, err := os.Stat(path + ".new"); err == nil {
		t.Error("a failed migration left kichat.db.new behind")
	}
}

// TestEncryptRefusesWhenTheOldCopyCannotBeRemoved covers the leftover RecoverMigration
// could not delete: renaming the plaintext database over it would discard a file that
// may hold a different copy of the history.
func TestEncryptRefusesWhenTheOldCopyCannotBeRemoved(t *testing.T) {
	path := filepath.Join(t.TempDir(), "kichat.db")
	st, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	seed(t, st)
	if err := st.Close(); err != nil {
		t.Fatal(err)
	}
	// A non-empty directory is what an undeletable leftover looks like to os.Remove.
	if err := os.MkdirAll(filepath.Join(path+".old", "x"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := Encrypt(path, testKey); err == nil {
		t.Fatal("the migration renamed the database over a copy it could not remove")
	}
	if enc, err := IsEncrypted(path); err != nil || enc {
		t.Fatalf("the source was encrypted anyway: %v, %v", enc, err)
	}
	st, err = Open(path)
	if err != nil {
		t.Fatalf("the source is no longer readable: %v", err)
	}
	defer st.Close()
	var n int
	if err := st.db.QueryRow(`SELECT COUNT(*) FROM messages`).Scan(&n); err != nil || n != 1 {
		t.Fatalf("messages = %d, %v; want the seeded message", n, err)
	}
	if _, err := os.Stat(path + ".new"); err == nil {
		t.Error("the failed migration left an encrypted copy behind")
	}
}

func TestRecoverMigration(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "kichat.db")

	// A database that was renamed aside and never replaced comes back.
	if err := os.WriteFile(path+".old", []byte("plaintext"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := RecoverMigration(path); err != nil {
		t.Fatalf("RecoverMigration: %v", err)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("the database was not restored: %v", err)
	}
	if _, err := os.Stat(path + ".old"); err == nil {
		t.Error("the .old copy survived recovery")
	}

	// An encrypted copy that was still being written is discarded.
	if err := os.WriteFile(path+".new", []byte("half a database"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := RecoverMigration(path); err != nil {
		t.Fatalf("RecoverMigration: %v", err)
	}
	if _, err := os.Stat(path + ".new"); err == nil {
		t.Error("an unfinished encrypted copy survived recovery")
	}
}
