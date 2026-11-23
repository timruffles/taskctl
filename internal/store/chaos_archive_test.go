package store

import (
	"bufio"
	"bytes"
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"fmt"
	"io"
	"math/rand"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"modernc.org/sqlite"
)

func archiveChaosIntegrity(t *testing.T, s *Store) {
	t.Helper()
	var check string
	if err := s.DB.QueryRow("PRAGMA integrity_check").Scan(&check); err != nil || check != "ok" {
		t.Fatalf("integrity: %q %v", check, err)
	}
	rows, err := s.DB.Query("PRAGMA foreign_key_check")
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	if rows.Next() {
		t.Fatal("foreign key violation")
	}
	if err = rows.Err(); err != nil {
		t.Fatal(err)
	}
}

func TestChaosArchiveRejectsUnreadableRestores(t *testing.T) {
	source := newTaskTestStore(t)
	mustCreateTask(t, source, CreateSpec{Title: "Preserve readable history"})
	cases := map[string]string{
		"broken-event-json":  "UPDATE events SET data='{' WHERE id=1",
		"empty-event-kind":   "UPDATE events SET kind='' WHERE id=1",
		"zero-event-id":      "UPDATE events SET id=0 WHERE id=1",
		"negative-event-id":  "UPDATE events SET id=-1 WHERE id=1",
		"invalid-event-time": "UPDATE events SET created_at='yesterday' WHERE id=1",
		"text-revision":      "UPDATE tasks SET revision='true' WHERE id=1",
		"blob-title":         "UPDATE tasks SET title=x'3432' WHERE id=1",
		"zero-task-id":       "UPDATE tasks SET id=0,origin_id=0 WHERE id=1; UPDATE events SET task_id=0 WHERE task_id=1",
	}
	for name, query := range cases {
		t.Run(name, func(t *testing.T) {
			path := backupTestPath(t, source)
			backupTestMutate(t, path, query)
			target := newTaskTestStore(t)
			if err := target.Restore(path); err == nil {
				t.Fatal("malformed backup restored successfully")
			}
			for _, table := range archiveTables {
				var n int
				if err := target.DB.QueryRow("SELECT COUNT(*) FROM " + table).Scan(&n); err != nil || n != 0 {
					t.Fatalf("partial restore in %s: count=%d err=%v", table, n, err)
				}
			}
			archiveChaosIntegrity(t, target)
		})
	}
}

func TestChaosSQLiteFullRollsBackAndRecovers(t *testing.T) {
	s := newTaskTestStore(t)
	task := mustClaimTask(t, s, mustCreateTask(t, s, CreateSpec{Title: "Disk quota test"}).ID, "worker")
	before, err := s.Get(task.ID)
	if err != nil {
		t.Fatal(err)
	}
	var beforeEvents int
	if err = s.DB.QueryRow("SELECT COUNT(*) FROM events").Scan(&beforeEvents); err != nil {
		t.Fatal(err)
	}
	var pages int64
	if err = s.DB.QueryRow("PRAGMA page_count").Scan(&pages); err != nil {
		t.Fatal(err)
	}
	if _, err = s.DB.Exec(fmt.Sprintf("PRAGMA max_page_count=%d", pages)); err != nil {
		t.Fatal(err)
	}
	_, err = s.Complete(task.ID, task.ClaimID, strings.Repeat("x", 1024*1024))
	if ErrorCode(err) != "storage_full" {
		t.Fatalf("expected bounded SQLite quota fault, got %v", err)
	}
	after, err := s.Get(task.ID)
	if err != nil {
		t.Fatal(err)
	}
	var afterEvents int
	if err = s.DB.QueryRow("SELECT COUNT(*) FROM events").Scan(&afterEvents); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(before, after) || beforeEvents != afterEvents {
		t.Fatal("SQLITE_FULL left partial task/event changes")
	}
	if _, err = s.DB.Exec("PRAGMA max_page_count=4294967294"); err != nil {
		t.Fatal(err)
	}
	if _, err = s.Progress(task.ID, task.ClaimID, "Recovered after quota was lifted"); err != nil {
		t.Fatal(err)
	}
	archiveChaosIntegrity(t, s)
}

func TestChaosArchiveStorageFaultsRemainOperational(t *testing.T) {
	source := newTaskTestStore(t)
	mustCreateTask(t, source, CreateSpec{Title: "Restore quota", Brief: strings.Repeat("x", 1024*1024)})
	path := backupTestPath(t, source)
	var err error
	target := newTaskTestStore(t)
	var pages int64
	if err = target.DB.QueryRow("PRAGMA page_count").Scan(&pages); err != nil {
		t.Fatal(err)
	}
	if _, err = target.DB.Exec(fmt.Sprintf("PRAGMA max_page_count=%d", pages)); err != nil {
		t.Fatal(err)
	}
	if err = target.Restore(path); ErrorCode(err) != "storage_full" {
		t.Fatalf("quota failure mislabeled: %v", err)
	}
	_, count, err := target.List("all", nil, nil, 20, 0)
	if err != nil || count != 0 {
		t.Fatalf("quota failure left partial restore: %d %v", count, err)
	}
	if _, err = target.DB.Exec("PRAGMA max_page_count=4294967294"); err != nil {
		t.Fatal(err)
	}
	if _, err = target.DB.Exec("PRAGMA query_only=ON"); err != nil {
		t.Fatal(err)
	}
	if err = target.Restore(path); ErrorCode(err) != "storage_read_only" {
		t.Fatalf("readonly failure mislabeled: %v", err)
	}
	if _, err = target.DB.Exec("PRAGMA query_only=OFF"); err != nil {
		t.Fatal(err)
	}
	if err = target.Restore(path); err != nil {
		t.Fatalf("restore did not recover: %v", err)
	}
	for _, test := range []struct {
		err  error
		code string
	}{{syscall.ENOSPC, "storage_full"}, {syscall.EDQUOT, "storage_full"}, {syscall.EIO, "storage_io"}, {syscall.EROFS, "storage_read_only"}} {
		wrapped := fmt.Errorf("publish snapshot: %w", &os.PathError{Op: "write", Path: "test-owned-file", Err: test.err})
		if got := ErrorCode(wrapped); got != test.code {
			t.Fatalf("filesystem error %v classified %s", test.err, got)
		}
		if got := ErrorCode(invalidArchive(wrapped)); got != test.code {
			t.Fatalf("restore changed operational error to %s", got)
		}
	}
	archiveChaosIntegrity(t, target)
}

func TestChaosOpenExistingFailsClosed(t *testing.T) {
	for _, mode := range []string{"missing", "empty", "foreign-sqlite", "missing-durable-table", "wrong-project", "future-schema"} {
		t.Run(mode, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "tasks.sqlite")
			if mode != "missing" && mode != "empty" && mode != "foreign-sqlite" {
				s, err := Open(path, "guarded-project")
				if err != nil {
					t.Fatal(err)
				}
				mustCreateTask(t, s, CreateSpec{Title: "Keep durable state"})
				switch mode {
				case "missing-durable-table":
					if _, err = s.DB.Exec("DROP TABLE metadata"); err != nil {
						t.Fatal(err)
					}
				case "future-schema":
					if _, err = s.DB.Exec("UPDATE store_info SET value='999' WHERE key='schema_version'"); err != nil {
						t.Fatal(err)
					}
				}
				if err = s.Close(); err != nil {
					t.Fatal(err)
				}
			} else if mode == "empty" {
				if err := os.WriteFile(path, nil, 0600); err != nil {
					t.Fatal(err)
				}
			} else if mode == "foreign-sqlite" {
				db, err := sql.Open("sqlite", path)
				if err != nil {
					t.Fatal(err)
				}
				if _, err = db.Exec("CREATE TABLE unrelated(value TEXT); INSERT INTO unrelated VALUES('preserve')"); err != nil {
					t.Fatal(err)
				}
				db.Close()
			}
			before, readErr := os.ReadFile(path)
			project := "guarded-project"
			if mode == "wrong-project" {
				project = "different-project"
			}
			s, err := OpenExisting(path, project)
			if err == nil {
				if s != nil {
					s.Close()
				}
				t.Fatal("invalid existing store opened successfully")
			}
			want := "storage_invalid"
			switch mode {
			case "missing":
				want = "storage_missing"
			case "wrong-project":
				want = "wrong_project"
			case "future-schema":
				want = "unsupported_schema"
			}
			if ErrorCode(err) != want {
				t.Fatalf("expected %s, got %v", want, err)
			}
			after, afterErr := os.ReadFile(path)
			if errors.Is(readErr, os.ErrNotExist) {
				if !errors.Is(afterErr, os.ErrNotExist) {
					t.Fatalf("missing database was recreated: %v", afterErr)
				}
			} else if afterErr != nil || !bytes.Equal(before, after) {
				t.Fatalf("failed open mutated database: %v", afterErr)
			}
		})
	}
}

func TestChaosOpenExistingAllowsAdditiveOperationalTables(t *testing.T) {
	path := filepath.Join(t.TempDir(), "tasks.sqlite")
	s, err := Open(path, "additive-project")
	if err != nil {
		t.Fatal(err)
	}
	task := mustCreateTask(t, s, CreateSpec{Title: "Preserve older state"})
	if _, err = s.DB.Exec("DROP TABLE scratch_cleanup_state; DROP TABLE create_requests"); err != nil {
		t.Fatal(err)
	}
	s.Close()
	s, err = OpenExisting(path, "additive-project")
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	got, err := s.Get(task.ID)
	if err != nil || got.Title != task.Title {
		t.Fatalf("additive initialization lost task: %+v %v", got, err)
	}
	for _, table := range []string{"scratch_cleanup_state", "create_requests"} {
		var count int
		if err = s.DB.QueryRow("SELECT COUNT(*) FROM " + table).Scan(&count); err != nil || count != 0 {
			t.Fatalf("additive table %s: %d %v", table, count, err)
		}
	}
	archiveChaosIntegrity(t, s)
	s.DB.SetConnMaxLifetime(time.Nanosecond)
	for _, setting := range []struct {
		name string
		want int
	}{{"foreign_keys", 1}, {"busy_timeout", 5000}, {"synchronous", 2}} {
		var value int
		if err = s.DB.QueryRow("PRAGMA " + setting.name).Scan(&value); err != nil || value != setting.want {
			t.Fatalf("renewed connection lost %s: %d %v", setting.name, value, err)
		}
	}
}

func TestChaosBackupPublicationFailuresDoNotOverwrite(t *testing.T) {
	s := newTaskTestStore(t)
	mustCreateTask(t, s, CreateSpec{Title: "Backup publication"})
	base := t.TempDir()
	foreign := filepath.Join(base, "foreign")
	if err := os.WriteFile(foreign, []byte("preserve"), 0600); err != nil {
		t.Fatal(err)
	}
	directory := filepath.Join(base, "directory")
	if err := os.Mkdir(directory, 0700); err != nil {
		t.Fatal(err)
	}
	symlink := filepath.Join(base, "symlink")
	if err := os.Symlink(foreign, symlink); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{foreign, directory, symlink, filepath.Join(base, "absent", "backup.sqlite")} {
		if err := s.Backup(path); err == nil {
			t.Fatalf("invalid destination accepted: %s", path)
		}
		data, err := os.ReadFile(foreign)
		if err != nil || string(data) != "preserve" {
			t.Fatalf("foreign target changed: %q %v", data, err)
		}
		leftovers, err := filepath.Glob(filepath.Join(base, ".taskctl-backup-*"))
		if err != nil || len(leftovers) != 0 {
			t.Fatalf("recoverable publication failure leaked staging files: %v %v", leftovers, err)
		}
	}
	if err := s.Backup(filepath.Join(base, "good.sqlite")); err != nil {
		t.Fatal(err)
	}
	archiveChaosIntegrity(t, s)
}

func archiveChaosSnapshot(t *testing.T, q Queryer) {
	t.Helper()
	var torn bool
	if err := q.QueryRow(`SELECT EXISTS(SELECT 1 FROM tasks t WHERE t.progress<>COALESCE((SELECT message FROM events WHERE task_id=t.id AND kind='progress' ORDER BY id DESC LIMIT 1),''))`).Scan(&torn); err != nil {
		t.Fatal(err)
	}
	if torn {
		t.Fatal("backup has a task/event snapshot mismatch")
	}
}

func TestChaosBackupsRemainConsistentUnderWriters(t *testing.T) {
	path := filepath.Join(t.TempDir(), "tasks.sqlite")
	s, err := Open(path, "snapshot-chaos")
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	const workers = 4
	stores := make([]*Store, workers)
	tasks := make([]Task, workers)
	for i := range stores {
		stores[i], err = Open(path, s.ProjectID)
		if err != nil {
			t.Fatal(err)
		}
		defer stores[i].Close()
		tasks[i] = mustClaimTask(t, s, mustCreateTask(t, s, CreateSpec{Title: fmt.Sprintf("worker-%d", i)}).ID, fmt.Sprintf("worker-%d", i))
	}
	start := make(chan struct{})
	errs := make(chan error, workers)
	var wg sync.WaitGroup
	for i, writer := range stores {
		wg.Add(1)
		go func(i int, w *Store) {
			defer wg.Done()
			<-start
			for step := 0; step < 40; step++ {
				if _, err := w.Progress(tasks[i].ID, tasks[i].ClaimID, fmt.Sprintf("worker-%d step-%d", i, step)); err != nil {
					errs <- err
					return
				}
			}
		}(i, writer)
	}
	close(start)
	for round := 0; round < 24; round++ {
		backup := backupTestPath(t, s)
		source := backupTestRead(t, backup)
		archiveChaosSnapshot(t, source)
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Fatal(err)
	}
	archiveChaosIntegrity(t, s)
	archiveChaosSnapshot(t, s.DB)
	var incomplete int
	if err = s.DB.QueryRow("SELECT COUNT(*) FROM tasks WHERE progress NOT LIKE '%step-39'").Scan(&incomplete); err != nil || incomplete != 0 {
		t.Fatalf("writers incomplete: %d %v", incomplete, err)
	}
}

func TestChaosSQLiteRestoreRejectsTruncation(t *testing.T) {
	s := newTaskTestStore(t)
	mustCreateTask(t, s, CreateSpec{Title: "Read interruption"})
	data, err := os.ReadFile(backupTestPath(t, s))
	if err != nil {
		t.Fatal(err)
	}
	rng := rand.New(rand.NewSource(53123))
	path := filepath.Join(t.TempDir(), "truncated.sqlite")
	for i := 0; i < 32; i++ {
		n := rng.Intn(len(data) - 1)
		if err = os.WriteFile(path, data[:n], 0600); err != nil {
			t.Fatal(err)
		}
		target := newTaskTestStore(t)
		if err = target.Restore(path); err == nil {
			t.Fatalf("truncated backup accepted at byte %d", n)
		}
		var count int
		if err = target.DB.QueryRow("SELECT COUNT(*) FROM tasks").Scan(&count); err != nil || count != 0 {
			t.Fatalf("partial restore: %d %v", count, err)
		}
	}
	if err = os.WriteFile(path, data, 0600); err != nil {
		t.Fatal(err)
	}
	if err = newTaskTestStore(t).Restore(path); err != nil {
		t.Fatal(err)
	}
}

type archivePauseDriver struct{}
type archivePauseConn struct{ driver.Conn }

func (archivePauseDriver) Open(name string) (driver.Conn, error) {
	conn, err := (&sqlite.Driver{}).Open(name)
	if err != nil {
		return nil, err
	}
	return archivePauseConn{conn}, nil
}
func (c archivePauseConn) ExecContext(ctx context.Context, query string, args []driver.NamedValue) (driver.Result, error) {
	executor, ok := c.Conn.(driver.ExecerContext)
	if !ok {
		return nil, fmt.Errorf("SQLite driver no longer exposes ExecContext")
	}
	result, err := executor.ExecContext(ctx, query, args)
	if err == nil && query == "VACUUM INTO ?" {
		fmt.Fprintln(os.Stdout, "ready")
		_, _ = io.Copy(io.Discard, os.Stdin)
	}
	return result, err
}

func TestChaosBackupProcessHelper(t *testing.T) {
	phase := os.Getenv("TASKCTL_BACKUP_CHAOS_PHASE")
	if phase == "" {
		t.Skip("subprocess helper")
	}
	path, destination := os.Getenv("TASKCTL_BACKUP_CHAOS_DB"), os.Getenv("TASKCTL_BACKUP_CHAOS_DEST")
	var s *Store
	if phase == "before-publication" {
		sql.Register("taskctl-chaos-backup-pause", archivePauseDriver{})
		db, err := sql.Open("taskctl-chaos-backup-pause", path)
		if err != nil {
			t.Fatal(err)
		}
		db.SetMaxOpenConns(1)
		s = &Store{DB: db, ProjectID: "core-tests", Now: time.Now}
	} else {
		var err error
		s, err = Open(path, "core-tests")
		if err != nil {
			t.Fatal(err)
		}
	}
	defer s.Close()
	if err := s.Backup(destination); err != nil {
		t.Fatal(err)
	}
	if phase == "after-publication" {
		fmt.Fprintln(os.Stdout, "ready")
		_, _ = io.Copy(io.Discard, os.Stdin)
	}
}

func TestChaosBackupSIGKILLAtPublicationBoundary(t *testing.T) {
	for _, phase := range []string{"before-publication", "after-publication"} {
		t.Run(phase, func(t *testing.T) {
			base := t.TempDir()
			path := filepath.Join(base, "source.sqlite")
			s, err := Open(path, "core-tests")
			if err != nil {
				t.Fatal(err)
			}
			defer s.Close()
			created := mustCreateTask(t, s, CreateSpec{Title: "Backup crash boundary", Brief: strings.Repeat("context ", 8192)})
			destination := filepath.Join(base, "published.sqlite")
			executable, err := os.Executable()
			if err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
			defer cancel()
			cmd := exec.CommandContext(ctx, executable, "-test.run=^TestChaosBackupProcessHelper$", "-test.count=1")
			cmd.Env = append(os.Environ(), "TASKCTL_BACKUP_CHAOS_PHASE="+phase, "TASKCTL_BACKUP_CHAOS_DB="+path, "TASKCTL_BACKUP_CHAOS_DEST="+destination)
			stdin, err := cmd.StdinPipe()
			if err != nil {
				t.Fatal(err)
			}
			defer stdin.Close()
			stdout, err := cmd.StdoutPipe()
			if err != nil {
				t.Fatal(err)
			}
			var stderr bytes.Buffer
			cmd.Stderr = &stderr
			if err = cmd.Start(); err != nil {
				t.Fatal(err)
			}
			ready := make(chan string, 1)
			go func() {
				scanner := bufio.NewScanner(stdout)
				if scanner.Scan() {
					ready <- scanner.Text()
				} else {
					ready <- ""
				}
			}()
			select {
			case line := <-ready:
				if line != "ready" {
					_ = cmd.Process.Kill()
					_ = cmd.Wait()
					t.Fatalf("backup child did not reach barrier: %q %s", line, stderr.String())
				}
			case <-ctx.Done():
				_ = cmd.Process.Kill()
				_ = cmd.Wait()
				t.Fatalf("backup barrier timed out: %s", stderr.String())
			}
			if err = cmd.Process.Kill(); err != nil {
				_ = cmd.Wait()
				t.Fatal(err)
			}
			if err = cmd.Wait(); err == nil {
				t.Fatal("backup child was not killed")
			}
			if phase == "before-publication" {
				if _, err = os.Stat(destination); !errors.Is(err, os.ErrNotExist) {
					t.Fatalf("unpublished destination exists: %v", err)
				}
				staging, err := filepath.Glob(filepath.Join(base, ".taskctl-backup-*"))
				if err != nil || len(staging) != 1 {
					t.Fatalf("expected only the interrupted private staging file: %v %v", staging, err)
				}
				info, err := os.Stat(staging[0])
				if err != nil || info.Mode().Perm() != 0600 {
					t.Fatalf("staging permissions: %v %v", info, err)
				}
				if err = s.Backup(destination); err != nil {
					t.Fatalf("retry failed after pre-publication crash: %v", err)
				}
			} else {
				if err = s.Backup(destination); err == nil {
					t.Fatal("lost-response retry overwrote completed backup")
				}
			}
			source := backupTestRead(t, destination)
			var title, brief string
			var count int
			if err = source.QueryRow("SELECT COUNT(*),title,brief FROM tasks").Scan(&count, &title, &brief); err != nil || count != 1 || title != created.Title || brief != created.Brief {
				t.Fatalf("published backup incomplete: count=%d %q %v", count, title, err)
			}
			archiveChaosIntegrity(t, s)
		})
	}
}
