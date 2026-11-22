package store

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"
)

type patternReader struct {
	remaining int64
	maxRead   int
}

func (r *patternReader) Read(p []byte) (int, error) {
	if len(p) > r.maxRead {
		r.maxRead = len(p)
	}
	if r.remaining == 0 {
		return 0, io.EOF
	}
	n := int(min(int64(len(p)), r.remaining))
	for i := 0; i < n; i++ {
		p[i] = 'x'
	}
	r.remaining -= int64(n)
	return n, nil
}

func TestNoteBodyStreamingAndCompactPrimary(t *testing.T) {
	s := newTaskTestStore(t)
	task := mustClaimTask(t, s, mustCreateTask(t, s, CreateSpec{Title: "Large evidence"}).ID, "worker")
	const size = 16*1024*1024 + 17
	reader := &patternReader{remaining: size}
	note, err := s.AddNote(task.ID, task.ClaimID, "Verified the parser", reader)
	if err != nil {
		t.Fatal(err)
	}
	if reader.maxRead > noteChunkBytes || note.BodyBytes != size {
		t.Fatalf("unbounded reader or incorrect size: %d %+v", reader.maxRead, note)
	}
	hash := sha256.New()
	if err := s.WriteNoteBody(note.ID, hash); err != nil || hex.EncodeToString(hash.Sum(nil)) != note.SHA256 {
		t.Fatalf("streamed digest mismatch: %v", err)
	}
	var pages, pageSize, textBytes int64
	if err := s.DB.QueryRow("PRAGMA page_count").Scan(&pages); err != nil {
		t.Fatal(err)
	}
	if err := s.DB.QueryRow("PRAGMA page_size").Scan(&pageSize); err != nil {
		t.Fatal(err)
	}
	if err := s.DB.QueryRow("SELECT octet_length(message) FROM events WHERE id=?", note.EventID).Scan(&textBytes); err != nil {
		t.Fatal(err)
	}
	if pages*pageSize > 1024*1024 || textBytes != int64(len(note.Title)) {
		t.Fatalf("body leaked into coordination store: %d bytes, event %d bytes", pages*pageSize, textBytes)
	}
}

type failedNoteReader struct{ once bool }

func (r *failedNoteReader) Read(p []byte) (int, error) {
	if r.once {
		return 0, io.ErrClosedPipe
	}
	r.once = true
	return copy(p, bytes.Repeat([]byte{'x'}, noteChunkBytes)), nil
}

func TestNoteReadFailureDoesNotPublish(t *testing.T) {
	s := newTaskTestStore(t)
	task := mustClaimTask(t, s, mustCreateTask(t, s, CreateSpec{Title: "Failed input"}).ID, "worker")
	before := coreEventCount(t, s)
	if _, err := s.AddNote(task.ID, task.ClaimID, "Never published", &failedNoteReader{}); !errors.Is(err, io.ErrClosedPipe) {
		t.Fatalf("input error lost: %v", err)
	}
	current, err := s.Get(task.ID)
	if err != nil || !reflect.DeepEqual(current, task) || coreEventCount(t, s) != before {
		t.Fatalf("failed input changed task/events: %+v %v", current, err)
	}
	db, err := s.openBodies(false)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var count int
	if err := db.QueryRow("SELECT COUNT(*) FROM bodies").Scan(&count); err != nil || count != 0 {
		t.Fatalf("partial body remained: %d %v", count, err)
	}
}

type pausedNoteReader struct {
	ready, proceed chan struct{}
	once           sync.Once
}

func (r *pausedNoteReader) Read(p []byte) (int, error) {
	r.once.Do(func() { close(r.ready); <-r.proceed })
	return 0, io.EOF
}

func TestNotePublishRechecksClaimAfterStreaming(t *testing.T) {
	s := newTaskTestStore(t)
	task := mustClaimTask(t, s, mustCreateTask(t, s, CreateSpec{Title: "Claim changed during input"}).ID, "old-worker")
	reader := &pausedNoteReader{ready: make(chan struct{}), proceed: make(chan struct{})}
	result := make(chan error, 1)
	go func() {
		_, err := s.AddNote(task.ID, task.ClaimID, "Stale writer", reader)
		result <- err
	}()
	select {
	case <-reader.ready:
	case <-time.After(10 * time.Second):
		t.Fatal("body writer did not start")
	}
	current, err := s.Takeover(task.ID, task.ClaimID, "new-worker", "The worker stopped")
	close(reader.proceed)
	if err != nil {
		t.Fatal(err)
	}
	if err := <-result; ErrorCode(err) != "conflict" {
		t.Fatalf("stale body published: %v", err)
	}
	actual, err := s.Get(task.ID)
	if err != nil || actual.ClaimID != current.ClaimID || actual.Progress != "" {
		t.Fatalf("stale writer changed current task: %+v %v", actual, err)
	}
	db, err := s.openBodies(false)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var count int
	if err := db.QueryRow("SELECT COUNT(*) FROM bodies").Scan(&count); err != nil || count != 0 {
		t.Fatalf("rejected body's cleanup failed: %d %v", count, err)
	}
}

func TestNoteTitleAndMissingBodyValidation(t *testing.T) {
	s := newTaskTestStore(t)
	task := mustClaimTask(t, s, mustCreateTask(t, s, CreateSpec{Title: "Validation"}).ID, "worker")
	for _, title := range []string{"", "x\ny", "x\u2028y", "x\u2029y", strings.Repeat("界", 121)} {
		if _, err := s.AddNote(task.ID, task.ClaimID, title, nil); ErrorCode(err) != "invalid_note_title" {
			t.Fatalf("invalid title accepted %q: %v", title, err)
		}
	}
	if _, err := s.AddNote(task.ID, task.ClaimID, strings.Repeat("界", 120), nil); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(s.NotesPath()); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("title-only note opened body store: %v", err)
	}
	note, err := s.AddNote(task.ID, task.ClaimID, "Has a body", strings.NewReader("body"))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(s.NotesPath()); err != nil {
		t.Fatal(err)
	}
	if err := s.WriteNoteBody(note.ID, io.Discard); ErrorCode(err) != "storage_missing" {
		t.Fatalf("missing body was hidden: %v", err)
	}
	if _, err := s.AddNote(task.ID, task.ClaimID, "Would mask missing data", strings.NewReader("new")); ErrorCode(err) != "storage_missing" {
		t.Fatalf("missing referenced store silently recreated: %v", err)
	}
}

func TestLegacyNoteUpgradePreservesHistoryAndCurrentText(t *testing.T) {
	s := newTaskTestStore(t)
	task := mustClaimTask(t, s, mustCreateTask(t, s, CreateSpec{Title: "Legacy notes"}).ID, "old-worker")
	want := []string{"First short checkpoint", "Second checkpoint\n\n" + strings.Repeat("Evidence λ.\n", 200000), "Current text never copied to an event\nOriginal details."}
	for _, body := range want[:2] {
		if _, err := s.DB.Exec("INSERT INTO events(task_id,kind,message,data,created_at) VALUES(1,'progress',?,'null',?)", body, timestamp(s.Now())); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := s.DB.Exec("UPDATE tasks SET progress=? WHERE id=1", want[2]); err != nil {
		t.Fatal(err)
	}
	if _, err := s.DB.Exec("DROP TABLE notes; UPDATE store_info SET value='1' WHERE key='schema_version'"); err != nil {
		t.Fatal(err)
	}
	path := s.databasePath
	s.Close()
	upgraded, err := OpenExisting(path, s.ProjectID)
	if err != nil {
		t.Fatal(err)
	}
	defer upgraded.Close()
	notes, more, err := upgraded.NotesPage(task.ID, 20, 0)
	if err != nil || more || len(notes) != len(want) {
		t.Fatalf("migration lost note history: %+v %t %v", notes, more, err)
	}
	for i, original := range want {
		note := notes[len(notes)-1-i]
		if i == 0 {
			if note.Title != original || note.SHA256 != "" {
				t.Fatalf("short legacy note changed: %+v", note)
			}
			continue
		}
		var actual bytes.Buffer
		if err := upgraded.WriteNoteBody(note.ID, &actual); err != nil || actual.String() != original {
			t.Fatalf("legacy body %d differs: %v", i, err)
		}
	}
	current, err := upgraded.Get(task.ID)
	if err != nil || current.Progress != legacyNoteTitle(want[2]) || current.ClaimID != task.ClaimID {
		t.Fatalf("upgrade changed claim/current text: %+v %v", current, err)
	}
	upgraded.Close()
	again, err := OpenExisting(path, s.ProjectID)
	if err != nil {
		t.Fatal(err)
	}
	defer again.Close()
	var count int
	if err := again.DB.QueryRow("SELECT COUNT(*) FROM notes").Scan(&count); err != nil || count != len(want) {
		t.Fatalf("reopen repeated migration: %d %v", count, err)
	}
}

func TestNoteConcurrentWritersAcrossConnections(t *testing.T) {
	s := newTaskTestStore(t)
	task := mustClaimTask(t, s, mustCreateTask(t, s, CreateSpec{Title: "Concurrent notes"}).ID, "worker")
	const count = 8
	start := make(chan struct{})
	results := make(chan error, count)
	for i := 0; i < count; i++ {
		peer, err := OpenExisting(s.databasePath, s.ProjectID)
		if err != nil {
			t.Fatal(err)
		}
		go func(i int) {
			defer peer.Close()
			<-start
			_, err := peer.AddNote(task.ID, task.ClaimID, fmt.Sprintf("Checkpoint %d", i), &patternReader{remaining: 200000})
			results <- err
		}(i)
	}
	close(start)
	for i := 0; i < count; i++ {
		if err := <-results; err != nil {
			t.Fatal(err)
		}
	}
	notes, more, err := s.NotesPage(task.ID, 20, 0)
	if err != nil || more || len(notes) != count {
		t.Fatalf("concurrent publication lost notes: %d %t %v", len(notes), more, err)
	}
	for _, note := range notes {
		if err := s.WriteNoteBody(note.ID, io.Discard); err != nil {
			t.Fatal(err)
		}
	}
	current, err := s.Get(task.ID)
	if err != nil || current.Progress != notes[0].Title || current.ProgressNoteID != notes[0].ID {
		t.Fatalf("latest task context is not the newest committed note: %+v %v", current, err)
	}
}

func BenchmarkNoteBodyStreaming(b *testing.B) {
	for _, size := range []int64{1024, 1024 * 1024, 32 * 1024 * 1024} {
		b.Run(fmt.Sprint(size), func(b *testing.B) {
			s, err := Open(filepath.Join(b.TempDir(), "tasks.sqlite"), "body-bench")
			if err != nil {
				b.Fatal(err)
			}
			defer s.Close()
			task, err := s.Create(CreateSpec{Title: "Streaming body"})
			if err != nil {
				b.Fatal(err)
			}
			task, err = s.Claim(task.ID, "bench")
			if err != nil {
				b.Fatal(err)
			}
			b.SetBytes(size)
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				note, err := s.AddNote(task.ID, task.ClaimID, "Checkpoint", &patternReader{remaining: size})
				if err != nil {
					b.Fatal(err)
				}
				if err := s.WriteNoteBody(note.ID, io.Discard); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}
