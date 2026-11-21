package scratch

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"taskctl/internal/store"
)

type scratchFixture struct {
	m       Manager
	task    store.Task
	area    Result
	now     time.Time
	dbPath  string
	project string
}

func newScratchFixture(t *testing.T) *scratchFixture {
	t.Helper()
	if runtime.GOOS != "linux" && runtime.GOOS != "darwin" && runtime.GOOS != "freebsd" && runtime.GOOS != "netbsd" && runtime.GOOS != "openbsd" && runtime.GOOS != "dragonfly" {
		t.Skip("scratch cleanup file locking is not supported on this OS")
	}
	base := t.TempDir()
	f := &scratchFixture{dbPath: filepath.Join(base, "tasks.sqlite"), project: "scratch-files", now: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)}
	s, err := store.Open(f.dbPath, f.project)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	s.Now = func() time.Time { return f.now }
	f.m = Manager{Store: s, Root: filepath.Join(base, "managed"), EntryBudget: 256, TimeBudget: time.Second}
	f.task, err = s.Create(store.CreateSpec{Title: "Disposable work"})
	if err != nil {
		t.Fatal(err)
	}
	f.area, err = f.m.Create(f.task.ID, "")
	if err != nil {
		t.Fatal(err)
	}
	return f
}

func (f *scratchFixture) terminal(t *testing.T) {
	t.Helper()
	if _, err := f.m.Store.Cancel(f.task.ID, "", "Work finished"); err != nil {
		t.Fatal(err)
	}
}

func (f *scratchFixture) reopen(t *testing.T) {
	t.Helper()
	if err := f.m.Store.Close(); err != nil {
		t.Fatal(err)
	}
	s, err := store.Open(f.dbPath, f.project)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	s.Now = func() time.Time { return f.now }
	f.m.Store = s
}

func writeScratchFile(t *testing.T, path, contents string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(contents), 0600); err != nil {
		t.Fatal(err)
	}
}

func requireScratchFile(t *testing.T, path, contents string) {
	t.Helper()
	got, err := os.ReadFile(path)
	if err != nil || string(got) != contents {
		t.Fatalf("protected file %s changed: %q %v", path, got, err)
	}
}

func TestCleanupBoundedBatchesResumeAndRemoveMarkerLast(t *testing.T) {
	f := newScratchFixture(t)
	f.m.EntryBudget = 1
	for i := 0; i < 12; i++ {
		writeScratchFile(t, filepath.Join(f.area.Path, fmt.Sprintf("file-%02d", i)), "scratch")
	}
	deep := f.area.Path
	for i := 0; i < 10; i++ {
		deep = filepath.Join(deep, fmt.Sprintf("level-%02d", i))
		if err := os.Mkdir(deep, 0700); err != nil {
			t.Fatal(err)
		}
	}
	writeScratchFile(t, filepath.Join(deep, "deep-file"), "scratch")
	f.terminal(t)
	newTask, err := f.m.Store.Create(store.CreateSpec{Title: "New live task"})
	if err != nil {
		t.Fatal(err)
	}
	finished, rounds := false, 0
	for ; rounds < 100; rounds++ {
		results, err := f.m.Cleanup(0, false, 100)
		if err != nil || len(results) != 1 {
			t.Fatalf("batch %d: %+v %v", rounds, results, err)
		}
		if results[0].EntriesRemoved > 1 {
			t.Fatalf("batch exceeded entry budget: %+v", results[0])
		}
		if results[0].State == "removed" {
			finished = true
			break
		}
		if results[0].State != "deleting" {
			t.Fatalf("partial batch reported completed: %+v", results[0])
		}
		if err := f.m.Store.ScratchAttach(newTask.ID, "", f.area.Area.ID); store.ErrorCode(err) != "conflict" {
			t.Fatalf("partially removed area was attachable: %v", err)
		}
		if _, err := os.Lstat(filepath.Join(f.area.Path, ownerMarker)); errors.Is(err, os.ErrNotExist) {
			entries, err := os.ReadDir(f.area.Path)
			if err != nil || len(entries) != 0 {
				t.Fatalf("owner marker was not the last entry removed: %+v %v", entries, err)
			}
		} else if err != nil {
			t.Fatal(err)
		}
		if rounds == 4 {
			f.reopen(t)
		}
	}
	if !finished || rounds < 12 {
		t.Fatalf("bounded cleanup did not converge incrementally: finished=%v rounds=%d", finished, rounds)
	}
	if _, err := os.Lstat(f.area.Path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("removed directory remains: %v", err)
	}
	areas, _, err := f.m.Store.ScratchListPage(f.task.ID, 200, 0)
	if err != nil || areas[0].RemovedAt == "" || areas[0].CleanupToken != "" {
		t.Fatalf("database removal did not finish: %+v %v", areas, err)
	}
}

func TestCleanupRetainsLiveReferencesAndDryRunDoesNotStartDeletion(t *testing.T) {
	f := newScratchFixture(t)
	writeScratchFile(t, filepath.Join(f.area.Path, "keep"), "live")
	f.now = f.now.Add(365 * 24 * time.Hour)
	results, err := f.m.Cleanup(0, false, 20)
	if err != nil || len(results) != 0 {
		t.Fatalf("live scratch was considered: %+v %v", results, err)
	}
	requireScratchFile(t, filepath.Join(f.area.Path, "keep"), "live")
	f.terminal(t)
	results, err = f.m.Cleanup(0, true, 20)
	if err != nil || len(results) != 1 || results[0].State != "eligible" {
		t.Fatalf("dry run = %+v %v", results, err)
	}
	newTask, err := f.m.Store.Create(store.CreateSpec{Title: "Reuse"})
	if err != nil {
		t.Fatal(err)
	}
	if err := f.m.Store.ScratchAttach(newTask.ID, "", f.area.Area.ID); err != nil {
		t.Fatalf("dry run reserved or deleted area: %v", err)
	}
	requireScratchFile(t, filepath.Join(f.area.Path, "keep"), "live")
}

func TestCleanupCrashLeaseAndSavedCursorRecover(t *testing.T) {
	f := newScratchFixture(t)
	nested := filepath.Join(f.area.Path, "nested")
	if err := os.Mkdir(nested, 0700); err != nil {
		t.Fatal(err)
	}
	writeScratchFile(t, filepath.Join(nested, "remaining"), "temporary")
	f.terminal(t)
	claims, err := f.m.Store.ScratchCleanupCandidates(0, true, 1)
	if err != nil || len(claims) != 1 {
		t.Fatalf("reservation = %+v %v", claims, err)
	}
	if err := f.m.Store.ScratchCleanupSave(claims[0].ID, claims[0].CleanupToken, "nested", false); err != nil {
		t.Fatal(err)
	}
	f.reopen(t)
	results, err := f.m.Cleanup(0, false, 1)
	if err != nil || len(results) != 0 {
		t.Fatalf("unexpired lease was stolen: %+v %v", results, err)
	}
	f.now = f.now.Add(31 * time.Second)
	results, err = f.m.Cleanup(0, false, 1)
	if err != nil || len(results) != 1 || results[0].State != "removed" {
		t.Fatalf("crashed cleaner did not recover: %+v %v", results, err)
	}
}

func TestCleanupRecoversAfterFinalMarkerUnlink(t *testing.T) {
	f := newScratchFixture(t)
	f.terminal(t)
	claims, err := f.m.Store.ScratchCleanupCandidates(0, true, 1)
	if err != nil || len(claims) != 1 {
		t.Fatalf("reservation = %+v %v", claims, err)
	}
	if err := f.m.Store.ScratchCleanupSave(claims[0].ID, claims[0].CleanupToken, ".", true); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(f.area.Path, ownerMarker)); err != nil {
		t.Fatal(err)
	}
	f.now = f.now.Add(31 * time.Second)
	f.reopen(t)
	results, err := f.m.Cleanup(0, false, 1)
	if err != nil || len(results) != 1 || results[0].State != "removed" {
		t.Fatalf("final unlink recovery = %+v %v", results, err)
	}
}

func TestCleanupRefusesMissingOrInvalidOwnerMarker(t *testing.T) {
	for _, mode := range []string{"missing", "mismatched", "symlink"} {
		t.Run(mode, func(t *testing.T) {
			f := newScratchFixture(t)
			writeScratchFile(t, filepath.Join(f.area.Path, "keep"), "unverified")
			path := filepath.Join(f.area.Path, ownerMarker)
			if err := os.Remove(path); err != nil {
				t.Fatal(err)
			}
			switch mode {
			case "mismatched":
				data, _ := json.Marshal(marker{ProjectID: "wrong", AreaID: f.area.Area.ID})
				writeScratchFile(t, path, string(data))
			case "symlink":
				outside := filepath.Join(t.TempDir(), "marker")
				data, _ := json.Marshal(marker{ProjectID: f.project, AreaID: f.area.Area.ID})
				writeScratchFile(t, outside, string(data))
				if err := os.Symlink(outside, path); err != nil {
					t.Fatal(err)
				}
			}
			f.terminal(t)
			if _, err := f.m.Cleanup(0, false, 1); err == nil {
				t.Fatal("cleanup accepted an unverified owner marker")
			}
			requireScratchFile(t, filepath.Join(f.area.Path, "keep"), "unverified")
		})
	}
}

func TestCleanupUnlinksNestedSymlinksWithoutFollowingThem(t *testing.T) {
	f := newScratchFixture(t)
	outside := t.TempDir()
	protected := filepath.Join(outside, "keep")
	writeScratchFile(t, protected, "outside")
	if err := os.Symlink(outside, filepath.Join(f.area.Path, "outside-directory")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(protected, filepath.Join(f.area.Path, "outside-file")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(".", filepath.Join(f.area.Path, "cycle")); err != nil {
		t.Fatal(err)
	}
	f.terminal(t)
	results, err := f.m.Cleanup(0, false, 1)
	if err != nil || len(results) != 1 || results[0].State != "removed" {
		t.Fatalf("symlink cleanup = %+v %v", results, err)
	}
	requireScratchFile(t, protected, "outside")
}

func TestCleanupRefusesAreaSymlink(t *testing.T) {
	f := newScratchFixture(t)
	if err := os.Remove(filepath.Join(f.area.Path, ownerMarker)); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(f.area.Path); err != nil {
		t.Fatal(err)
	}
	outside := t.TempDir()
	writeScratchFile(t, filepath.Join(outside, "keep"), "outside")
	if err := os.Symlink(outside, f.area.Path); err != nil {
		t.Fatal(err)
	}
	f.terminal(t)
	if _, err := f.m.Cleanup(0, false, 1); err == nil {
		t.Fatal("cleanup followed an area symlink")
	}
	requireScratchFile(t, filepath.Join(outside, "keep"), "outside")
	st, err := os.Lstat(f.area.Path)
	if err != nil || st.Mode()&os.ModeSymlink == 0 {
		t.Fatalf("area symlink was modified: %v", err)
	}
}

func TestCleanupNeverDeletesGitWorktrees(t *testing.T) {
	for _, nested := range []bool{false, true} {
		t.Run(fmt.Sprintf("nested=%v", nested), func(t *testing.T) {
			f := newScratchFixture(t)
			worktree := f.area.Path
			if nested {
				worktree = filepath.Join(worktree, "worktree")
				if err := os.Mkdir(worktree, 0700); err != nil {
					t.Fatal(err)
				}
			}
			writeScratchFile(t, filepath.Join(worktree, ".git"), "gitdir: /elsewhere/repository")
			writeScratchFile(t, filepath.Join(worktree, "important.go"), "important")
			f.terminal(t)
			if _, err := f.m.Cleanup(0, false, 1); err == nil || !strings.Contains(err.Error(), "worktree") {
				t.Fatalf("worktree protection = %v", err)
			}
			requireScratchFile(t, filepath.Join(worktree, "important.go"), "important")
			requireScratchFile(t, filepath.Join(worktree, ".git"), "gitdir: /elsewhere/repository")
		})
	}
}

func TestCleanupFilesystemLockIsNonblockingAndExclusive(t *testing.T) {
	f := newScratchFixture(t)
	writeScratchFile(t, filepath.Join(f.area.Path, "keep"), "temporary")
	f.terminal(t)
	root, err := os.OpenRoot(f.m.Root)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	lock, err := openCleanupLock(root)
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Close()
	unlock, acquired, err := tryCleanupLock(lock)
	if err != nil || !acquired {
		t.Fatalf("first lock = %v %v", acquired, err)
	}
	start := time.Now()
	results, err := f.m.Cleanup(0, false, 1)
	if err != nil || len(results) != 0 || time.Since(start) > time.Second {
		unlock()
		t.Fatalf("second cleaner overlapped or blocked: %+v %v", results, err)
	}
	requireScratchFile(t, filepath.Join(f.area.Path, "keep"), "temporary")
	unlock()
	results, err = f.m.Cleanup(0, false, 1)
	if err != nil || len(results) != 1 || results[0].State != "removed" {
		t.Fatalf("cleanup after unlock = %+v %v", results, err)
	}
}

func TestCleanupTimeBudgetDoesNotBeginUnboundedWork(t *testing.T) {
	f := newScratchFixture(t)
	writeScratchFile(t, filepath.Join(f.area.Path, "keep"), "temporary")
	f.terminal(t)
	f.m.TimeBudget = time.Nanosecond
	results, err := f.m.Cleanup(0, false, 1000)
	if err != nil || len(results) != 0 {
		t.Fatalf("expired time budget = %+v %v", results, err)
	}
	requireScratchFile(t, filepath.Join(f.area.Path, "keep"), "temporary")
}

func TestFinalizingDoesNotDeleteFilesAddedAfterMarkerUnlink(t *testing.T) {
	f := newScratchFixture(t)
	f.terminal(t)
	claims, err := f.m.Store.ScratchCleanupCandidates(0, true, 1)
	if err != nil || len(claims) != 1 {
		t.Fatalf("reservation = %+v %v", claims, err)
	}
	if err := f.m.Store.ScratchCleanupSave(claims[0].ID, claims[0].CleanupToken, ".", true); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(f.area.Path, ownerMarker)); err != nil {
		t.Fatal(err)
	}
	writeScratchFile(t, filepath.Join(f.area.Path, "unexpected"), "preserve")
	f.now = f.now.Add(31 * time.Second)
	if _, err := f.m.Cleanup(0, false, 1); err == nil {
		t.Fatal("finalizer deleted new unmarked contents")
	}
	requireScratchFile(t, filepath.Join(f.area.Path, "unexpected"), "preserve")
}
