package store

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"unicode/utf8"
)

const directoryBackupMarker = "taskctl directory backup 1\n"

func (s *Store) checkBackupDestination(path string) error {
	abs, err := filepath.Abs(path)
	if err != nil {
		return err
	}
	parent, err := os.Stat(filepath.Dir(abs))
	if err != nil {
		return classify(err)
	}
	for _, database := range []string{s.databasePath, s.NotesPath()} {
		managedParent, err := os.Stat(filepath.Dir(database))
		if err != nil {
			return classify(err)
		}
		if !os.SameFile(parent, managedParent) {
			continue
		}
		for _, suffix := range []string{"", "-wal", "-shm", "-journal"} {
			if filepath.Base(abs) == filepath.Base(database)+suffix {
				return failure("invalid_backup_destination", "backup destination is a managed project database path")
			}
		}
	}
	return nil
}

func hasNoteBodies(q Queryer) (bool, error) {
	var present bool
	if err := q.QueryRow("SELECT EXISTS(SELECT 1 FROM sqlite_schema WHERE name='notes')").Scan(&present); err != nil {
		return false, classify(err)
	}
	if !present {
		return false, nil
	}
	var kind string
	if err := q.QueryRow("SELECT type FROM pragma_table_list WHERE schema='main' AND name='notes'").Scan(&kind); err != nil {
		return false, invalidArchive(err)
	}
	if kind != "table" {
		return false, failure("invalid_backup", "expected ordinary notes table")
	}
	if err := q.QueryRow("SELECT EXISTS(SELECT 1 FROM notes WHERE deleted_at='' AND sha256<>'')").Scan(&present); err != nil {
		return false, classify(err)
	}
	return present, nil
}

func requireNoNoteBodies(q Queryer) error {
	present, err := hasNoteBodies(q)
	if err != nil {
		return err
	}
	if present {
		return failure("note_bodies_present", "note bodies need a complete backup; use --directory instead of --file")
	}
	return nil
}

func validArchiveDigest(value string) bool {
	decoded, err := hex.DecodeString(value)
	return err == nil && len(decoded) == 32 && hex.EncodeToString(decoded) == value
}

func validateBackupNotes(q Queryer) error {
	rows, err := q.Query(`SELECT n.id,n.title,n.author,n.body_bytes,n.sha256,n.deleted_at,
 e.kind,e.task_id=n.task_id FROM notes n LEFT JOIN events e ON e.id=n.event_id`)
	if err != nil {
		return invalidArchive(err)
	}
	defer rows.Close()
	for rows.Next() {
		var id, title, author, digest, deleted string
		var bytes int64
		var kind sql.NullString
		var sameTask sql.NullBool
		if err := rows.Scan(&id, &title, &author, &bytes, &digest, &deleted, &kind, &sameTask); err != nil {
			return invalidArchive(err)
		}
		if !validNoteID(id) || !utf8.ValidString(title) || utf8.RuneCountInString(title) > 120 || strings.ContainsFunc(title, invalidTitleRune) || strings.TrimSpace(title) != title || (title == "" && deleted == "") {
			return failure("invalid_backup", "invalid note identity or title")
		}
		if author != "" {
			if normalized, err := validateLabel(author, "author"); err != nil || normalized != author {
				return failure("invalid_backup", "invalid note author")
			}
		}
		if bytes < 0 || (digest == "" && bytes != 0) || (digest != "" && !validArchiveDigest(digest)) {
			return failure("invalid_backup", "invalid note body metadata")
		}
		if !kind.Valid || kind.String != "progress" || !sameTask.Valid || !sameTask.Bool {
			return failure("invalid_backup", "note event does not belong to its task")
		}
	}
	return invalidArchive(rows.Err())
}

func (s *Store) BackupDirectory(path string) (err error) {
	unlock, err := s.noteLock(true)
	if err != nil {
		return err
	}
	defer unlock()
	if err = s.checkBackupDestination(path); err != nil {
		return err
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return err
	}
	if err = os.Mkdir(abs, 0700); err != nil {
		return classify(err)
	}
	defer func() {
		if err != nil {
			_ = os.RemoveAll(abs)
		}
	}()
	if err = s.backupSQLite(filepath.Join(abs, "tasks.sqlite")); err != nil {
		return err
	}
	db, snapshot, projectID, err := openBackup(filepath.Join(abs, "tasks.sqlite"))
	if err != nil {
		return err
	}
	defer db.Close()
	defer snapshot.Rollback()
	hasBodies, err := hasNoteBodies(snapshot)
	if err != nil {
		return err
	}
	if hasBodies {
		if err = snapshotSQLite(s.NotesPath(), filepath.Join(abs, "notes.sqlite")); err != nil {
			return err
		}
		bodies, bodySnapshot, err := openBodyBackup(filepath.Join(abs, "notes.sqlite"), projectID)
		if err != nil {
			return err
		}
		err = validateBodyReferences(snapshot, bodySnapshot)
		bodySnapshot.Rollback()
		bodies.Close()
		if err != nil {
			return err
		}
	}
	marker, err := os.OpenFile(filepath.Join(abs, "COMPLETE"), os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		return classify(err)
	}
	if _, err = io.WriteString(marker, directoryBackupMarker); err == nil {
		err = marker.Sync()
	}
	closeErr := marker.Close()
	if err != nil {
		return classify(err)
	}
	if closeErr != nil {
		return classify(closeErr)
	}
	if err = syncDirectory(abs); err != nil {
		return classify(err)
	}
	return classify(syncDirectory(filepath.Dir(abs)))
}

func openReadOnlySQLite(path string) (*sql.DB, error) {
	if err := validateSQLiteFile(path); err != nil {
		return nil, err
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return nil, err
	}
	u := url.URL{Scheme: "file", Path: filepath.ToSlash(abs)}
	query := u.Query()
	query.Set("mode", "ro")
	query.Add("_pragma", "busy_timeout(5000)")
	query.Add("_pragma", "trusted_schema(0)")
	u.RawQuery = query.Encode()
	db, err := sql.Open("sqlite", u.String())
	if err != nil {
		return nil, classify(err)
	}
	db.SetMaxOpenConns(1)
	return db, nil
}

func snapshotSQLite(source, destination string) error {
	db, err := openReadOnlySQLite(source)
	if err != nil {
		return err
	}
	defer db.Close()
	return (&Store{DB: db}).backupSQLite(destination)
}

func openBodyBackup(path, projectID string) (*sql.DB, *sql.Tx, error) {
	db, err := openReadOnlySQLite(path)
	if err != nil {
		return nil, nil, err
	}
	tx, err := db.BeginTx(context.Background(), &sql.TxOptions{ReadOnly: true})
	if err != nil {
		db.Close()
		return nil, nil, invalidArchive(err)
	}
	fail := func(err error) (*sql.DB, *sql.Tx, error) {
		tx.Rollback()
		db.Close()
		return nil, nil, err
	}
	var integrity string
	if err = tx.QueryRow("PRAGMA quick_check(1)").Scan(&integrity); err != nil {
		return fail(invalidArchive(err))
	}
	if integrity != "ok" {
		return fail(failure("invalid_backup", integrity))
	}
	schemas := map[string][]archiveColumn{
		"body_info":   {{name: "key", kind: "TEXT", required: true}, {name: "value", kind: "TEXT", required: true}},
		"bodies":      {{name: "id", kind: "TEXT", required: true}, {name: "body_bytes", kind: "INTEGER", required: true}, {name: "sha256", kind: "TEXT", required: true}},
		"body_chunks": {{name: "note_id", kind: "TEXT", required: true}, {name: "sequence", kind: "INTEGER", required: true}, {name: "data", kind: "BLOB", required: true}},
	}
	for table, columns := range schemas {
		if err := validateBackupColumns(tx, table, columns); err != nil {
			return fail(err)
		}
	}
	var version, identity string
	if err = tx.QueryRow("SELECT (SELECT value FROM body_info WHERE key='schema_version'),(SELECT value FROM body_info WHERE key='project_id')").Scan(&version, &identity); err != nil {
		return fail(invalidArchive(err))
	}
	if version != "1" || identity != projectID {
		return fail(failure("invalid_backup", "note body database has the wrong schema version or project identity"))
	}
	return db, tx, nil
}

func validateBodyReferences(index Queryer, bodies Queryer) error {
	rows, err := index.Query("SELECT id,body_bytes,sha256 FROM notes WHERE deleted_at='' AND sha256<>'' ORDER BY event_id")
	if err != nil {
		return invalidArchive(err)
	}
	defer rows.Close()
	buffer := make([]byte, noteChunkBytes)
	for rows.Next() {
		var id, digest string
		var size int64
		if err := rows.Scan(&id, &size, &digest); err != nil {
			return invalidArchive(err)
		}
		if err := validateBodyReference(bodies, id, size, digest); err != nil {
			return err
		}
		reader, err := newBackupBodyReader(bodies, id, size)
		if err != nil {
			return err
		}
		hash := sha256.New()
		_, err = io.CopyBuffer(hash, reader, buffer)
		reader.Close()
		if err != nil {
			return err
		}
		if hex.EncodeToString(hash.Sum(nil)) != digest {
			return failure("invalid_backup", "note body checksum mismatch for "+id)
		}
	}
	return invalidArchive(rows.Err())
}

func validateBodyReference(bodies Queryer, id string, size int64, digest string) error {
	var actualSize int64
	var actualDigest string
	if err := bodies.QueryRow("SELECT body_bytes,sha256 FROM bodies WHERE id=?", id).Scan(&actualSize, &actualDigest); err != nil {
		return invalidArchive(err)
	}
	if actualSize != size || actualDigest != digest {
		return failure("invalid_backup", "note body metadata mismatch for "+id)
	}
	return nil
}

func openBackupMember(root *os.Root, name string) (*os.File, error) {
	before, err := root.Lstat(name)
	if err != nil {
		return nil, invalidArchive(err)
	}
	if !before.Mode().IsRegular() {
		return nil, failure("invalid_backup", "backup member is not a regular file: "+name)
	}
	f, err := root.Open(name)
	if err != nil {
		return nil, invalidArchive(err)
	}
	after, err := f.Stat()
	if err != nil || !after.Mode().IsRegular() || !os.SameFile(before, after) {
		f.Close()
		return nil, failure("invalid_backup", "backup member changed while opening: "+name)
	}
	return f, nil
}

func stageBackupMember(root *os.Root, name, directory string) error {
	source, err := openBackupMember(root, name)
	if err != nil {
		return err
	}
	defer source.Close()
	target, err := os.OpenFile(filepath.Join(directory, name), os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return classify(err)
	}
	_, copyErr := io.Copy(target, source)
	closeErr := target.Close()
	if copyErr != nil {
		return classify(copyErr)
	}
	return classify(closeErr)
}

func (s *Store) RestoreDirectory(path string) error {
	unlock, err := s.noteLock(false)
	if err != nil {
		return err
	}
	defer unlock()
	info, err := os.Lstat(path)
	if err != nil {
		return invalidArchive(err)
	}
	if !info.IsDir() {
		return failure("invalid_backup", "expected a complete backup directory")
	}
	root, err := os.OpenRoot(path)
	if err != nil {
		return invalidArchive(err)
	}
	defer root.Close()
	marker, err := openBackupMember(root, "COMPLETE")
	if err != nil {
		return err
	}
	data, err := io.ReadAll(io.LimitReader(marker, int64(len(directoryBackupMarker)+1)))
	marker.Close()
	if err != nil || string(data) != directoryBackupMarker {
		return failure("invalid_backup", "backup directory is incomplete or has an unsupported format")
	}
	stage, err := os.MkdirTemp(filepath.Dir(s.NotesPath()), ".taskctl-restore-*")
	if err != nil {
		return classify(err)
	}
	defer os.RemoveAll(stage)
	if err := stageBackupMember(root, "tasks.sqlite", stage); err != nil {
		return err
	}
	db, source, projectID, err := openBackup(filepath.Join(stage, "tasks.sqlite"))
	if err != nil {
		return err
	}
	defer db.Close()
	defer source.Rollback()
	hasBodies, err := hasNoteBodies(source)
	if err != nil {
		return err
	}
	if !hasBodies {
		return s.restoreSource(source, projectID, nil)
	}
	if err := stageBackupMember(root, "notes.sqlite", stage); err != nil {
		return err
	}
	bodyDB, bodies, err := openBodyBackup(filepath.Join(stage, "notes.sqlite"), projectID)
	if err != nil {
		return err
	}
	defer bodyDB.Close()
	defer bodies.Rollback()
	return s.restoreSource(source, projectID, func(target *sql.Tx) error {
		return s.restoreNoteBodies(target, bodies)
	})
}

func (s *Store) restoreNoteBodies(index Queryer, source Queryer) error {
	writer := s.newBodyWriter(true)
	defer writer.Close()
	rows, err := index.Query("SELECT id,body_bytes,sha256 FROM notes WHERE deleted_at='' AND sha256<>'' ORDER BY event_id")
	if err != nil {
		return invalidArchive(err)
	}
	defer rows.Close()
	for rows.Next() {
		var id, digest string
		var size int64
		if err := rows.Scan(&id, &size, &digest); err != nil {
			return invalidArchive(err)
		}
		if err := validateBodyReference(source, id, size, digest); err != nil {
			return err
		}
		reader, err := newBackupBodyReader(source, id, size)
		if err != nil {
			return err
		}
		actualSize, actualDigest, err := writer.ReplaceUnreferenced(id, reader)
		reader.Close()
		if err != nil {
			return invalidArchive(err)
		}
		if actualSize != size || actualDigest != digest {
			return failure("invalid_backup", "note body checksum mismatch for "+id)
		}
	}
	if err := rows.Err(); err != nil {
		return invalidArchive(err)
	}
	return writer.Commit()
}

type backupBodyReader struct {
	rows     *sql.Rows
	current  []byte
	sequence int64
	read     int64
	expected int64
	done     bool
}

func newBackupBodyReader(source Queryer, id string, size int64) (*backupBodyReader, error) {
	rows, err := source.Query(`SELECT CASE WHEN typeof(sequence)='integer' THEN sequence ELSE -1 END,typeof(sequence),typeof(data),
 CASE WHEN typeof(data)='blob' THEN length(data) ELSE -1 END,
 CASE WHEN typeof(data)='blob' AND length(data) BETWEEN 1 AND 65536 THEN data ELSE NULL END
 FROM body_chunks WHERE note_id=? ORDER BY sequence`, id)
	if err != nil {
		return nil, invalidArchive(err)
	}
	return &backupBodyReader{rows: rows, expected: size}, nil
}

func (r *backupBodyReader) Read(p []byte) (int, error) {
	if len(p) == 0 {
		return 0, nil
	}
	if r.done {
		return 0, io.EOF
	}
	if len(r.current) == 0 {
		if !r.rows.Next() {
			r.done = true
			if err := r.rows.Err(); err != nil {
				return 0, invalidArchive(err)
			}
			if r.read != r.expected {
				return 0, failure("invalid_backup", "note body has missing bytes")
			}
			return 0, io.EOF
		}
		var sequence, size int64
		var sequenceType, dataType string
		if err := r.rows.Scan(&sequence, &sequenceType, &dataType, &size, &r.current); err != nil {
			return 0, invalidArchive(err)
		}
		if sequenceType != "integer" || dataType != "blob" || sequence != r.sequence || size < 1 || size > 65536 || int64(len(r.current)) != size || size > r.expected-r.read {
			return 0, failure("invalid_backup", "note body contains an invalid chunk")
		}
		r.sequence++
		r.read += size
	}
	n := copy(p, r.current)
	r.current = r.current[n:]
	return n, nil
}

func (r *backupBodyReader) Close() error {
	return r.rows.Close()
}

var _ io.ReadCloser = (*backupBodyReader)(nil)
