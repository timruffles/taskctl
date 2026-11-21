package scratch

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
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"taskctl/internal/store"
)

func TestChaosScratchProcessHelper(t *testing.T) {
	phase := os.Getenv("TASKCTL_SCRATCH_CHAOS_PHASE")
	if phase == "" {
		t.Skip("subprocess helper")
	}
	s, err := store.Open(os.Getenv("TASKCTL_SCRATCH_CHAOS_DB"), "scratch-files")
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	now, err := time.Parse(time.RFC3339Nano, os.Getenv("TASKCTL_SCRATCH_CHAOS_NOW"))
	if err != nil {
		t.Fatal(err)
	}
	s.Now = func() time.Time { return now }
	m := Manager{Store: s, Root: os.Getenv("TASKCTL_SCRATCH_CHAOS_ROOT")}
	root, err := os.OpenRoot(m.Root)
	if err != nil {
		t.Fatal(err)
	}
	defer root.Close()
	lock, err := openCleanupLock(root)
	if err != nil {
		t.Fatal(err)
	}
	defer lock.Close()
	unlock, locked, err := tryCleanupLock(lock)
	if err != nil || !locked {
		t.Fatalf("child lock: %t %v", locked, err)
	}
	defer unlock()
	areas, err := s.ScratchCleanupCandidates(0, true, 1)
	if err != nil || len(areas) != 1 {
		t.Fatalf("child claim: %+v %v", areas, err)
	}
	a := areas[0]
	area, err := openVerifiedDirectory(root, a.ID)
	if err != nil {
		t.Fatal(err)
	}
	defer area.Close()
	switch phase {
	case "reserved":
	case "partial-batch":
		budget := cleanupBudget{remaining: 2, deadline: time.Now().Add(time.Minute)}
		cursor, finalizing, removed, err := m.removeBatch(area, a, &budget, ".")
		if err != nil || removed == 0 || finalizing {
			t.Fatalf("partial batch: %d %t %v", removed, finalizing, err)
		}
		if err = s.ScratchCleanupSave(a.ID, a.CleanupToken, cursor, finalizing); err != nil {
			t.Fatal(err)
		}
	case "marker-unlinked", "directory-unlinked":
		if err = s.ScratchCleanupSave(a.ID, a.CleanupToken, ".", true); err != nil {
			t.Fatal(err)
		}
		limit := 1
		if phase == "directory-unlinked" {
			limit = 2
		}
		budget := cleanupBudget{remaining: limit, deadline: time.Now().Add(time.Minute)}
		done, _, err := finishDirectory(root, area, a.ID, &budget)
		if err != nil || done != (phase == "directory-unlinked") {
			t.Fatalf("final stage: %t %v", done, err)
		}
	default:
		t.Fatal("unknown crash phase")
	}
	fmt.Fprintln(os.Stdout, "ready")
	_, _ = io.Copy(io.Discard, os.Stdin)
}

func killScratchChildAtBarrier(t *testing.T, f *scratchFixture, phase string) {
	t.Helper()
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, executable, "-test.run=^TestChaosScratchProcessHelper$", "-test.count=1")
	cmd.Env = append(os.Environ(), "TASKCTL_SCRATCH_CHAOS_PHASE="+phase, "TASKCTL_SCRATCH_CHAOS_DB="+f.dbPath, "TASKCTL_SCRATCH_CHAOS_ROOT="+f.m.Root, "TASKCTL_SCRATCH_CHAOS_NOW="+f.now.Format(time.RFC3339Nano))
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
			t.Fatalf("child did not reach %s: %q %s", phase, line, stderr.String())
		}
	case <-ctx.Done():
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
		t.Fatalf("child barrier timeout: %s", stderr.String())
	}
	if err = cmd.Process.Kill(); err != nil {
		_ = cmd.Wait()
		t.Fatal(err)
	}
	if err = cmd.Wait(); err == nil {
		t.Fatal("child was not killed")
	}
}

func TestChaosScratchSIGKILLRecoveryAtDurablePhases(t *testing.T) {
	for _, phase := range []string{"reserved", "partial-batch", "marker-unlinked", "directory-unlinked"} {
		t.Run(phase, func(t *testing.T) {
			f := newScratchFixture(t)
			if phase == "partial-batch" {
				for i := 0; i < 8; i++ {
					writeScratchFile(t, filepath.Join(f.area.Path, fmt.Sprintf("fragment-%d", i)), "temporary")
				}
			}
			foreign := filepath.Join(filepath.Dir(f.m.Root), "foreign.txt")
			writeScratchFile(t, foreign, "preserve")
			f.terminal(t)
			killScratchChildAtBarrier(t, f, phase)
			f.reopen(t)
			live, err := f.m.Store.Create(store.CreateSpec{Title: "Cannot reuse partially removed area"})
			if err != nil {
				t.Fatal(err)
			}
			if err = f.m.Store.ScratchAttach(live.ID, "", f.area.Area.ID); store.ErrorCode(err) != "conflict" {
				t.Fatalf("crashed area was attachable: %v", err)
			}
			if result, err := f.m.Cleanup(0, false, 1); err != nil || len(result) != 0 {
				t.Fatalf("unexpired child claim was stolen: %+v %v", result, err)
			}
			f.now = f.now.Add(31 * time.Second)
			result, err := f.m.Cleanup(0, false, 1)
			if err != nil || len(result) != 1 || result[0].State != "removed" {
				t.Fatalf("crash recovery: %+v %v", result, err)
			}
			if _, err = os.Stat(f.area.Path); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("removed area remains: %v", err)
			}
			areas, _, err := f.m.Store.ScratchListPage(f.task.ID, 200, 0)
			if err != nil || len(areas) != 1 || areas[0].RemovedAt == "" {
				t.Fatalf("removal history missing: %+v %v", areas, err)
			}
			requireScratchFile(t, foreign, "preserve")
		})
	}
}

func TestChaosScratchAttachRacesCleanup(t *testing.T) {
	for round := 0; round < 40; round++ {
		t.Run(strconv.Itoa(round), func(t *testing.T) {
			f := newScratchFixture(t)
			writeScratchFile(t, filepath.Join(f.area.Path, "context"), "preserve if attached")
			f.terminal(t)
			active, err := f.m.Store.Create(store.CreateSpec{Title: "Live successor"})
			if err != nil {
				t.Fatal(err)
			}
			other, err := store.Open(f.dbPath, f.project)
			if err != nil {
				t.Fatal(err)
			}
			defer other.Close()
			other.Now = func() time.Time { return f.now }
			start := make(chan struct{})
			var wg sync.WaitGroup
			wg.Add(2)
			var attached error
			var cleaned error
			var results []Result
			go func() { defer wg.Done(); <-start; attached = other.ScratchAttach(active.ID, "", f.area.Area.ID) }()
			go func() { defer wg.Done(); <-start; results, cleaned = f.m.Cleanup(0, false, 1) }()
			close(start)
			wg.Wait()
			if cleaned != nil {
				t.Fatal(cleaned)
			}
			if attached == nil {
				if len(results) != 0 {
					t.Fatalf("deleted attached area: %+v", results)
				}
				requireScratchFile(t, filepath.Join(f.area.Path, "context"), "preserve if attached")
			} else if store.ErrorCode(attached) != "conflict" {
				t.Fatal(attached)
			} else if len(results) != 1 || results[0].State != "removed" {
				t.Fatalf("neither attachment nor cleanup completed: %+v %v", results, attached)
			}
		})
	}
}

func TestChaosScratchPermissionFailureAndLeaseExpiryRecover(t *testing.T) {
	t.Run("permission", func(t *testing.T) {
		if os.Geteuid() == 0 {
			t.Skip("root bypasses directory permissions")
		}
		f := newScratchFixture(t)
		writeScratchFile(t, filepath.Join(f.area.Path, "remaining"), "temporary")
		f.terminal(t)
		if err := os.Chmod(f.area.Path, 0500); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = os.Chmod(f.area.Path, 0700) })
		if _, err := f.m.Cleanup(0, false, 1); err == nil {
			t.Fatal("cleanup unexpectedly bypassed permissions")
		}
		requireScratchFile(t, filepath.Join(f.area.Path, "remaining"), "temporary")
		if err := os.Chmod(f.area.Path, 0700); err != nil {
			t.Fatal(err)
		}
		results, err := f.m.Cleanup(0, false, 1)
		if err != nil || len(results) != 1 || results[0].State != "removed" {
			t.Fatalf("permission recovery: %+v %v", results, err)
		}
	})
	t.Run("lease-expires-before-saving", func(t *testing.T) {
		f := newScratchFixture(t)
		writeScratchFile(t, filepath.Join(f.area.Path, "remaining"), "temporary")
		f.terminal(t)
		calls := 0
		f.m.Store.Now = func() time.Time {
			calls++
			if calls > 2 {
				return f.now.Add(31 * time.Second)
			}
			return f.now
		}
		if _, err := f.m.Cleanup(0, false, 1); store.ErrorCode(err) != "conflict" {
			t.Fatalf("expired claim was accepted: %v", err)
		}
		if _, err := os.Stat(filepath.Join(f.area.Path, ownerMarker)); err != nil {
			t.Fatalf("expired cleaner unlinked owner marker: %v", err)
		}
		f.now = f.now.Add(32 * time.Second)
		f.m.Store.Now = func() time.Time { return f.now }
		result, err := f.m.Cleanup(0, false, 1)
		if err != nil || len(result) != 1 || result[0].State != "removed" {
			t.Fatalf("lease recovery: %+v %v", result, err)
		}
	})
}

func TestChaosScratchSavedDirectoryReplacedBySymlink(t *testing.T) {
	f := newScratchFixture(t)
	nested := filepath.Join(f.area.Path, "nested")
	if err := os.Mkdir(nested, 0700); err != nil {
		t.Fatal(err)
	}
	writeScratchFile(t, filepath.Join(nested, "original"), "temporary")
	f.terminal(t)
	claims, err := f.m.Store.ScratchCleanupCandidates(0, true, 1)
	if err != nil || len(claims) != 1 {
		t.Fatalf("claim: %+v %v", claims, err)
	}
	if err = f.m.Store.ScratchCleanupSave(claims[0].ID, claims[0].CleanupToken, "nested", false); err != nil {
		t.Fatal(err)
	}
	if err = f.m.Store.ScratchCleanupRelease(claims[0].ID, claims[0].CleanupToken); err != nil {
		t.Fatal(err)
	}
	foreign := t.TempDir()
	writeScratchFile(t, filepath.Join(foreign, "important"), "preserve")
	if err = os.Rename(nested, nested+"-moved"); err != nil {
		t.Fatal(err)
	}
	if err = os.Symlink(foreign, nested); err != nil {
		t.Fatal(err)
	}
	if _, err = f.m.Cleanup(0, false, 1); err == nil {
		t.Fatal("saved cursor traversed a replacement symlink")
	}
	requireScratchFile(t, filepath.Join(foreign, "important"), "preserve")
	requireScratchFile(t, filepath.Join(nested+"-moved", "original"), "temporary")
	if err = os.Remove(nested); err != nil {
		t.Fatal(err)
	}
	if err = os.Rename(nested+"-moved", nested); err != nil {
		t.Fatal(err)
	}
	result, err := f.m.Cleanup(0, false, 1)
	if err != nil || len(result) != 1 || result[0].State != "removed" {
		t.Fatalf("recovery from path replacement: %+v %v", result, err)
	}
	requireScratchFile(t, filepath.Join(foreign, "important"), "preserve")
}

func TestChaosScratchPathsBeyondSystemPathLimit(t *testing.T) {
	f := newScratchFixture(t)
	dir, err := os.OpenRoot(f.area.Path)
	if err != nil {
		t.Fatal(err)
	}
	for depth := 0; depth < 24; depth++ {
		name := fmt.Sprintf("%02d-", depth) + strings.Repeat("x", 200)
		if err = dir.Mkdir(name, 0700); err != nil {
			dir.Close()
			t.Fatal(err)
		}
		next, err := dir.OpenRoot(name)
		dir.Close()
		if err != nil {
			t.Fatal(err)
		}
		dir = next
	}
	file, err := dir.Create("leaf")
	if err != nil {
		dir.Close()
		t.Fatal(err)
	}
	if _, err = file.WriteString("temporary"); err != nil {
		file.Close()
		dir.Close()
		t.Fatal(err)
	}
	file.Close()
	dir.Close()
	f.terminal(t)
	f.m.EntryBudget = 7
	for batch := 0; batch < 128; batch++ {
		results, err := f.m.Cleanup(0, false, 1)
		if err != nil {
			t.Fatal(err)
		}
		if len(results) == 1 && results[0].State == "removed" {
			return
		}
	}
	t.Fatal("long-path cleanup did not converge")
}
