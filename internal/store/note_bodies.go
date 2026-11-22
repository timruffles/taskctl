package store

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"io"
	"net/url"
	"os"
	"path/filepath"
)

const noteChunkBytes = 64 * 1024

const bodySchema = `
CREATE TABLE IF NOT EXISTS body_info(key TEXT PRIMARY KEY,value TEXT NOT NULL);
CREATE TABLE IF NOT EXISTS bodies(
 id TEXT PRIMARY KEY,
 body_bytes INTEGER NOT NULL CHECK(body_bytes>=0),
 sha256 TEXT NOT NULL CHECK(length(sha256)=64)
);
CREATE TABLE IF NOT EXISTS body_chunks(
 note_id TEXT NOT NULL REFERENCES bodies(id) ON DELETE CASCADE,
 sequence INTEGER NOT NULL CHECK(sequence>=0),
 data BLOB NOT NULL CHECK(typeof(data)='blob' AND length(data)>0 AND length(data)<=65536),
 PRIMARY KEY(note_id,sequence)
) WITHOUT ROWID;
`

func (s *Store) resolveDatabasePath() error {
	if s.databasePath != "" {
		return nil
	}
	var sequence int
	var name, path string
	if err := s.DB.QueryRow("PRAGMA database_list").Scan(&sequence, &name, &path); err != nil {
		return classify(err)
	}
	if path == "" {
		return failure("storage_invalid", "notes require a file-backed task database")
	}
	s.databasePath = path
	return nil
}

func (s *Store) NotesPath() string {
	if s.databasePath == "" {
		_ = s.resolveDatabasePath()
	}
	if filepath.Base(s.databasePath) == "tasks.sqlite" {
		return filepath.Join(filepath.Dir(s.databasePath), "notes.sqlite")
	}
	return s.databasePath + ".notes.sqlite"
}

func (s *Store) openBodies(create bool) (*sql.DB, error) {
	path := s.NotesPath()
	info, err := os.Lstat(path)
	missing := errors.Is(err, os.ErrNotExist)
	if err != nil && !missing {
		return nil, classify(err)
	}
	if missing && !create {
		return nil, failure("storage_missing", "note body database is missing")
	}
	if !missing && !info.Mode().IsRegular() {
		return nil, failure("storage_invalid", "note body database must be a regular file")
	}
	if missing {
		if err := s.initializeBodies(path); err != nil {
			return nil, err
		}
	}
	db, err := openBodySQLite(path, false)
	if err != nil {
		return nil, err
	}
	fail := func(err error) (*sql.DB, error) { db.Close(); return nil, classify(err) }
	var version, project string
	if err = db.QueryRow("SELECT (SELECT value FROM body_info WHERE key='schema_version'),(SELECT value FROM body_info WHERE key='project_id')").Scan(&version, &project); err != nil {
		return fail(failure("storage_invalid", "invalid note body database: "+err.Error()))
	}
	if version != "1" || project != s.ProjectID {
		return fail(failure("storage_invalid", "note body database identity or version mismatch"))
	}
	return db, nil
}

func openBodySQLite(path string, create bool) (*sql.DB, error) {
	u := url.URL{Scheme: "file", Path: filepath.ToSlash(path)}
	q := u.Query()
	q.Set("_txlock", "immediate")
	q.Add("_pragma", "busy_timeout(5000)")
	q.Add("_pragma", "foreign_keys(1)")
	q.Add("_pragma", "synchronous(FULL)")
	q.Add("_pragma", "temp_store(FILE)")
	q.Add("_pragma", "trusted_schema(0)")
	if !create {
		q.Set("mode", "rw")
	}
	u.RawQuery = q.Encode()
	db, err := sql.Open("sqlite", u.String())
	if err != nil {
		return nil, classify(err)
	}
	db.SetMaxOpenConns(1)
	return db, nil
}

func (s *Store) initializeBodies(path string) error {
	f, err := os.CreateTemp(filepath.Dir(path), ".taskctl-notes-*")
	if err != nil {
		return classify(err)
	}
	tmp := f.Name()
	defer func() {
		for _, suffix := range []string{"", "-wal", "-shm", "-journal"} {
			_ = os.Remove(tmp + suffix)
		}
	}()
	if err := f.Close(); err != nil {
		return classify(err)
	}
	db, err := openBodySQLite(tmp, true)
	if err != nil {
		return err
	}
	defer db.Close()
	if _, err := db.Exec("PRAGMA journal_mode=WAL"); err != nil {
		return classify(err)
	}
	tx, err := db.BeginTx(context.Background(), nil)
	if err != nil {
		return classify(err)
	}
	defer tx.Rollback()
	if _, err = tx.Exec(bodySchema); err != nil {
		return classify(err)
	}
	if _, err = tx.Exec("INSERT INTO body_info(key,value) VALUES('schema_version','1'),('project_id',?)", s.ProjectID); err != nil {
		return classify(err)
	}
	if err = tx.Commit(); err != nil {
		return classify(err)
	}
	if err = db.Close(); err != nil {
		return classify(err)
	}
	f, err = os.OpenFile(tmp, os.O_RDWR, 0)
	if err != nil {
		return classify(err)
	}
	err = f.Sync()
	closeErr := f.Close()
	if err != nil {
		return classify(err)
	}
	if closeErr != nil {
		return classify(closeErr)
	}
	if err := os.Link(tmp, path); err != nil {
		return classify(err)
	}
	return classify(syncDirectory(filepath.Dir(path)))
}

func (s *Store) putBody(id string, body io.Reader) (int64, string, error) {
	_, err := os.Lstat(s.NotesPath())
	allowCreate := false
	if errors.Is(err, os.ErrNotExist) {
		live, checkErr := hasNoteBodies(s.DB)
		if checkErr != nil {
			return 0, "", checkErr
		}
		allowCreate = !live
	} else if err != nil {
		return 0, "", err
	}
	writer := s.newBodyWriter(allowCreate)
	defer writer.Close()
	size, digest, err := writer.Put(id, body)
	if err != nil {
		return 0, "", err
	}
	if err := writer.Commit(); err != nil {
		return 0, "", err
	}
	return size, digest, nil
}

type bodyWriter struct {
	store       *Store
	allowCreate bool
	db          *sql.DB
	tx          *sql.Tx
	insert      *sql.Stmt
	buffer      []byte
	err         error
}

// Commit bodies before publishing their primary-database references.
func (s *Store) newBodyWriter(allowCreate bool) *bodyWriter {
	return &bodyWriter{store: s, allowCreate: allowCreate}
}

func (w *bodyWriter) start() error {
	if w.tx != nil {
		return nil
	}
	var err error
	w.db, err = w.store.openBodies(w.allowCreate)
	if err != nil {
		return err
	}
	w.tx, err = w.db.BeginTx(context.Background(), nil)
	if err != nil {
		return classify(err)
	}
	w.insert, err = w.tx.Prepare("INSERT INTO body_chunks(note_id,sequence,data) VALUES(?,?,?)")
	if err != nil {
		return classify(err)
	}
	w.buffer = make([]byte, noteChunkBytes)
	return nil
}

func (w *bodyWriter) Put(id string, body io.Reader) (int64, string, error) {
	return w.put(id, body, false)
}

func (w *bodyWriter) ReplaceUnreferenced(id string, body io.Reader) (int64, string, error) {
	return w.put(id, body, true)
}

func (w *bodyWriter) put(id string, body io.Reader, replace bool) (size int64, digest string, err error) {
	defer func() {
		if err != nil {
			w.err = err
		}
	}()
	if w.err != nil {
		return 0, "", w.err
	}
	if err = w.start(); err != nil {
		return 0, "", err
	}
	if replace {
		if _, err = w.tx.Exec("DELETE FROM bodies WHERE id=?", id); err != nil {
			return 0, "", classify(err)
		}
	}
	if _, err = w.tx.Exec("INSERT INTO bodies(id,body_bytes,sha256) VALUES(?,0,?)", id, hex.EncodeToString(sha256.New().Sum(nil))); err != nil {
		return 0, "", classify(err)
	}
	hash := sha256.New()
	var sequence int64
	for {
		n, readErr := io.ReadFull(body, w.buffer)
		if n > 0 {
			if _, err = w.insert.Exec(id, sequence, w.buffer[:n]); err != nil {
				return 0, "", classify(err)
			}
			hash.Write(w.buffer[:n])
			size += int64(n)
			sequence++
		}
		if readErr == io.EOF || readErr == io.ErrUnexpectedEOF {
			break
		}
		if readErr != nil {
			return 0, "", readErr
		}
	}
	digest = hex.EncodeToString(hash.Sum(nil))
	if _, err = w.tx.Exec("UPDATE bodies SET body_bytes=?,sha256=? WHERE id=?", size, digest, id); err != nil {
		return 0, "", classify(err)
	}
	return size, digest, nil
}

func (w *bodyWriter) Commit() error {
	if w.err != nil {
		return w.err
	}
	if w.tx == nil {
		return nil
	}
	return classify(w.tx.Commit())
}

func (w *bodyWriter) Close() {
	if w.insert != nil {
		w.insert.Close()
	}
	if w.tx != nil {
		w.tx.Rollback()
	}
	if w.db != nil {
		w.db.Close()
	}
}

func (s *Store) removeBody(id string) error {
	if _, err := os.Lstat(s.NotesPath()); errors.Is(err, os.ErrNotExist) {
		return nil
	}
	db, err := s.openBodies(false)
	if err != nil {
		return err
	}
	defer db.Close()
	_, err = db.Exec("DELETE FROM bodies WHERE id=?", id)
	return classify(err)
}

func (s *Store) WriteNoteBody(id string, out io.Writer) error {
	unlock, err := s.noteLock(true)
	if err != nil {
		return err
	}
	defer unlock()
	note, err := s.GetNote(id)
	if err != nil {
		return err
	}
	if note.DeletedAt != "" {
		return failure("note_deleted", "note "+id+" was deleted")
	}
	if note.SHA256 == "" {
		return nil
	}
	db, err := s.openBodies(false)
	if err != nil {
		return err
	}
	defer db.Close()
	var size int64
	var digest string
	if err = db.QueryRow("SELECT body_bytes,sha256 FROM bodies WHERE id=?", id).Scan(&size, &digest); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return failure("storage_missing", "note body is missing: "+id)
		}
		return classify(err)
	}
	if size != note.BodyBytes || digest != note.SHA256 {
		return failure("storage_invalid", "note body metadata does not match: "+id)
	}
	rows, err := db.Query(`SELECT CASE WHEN typeof(sequence)='integer' THEN sequence ELSE -1 END,typeof(sequence),typeof(data),
 CASE WHEN typeof(data)='blob' THEN length(data) ELSE -1 END,
 CASE WHEN typeof(data)='blob' AND length(data) BETWEEN 1 AND 65536 THEN data ELSE NULL END
 FROM body_chunks WHERE note_id=? ORDER BY sequence`, id)
	if err != nil {
		return classify(err)
	}
	defer rows.Close()
	var sequence, total int64
	hash := sha256.New()
	for rows.Next() {
		var index, bytes int64
		var sequenceType, dataType string
		var chunk []byte
		if err = rows.Scan(&index, &sequenceType, &dataType, &bytes, &chunk); err != nil {
			return failure("storage_invalid", "invalid note body chunks: "+id)
		}
		if sequenceType != "integer" || dataType != "blob" || index != sequence || bytes < 1 || bytes > noteChunkBytes || int64(len(chunk)) != bytes || bytes > size-total {
			return failure("storage_invalid", "invalid note body chunks: "+id)
		}
		n, err := out.Write(chunk)
		if err != nil {
			return err
		}
		if n != len(chunk) {
			return io.ErrShortWrite
		}
		hash.Write(chunk)
		total += int64(len(chunk))
		sequence++
	}
	if err = rows.Err(); err != nil {
		return classify(err)
	}
	if total != size || hex.EncodeToString(hash.Sum(nil)) != digest {
		return failure("storage_invalid", "note body length or checksum mismatch: "+id)
	}
	return nil
}
