package store

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestNoteBodiesRequireCompleteBackup(t *testing.T) {
	s := newTaskTestStore(t)
	task := mustClaimTask(t, s, mustCreateTask(t, s, CreateSpec{Title: "Journal backup"}).ID, "worker")
	if _, err := s.AddNote(task.ID, task.ClaimID, "Checkpoint", strings.NewReader("Detailed findings\n")); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "incomplete.sqlite")
	if err := s.Backup(path); err == nil || !strings.Contains(err.Error(), "--directory") {
		t.Fatalf("bodyless backup was accepted: %v", err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("failed backup published a file: %v", err)
	}
}

func TestBackupCannotOccupyReservedBodyDatabasePath(t *testing.T) {
	for _, directory := range []bool{false, true} {
		s := newTaskTestStore(t)
		path := s.NotesPath()
		var err error
		if directory {
			err = s.BackupDirectory(path)
		} else {
			err = s.Backup(path)
		}
		if ErrorCode(err) != "invalid_backup_destination" {
			t.Fatalf("reserved database path accepted: %v", err)
		}
		if _, err := os.Stat(path); !os.IsNotExist(err) {
			t.Fatalf("reserved database path changed: %v", err)
		}
	}
}

func TestNoteDirectoryBackupRoundTrip(t *testing.T) {
	s := newTaskTestStore(t)
	task := mustClaimTask(t, s, mustCreateTask(t, s, CreateSpec{Title: "Journal backup"}).ID, "worker")
	body := strings.Repeat("A Unicode checkpoint: café, 日本語.\n", 12000)
	note, err := s.AddNote(task.ID, task.ClaimID, "Parser verified; integration remains", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.Subscribe(task.ID, "supervisor", true); err != nil {
		t.Fatal(err)
	}
	directory := filepath.Join(t.TempDir(), "complete")
	if err = s.BackupDirectory(directory); err != nil {
		t.Fatal(err)
	}
	restored := newTaskTestStore(t)
	if err = restored.RestoreDirectory(directory); err != nil {
		t.Fatal(err)
	}
	var actual bytes.Buffer
	if err = restored.WriteNoteBody(note.ID, &actual); err != nil || actual.String() != body {
		t.Fatalf("restored note body: %d bytes, %v", actual.Len(), err)
	}
	current, err := restored.Get(task.ID)
	if err != nil || current.Status != "pending" || current.ClaimID != "" || current.Progress != note.Title {
		t.Fatalf("restored task state: %+v %v", current, err)
	}
	var subscriptions int
	if err = restored.DB.QueryRow("SELECT COUNT(*) FROM subscriptions").Scan(&subscriptions); err != nil || subscriptions != 0 {
		t.Fatalf("operational subscriptions survived: %d %v", subscriptions, err)
	}
	if err = restored.RestoreDirectory(directory); ErrorCode(err) != "conflict" {
		t.Fatalf("restore overwrote live work: %v", err)
	}
	still, err := s.Get(task.ID)
	if err != nil || still.ClaimID != task.ClaimID {
		t.Fatalf("backup changed the source claim: %+v %v", still, err)
	}
}

func TestNoteDirectoryBackupRefusesExistingDestination(t *testing.T) {
	s := newTaskTestStore(t)
	directory := t.TempDir()
	marker := filepath.Join(directory, "keep.txt")
	if err := os.WriteFile(marker, []byte("unrelated data"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := s.BackupDirectory(directory); err == nil {
		t.Fatal("existing directory was accepted")
	}
	data, err := os.ReadFile(marker)
	if err != nil || string(data) != "unrelated data" {
		t.Fatalf("existing directory changed: %q %v", data, err)
	}
}

func TestNoteDirectoryBackupRejectsCorruptBodies(t *testing.T) {
	s := newTaskTestStore(t)
	task := mustClaimTask(t, s, mustCreateTask(t, s, CreateSpec{Title: "Corrupt source body"}).ID, "worker")
	if _, err := s.AddNote(task.ID, task.ClaimID, "Checkpoint", strings.NewReader("Evidence")); err != nil {
		t.Fatal(err)
	}
	backupTestMutate(t, s.NotesPath(), "UPDATE body_chunks SET data=zeroblob(length(data))")
	directory := filepath.Join(t.TempDir(), "backup")
	if err := s.BackupDirectory(directory); ErrorCode(err) != "invalid_backup" {
		t.Fatalf("corrupt source body was backed up: %v", err)
	}
	if _, err := os.Stat(directory); !os.IsNotExist(err) {
		t.Fatalf("failed backup left a published directory: %v", err)
	}
}

func TestNoteDirectoryRestoreRejectsIncompleteBackup(t *testing.T) {
	s := newTaskTestStore(t)
	mustCreateTask(t, s, CreateSpec{Title: "Requires a completion marker"})
	directory := t.TempDir()
	if err := s.Backup(filepath.Join(directory, "tasks.sqlite")); err != nil {
		t.Fatal(err)
	}
	target := newTaskTestStore(t)
	if err := target.RestoreDirectory(directory); ErrorCode(err) != "invalid_backup" {
		t.Fatalf("incomplete backup accepted: %v", err)
	}
	var count int
	if err := target.DB.QueryRow("SELECT COUNT(*) FROM tasks").Scan(&count); err != nil || count != 0 {
		t.Fatalf("incomplete restore changed target: %d %v", count, err)
	}
}

func TestLegacyNoteBodyMigratesDuringRestore(t *testing.T) {
	s := newTaskTestStore(t)
	task := mustClaimTask(t, s, mustCreateTask(t, s, CreateSpec{Title: "Legacy progress"}).ID, "old-worker")
	body := "Legacy checkpoint\n\n" + strings.Repeat("Detailed investigation with evidence.\n", 3000)
	n, _ := taskNumber(task.ID)
	if _, err := s.DB.Exec("UPDATE tasks SET progress=? WHERE id=?", body, n); err != nil {
		t.Fatal(err)
	}
	if _, err := s.DB.Exec("INSERT INTO events(task_id,kind,message,data,created_at) VALUES(?,'progress',?,'{}',?)", n, body, timestamp(s.Now())); err != nil {
		t.Fatal(err)
	}
	if _, err := s.DB.Exec("UPDATE store_info SET value='1' WHERE key='schema_version'"); err != nil {
		t.Fatal(err)
	}
	path := backupTestPath(t, s)
	target := newTaskTestStore(t)
	if err := target.Restore(path); err != nil {
		t.Fatal(err)
	}
	var noteID string
	if err := target.DB.QueryRow("SELECT id FROM notes WHERE task_id=? ORDER BY event_id DESC LIMIT 1", n).Scan(&noteID); err != nil {
		t.Fatal(err)
	}
	var actual bytes.Buffer
	if err := target.WriteNoteBody(noteID, &actual); err != nil || actual.String() != body {
		t.Fatalf("legacy body changed during restore: %d bytes %v", actual.Len(), err)
	}
	var message string
	if err := target.DB.QueryRow("SELECT message FROM events WHERE kind='progress' ORDER BY id DESC LIMIT 1").Scan(&message); err != nil || len(message) > 120 {
		t.Fatalf("legacy event retained its long body: %d bytes %v", len(message), err)
	}
}

func TestNoteDirectoryRestoreRejectsDamagedBodies(t *testing.T) {
	cases := map[string]func(*testing.T, string){
		"checksum": func(t *testing.T, directory string) {
			backupTestMutate(t, filepath.Join(directory, "notes.sqlite"), "UPDATE body_chunks SET data=zeroblob(length(data)) WHERE sequence=0")
		},
		"missing-chunk": func(t *testing.T, directory string) {
			backupTestMutate(t, filepath.Join(directory, "notes.sqlite"), "DELETE FROM body_chunks WHERE sequence=0")
		},
		"oversized-chunk": func(t *testing.T, directory string) {
			backupTestMutate(t, filepath.Join(directory, "notes.sqlite"), "UPDATE body_chunks SET data=zeroblob(1048576) WHERE sequence=0")
		},
		"wrong-project": func(t *testing.T, directory string) {
			backupTestMutate(t, filepath.Join(directory, "notes.sqlite"), "UPDATE body_info SET value='different-project' WHERE key='project_id'")
		},
		"missing-database": func(t *testing.T, directory string) {
			if err := os.Remove(filepath.Join(directory, "notes.sqlite")); err != nil {
				t.Fatal(err)
			}
		},
		"body-symlink": func(t *testing.T, directory string) {
			path := filepath.Join(directory, "notes.sqlite")
			real := filepath.Join(t.TempDir(), "real.sqlite")
			if err := os.Rename(path, real); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(real, path); err != nil {
				t.Fatal(err)
			}
		},
		"index-symlink": func(t *testing.T, directory string) {
			path := filepath.Join(directory, "tasks.sqlite")
			real := filepath.Join(t.TempDir(), "real.sqlite")
			if err := os.Rename(path, real); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(real, path); err != nil {
				t.Fatal(err)
			}
		},
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			s := newTaskTestStore(t)
			task := mustClaimTask(t, s, mustCreateTask(t, s, CreateSpec{Title: "Damaged backup"}).ID, "worker")
			if _, err := s.AddNote(task.ID, task.ClaimID, "Investigated", strings.NewReader(strings.Repeat("findings\n", 20000))); err != nil {
				t.Fatal(err)
			}
			directory := filepath.Join(t.TempDir(), "backup")
			if err := s.BackupDirectory(directory); err != nil {
				t.Fatal(err)
			}
			mutate(t, directory)
			target := newTaskTestStore(t)
			if err := target.RestoreDirectory(directory); ErrorCode(err) != "invalid_backup" {
				t.Fatalf("invalid body accepted: %v", err)
			}
			for _, table := range []string{"tasks", "events", "notes"} {
				var count int
				if err := target.DB.QueryRow("SELECT COUNT(*) FROM " + table).Scan(&count); err != nil || count != 0 {
					t.Fatalf("failed restore changed %s: %d %v", table, count, err)
				}
			}
		})
	}
}

func TestNoteBodyRestoreIgnoresSourceTriggers(t *testing.T) {
	s := newTaskTestStore(t)
	task := mustClaimTask(t, s, mustCreateTask(t, s, CreateSpec{Title: "Untrusted source schema"}).ID, "worker")
	note, err := s.AddNote(task.ID, task.ClaimID, "Checkpoint", strings.NewReader("Preserved details"))
	if err != nil {
		t.Fatal(err)
	}
	directory := filepath.Join(t.TempDir(), "backup")
	if err := s.BackupDirectory(directory); err != nil {
		t.Fatal(err)
	}
	backupTestMutate(t, filepath.Join(directory, "notes.sqlite"), `CREATE TRIGGER injected BEFORE INSERT ON bodies BEGIN SELECT RAISE(ABORT,'do not import this trigger'); END`)
	target := newTaskTestStore(t)
	if err := target.RestoreDirectory(directory); err != nil {
		t.Fatal(err)
	}
	var body bytes.Buffer
	if err := target.WriteNoteBody(note.ID, &body); err != nil || body.String() != "Preserved details" {
		t.Fatalf("body changed: %q %v", body.String(), err)
	}
	db, err := target.openBodies(false)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var trigger bool
	if err := db.QueryRow("SELECT EXISTS(SELECT 1 FROM sqlite_schema WHERE type='trigger')").Scan(&trigger); err != nil || trigger {
		t.Fatalf("source trigger was imported: %t %v", trigger, err)
	}
}

func TestTitleOnlyAndEmptyNoteBodiesRestore(t *testing.T) {
	for _, withEmptyBody := range []bool{false, true} {
		t.Run(map[bool]string{false: "title-only", true: "empty-body"}[withEmptyBody], func(t *testing.T) {
			s := newTaskTestStore(t)
			task := mustClaimTask(t, s, mustCreateTask(t, s, CreateSpec{Title: "Empty body"}).ID, "worker")
			var note Note
			var err error
			if withEmptyBody {
				note, err = s.AddNote(task.ID, task.ClaimID, "A useful short update", strings.NewReader(""))
			} else {
				note, err = s.AddNote(task.ID, task.ClaimID, "A useful short update", nil)
			}
			if err != nil {
				t.Fatal(err)
			}
			target := newTaskTestStore(t)
			if withEmptyBody {
				directory := filepath.Join(t.TempDir(), "backup")
				if err = s.BackupDirectory(directory); err == nil {
					err = target.RestoreDirectory(directory)
				}
			} else {
				err = target.Restore(backupTestPath(t, s))
			}
			if err != nil {
				t.Fatal(err)
			}
			actual, err := target.GetNote(note.ID)
			if err != nil || actual.Title != note.Title || actual.SHA256 != note.SHA256 {
				t.Fatalf("note metadata changed: %+v %v", actual, err)
			}
			var body bytes.Buffer
			if err := target.WriteNoteBody(note.ID, &body); err != nil || body.Len() != 0 {
				t.Fatalf("empty body changed: %d bytes %v", body.Len(), err)
			}
		})
	}
}
