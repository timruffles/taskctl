//go:build unix

package store

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

func noteCrashBarrier() error {
	if _, err := fmt.Fprintln(os.Stdout, "ready"); err != nil {
		return err
	}
	var release [1]byte
	_, err := io.ReadFull(os.Stdin, release[:])
	if err != nil {
		return err
	}
	return errors.New("crash barrier unexpectedly released")
}

type noteCrashReader struct{ remaining int }

func (r *noteCrashReader) Read(p []byte) (int, error) {
	if r.remaining == 0 {
		// The next chunk is requested only after the first INSERT succeeds.
		return 0, noteCrashBarrier()
	}
	n := min(len(p), r.remaining)
	for i := range p[:n] {
		p[i] = 'x'
	}
	r.remaining -= n
	return n, nil
}

func TestNoteCrashProcessHelper(t *testing.T) {
	mode := os.Getenv("TASKCTL_NOTE_CRASH_MODE")
	if mode == "" {
		t.Skip("subprocess helper")
	}
	s, err := OpenExisting(os.Getenv("TASKCTL_NOTE_CRASH_DB"), os.Getenv("TASKCTL_NOTE_CRASH_PROJECT"))
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	task, claim, note := os.Getenv("TASKCTL_NOTE_CRASH_TASK"), os.Getenv("TASKCTL_NOTE_CRASH_CLAIM"), os.Getenv("TASKCTL_NOTE_CRASH_NOTE")
	switch mode {
	case "body-transaction":
		_, err = s.AddNote(task, claim, "Uncommitted note", &noteCrashReader{remaining: noteChunkBytes})
	case "body-committed":
		var unlock func()
		unlock, err = s.noteLock(false)
		if err == nil {
			defer unlock()
			_, _, err = s.putBody(note, strings.NewReader(strings.Repeat("orphan body\n", 10000)))
		}
		if err == nil {
			err = noteCrashBarrier()
		}
	case "delete-body-locked":
		if _, err = fmt.Fprintln(os.Stdout, "ready"); err == nil {
			err = s.DeleteNote(note, claim)
		}
	default:
		t.Fatalf("unknown crash mode %q", mode)
	}
	t.Fatalf("crash child unexpectedly returned: %v", err)
}

type noteCrashProcess struct {
	cmd    *exec.Cmd
	stdin  io.WriteCloser
	done   chan struct{}
	err    error
	stderr bytes.Buffer
	cancel context.CancelFunc
}

func startNoteCrashProcess(t *testing.T, s *Store, task Task, mode, note string) *noteCrashProcess {
	t.Helper()
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	p := &noteCrashProcess{
		cmd:    exec.CommandContext(ctx, executable, "-test.run=^TestNoteCrashProcessHelper$", "-test.count=1"),
		done:   make(chan struct{}),
		cancel: cancel,
	}
	p.cmd.Env = append(os.Environ(),
		"TASKCTL_NOTE_CRASH_MODE="+mode,
		"TASKCTL_NOTE_CRASH_DB="+s.databasePath,
		"TASKCTL_NOTE_CRASH_PROJECT="+s.ProjectID,
		"TASKCTL_NOTE_CRASH_TASK="+task.ID,
		"TASKCTL_NOTE_CRASH_CLAIM="+task.ClaimID,
		"TASKCTL_NOTE_CRASH_NOTE="+note,
	)
	p.stdin, err = p.cmd.StdinPipe()
	if err != nil {
		cancel()
		t.Fatal(err)
	}
	stdout, err := p.cmd.StdoutPipe()
	if err != nil {
		p.stdin.Close()
		cancel()
		t.Fatal(err)
	}
	p.cmd.Stderr = &p.stderr
	if err = p.cmd.Start(); err != nil {
		p.stdin.Close()
		cancel()
		t.Fatal(err)
	}
	go func() {
		p.err = p.cmd.Wait()
		close(p.done)
	}()
	t.Cleanup(func() {
		_ = p.cmd.Process.Kill()
		_ = p.stdin.Close()
		cancel()
		select {
		case <-p.done:
		case <-time.After(5 * time.Second):
			t.Error("crash child did not exit during cleanup")
		}
	})
	ready := make(chan string, 1)
	go func() {
		line, _ := bufio.NewReader(stdout).ReadString('\n')
		ready <- strings.TrimSpace(line)
	}()
	select {
	case line := <-ready:
		if line != "ready" {
			p.kill(t)
			t.Fatalf("child did not reach %s barrier: %q; %s", mode, line, p.stderr.String())
		}
	case <-p.done:
		t.Fatalf("child exited before %s barrier: %v; %s", mode, p.err, p.stderr.String())
	case <-ctx.Done():
		t.Fatalf("child timed out before %s barrier", mode)
	}
	return p
}

func (p *noteCrashProcess) kill(t *testing.T) {
	t.Helper()
	if err := p.cmd.Process.Kill(); err != nil {
		t.Fatalf("kill crash child: %v", err)
	}
	select {
	case <-p.done:
	case <-time.After(5 * time.Second):
		t.Fatal("killed child did not exit")
	}
	p.cancel()
	var exited *exec.ExitError
	if !errors.As(p.err, &exited) {
		t.Fatalf("child did not die from SIGKILL: %v; %s", p.err, p.stderr.String())
	}
	status, ok := exited.Sys().(syscall.WaitStatus)
	if !ok || !status.Signaled() || status.Signal() != syscall.SIGKILL {
		t.Fatalf("child exited without SIGKILL: %v; %s", p.err, p.stderr.String())
	}
}

func reopenNoteCrashStore(t *testing.T, s *Store) *Store {
	t.Helper()
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := OpenExisting(s.databasePath, s.ProjectID)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { reopened.Close() })
	return reopened
}

func assertNoteCrashBodyCount(t *testing.T, s *Store, id string, want int) {
	t.Helper()
	db, err := s.openBodies(false)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var bodies, chunks int
	if err = db.QueryRow("SELECT COUNT(*) FROM bodies WHERE id=?", id).Scan(&bodies); err != nil {
		t.Fatal(err)
	}
	if err = db.QueryRow("SELECT COUNT(*) FROM body_chunks WHERE note_id=?", id).Scan(&chunks); err != nil {
		t.Fatal(err)
	}
	if bodies != want || (want == 0 && chunks != 0) || (want > 0 && chunks == 0) {
		t.Fatalf("body rows for %s: bodies=%d chunks=%d; want bodies=%d", id, bodies, chunks, want)
	}
}

func TestNoteCrashDuringBodyTransaction(t *testing.T) {
	s := newTaskTestStore(t)
	task := mustClaimTask(t, s, mustCreateTask(t, s, CreateSpec{Title: "Interrupted body write"}).ID, "worker")
	before := coreEventCount(t, s)
	child := startNoteCrashProcess(t, s, task, "body-transaction", "")
	child.kill(t)
	s = reopenNoteCrashStore(t, s)
	page, more, err := s.NotesPage(task.ID, 20, 0)
	if err != nil || more || len(page) != 0 || coreEventCount(t, s) != before {
		t.Fatalf("uncommitted note became visible: %+v %v %v", page, more, err)
	}
	current, err := s.Get(task.ID)
	if err != nil || current.Progress != "" || current.Revision != task.Revision || current.ClaimID != task.ClaimID {
		t.Fatalf("uncommitted body changed the task: %+v %v", current, err)
	}
	db, err := s.openBodies(false)
	if err != nil {
		t.Fatal(err)
	}
	var bodies, chunks int
	err = db.QueryRow("SELECT (SELECT COUNT(*) FROM bodies),(SELECT COUNT(*) FROM body_chunks)").Scan(&bodies, &chunks)
	db.Close()
	if err != nil || bodies != 0 || chunks != 0 {
		t.Fatalf("partial body survived transaction kill: bodies=%d chunks=%d err=%v", bodies, chunks, err)
	}
	body := "Recovered after the interrupted writer\n"
	note, err := s.AddNote(task.ID, task.ClaimID, "Recovery works", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	var actual bytes.Buffer
	if err = s.WriteNoteBody(note.ID, &actual); err != nil || actual.String() != body {
		t.Fatalf("post-crash note body: %q %v", actual.String(), err)
	}
	chaosIntegrity(t, s)
}

func TestNoteCrashAfterBodyCommit(t *testing.T) {
	s := newTaskTestStore(t)
	task := mustClaimTask(t, s, mustCreateTask(t, s, CreateSpec{Title: "Interrupted publication"}).ID, "worker")
	orphan := newID("N")
	before := coreEventCount(t, s)
	child := startNoteCrashProcess(t, s, task, "body-committed", orphan)
	child.kill(t)
	s = reopenNoteCrashStore(t, s)
	if _, err := s.GetNote(orphan); ErrorCode(err) != "not_found" {
		t.Fatalf("orphan body was published: %v", err)
	}
	if coreEventCount(t, s) != before {
		t.Fatal("orphan body changed event history")
	}
	current, err := s.Get(task.ID)
	if err != nil || current.Progress != "" || current.Revision != task.Revision || current.ClaimID != task.ClaimID {
		t.Fatalf("orphan body changed the task: %+v %v", current, err)
	}
	assertNoteCrashBodyCount(t, s, orphan, 1)
	directory := filepath.Join(t.TempDir(), "orphan-only")
	if err := s.BackupDirectory(directory); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(directory, "notes.sqlite")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("orphan-only backup included body storage: %v", err)
	}
	empty := newTaskTestStore(t)
	if err := empty.RestoreDirectory(directory); err != nil {
		t.Fatal(err)
	}
	if _, err := empty.GetNote(orphan); ErrorCode(err) != "not_found" {
		t.Fatalf("orphan became a restored note: %v", err)
	}
	body := "A committed note after the crash\n"
	valid, err := s.AddNote(task.ID, task.ClaimID, "Publication recovered", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	directory = filepath.Join(t.TempDir(), "with-live-note")
	if err = s.BackupDirectory(directory); err != nil {
		t.Fatal(err)
	}
	restored := newTaskTestStore(t)
	if err = restored.RestoreDirectory(directory); err != nil {
		t.Fatal(err)
	}
	assertNoteCrashBodyCount(t, restored, orphan, 0)
	assertNoteCrashBodyCount(t, restored, valid.ID, 1)
	var actual bytes.Buffer
	if err = restored.WriteNoteBody(valid.ID, &actual); err != nil || actual.String() != body {
		t.Fatalf("restored post-crash note: %q %v", actual.String(), err)
	}
	chaosIntegrity(t, restored)
}

func TestNoteCrashAfterDeletionTombstone(t *testing.T) {
	s := newTaskTestStore(t)
	task := mustClaimTask(t, s, mustCreateTask(t, s, CreateSpec{Title: "Interrupted note deletion"}).ID, "worker")
	old, err := s.AddNote(task.ID, task.ClaimID, "Older progress", strings.NewReader("Earlier details\n"))
	if err != nil {
		t.Fatal(err)
	}
	latest, err := s.AddNote(task.ID, task.ClaimID, "Current progress", strings.NewReader(strings.Repeat("Current details\n", 10000)))
	if err != nil {
		t.Fatal(err)
	}
	db, err := s.openBodies(false)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	locked, err := db.BeginTx(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	defer locked.Rollback()
	child := startNoteCrashProcess(t, s, task, "delete-body-locked", latest.ID)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	tick := time.NewTicker(time.Millisecond)
	defer tick.Stop()
	var deleted string
	for {
		if err = s.DB.QueryRowContext(ctx, "SELECT deleted_at FROM notes WHERE id=?", latest.ID).Scan(&deleted); err != nil {
			t.Fatal(err)
		}
		if deleted != "" {
			break
		}
		select {
		case <-child.done:
			t.Fatalf("delete exited before its tombstone: %v; %s", child.err, child.stderr.String())
		case <-ctx.Done():
			t.Fatal("delete did not commit its tombstone while body storage was locked")
		case <-tick.C:
		}
	}
	var stillPresent int
	if err = locked.QueryRow("SELECT COUNT(*) FROM bodies WHERE id=?", latest.ID).Scan(&stillPresent); err != nil || stillPresent != 1 {
		t.Fatalf("body disappeared while its database was write-locked: %d %v", stillPresent, err)
	}
	child.kill(t)
	if err = locked.Rollback(); err != nil {
		t.Fatal(err)
	}
	s = reopenNoteCrashStore(t, s)
	current, err := s.Get(task.ID)
	if err != nil || current.Progress != "" {
		t.Fatalf("deletion resurrected old progress: %+v %v", current, err)
	}
	assertNoteCrashBodyCount(t, s, latest.ID, 1)
	if err = s.WriteNoteBody(latest.ID, io.Discard); ErrorCode(err) != "note_deleted" {
		t.Fatalf("tombstoned body remained readable: %v", err)
	}
	if err = s.DeleteNote(latest.ID, task.ClaimID); err != nil {
		t.Fatal(err)
	}
	assertNoteCrashBodyCount(t, s, latest.ID, 0)
	assertNoteCrashBodyCount(t, s, old.ID, 1)
	current, err = s.Get(task.ID)
	if err != nil || current.Progress != "" {
		t.Fatalf("retry resurrected old progress: %+v %v", current, err)
	}
	var deletions int
	if err = s.DB.QueryRow("SELECT COUNT(*) FROM events WHERE kind='note_deleted'").Scan(&deletions); err != nil || deletions != 1 {
		t.Fatalf("delete retry duplicated its event: %d %v", deletions, err)
	}
	chaosIntegrity(t, s)
}
