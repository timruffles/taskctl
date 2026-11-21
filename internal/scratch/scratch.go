package scratch

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"taskctl/internal/store"
)

type Manager struct {
	Store       *store.Store
	Root        string
	EntryBudget int
	TimeBudget  time.Duration
}
type Result struct {
	Area           store.ScratchArea `json:"area"`
	Path           string            `json:"path"`
	Guidance       string            `json:"guidance,omitempty"`
	State          string            `json:"state,omitempty"`
	EntriesRemoved int               `json:"entries_removed,omitempty"`
}
type marker struct {
	ProjectID string `json:"project_id"`
	AreaID    string `json:"area_id"`
}

const guidance = "Disposable scratch. Put important work in the repository and reference it in the task."
const ownerMarker = ".taskctl-owner.json"
const cleanerLock = ".taskctl-cleanup.lock"

func (m Manager) Create(task, claim string) (Result, error) {
	var bytes [16]byte
	if _, err := rand.Read(bytes[:]); err != nil {
		return Result{}, err
	}
	id := "A" + hex.EncodeToString(bytes[:])
	if err := os.MkdirAll(m.Root, 0700); err != nil {
		return Result{}, err
	}
	root, err := os.OpenRoot(m.Root)
	if err != nil {
		return Result{}, err
	}
	defer root.Close()
	if err = root.Mkdir(id, 0700); err != nil {
		return Result{}, err
	}
	area, err := openVerifiedDirectory(root, id)
	if err != nil {
		_ = root.Remove(id)
		return Result{}, err
	}
	defer area.Close()
	markerCreated := false
	discard := func() {
		if markerCreated {
			_ = area.Remove(ownerMarker)
		}
		_ = root.Remove(id)
	}
	data, err := json.Marshal(marker{m.Store.ProjectID, id})
	if err != nil {
		discard()
		return Result{}, err
	}
	f, err := area.OpenFile(ownerMarker, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		discard()
		return Result{}, err
	}
	markerCreated = true
	_, err = f.Write(data)
	if err == nil {
		err = f.Sync()
	}
	closeErr := f.Close()
	if err == nil {
		err = closeErr
	}
	if err != nil {
		discard()
		return Result{}, err
	}
	if err = m.Store.ScratchCreate(task, claim, id); err != nil {
		discard()
		return Result{}, err
	}
	a, err := m.Store.ScratchGet(task, id)
	if err != nil {
		return Result{}, err
	}
	return Result{Area: a, Path: filepath.Join(m.Root, id), Guidance: guidance, State: "retained"}, nil
}

type cleanupBudget struct {
	remaining int
	deadline  time.Time
}

func (b *cleanupBudget) available() bool { return b.remaining > 0 && time.Now().Before(b.deadline) }
func (b *cleanupBudget) take() bool {
	if !b.available() {
		return false
	}
	b.remaining--
	return true
}

func (m Manager) Cleanup(grace time.Duration, dry bool, limit int) ([]Result, error) {
	if m.EntryBudget < 0 || m.TimeBudget < 0 {
		return nil, fmt.Errorf("cleanup budgets cannot be negative")
	}
	entries, duration := m.EntryBudget, m.TimeBudget
	if entries == 0 {
		entries = 256
	}
	if duration == 0 {
		duration = 50 * time.Millisecond
	}
	if limit < 0 {
		return nil, fmt.Errorf("cleanup limit cannot be negative")
	}
	if limit == 0 {
		limit = 20
	}
	if limit > 1000 {
		limit = 1000
	}
	budget := cleanupBudget{remaining: entries, deadline: time.Now().Add(duration)}
	result := []Result{}
	root, err := os.OpenRoot(m.Root)
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return result, err
	}
	if root != nil {
		defer root.Close()
		if !dry {
			lock, err := openCleanupLock(root)
			if err != nil {
				return result, err
			}
			defer lock.Close()
			unlock, locked, err := tryCleanupLock(lock)
			if err != nil {
				return result, err
			}
			if !locked {
				return result, nil
			}
			defer unlock()
		}
	}
	if dry {
		areas, err := m.Store.ScratchCleanupCandidates(grace, false, limit)
		if err != nil {
			return result, err
		}
		for _, a := range areas {
			if !budget.take() {
				break
			}
			if !store.ValidAreaID(a.ID) {
				return result, fmt.Errorf("refusing invalid scratch area ID")
			}
			if root != nil {
				area, err := openVerifiedDirectory(root, a.ID)
				if err != nil && !errors.Is(err, os.ErrNotExist) {
					return result, err
				}
				if area != nil {
					err = validateMarker(area, m.Store.ProjectID, a.ID, a.CleanupFinalizing)
					if err == nil {
						err = rejectWorktree(area)
					}
					area.Close()
					if err != nil {
						return result, err
					}
				}
			}
			result = append(result, Result{Area: a, Path: filepath.Join(m.Root, a.ID), State: "eligible"})
		}
		return result, nil
	}
	for len(result) < limit && budget.available() {
		areas, err := m.Store.ScratchCleanupCandidates(grace, true, 1)
		if err != nil {
			return result, err
		}
		if len(areas) == 0 {
			break
		}
		cleaned, err := m.cleanupReserved(root, areas[0], &budget)
		if cleaned.State != "" {
			result = append(result, cleaned)
		}
		if err != nil {
			return result, err
		}
		if cleaned.State != "removed" {
			break
		}
	}
	return result, nil
}

func openCleanupLock(root *os.Root) (*os.File, error) {
	f, err := root.OpenFile(cleanerLock, os.O_CREATE|os.O_EXCL|os.O_RDWR, 0600)
	if err == nil {
		return f, nil
	}
	if !errors.Is(err, os.ErrExist) {
		return nil, err
	}
	before, err := root.Lstat(cleanerLock)
	if err != nil {
		return nil, err
	}
	if !before.Mode().IsRegular() {
		return nil, fmt.Errorf("scratch cleanup lock is not a regular file")
	}
	f, err = root.OpenFile(cleanerLock, os.O_RDWR, 0600)
	if err != nil {
		return nil, err
	}
	actual, err := f.Stat()
	if err != nil || !os.SameFile(before, actual) {
		f.Close()
		return nil, fmt.Errorf("scratch cleanup lock changed while opening")
	}
	return f, nil
}

func openVerifiedDirectory(root *os.Root, name string) (*os.Root, error) {
	if !filepath.IsLocal(name) || filepath.Clean(name) != name {
		return nil, fmt.Errorf("unsafe scratch directory path")
	}
	before, err := root.Lstat(name)
	if err != nil {
		return nil, err
	}
	if !before.IsDir() || before.Mode()&os.ModeSymlink != 0 {
		return nil, fmt.Errorf("refusing scratch cleanup: %s is not a real directory", name)
	}
	opened, err := root.OpenRoot(name)
	if err != nil {
		return nil, err
	}
	actual, err := opened.Stat(".")
	if err != nil || !os.SameFile(before, actual) {
		opened.Close()
		return nil, fmt.Errorf("scratch directory changed while opening: %s", name)
	}
	current, err := root.Lstat(name)
	if err != nil || current.Mode()&os.ModeSymlink != 0 || !os.SameFile(actual, current) {
		opened.Close()
		return nil, fmt.Errorf("scratch directory changed while opening: %s", name)
	}
	return opened, nil
}

func validateMarker(area *os.Root, project, id string, allowMissing bool) error {
	before, err := area.Lstat(ownerMarker)
	if allowMissing && errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("refusing scratch cleanup without owner marker: %w", err)
	}
	if !before.Mode().IsRegular() {
		return fmt.Errorf("refusing scratch cleanup: owner marker is not a regular file")
	}
	f, err := area.Open(ownerMarker)
	if err != nil {
		return err
	}
	defer f.Close()
	actual, err := f.Stat()
	if err != nil {
		return err
	}
	if !os.SameFile(before, actual) {
		return fmt.Errorf("scratch owner marker changed while opening")
	}
	data, err := io.ReadAll(io.LimitReader(f, 4097))
	if err != nil {
		return err
	}
	if len(data) > 4096 {
		return fmt.Errorf("scratch owner marker is too large")
	}
	var mark marker
	if err := json.Unmarshal(data, &mark); err != nil {
		return fmt.Errorf("invalid scratch owner marker: %w", err)
	}
	if mark.ProjectID != project || mark.AreaID != id {
		return fmt.Errorf("refusing scratch cleanup with mismatched owner marker: %s", id)
	}
	return nil
}

func rejectWorktree(dir *os.Root) error {
	_, err := dir.Lstat(".git")
	if err == nil {
		return fmt.Errorf("refusing scratch cleanup of a Git repository or worktree")
	}
	if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}

func openCleanupDirectory(area *os.Root, path string) (*os.Root, error) {
	if !filepath.IsLocal(path) || filepath.Clean(path) != path {
		return nil, fmt.Errorf("invalid saved scratch cleanup directory")
	}
	dir, err := openVerifiedDirectory(area, ".")
	if err != nil {
		return nil, err
	}
	if err := rejectWorktree(dir); err != nil {
		dir.Close()
		return nil, err
	}
	if path == "." {
		return dir, nil
	}
	for _, part := range strings.Split(path, string(filepath.Separator)) {
		child, err := openVerifiedDirectory(dir, part)
		dir.Close()
		if err != nil {
			return nil, err
		}
		dir = child
		if err := rejectWorktree(dir); err != nil {
			dir.Close()
			return nil, err
		}
	}
	return dir, nil
}

func (m Manager) cleanupReserved(root *os.Root, a store.ScratchArea, budget *cleanupBudget) (result Result, err error) {
	result = Result{Area: a, Path: filepath.Join(m.Root, a.ID), State: "deleting"}
	finished := false
	defer func() {
		if !finished {
			releaseErr := m.Store.ScratchCleanupRelease(a.ID, a.CleanupToken)
			if err == nil && releaseErr != nil {
				err = releaseErr
			}
		}
	}()
	if !store.ValidAreaID(a.ID) {
		return result, fmt.Errorf("refusing invalid scratch area ID")
	}
	if root == nil {
		if !budget.take() {
			return result, nil
		}
		if err = m.Store.ScratchCleanupFinish(a.ID, a.CleanupToken); err != nil {
			return result, err
		}
		finished = true
		result = removedResult(m, a, 0)
		return result, nil
	}
	area, err := openVerifiedDirectory(root, a.ID)
	if errors.Is(err, os.ErrNotExist) {
		if !budget.take() {
			return result, nil
		}
		if err = m.Store.ScratchCleanupFinish(a.ID, a.CleanupToken); err != nil {
			return result, err
		}
		finished = true
		return removedResult(m, a, 0), nil
	}
	if err != nil {
		return result, err
	}
	defer area.Close()
	if err = validateMarker(area, m.Store.ProjectID, a.ID, a.CleanupFinalizing); err != nil {
		return result, err
	}
	if err = rejectWorktree(area); err != nil {
		return result, err
	}
	cursor := a.CleanupCursor
	if cursor == "" {
		cursor = "."
	}
	finalizing := a.CleanupFinalizing
	removed := 0
	if !finalizing {
		cursor, finalizing, removed, err = m.removeBatch(area, a, budget, cursor)
		result.EntriesRemoved = removed
		if saveErr := m.Store.ScratchCleanupSave(a.ID, a.CleanupToken, cursor, finalizing); saveErr != nil {
			return result, errors.Join(err, saveErr)
		}
		if err != nil {
			return result, err
		}
	}
	if finalizing {
		done, count, finalErr := finishDirectory(root, area, a.ID, budget)
		removed += count
		result.EntriesRemoved = removed
		if finalErr != nil {
			return result, finalErr
		}
		if done {
			if err = m.Store.ScratchCleanupFinish(a.ID, a.CleanupToken); err != nil {
				return result, err
			}
			finished = true
			return removedResult(m, a, removed), nil
		}
	}
	return result, nil
}

func removedResult(m Manager, a store.ScratchArea, count int) Result {
	a.RemovedAt = m.Store.Now().UTC().Format(time.RFC3339Nano)
	a.CleanupToken = ""
	a.CleanupLeaseUntil = ""
	a.CleanupCursor = ""
	a.CleanupFinalizing = false
	return Result{Area: a, Path: filepath.Join(m.Root, a.ID), State: "removed", EntriesRemoved: count}
}

func (m Manager) removeBatch(area *os.Root, a store.ScratchArea, budget *cleanupBudget, cursor string) (string, bool, int, error) {
	var dir *os.Root
	var reader *os.File
	closeDir := func() {
		if reader != nil {
			reader.Close()
			reader = nil
		}
		if dir != nil {
			dir.Close()
			dir = nil
		}
	}
	defer closeDir()
	removed := 0
	for budget.take() {
		if dir == nil {
			var err error
			dir, err = openCleanupDirectory(area, cursor)
			if errors.Is(err, os.ErrNotExist) && cursor != "." {
				cursor = filepath.Dir(cursor)
				continue
			}
			if err != nil {
				return cursor, false, removed, err
			}
			reader, err = dir.Open(".")
			if err != nil {
				return cursor, false, removed, err
			}
		}
		entries, err := reader.ReadDir(1)
		if err != nil && !errors.Is(err, io.EOF) {
			return cursor, false, removed, err
		}
		if len(entries) == 0 {
			closeDir()
			if cursor == "." {
				// Persist this before removing the last ownership marker, so an interrupted final unlink is recoverable.
				if err := m.Store.ScratchCleanupSave(a.ID, a.CleanupToken, ".", true); err != nil {
					return cursor, false, removed, err
				}
				return ".", true, removed, nil
			}
			if err := area.Remove(cursor); err != nil && !errors.Is(err, os.ErrNotExist) {
				return cursor, false, removed, err
			} else if err == nil {
				removed++
			}
			cursor = filepath.Dir(cursor)
			continue
		}
		name := entries[0].Name()
		if cursor == "." && name == ownerMarker {
			budget.remaining++
			continue
		}
		st, err := dir.Lstat(name)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return cursor, false, removed, err
		}
		if st.IsDir() && st.Mode()&os.ModeSymlink == 0 {
			cursor = filepath.Join(cursor, name)
			closeDir()
			continue
		}
		if err := dir.Remove(name); err != nil && !errors.Is(err, os.ErrNotExist) {
			return cursor, false, removed, err
		} else if err == nil {
			removed++
		}
	}
	return cursor, false, removed, nil
}

func finishDirectory(root, area *os.Root, id string, budget *cleanupBudget) (bool, int, error) {
	if err := rejectWorktree(area); err != nil {
		return false, 0, err
	}
	current, err := root.Lstat(id)
	if errors.Is(err, os.ErrNotExist) {
		return true, 0, nil
	}
	if err != nil {
		return false, 0, err
	}
	actual, err := area.Stat(".")
	if err != nil {
		return false, 0, err
	}
	if !current.IsDir() || current.Mode()&os.ModeSymlink != 0 || !os.SameFile(current, actual) {
		return false, 0, fmt.Errorf("scratch directory changed before final removal")
	}
	reader, err := area.Open(".")
	if err != nil {
		return false, 0, err
	}
	entries, readErr := reader.ReadDir(2)
	reader.Close()
	if readErr != nil && !errors.Is(readErr, io.EOF) {
		return false, 0, readErr
	}
	for _, entry := range entries {
		if entry.Name() != ownerMarker {
			return false, 0, fmt.Errorf("scratch area changed after cleanup reached its final stage")
		}
	}
	removed := 0
	if _, err := area.Lstat(ownerMarker); err == nil {
		if !budget.take() {
			return false, removed, nil
		}
		if err := area.Remove(ownerMarker); err != nil && !errors.Is(err, os.ErrNotExist) {
			return false, removed, err
		} else if err == nil {
			removed++
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return false, removed, err
	}
	if !budget.take() {
		return false, removed, nil
	}
	if err := root.Remove(id); err != nil && !errors.Is(err, os.ErrNotExist) {
		return false, removed, err
	} else if err == nil {
		removed++
	}
	return true, removed, nil
}
