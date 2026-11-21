package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"io"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"time"
)

var archiveTables = []string{"tasks", "tags", "metadata", "prerequisites", "events", "subscriptions", "inactivity_marks", "scratch_areas", "scratch_refs"}

type archiveColumn struct {
	name     string
	kind     string
	required bool
}

func archiveColumns(q Queryer, table string) ([]archiveColumn, error) {
	rows, err := q.Query("PRAGMA table_info(" + table + ")")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var columns []archiveColumn
	for rows.Next() {
		var index, required, primary int
		var column archiveColumn
		var defaultValue any
		if err := rows.Scan(&index, &column.name, &column.kind, &required, &defaultValue, &primary); err != nil {
			return nil, err
		}
		column.required = required != 0 || primary != 0
		columns = append(columns, column)
	}
	return columns, rows.Err()
}

func archiveValue(table string, column archiveColumn, value any) (any, error) {
	invalid := func(reason string) (any, error) {
		return nil, failure("invalid_backup", table+"."+column.name+": "+reason)
	}
	if value == nil {
		if column.required {
			return invalid("null is not permitted")
		}
		return nil, nil
	}
	if column.kind == "INTEGER" {
		var number int64
		switch value := value.(type) {
		case int64:
			number = value
		default:
			return invalid("expected an integer")
		}
		switch column.name {
		case "id", "task_id", "prerequisite_id", "origin_id", "parent_id", "event_id", "revision":
			if number <= 0 {
				return invalid("expected a positive integer")
			}
		case "resume_idle", "snapshot_pending", "queue_snapshot_pending", "offered_snapshot":
			if number != 0 && number != 1 {
				return invalid("expected 0 or 1")
			}
		default:
			if number < 0 {
				return invalid("expected a nonnegative integer")
			}
		}
		return number, nil
	}
	text, ok := value.(string)
	if column.kind != "TEXT" || !ok {
		return invalid("expected a string")
	}
	if strings.HasSuffix(column.name, "_at") || column.name == "lease_until" {
		if text != "" {
			parsed, err := time.Parse(time.RFC3339Nano, text)
			if err != nil || timestamp(parsed) != text {
				return invalid("expected a canonical UTC timestamp")
			}
		} else if column.name == "created_at" || column.name == "updated_at" || column.name == "activity_at" || column.name == "detected_at" {
			return invalid("timestamp must not be empty")
		}
	}
	switch {
	case table == "events" && column.name == "data":
		if !json.Valid([]byte(text)) {
			return invalid("invalid event JSON")
		}
	case table == "tasks" && column.name == "title", table == "events" && column.name == "kind", table == "tags" && column.name == "tag":
		normalized, err := validateLabel(text, column.name)
		if err != nil || normalized != text {
			return invalid("invalid label")
		}
	case table == "metadata" && column.name == "key":
		normalized, err := validateMetadataKey(text)
		if err != nil || normalized != text {
			return invalid("invalid metadata key")
		}
	case table == "tasks" && column.name == "owner" && text != "":
		normalized, err := validateLabel(text, "owner")
		if err != nil || normalized != text {
			return invalid("invalid owner")
		}
	case column.name == "session":
		if err := validateSession(text); err != nil {
			return invalid("invalid session identity")
		}
	case table == "scratch_areas" && column.name == "id", table == "scratch_refs" && column.name == "area_id":
		if !ValidAreaID(text) {
			return invalid("invalid scratch area ID")
		}
	}
	return text, nil
}

func invalidArchive(err error) error {
	if err == nil {
		return nil
	}
	classified := classify(err)
	if code := ErrorCode(classified); strings.HasPrefix(code, "storage_") || code == "busy" {
		return classified
	}
	return failure("invalid_backup", err.Error())
}

func (s *Store) Backup(path string) error {
	abs, err := filepath.Abs(path)
	if err != nil {
		return err
	}
	f, err := os.CreateTemp(filepath.Dir(abs), ".taskctl-backup-*")
	if err != nil {
		return err
	}
	tmp := f.Name()
	defer os.Remove(tmp)
	if err = f.Close(); err != nil {
		return err
	}
	if _, err = s.DB.Exec("VACUUM INTO ?", tmp); err != nil {
		return classify(err)
	}
	f, err = os.OpenFile(tmp, os.O_RDWR, 0)
	if err != nil {
		return classify(err)
	}
	if err = f.Sync(); err != nil {
		f.Close()
		return classify(err)
	}
	if err = f.Close(); err != nil {
		return classify(err)
	}
	// A link publishes the complete file atomically and refuses an existing destination.
	if err = os.Link(tmp, abs); err != nil {
		return err
	}
	return syncDirectory(filepath.Dir(abs))
}

func syncDirectory(path string) error {
	f, err := os.Open(path)
	if err != nil {
		return err
	}
	defer f.Close()
	return f.Sync()
}

func openBackup(path string) (*sql.DB, *sql.Tx, string, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, nil, "", classify(err)
	}
	var header [16]byte
	_, err = io.ReadFull(f, header[:])
	f.Close()
	if err != nil && err != io.EOF && err != io.ErrUnexpectedEOF {
		return nil, nil, "", classify(err)
	}
	if err != nil || string(header[:]) != "SQLite format 3\x00" {
		return nil, nil, "", failure("invalid_backup", "expected a taskctl SQLite backup")
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return nil, nil, "", err
	}
	u := url.URL{Scheme: "file", Path: filepath.ToSlash(abs)}
	q := u.Query()
	q.Set("mode", "ro")
	q.Add("_pragma", "busy_timeout(5000)")
	q.Add("_pragma", "query_only(1)")
	q.Add("_pragma", "trusted_schema(0)")
	u.RawQuery = q.Encode()
	db, err := sql.Open("sqlite", u.String())
	if err != nil {
		return nil, nil, "", classify(err)
	}
	db.SetMaxOpenConns(1)
	tx, err := db.BeginTx(context.Background(), &sql.TxOptions{ReadOnly: true})
	if err != nil {
		db.Close()
		return nil, nil, "", invalidArchive(err)
	}
	fail := func(err error) (*sql.DB, *sql.Tx, string, error) {
		tx.Rollback()
		db.Close()
		return nil, nil, "", err
	}
	var integrity string
	if err = tx.QueryRow("PRAGMA quick_check(1)").Scan(&integrity); err != nil {
		return fail(invalidArchive(err))
	}
	if integrity != "ok" {
		return fail(failure("invalid_backup", integrity))
	}
	if err = validateBackupColumns(tx, "store_info", []archiveColumn{{name: "key", kind: "TEXT", required: true}, {name: "value", kind: "TEXT", required: true}}); err != nil {
		return fail(err)
	}
	var version, projectID string
	if err = tx.QueryRow("SELECT value FROM store_info WHERE key='schema_version'").Scan(&version); err != nil {
		return fail(invalidArchive(err))
	}
	if version != "1" {
		return fail(failure("unsupported_schema", "backup schema must be version 1"))
	}
	if err = tx.QueryRow("SELECT value FROM store_info WHERE key='project_id'").Scan(&projectID); err != nil {
		return fail(invalidArchive(err))
	}
	if projectID == "" {
		return fail(failure("invalid_backup", "backup project identity is empty"))
	}
	return db, tx, projectID, nil
}

func validateBackupColumns(source *sql.Tx, table string, columns []archiveColumn) error {
	var tableType string
	if err := source.QueryRow("SELECT type FROM pragma_table_list WHERE schema='main' AND name=?", table).Scan(&tableType); err != nil {
		return invalidArchive(err)
	}
	if tableType != "table" {
		return failure("invalid_backup", "expected ordinary backup table "+table)
	}
	sourceColumns, err := archiveColumns(source, table)
	if err != nil {
		return invalidArchive(err)
	}
	if len(sourceColumns) != len(columns) {
		return failure("invalid_backup", "column count mismatch in "+table)
	}
	for i, column := range columns {
		if sourceColumns[i].name != column.name || sourceColumns[i].kind != column.kind {
			return failure("invalid_backup", "column mismatch in "+table)
		}
	}
	var hidden int
	if err := source.QueryRow("SELECT COUNT(*) FROM pragma_table_xinfo(?) WHERE hidden<>0", table).Scan(&hidden); err != nil {
		return invalidArchive(err)
	}
	if hidden != 0 {
		return failure("invalid_backup", "generated or hidden backup columns are not supported")
	}
	return nil
}

func restoreTable(source, target *sql.Tx, table string) error {
	columns, err := archiveColumns(target, table)
	if err != nil {
		return err
	}
	if err := validateBackupColumns(source, table, columns); err != nil {
		return err
	}
	names, slots := make([]string, len(columns)), make([]string, len(columns))
	values, destinations := make([]any, len(columns)), make([]any, len(columns))
	for i, column := range columns {
		names[i], slots[i], destinations[i] = "\""+column.name+"\"", "?", &values[i]
	}
	columnList := strings.Join(names, ",")
	rows, err := source.Query("SELECT " + columnList + " FROM " + table)
	if err != nil {
		return invalidArchive(err)
	}
	defer rows.Close()
	insert, err := target.Prepare("INSERT INTO " + table + "(" + columnList + ") VALUES(" + strings.Join(slots, ",") + ")")
	if err != nil {
		return err
	}
	defer insert.Close()
	for rows.Next() {
		if err = rows.Scan(destinations...); err != nil {
			return invalidArchive(err)
		}
		for i, column := range columns {
			if _, err = archiveValue(table, column, values[i]); err != nil {
				return err
			}
		}
		if _, err = insert.Exec(values...); err != nil {
			return invalidArchive(err)
		}
	}
	return invalidArchive(rows.Err())
}

func (s *Store) Restore(path string) error {
	sourceDB, source, projectID, err := openBackup(path)
	if err != nil {
		return err
	}
	defer sourceDB.Close()
	defer source.Rollback()
	return s.write(func(tx *sql.Tx) error {
		for _, table := range append(append([]string{}, archiveTables...), "create_requests", "scratch_cleanup_state") {
			var occupied bool
			if err := tx.QueryRow("SELECT EXISTS(SELECT 1 FROM " + table + ")").Scan(&occupied); err != nil {
				return err
			}
			if occupied {
				return failure("conflict", "restore requires an empty new project")
			}
		}
		if _, err := tx.Exec("PRAGMA defer_foreign_keys=ON"); err != nil {
			return err
		}
		for _, table := range archiveTables {
			if err := restoreTable(source, tx, table); err != nil {
				return err
			}
		}
		if err := checkTaskCycles(tx); err != nil {
			return invalidArchive(err)
		}
		var invalid bool
		if err := tx.QueryRow(`SELECT EXISTS(SELECT 1 FROM tasks t LEFT JOIN tasks p ON p.id=t.parent_id LEFT JOIN tasks root ON root.id=t.origin_id
 WHERE root.id IS NULL OR root.parent_id IS NOT NULL OR root.origin_id<>root.id
 OR (t.parent_id IS NULL AND t.origin_id<>t.id)
 OR (t.parent_id IS NOT NULL AND (p.status<>'superseded' OR p.origin_id<>t.origin_id))
 OR (t.status='superseded' AND NOT EXISTS(SELECT 1 FROM tasks c WHERE c.parent_id=t.id)))`).Scan(&invalid); err != nil {
			return err
		}
		if invalid {
			return failure("invalid_backup", "invalid task lineage")
		}
		rows, err := tx.Query("PRAGMA foreign_key_check")
		if err != nil {
			return err
		}
		hasBadFK := rows.Next()
		err = rows.Err()
		rows.Close()
		if err != nil {
			return err
		}
		if hasBadFK {
			return failure("invalid_backup", "broken foreign key")
		}
		now := timestamp(s.Now())
		data, err := json.Marshal(map[string]string{"source_project": projectID})
		if err != nil {
			return err
		}
		if _, err = tx.Exec(`INSERT INTO events(task_id,kind,message,data,created_at)
 SELECT id,'restored','Active claim cleared during restore',?,? FROM tasks WHERE status='in_progress' ORDER BY id`, string(data), now); err != nil {
			return err
		}
		if _, err = tx.Exec(`UPDATE tasks SET status='pending',owner='',claim_id='',last_activity_at='',revision=revision+1,updated_at=? WHERE status='in_progress'`, now); err != nil {
			return err
		}
		if _, err = tx.Exec("DELETE FROM subscriptions; DELETE FROM inactivity_marks"); err != nil {
			return err
		}
		if _, err = tx.Exec("UPDATE scratch_areas SET removed_at=COALESCE(removed_at,?),cleanup_token=NULL", now); err != nil {
			return err
		}
		return nil
	})
}
