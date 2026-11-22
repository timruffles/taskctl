package store

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"modernc.org/sqlite"
)

type Error struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

func (e *Error) Error() string           { return e.Code + ": " + e.Message }
func failure(code, message string) error { return &Error{code, message} }
func ErrorCode(err error) string {
	var e *Error
	if errors.As(classify(err), &e) {
		return e.Code
	}
	return "internal"
}

type Queryer interface {
	Query(string, ...any) (*sql.Rows, error)
	QueryRow(string, ...any) *sql.Row
}

type connectionQuery struct{ *sql.Conn }

func (q connectionQuery) Query(query string, args ...any) (*sql.Rows, error) {
	return q.QueryContext(context.Background(), query, args...)
}
func (q connectionQuery) QueryRow(query string, args ...any) *sql.Row {
	return q.QueryRowContext(context.Background(), query, args...)
}

type Store struct {
	DB           *sql.DB
	ProjectID    string
	Now          func() time.Time
	databasePath string
}

func Open(path, projectID string) (*Store, error) {
	return openStore(path, projectID, true)
}

func OpenExisting(path, projectID string) (*Store, error) {
	return openStore(path, projectID, false)
}

func openStore(path, projectID string, create bool) (*Store, error) {
	if create {
		if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
			return nil, classify(err)
		}
	}
	u := url.URL{Scheme: "file", Path: filepath.ToSlash(path)}
	q := u.Query()
	q.Set("_txlock", "immediate")
	q.Add("_pragma", "busy_timeout(5000)")
	q.Add("_pragma", "foreign_keys(1)")
	q.Add("_pragma", "synchronous(FULL)")
	q.Add("_pragma", "temp_store(FILE)")
	if !create {
		q.Set("mode", "rw")
	}
	u.RawQuery = q.Encode()
	db, err := sql.Open("sqlite", u.String())
	if err != nil {
		return nil, err
	}
	db.SetMaxOpenConns(1)
	s := &Store{DB: db, ProjectID: projectID, Now: time.Now, databasePath: path}
	if !create {
		if err = s.validateExisting(db); err != nil {
			db.Close()
			if _, statErr := os.Stat(path); errors.Is(statErr, os.ErrNotExist) {
				return nil, failure("storage_missing", "project database is missing; restore a backup or select the correct project")
			}
			return nil, err
		}
	}
	if _, err = db.Exec("PRAGMA journal_mode=WAL"); err != nil {
		db.Close()
		return nil, classify(err)
	}
	current, err := schemaCurrent(db)
	if err != nil {
		db.Close()
		return nil, classify(err)
	}
	if !current || create {
		unlock, lockErr := s.noteLock(false)
		if lockErr != nil {
			db.Close()
			return nil, lockErr
		}
		err = s.write(func(tx *sql.Tx) error {
			if !create {
				if err := s.validateExisting(tx); err != nil {
					return err
				}
			}
			if _, err := tx.Exec(`CREATE TABLE IF NOT EXISTS store_info(key TEXT PRIMARY KEY,value TEXT NOT NULL);` + taskSchema + notificationSchema + scratchSchema + noteSchema); err != nil {
				return err
			}
			if err := ensureHookIndex(tx); err != nil {
				return err
			}
			var existing string
			err := tx.QueryRow("SELECT value FROM store_info WHERE key='project_id'").Scan(&existing)
			if errors.Is(err, sql.ErrNoRows) {
				_, err = tx.Exec("INSERT INTO store_info(key,value) VALUES('project_id',?),('schema_version','2')", projectID)
				return err
			}
			if err != nil {
				return err
			}
			if existing != projectID {
				return failure("wrong_project", "database belongs to a different project")
			}
			var version string
			if err = tx.QueryRow("SELECT value FROM store_info WHERE key='schema_version'").Scan(&version); err != nil {
				return err
			}
			if version != "1" && version != "2" {
				return failure("unsupported_schema", "expected schema version 1 or 2, found "+version)
			}
			if version == "1" {
				if err := s.migrateNotesTx(tx); err != nil {
					return err
				}
				_, err = tx.Exec("UPDATE store_info SET value='2' WHERE key='schema_version'")
				return err
			}
			return nil
		})
		unlock()
	}
	if err != nil {
		db.Close()
		return nil, err
	}
	if err = os.Chmod(path, 0600); err != nil {
		db.Close()
		return nil, classify(err)
	}
	return s, nil
}

func (s *Store) validateExisting(q Queryer) error {
	var project, version sql.NullString
	err := q.QueryRow("SELECT (SELECT value FROM store_info WHERE key='project_id'),(SELECT value FROM store_info WHERE key='schema_version')").Scan(&project, &version)
	if err != nil {
		classified := classify(err)
		if code := ErrorCode(classified); strings.HasPrefix(code, "storage_") || code == "busy" {
			return classified
		}
		return failure("storage_invalid", "project database is not a readable taskctl store: "+err.Error())
	}
	if !project.Valid || !version.Valid {
		return failure("storage_invalid", "project database is missing its identity or schema version")
	}
	if project.String != s.ProjectID {
		return failure("wrong_project", "database belongs to a different project")
	}
	if version.String != "1" && version.String != "2" {
		return failure("unsupported_schema", "expected schema version 1 or 2, found "+version.String)
	}
	for _, table := range archiveTables {
		var present bool
		if err := q.QueryRow("SELECT EXISTS(SELECT 1 FROM sqlite_schema WHERE type='table' AND name=?)", table).Scan(&present); err != nil {
			return classify(err)
		}
		if !present {
			return failure("storage_invalid", "project database is missing durable table "+table)
		}
	}
	if version.String == "2" {
		var present bool
		if err := q.QueryRow("SELECT EXISTS(SELECT 1 FROM sqlite_schema WHERE type='table' AND name='notes')").Scan(&present); err != nil {
			return classify(err)
		}
		if !present {
			return failure("storage_invalid", "project database is missing durable table notes")
		}
	}
	return nil
}

func schemaCurrent(q Queryer) (bool, error) {
	var current bool
	if err := q.QueryRow("SELECT EXISTS(SELECT 1 FROM sqlite_schema WHERE type='table' AND name='store_info')").Scan(&current); err != nil || !current {
		return false, err
	}
	err := q.QueryRow(`WITH required(type,name) AS (VALUES
 ('table','create_requests'),('table','scratch_cleanup_state'),('table','notes'),
 ('index','notes_task_order'),('index','notes_task_live_order'),('index','notes_with_bodies'),
 ('index','events_hook_task_id'),
 ('index','tasks_status_id'),('index','tasks_owner'),('index','tasks_origin'),('index','tasks_parent'),
 ('index','tags_lookup'),('index','prerequisites_reverse'),('index','events_task_id'),
 ('index','subscriptions_session'),('index','subscriptions_session_order'),
 ('index','scratch_refs_area_task'),('index','scratch_unreferenced')
) SELECT (SELECT value='2' FROM store_info WHERE key='schema_version') AND NOT EXISTS(SELECT 1 FROM required r WHERE NOT EXISTS(
 SELECT 1 FROM sqlite_schema s WHERE s.type=r.type AND s.name=r.name
 AND (r.name<>'events_hook_task_id' OR s.sql=?)))`, hookIndexSQL).Scan(&current)
	return current, err
}
func (s *Store) Close() error { return s.DB.Close() }

func (s *Store) read(fn func(Queryer) error) error {
	ctx := context.Background()
	conn, err := s.DB.Conn(ctx)
	if err != nil {
		return classify(err)
	}
	defer conn.Close()
	// BeginTx uses the driver's immediate writer lock, even for ReadOnly options.
	if _, err = conn.ExecContext(ctx, "BEGIN"); err != nil {
		return classify(err)
	}
	defer conn.ExecContext(ctx, "ROLLBACK")
	if err = fn(connectionQuery{conn}); err != nil {
		return classify(err)
	}
	_, err = conn.ExecContext(ctx, "COMMIT")
	return classify(err)
}

func (s *Store) write(fn func(*sql.Tx) error) error {
	tx, err := s.DB.BeginTx(context.Background(), nil)
	if err != nil {
		return classify(err)
	}
	defer tx.Rollback()
	if err = fn(tx); err != nil {
		return classify(err)
	}
	return classify(tx.Commit())
}
func classify(err error) error {
	if err == nil {
		return nil
	}
	var known *Error
	if errors.As(err, &known) {
		return err
	}
	var sqliteError *sqlite.Error
	if errors.As(err, &sqliteError) {
		switch sqliteError.Code() & 0xff {
		case 5, 6:
			return failure("busy", "database is busy; retry the operation")
		case 8:
			return failure("storage_read_only", err.Error())
		case 10:
			return failure("storage_io", err.Error())
		case 13:
			return failure("storage_full", err.Error())
		}
	}
	if errors.Is(err, syscall.ENOSPC) || errors.Is(err, syscall.EDQUOT) {
		return failure("storage_full", err.Error())
	}
	if errors.Is(err, syscall.EROFS) {
		return failure("storage_read_only", err.Error())
	}
	if errors.Is(err, syscall.EIO) {
		return failure("storage_io", err.Error())
	}
	if strings.Contains(err.Error(), "SQLITE_BUSY") || strings.Contains(err.Error(), "database is locked") {
		return failure("busy", "database is busy; retry the operation")
	}
	return err
}
func timestamp(t time.Time) string { return t.UTC().Format("2006-01-02T15:04:05.000000000Z") }
func newID(prefix string) string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic(err)
	}
	return prefix + hex.EncodeToString(b[:])
}
func taskNumber(id string) (int64, error) {
	if !strings.HasPrefix(id, "T") || len(id) < 2 {
		return 0, failure("invalid_id", "task ID must be T followed by a positive integer")
	}
	n, err := strconv.ParseInt(id[1:], 10, 64)
	if err != nil || n <= 0 || id != "T"+strconv.FormatInt(n, 10) {
		return 0, failure("invalid_id", "invalid task ID "+id)
	}
	return n, nil
}
func taskID(n int64) string {
	if n == 0 {
		return ""
	}
	return fmt.Sprintf("T%d", n)
}
func (s *Store) emit(tx *sql.Tx, id int64, kind, message string, data any) (int64, error) {
	b, err := json.Marshal(data)
	if err != nil {
		return 0, err
	}
	r, err := tx.Exec("INSERT INTO events(task_id,kind,message,data,created_at) VALUES(?,?,?,?,?)", id, kind, message, string(b), timestamp(s.Now()))
	if err != nil {
		return 0, err
	}
	return r.LastInsertId()
}

const scratchSchema = `
CREATE TABLE IF NOT EXISTS scratch_areas(id TEXT PRIMARY KEY,created_at TEXT NOT NULL,unreferenced_at TEXT,cleanup_token TEXT,removed_at TEXT);
CREATE TABLE IF NOT EXISTS scratch_refs(task_id INTEGER NOT NULL REFERENCES tasks(id),area_id TEXT NOT NULL REFERENCES scratch_areas(id),PRIMARY KEY(task_id,area_id));
CREATE INDEX IF NOT EXISTS scratch_refs_area_task ON scratch_refs(area_id,task_id);
CREATE INDEX IF NOT EXISTS scratch_unreferenced ON scratch_areas(unreferenced_at,id) WHERE removed_at IS NULL;
CREATE TABLE IF NOT EXISTS scratch_cleanup_state(area_id TEXT PRIMARY KEY REFERENCES scratch_areas(id),lease_token TEXT NOT NULL DEFAULT '',lease_until TEXT NOT NULL DEFAULT '',cursor TEXT NOT NULL DEFAULT '.',finalizing INTEGER NOT NULL DEFAULT 0 CHECK(finalizing IN (0,1)));
`

func (s *Store) refreshScratch(tx *sql.Tx, task int64) error {
	_, err := tx.Exec(`UPDATE scratch_areas SET unreferenced_at=CASE WHEN EXISTS(SELECT 1 FROM scratch_refs r JOIN tasks t ON t.id=r.task_id WHERE r.area_id=scratch_areas.id AND t.status IN ('pending','in_progress')) THEN NULL ELSE COALESCE(unreferenced_at,?) END
 WHERE id IN (SELECT area_id FROM scratch_refs WHERE task_id=?) AND removed_at IS NULL AND cleanup_token IS NULL`, timestamp(s.Now()), task)
	return err
}
