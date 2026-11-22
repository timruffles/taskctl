package store

import (
	"bytes"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"testing/iotest"
)

func TestMissingBodyDatabaseCannotBeRecreatedOverPublishedNotes(t *testing.T) {
	s := newTaskTestStore(t)
	task := mustClaimTask(t, s, mustCreateTask(t, s, CreateSpec{Title: "Missing bodies"}).ID, "worker")
	note, err := s.AddNote(task.ID, task.ClaimID, "Saved checkpoint", strings.NewReader("Original evidence"))
	if err != nil {
		t.Fatal(err)
	}
	path := s.NotesPath()
	if err := os.Rename(path, path+".saved"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.AddNote(task.ID, task.ClaimID, "New checkpoint", strings.NewReader("New evidence")); ErrorCode(err) != "storage_missing" {
		t.Fatalf("missing database was recreated: %v", err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("missing database path changed: %v", err)
	}
	if err := os.Rename(path+".saved", path); err != nil {
		t.Fatal(err)
	}
	var body bytes.Buffer
	if err := s.WriteNoteBody(note.ID, &body); err != nil || body.String() != "Original evidence" {
		t.Fatalf("original body changed: %q %v", body.String(), err)
	}
	notes, _, err := s.NotesPage(task.ID, 20, 0)
	if err != nil || len(notes) != 1 {
		t.Fatalf("failed write published a note: %+v %v", notes, err)
	}
}

func TestPublishedNoteBodyCannotBeOverwritten(t *testing.T) {
	s := newTaskTestStore(t)
	task := mustClaimTask(t, s, mustCreateTask(t, s, CreateSpec{Title: "Immutable bodies"}).ID, "worker")
	note, err := s.AddNote(task.ID, task.ClaimID, "Saved checkpoint", strings.NewReader("Original evidence"))
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.putBody(note.ID, strings.NewReader("Replacement evidence")); err == nil {
		t.Fatal("duplicate body ID replaced published content")
	}
	var body bytes.Buffer
	if err := s.WriteNoteBody(note.ID, &body); err != nil || body.String() != "Original evidence" {
		t.Fatalf("original body changed: %q %v", body.String(), err)
	}
}

func TestNoteBodyBatchFailureRollsBackAndRetries(t *testing.T) {
	s := newTaskTestStore(t)
	first, second := newID("N"), newID("N")
	writer := s.newBodyWriter(true)
	if _, _, err := writer.Put(first, strings.NewReader("First body")); err != nil {
		writer.Close()
		t.Fatal(err)
	}
	readErr := errors.New("interrupted input")
	input := io.MultiReader(strings.NewReader(strings.Repeat("x", noteChunkBytes+17)), iotest.ErrReader(readErr))
	if _, _, err := writer.Put(second, input); !errors.Is(err, readErr) {
		writer.Close()
		t.Fatalf("input failure not returned: %v", err)
	}
	if err := writer.Commit(); !errors.Is(err, readErr) {
		writer.Close()
		t.Fatalf("failed batch was committed: %v", err)
	}
	writer.Close()
	db, err := s.openBodies(false)
	if err != nil {
		t.Fatal(err)
	}
	var count int
	err = db.QueryRow("SELECT COUNT(*) FROM bodies").Scan(&count)
	db.Close()
	if err != nil || count != 0 {
		t.Fatalf("partial body batch survived: %d %v", count, err)
	}
	if _, _, err := s.putBody(first, strings.NewReader("Retried body")); err != nil {
		t.Fatalf("failed first write blocked retry: %v", err)
	}
}

type noteShortWriter struct{}

func (noteShortWriter) Write(p []byte) (int, error) { return len(p) - 1, nil }

func TestNoteBodyReportsShortOutputWrite(t *testing.T) {
	s := newTaskTestStore(t)
	task := mustClaimTask(t, s, mustCreateTask(t, s, CreateSpec{Title: "Output interrupted"}).ID, "worker")
	note, err := s.AddNote(task.ID, task.ClaimID, "Checkpoint", strings.NewReader("Evidence"))
	if err != nil {
		t.Fatal(err)
	}
	if err := s.WriteNoteBody(note.ID, noteShortWriter{}); !errors.Is(err, io.ErrShortWrite) {
		t.Fatalf("short output was reported as success: %v", err)
	}
}

func TestNoteBodyRejectsOversizedChunkBeforeOutput(t *testing.T) {
	s := newTaskTestStore(t)
	task := mustClaimTask(t, s, mustCreateTask(t, s, CreateSpec{Title: "Damaged chunk"}).ID, "worker")
	note, err := s.AddNote(task.ID, task.ClaimID, "Checkpoint", strings.NewReader("Evidence"))
	if err != nil {
		t.Fatal(err)
	}
	backupTestMutate(t, s.NotesPath(), "UPDATE body_chunks SET data=zeroblob(1048576)")
	var body bytes.Buffer
	if err := s.WriteNoteBody(note.ID, &body); ErrorCode(err) != "storage_invalid" || body.Len() != 0 {
		t.Fatalf("oversized chunk was read: %d bytes %v", body.Len(), err)
	}
}

func TestBodyInitializationNeverOverwritesExistingFile(t *testing.T) {
	s := newTaskTestStore(t)
	path := s.NotesPath()
	if err := os.WriteFile(path, []byte("Existing data"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := s.initializeBodies(path); err == nil {
		t.Fatal("body database initialization replaced an existing file")
	}
	data, err := os.ReadFile(path)
	if err != nil || string(data) != "Existing data" {
		t.Fatalf("existing file changed: %q %v", data, err)
	}
	matches, err := filepath.Glob(filepath.Join(filepath.Dir(path), ".taskctl-notes-*"))
	if err != nil || len(matches) != 0 {
		t.Fatalf("failed publication left temporary files: %v %v", matches, err)
	}
}
