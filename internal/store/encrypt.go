package store

import (
	"bytes"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

// sqliteMagic is the first page's signature. An encrypted database does not have
// it: the encrypting VFS writes ciphertext where SQLite would write these bytes, so
// the presence of the magic is an unambiguous "this file is plaintext".
const sqliteMagic = "SQLite format 3\x00"

// regenerable holds the tables a migration does not copy. They are second copies of
// pages the chats already quote, they are rebuilt on demand, and they are the bulk of
// a database's size — copying them would make the migration slower for nothing.
var regenerable = map[string]bool{
	"search_cache": true,
	"scrape_cache": true,
	"digest_cache": true,
}

// IsEncrypted reports whether the database at path is encrypted.
//
// A file that does not exist, and an empty one, answer false: both are databases
// SQLite is about to create, and neither holds a history to protect yet.
func IsEncrypted(path string) (bool, error) {
	f, err := os.Open(path)
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	defer f.Close()
	// SQLite's header is 100 bytes and the first page is a full page (4096 here), so
	// anything shorter is neither a database SQLite wrote nor one this app encrypted.
	// Treating it as encrypted would hand the caller an "already encrypted" answer for
	// a truncated or foreign file and skip the copy that would have reported it.
	var head [100]byte
	n, err := io.ReadFull(f, head[:])
	if err != nil && !errors.Is(err, io.EOF) && !errors.Is(err, io.ErrUnexpectedEOF) {
		return false, err
	}
	if n < len(head) {
		return false, nil
	}
	return !bytes.Equal(head[:len(sqliteMagic)], []byte(sqliteMagic)), nil
}

// Encrypt converts the plaintext database at path into one encrypted under key.
//
// The conversion is a copy, never an in-place rewrite: a keyed connection writes a
// fresh file beside the old one, every table except the regenerable caches is copied
// into it, chunks_fts is rebuilt from chunks, and only after integrity_check and a
// per-table row count agree does the new file replace the old. A failure before that
// point leaves the plaintext database where it was.
//
// The replaced file is overwritten with zeros before it is removed. That is best
// effort by nature — on a copy-on-write or journaled filesystem the old blocks may
// survive elsewhere — which is why full-disk encryption is still the foundation.
func Encrypt(path string, key []byte) error {
	if err := RecoverMigration(path); err != nil {
		return err
	}
	if enc, err := IsEncrypted(path); err != nil {
		return err
	} else if enc {
		return nil
	}
	if _, err := os.Stat(path); errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("encrypt %s: no such database", path)
	}

	// The source opens with immediate transactions, so the read below runs under
	// SQLite's write lock: a second instance cannot append to the database while it is
	// being copied, and what it would have written is not silently lost.
	src, err := openWith(path, nil, true)
	if err != nil {
		return fmt.Errorf("read plaintext database: %w", err)
	}
	newPath := path + ".new"
	dst, err := OpenKeyed(newPath, key)
	if err != nil {
		src.Close()
		return fmt.Errorf("create encrypted database: %w", err)
	}

	source, err := src.db.Begin()
	if err != nil {
		dst.Close()
		src.Close()
		return fmt.Errorf("encrypt %s: another instance has the database open: %w", path, err)
	}
	defer source.Rollback() // the read lock is released with it
	err = copyDatabase(dst, source)
	if err == nil {
		err = verifyCopy(dst, source)
	}
	closeSrcTx := source.Rollback()
	closeDst := dst.Close()
	closeSrc := src.Close()
	if err != nil {
		wipeDatabase(newPath)
		return err
	}
	if closeDst != nil || closeSrc != nil || closeSrcTx != nil {
		wipeDatabase(newPath)
		return fmt.Errorf("close database during migration: %v", errors.Join(closeDst, closeSrc, closeSrcTx))
	}
	// The -wal and -shm are named after the database, not after the .old copy, so they
	// would otherwise survive the rename and sit beside the encrypted file under its
	// name, holding plaintext pages from the old one.
	wipeSidecars(path)

	// Both files are closed and the plaintext log has been checkpointed into the
	// file by the clean close, so the copy is the whole history. Renaming the old
	// file aside first means the encrypted one never has to overwrite a live file,
	// which also keeps this working on Windows.
	old := path + ".old"
	if _, err := os.Stat(old); err == nil {
		// RecoverMigration removes this; a failure there is silent because the file is
		// not needed. Here it is not silent: renaming over a plaintext copy that could
		// not be wiped would lose whatever that copy holds.
		wipeDatabase(newPath)
		return fmt.Errorf("encrypt %s: %s could not be removed", path, old)
	}
	if err := os.Rename(path, old); err != nil {
		wipeDatabase(newPath)
		return fmt.Errorf("move plaintext database aside: %w", err)
	}
	if err := os.Rename(newPath, path); err != nil {
		// Put the plaintext database back; nothing has been destroyed.
		os.Rename(old, path)
		wipeDatabase(newPath)
		return fmt.Errorf("install encrypted database: %w", err)
	}
	wipeDatabase(old)
	syncDir(filepath.Dir(path))
	return nil
}

// RecoverMigration finishes or rolls back a migration that was interrupted.
//
// The rename sequence has one window where the database is called "kichat.db.old"
// and another where an unused "kichat.db.new" exists. Nothing has been destroyed in
// either, and this is what makes the next start the one that completes the work:
// a database that is missing comes back from the .old copy, a completed migration
// has its leftover plaintext wiped, and an unfinished encrypted copy is discarded.
func RecoverMigration(path string) error {
	old, fresh := path+".old", path+".new"
	if _, err := os.Stat(path); errors.Is(err, os.ErrNotExist) {
		if _, err := os.Stat(old); err == nil {
			if err := os.Rename(old, path); err != nil {
				return fmt.Errorf("restore database after interrupted migration: %w", err)
			}
		}
	}
	if _, err := os.Stat(path); err == nil {
		// The encrypted database is in place, so any plaintext copy is a leftover
		// from a migration that had already succeeded.
		if _, err := os.Stat(old); err == nil {
			wipeDatabase(old)
		}
	}
	wipeDatabase(fresh)
	return nil
}

// copyDatabase copies every table that is not regenerable, in one transaction.
//
// Foreign keys are enforced at commit rather than per row, because the order the
// tables come back from sqlite_master is not a dependency order: a message may be
// copied before the chat it belongs to.
func copyDatabase(dst *Store, src queryer) error {
	tables, err := listTables(src)
	if err != nil {
		return err
	}
	tx, err := dst.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.Exec("PRAGMA defer_foreign_keys = ON"); err != nil {
		return fmt.Errorf("defer foreign keys: %w", err)
	}
	for _, table := range tables {
		if _, err := copyTable(tx, src, table); err != nil {
			return fmt.Errorf("copy %s: %w", table, err)
		}
	}
	if err := copySequence(tx, src); err != nil {
		return fmt.Errorf("copy the autoincrement sequence: %w", err)
	}
	return tx.Commit()
}

// copySequence carries over how far the chunks counter had got.
//
// `chunks.id` is AUTOINCREMENT, which exists precisely so a deleted row's id is never
// handed out again, and the counter lives in sqlite_sequence rather than in any row. The
// copied rows alone would restart it at the highest surviving id. SQLite has no supported
// way to write that table before it exists, so the counter is advanced the only way it
// can be: one insert with the target id, removed again. The copy's transaction defers
// foreign keys to commit, so the throwaway row can name a document that is not there.
//
// It is chunks specifically because it is the one AUTOINCREMENT table in the schema; a
// second one would need its columns here too.
func copySequence(dst *sql.Tx, src queryer) error {
	var seq, highest int64
	err := src.QueryRow(`SELECT seq FROM sqlite_sequence WHERE name='chunks'`).Scan(&seq)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) || strings.Contains(err.Error(), "no such table") {
			return nil
		}
		return err
	}
	if err := dst.QueryRow(`SELECT COALESCE(MAX(id),0) FROM chunks`).Scan(&highest); err != nil {
		return err
	}
	if seq <= highest {
		return nil
	}
	if _, err := dst.Exec(`INSERT INTO chunks(id,document_id,chat_id,ordinal,heading,page,text,tokens)
VALUES(?,?,?,0,'',0,'',0)`, seq, "", ""); err != nil {
		return err
	}
	_, err = dst.Exec(`DELETE FROM chunks WHERE id=?`, seq)
	return err
}

// listTables names the tables a migration carries over, in sqlite_master order.
func listTables(src queryer) ([]string, error) {
	rows, err := src.Query(`
SELECT name FROM sqlite_master
WHERE type='table' AND name NOT LIKE 'sqlite_%'
ORDER BY rowid`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return nil, err
		}
		// chunks_fts and its shadow tables are an index over chunks, not data. The
		// rowids in it belong to the database that wrote them.
		if regenerable[name] || strings.HasPrefix(name, "chunks_fts") {
			continue
		}
		out = append(out, name)
	}
	return out, rows.Err()
}

// queryer is what a read during the migration runs on: the source's transaction, so
// the copy and the verification see the same database, under the same lock.
type queryer interface {
	Query(string, ...any) (*sql.Rows, error)
	QueryRow(string, ...any) *sql.Row
}

// copyTable moves every row and returns how many it wrote.
func copyTable(dst *sql.Tx, src queryer, table string) (int64, error) {
	rows, err := src.Query(`SELECT * FROM "` + table + `"`)
	if err != nil {
		return 0, err
	}
	defer rows.Close()
	cols, err := rows.Columns()
	if err != nil {
		return 0, err
	}
	quoted := make([]string, len(cols))
	marks := make([]string, len(cols))
	for i, c := range cols {
		quoted[i] = `"` + c + `"`
		marks[i] = "?"
	}
	insert := `INSERT INTO "` + table + `" (` + strings.Join(quoted, ",") + `) VALUES (` + strings.Join(marks, ",") + `)`
	stmt, err := dst.Prepare(insert)
	if err != nil {
		return 0, err
	}
	defer stmt.Close()

	var n int64
	values := make([]any, len(cols))
	pointers := make([]any, len(cols))
	for i := range values {
		pointers[i] = &values[i]
	}
	for rows.Next() {
		if err := rows.Scan(pointers...); err != nil {
			return n, err
		}
		if _, err := stmt.Exec(values...); err != nil {
			return n, err
		}
		n++
	}
	return n, rows.Err()
}

// verifyCopy rebuilds the search index and checks that the new file is a database
// holding exactly what the old one held.
func verifyCopy(dst *Store, src queryer) error {
	if dst.FTS {
		if _, err := dst.db.Exec(`
INSERT INTO chunks_fts(rowid,text,heading,chat_id,doc_id)
SELECT id,text,heading,chat_id,document_id FROM chunks`); err != nil {
			return fmt.Errorf("rebuild search index: %w", err)
		}
	}
	var integrity string
	if err := dst.db.QueryRow("PRAGMA integrity_check").Scan(&integrity); err != nil {
		return fmt.Errorf("integrity check: %w", err)
	}
	if integrity != "ok" {
		return fmt.Errorf("integrity check: %s", integrity)
	}
	tables, err := listTables(src)
	if err != nil {
		return err
	}
	for _, table := range tables {
		var want, got int64
		if err := src.QueryRow(`SELECT COUNT(*) FROM "` + table + `"`).Scan(&want); err != nil {
			return fmt.Errorf("count %s: %w", table, err)
		}
		if err := dst.db.QueryRow(`SELECT COUNT(*) FROM "` + table + `"`).Scan(&got); err != nil {
			return fmt.Errorf("count %s in the encrypted copy: %w", table, err)
		}
		if want != got {
			return fmt.Errorf("copy %s: %d rows written, %d expected", table, got, want)
		}
	}
	return nil
}

// wipeDatabase overwrites a database and its sidecars with zeros and removes them.
//
// Overwriting is a courtesy to the next owner of the disk blocks, not a guarantee: a
// copy-on-write or journaled filesystem may keep the old contents somewhere else.
// Removing the file is what has to succeed, and it does even when a write fails.
func wipeDatabase(path string) {
	wipeSidecars(path)
	zeroFile(path)
	os.Remove(path)
}

// wipeSidecars removes the write-ahead log and its shared-memory index, which are named
// after the database rather than after a renamed copy of it.
func wipeSidecars(path string) {
	for _, p := range []string{path + "-wal", path + "-shm"} {
		if _, err := os.Stat(p); err == nil {
			zeroFile(p)
			os.Remove(p)
		}
	}
}

func zeroFile(path string) {
	f, err := os.OpenFile(path, os.O_WRONLY, 0)
	if err != nil {
		return
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return
	}
	buf := make([]byte, 1<<20)
	for left := info.Size(); left > 0; {
		n := int64(len(buf))
		if n > left {
			n = left
		}
		if _, err := f.Write(buf[:n]); err != nil {
			return
		}
		left -= n
	}
	f.Sync()
}

// syncDir flushes a rename to the directory itself, which is what makes it survive a
// power cut. Unix only: Windows has no directory handle to sync, and opening one
// there fails instead.
func syncDir(dir string) {
	if dir == "" || dir == "." {
		return
	}
	d, err := os.Open(dir)
	if err != nil {
		return
	}
	d.Sync()
	d.Close()
}
