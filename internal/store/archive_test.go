package store

import (
	"bytes"
	"database/sql"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func backupTestPath(t *testing.T, s *Store) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "backup.sqlite")
	if err := s.Backup(path); err != nil {
		t.Fatal(err)
	}
	return path
}

func backupTestMutate(t *testing.T, path, query string) {
	t.Helper()
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err = db.Exec("PRAGMA foreign_keys=OFF; PRAGMA ignore_check_constraints=ON;" + query); err != nil {
		t.Fatal(err)
	}
}

func backupTestRead(t *testing.T, path string) *sql.Tx {
	t.Helper()
	db, tx, _, err := openBackup(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { tx.Rollback(); db.Close() })
	return tx
}

func TestSQLiteRoundTripAndRestoreFencing(t *testing.T) {
	s := newTaskTestStore(t)
	original := mustCreateTask(t, s, CreateSpec{Title: "Original intent", Brief: "Build the small project", Tags: []string{"v1"}, Metadata: map[string]string{"worktree": "/repo/worktree"}})
	children, err := s.Evolve(original.ID, "", original.Revision, EvolveSpec{Summary: "Remaining work is implementation and review", Replacements: []CreateSpec{{Key: "impl", Title: "Implementation", Brief: "Write the function"}, {Key: "review", Title: "Review", Brief: "Verify the function", Prerequisites: []string{"@impl"}}}})
	if err != nil {
		t.Fatal(err)
	}
	claimed := mustClaimTask(t, s, children[0].ID, "worker")
	if _, err = s.Progress(claimed.ID, claimed.ClaimID, "Parser implemented; boundary tests remain"); err != nil {
		t.Fatal(err)
	}
	if _, err = s.Subscribe(original.ID, "coordinator", true); err != nil {
		t.Fatal(err)
	}
	area := "A0123456789abcdef0123456789abcdef"
	if err = s.ScratchCreate(claimed.ID, claimed.ClaimID, area); err != nil {
		t.Fatal(err)
	}
	path := backupTestPath(t, s)
	restored, err := Open(filepath.Join(t.TempDir(), "tasks.sqlite"), "restored-project")
	if err != nil {
		t.Fatal(err)
	}
	defer restored.Close()
	if err = restored.Restore(path); err != nil {
		t.Fatal(err)
	}
	task, err := restored.Get(claimed.ID)
	if err != nil {
		t.Fatal(err)
	}
	if task.Status != "pending" || task.ClaimID != "" || task.Owner != "" || task.Progress != "Parser implemented; boundary tests remain" {
		t.Fatalf("restored task: %+v", task)
	}
	if _, err = restored.Complete(claimed.ID, claimed.ClaimID, "stale worker"); err == nil {
		t.Fatal("restored old claim remains valid")
	}
	if task.OriginID != original.ID || !reflect.DeepEqual(task.Tags, []string{"v1"}) || task.Metadata["worktree"] != "/repo/worktree" {
		t.Fatalf("lost lineage or context: %+v", task)
	}
	available, total, err := restored.List("available", nil, 20, 0)
	if err != nil || total != 1 || available[0].ID != task.ID {
		t.Fatalf("availability: %+v %d %v", available, total, err)
	}
	var count int
	if err = restored.DB.QueryRow("SELECT COUNT(*) FROM subscriptions").Scan(&count); err != nil || count != 0 {
		t.Fatalf("restored stale subscriptions: %d %v", count, err)
	}
	var removed string
	if err = restored.DB.QueryRow("SELECT removed_at FROM scratch_areas WHERE id=?", area).Scan(&removed); err != nil || removed == "" {
		t.Fatalf("scratch references: %q %v", removed, err)
	}
	if err = restored.Restore(path); ErrorCode(err) != "conflict" {
		t.Fatalf("restore over live project: %v", err)
	}
	still, err := s.Get(claimed.ID)
	if err != nil || still.ClaimID != claimed.ClaimID {
		t.Fatalf("source changed: %+v %v", still, err)
	}
}

func TestSQLiteRestoreAcceptsLongLegalOwner(t *testing.T) {
	for _, operation := range []string{"claim", "takeover"} {
		t.Run(operation, func(t *testing.T) {
			source := newTaskTestStore(t)
			task := mustCreateTask(t, source, CreateSpec{Title: "Long legal owner"})
			owner := strings.Repeat("worker-", 200)
			var claimed Task
			var err error
			if operation == "claim" {
				claimed, err = source.Claim(task.ID, owner)
			} else {
				previous := mustClaimTask(t, source, task.ID, "previous-worker")
				claimed, err = source.Takeover(task.ID, previous.ClaimID, owner, "Recover work")
			}
			if err != nil || claimed.Owner != owner {
				t.Fatalf("legal ownership: %+v %v", claimed, err)
			}
			target := newTaskTestStore(t)
			if err = target.Restore(backupTestPath(t, source)); err != nil {
				t.Fatal(err)
			}
			restored, err := target.Get(task.ID)
			if err != nil || restored.Status != "pending" || restored.Owner != "" || restored.ClaimID != "" {
				t.Fatalf("restored ownership: %+v %v", restored, err)
			}
			still, err := source.Get(task.ID)
			if err != nil || still.Owner != owner || still.ClaimID != claimed.ClaimID {
				t.Fatalf("source ownership changed: %+v %v", still, err)
			}
		})
	}
}

func TestSQLiteBackupIncludesWALAndDoesNotOverwrite(t *testing.T) {
	s := newTaskTestStore(t)
	first := mustCreateTask(t, s, CreateSpec{Title: "Committed before backup"})
	path := backupTestPath(t, s)
	mustCreateTask(t, s, CreateSpec{Title: "Committed after backup"})
	source := backupTestRead(t, path)
	var count int
	var title string
	if err := source.QueryRow("SELECT COUNT(*),title FROM tasks").Scan(&count, &title); err != nil || count != 1 || title != first.Title {
		t.Fatalf("backup contents: %d %q %v", count, title, err)
	}
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err = s.Backup(path); err == nil {
		t.Fatal("backup overwrote existing destination")
	}
	after, _ := os.ReadFile(path)
	if !bytes.Equal(before, after) {
		t.Fatal("existing backup changed")
	}
	restored := newTaskTestStore(t)
	if err = restored.Restore(path); err != nil {
		t.Fatal(err)
	}
	archiveChaosIntegrity(t, restored)
}

func TestInvalidRestoreRollsBack(t *testing.T) {
	s := newTaskTestStore(t)
	mustCreateTask(t, s, CreateSpec{Title: "A"})
	mustCreateTask(t, s, CreateSpec{Title: "B"})
	cases := map[string]string{
		"foreign-key":       "INSERT INTO tags VALUES(999,'bad')",
		"cycle":             "INSERT INTO prerequisites VALUES(1,2),(2,1)",
		"columns":           "ALTER TABLE tasks ADD COLUMN extra TEXT",
		"lineage":           "UPDATE tasks SET origin_id=NULL WHERE id=1",
		"replacement-cycle": "UPDATE tasks SET status='superseded',parent_id=CASE id WHEN 1 THEN 2 ELSE 1 END",
		"view":              "ALTER TABLE metadata RENAME TO hidden_metadata; CREATE VIEW metadata AS SELECT * FROM hidden_metadata",
		"generated-column":  "ALTER TABLE metadata ADD COLUMN extra TEXT GENERATED ALWAYS AS (value) VIRTUAL",
	}
	for kind, query := range cases {
		t.Run(kind, func(t *testing.T) {
			path := backupTestPath(t, s)
			backupTestMutate(t, path, query)
			target := newTaskTestStore(t)
			if err := target.Restore(path); ErrorCode(err) != "invalid_backup" {
				t.Fatalf("invalid restore: %v", err)
			}
			for _, table := range archiveTables {
				var count int
				if err := target.DB.QueryRow("SELECT COUNT(*) FROM " + table).Scan(&count); err != nil || count != 0 {
					t.Fatalf("restore was partial in %s: %d %v", table, count, err)
				}
			}
		})
	}
}

func TestSQLiteRestoreExceedsFormerJSONLimit(t *testing.T) {
	source := newTaskTestStore(t)
	task := mustCreateTask(t, source, CreateSpec{Title: "Large backup"})
	id, _ := taskNumber(task.ID)
	const records = 132
	payload := strings.Repeat("x", 512*1024)
	if err := source.write(func(tx *sql.Tx) error {
		stmt, err := tx.Prepare("INSERT INTO events(task_id,kind,message,data,created_at) VALUES(?,'progress',?,'{}',?)")
		if err != nil {
			return err
		}
		defer stmt.Close()
		for i := 0; i < records; i++ {
			if _, err = stmt.Exec(id, payload, timestamp(source.Now())); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	path := backupTestPath(t, source)
	info, err := os.Stat(path)
	if err != nil || info.Size() <= 64*1024*1024 {
		t.Fatalf("fixture size: %v %v", info, err)
	}
	target := newTaskTestStore(t)
	if err = target.Restore(path); err != nil {
		t.Fatal(err)
	}
	var count, bytes int64
	if err = target.DB.QueryRow("SELECT COUNT(*),SUM(length(message)) FROM events WHERE kind='progress'").Scan(&count, &bytes); err != nil || count != records || bytes != records*int64(len(payload)) {
		t.Fatalf("large restore: %d records %d bytes %v", count, bytes, err)
	}
	archiveChaosIntegrity(t, target)
}

func TestRestoreRejectsJSONWithoutChangingSource(t *testing.T) {
	path := filepath.Join(t.TempDir(), "removed-format.json")
	data := []byte(`{"format":"taskctl-export","schema_version":1}`)
	if err := os.WriteFile(path, data, 0600); err != nil {
		t.Fatal(err)
	}
	target := newTaskTestStore(t)
	if err := target.Restore(path); ErrorCode(err) != "invalid_backup" {
		t.Fatalf("JSON restore: %v", err)
	}
	after, err := os.ReadFile(path)
	if err != nil || !bytes.Equal(after, data) {
		t.Fatalf("source changed: %v", err)
	}
}

func TestSQLiteRestoreDoesNotImportSourceSchema(t *testing.T) {
	source := newTaskTestStore(t)
	task := mustCreateTask(t, source, CreateSpec{Title: "Keep data, use application schema"})
	path := backupTestPath(t, source)
	backupTestMutate(t, path, `CREATE TRIGGER backup_trigger AFTER INSERT ON tasks BEGIN SELECT RAISE(ABORT,'source trigger executed'); END`)
	before, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	target := newTaskTestStore(t)
	if err = target.Restore(path); err != nil {
		t.Fatal(err)
	}
	got, err := target.Get(task.ID)
	if err != nil || got.Title != task.Title {
		t.Fatalf("restored task: %+v %v", got, err)
	}
	var imported int
	if err = target.DB.QueryRow("SELECT COUNT(*) FROM sqlite_schema WHERE name='backup_trigger'").Scan(&imported); err != nil || imported != 0 {
		t.Fatalf("source trigger imported: %d %v", imported, err)
	}
	after, err := os.ReadFile(path)
	if err != nil || !bytes.Equal(before, after) {
		t.Fatalf("read-only source changed: %v", err)
	}
}

func TestSQLiteRestoreUsesOneSourceSnapshot(t *testing.T) {
	path := filepath.Join(t.TempDir(), "source.sqlite")
	source, err := Open(path, "snapshot-source")
	if err != nil {
		t.Fatal(err)
	}
	defer source.Close()
	task := mustClaimTask(t, source, mustCreateTask(t, source, CreateSpec{Title: "Snapshot boundary"}).ID, "worker")
	if _, err = source.Progress(task.ID, task.ClaimID, "Before snapshot"); err != nil {
		t.Fatal(err)
	}
	db, snapshot, _, err := openBackup(path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	defer snapshot.Rollback()
	if _, err = source.Progress(task.ID, task.ClaimID, "After snapshot"); err != nil {
		t.Fatal(err)
	}
	mustCreateTask(t, source, CreateSpec{Title: "After snapshot"})
	target := newTaskTestStore(t)
	if err = target.write(func(tx *sql.Tx) error {
		if _, err := tx.Exec("PRAGMA defer_foreign_keys=ON"); err != nil {
			return err
		}
		for _, table := range archiveTables {
			if err := restoreTable(snapshot, tx, table); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	got, err := target.Get(task.ID)
	if err != nil || got.Progress != "Before snapshot" {
		t.Fatalf("snapshot task: %+v %v", got, err)
	}
	var count int
	if err = target.DB.QueryRow("SELECT COUNT(*) FROM tasks").Scan(&count); err != nil || count != 1 {
		t.Fatalf("snapshot task count: %d %v", count, err)
	}
	archiveChaosSnapshot(t, target.DB)
}
