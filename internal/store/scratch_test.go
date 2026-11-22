package store

import (
	"path/filepath"
	"sync"
	"testing"
	"time"
)

const testScratchID = "A0123456789abcdef0123456789abcdef"

func TestScratchCleanupClaimsAreExclusiveAndFenced(t *testing.T) {
	path := filepath.Join(t.TempDir(), "tasks.sqlite")
	first, err := Open(path, "scratch-claims")
	if err != nil {
		t.Fatal(err)
	}
	defer first.Close()
	second, err := Open(path, "scratch-claims")
	if err != nil {
		t.Fatal(err)
	}
	defer second.Close()
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	first.Now = func() time.Time { return now }
	second.Now = first.Now
	task := mustCreateTask(t, first, CreateSpec{Title: "Disposable work"})
	if err := first.ScratchCreate(task.ID, "", testScratchID); err != nil {
		t.Fatal(err)
	}
	if _, err := first.Cancel(task.ID, "", "Finished with scratch"); err != nil {
		t.Fatal(err)
	}
	start := make(chan struct{})
	claims := make(chan []ScratchArea, 2)
	errors := make(chan error, 2)
	var wg sync.WaitGroup
	for _, s := range []*Store{first, second} {
		wg.Add(1)
		go func(s *Store) {
			defer wg.Done()
			<-start
			areas, err := s.ScratchCleanupCandidates(0, true, 1)
			claims <- areas
			errors <- err
		}(s)
	}
	close(start)
	wg.Wait()
	close(claims)
	close(errors)
	for err := range errors {
		if err != nil {
			t.Fatal(err)
		}
	}
	var claimed []ScratchArea
	for areas := range claims {
		claimed = append(claimed, areas...)
	}
	if len(claimed) != 1 || claimed[0].CleanupToken == "" {
		t.Fatalf("concurrent cleaners obtained %d claims: %+v", len(claimed), claimed)
	}
	old := claimed[0]
	now = now.Add(scratchCleanupLease + time.Second)
	areas, err := second.ScratchCleanupCandidates(0, true, 1)
	if err != nil || len(areas) != 1 || areas[0].CleanupToken == old.CleanupToken {
		t.Fatalf("expired claim was not replaced: %+v %v", areas, err)
	}
	assertCoreError(t, first.ScratchCleanupSave(old.ID, old.CleanupToken, ".", true), "conflict")
	assertCoreError(t, first.ScratchCleanupRelease(old.ID, old.CleanupToken), "conflict")
	assertCoreError(t, first.ScratchCleanupFinish(old.ID, old.CleanupToken), "conflict")
	if err := second.ScratchCleanupSave(areas[0].ID, areas[0].CleanupToken, "nested", false); err != nil {
		t.Fatal(err)
	}
	if err := second.ScratchCleanupRelease(areas[0].ID, areas[0].CleanupToken); err != nil {
		t.Fatal(err)
	}
	newTask := mustCreateTask(t, first, CreateSpec{Title: "New active work"})
	assertCoreError(t, first.ScratchAttach(newTask.ID, "", old.ID), "conflict")
	areas, err = first.ScratchCleanupCandidates(365*24*time.Hour, true, 1)
	if err != nil || len(areas) != 1 || areas[0].CleanupCursor != "nested" {
		t.Fatalf("partial cleanup lost its durable marker/cursor: %+v %v", areas, err)
	}
	if err := first.ScratchCleanupFinish(areas[0].ID, areas[0].CleanupToken); err != nil {
		t.Fatal(err)
	}
	assertCoreError(t, first.ScratchAttach(newTask.ID, "", old.ID), "conflict")
}

func TestScratchRetentionFollowsReplacementReferencesAndGrace(t *testing.T) {
	s := newTaskTestStore(t)
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	s.Now = func() time.Time { return now }
	original := mustCreateTask(t, s, CreateSpec{Title: "Original"})
	if err := s.ScratchCreate(original.ID, "", testScratchID); err != nil {
		t.Fatal(err)
	}
	now = now.Add(30 * 24 * time.Hour)
	assertNone := func(grace time.Duration) {
		t.Helper()
		areas, err := s.ScratchCleanupCandidates(grace, false, 20)
		if err != nil || len(areas) != 0 {
			t.Fatalf("live or recent area became eligible: %+v %v", areas, err)
		}
	}
	assertNone(0)
	children, err := s.Evolve(original.ID, "", original.Revision, EvolveSpec{Summary: "Split", Replacements: []CreateSpec{{Title: "First", Brief: "First work"}, {Title: "Second", Brief: "Second work"}}})
	if err != nil {
		t.Fatal(err)
	}
	for _, child := range children {
		areas, _, err := s.ScratchListPage(child.ID, 200, 0)
		if err != nil || len(areas) != 1 || areas[0].ID != testScratchID {
			t.Fatalf("replacement lost scratch reference: %+v %v", areas, err)
		}
	}
	if _, err := s.Cancel(children[0].ID, "", "Done"); err != nil {
		t.Fatal(err)
	}
	now = now.Add(30 * 24 * time.Hour)
	assertNone(0)
	if _, err := s.Cancel(children[1].ID, "", "Done"); err != nil {
		t.Fatal(err)
	}
	now = now.Add(7*24*time.Hour - time.Nanosecond)
	assertNone(7 * 24 * time.Hour)
	now = now.Add(time.Nanosecond)
	areas, err := s.ScratchCleanupCandidates(7*24*time.Hour, false, 20)
	if err != nil || len(areas) != 1 || areas[0].CleanupToken != "" {
		t.Fatalf("grace boundary/dry selection incorrect: %+v %v", areas, err)
	}
	active := mustCreateTask(t, s, CreateSpec{Title: "Reuse before deletion begins"})
	if err := s.ScratchAttach(active.ID, "", testScratchID); err != nil {
		t.Fatal(err)
	}
	assertNone(0)
	areas, _, err = s.ScratchListPage(active.ID, 200, 0)
	if err != nil || areas[0].UnreferencedAt != "" {
		t.Fatalf("reattachment did not reset grace: %+v %v", areas, err)
	}
}

func TestScratchFinalizingIsMonotonicAndFinishChecksLiveReferences(t *testing.T) {
	s := newTaskTestStore(t)
	task := mustCreateTask(t, s, CreateSpec{Title: "Scratch"})
	if err := s.ScratchCreate(task.ID, "", testScratchID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Cancel(task.ID, "", "Done"); err != nil {
		t.Fatal(err)
	}
	areas, err := s.ScratchCleanupCandidates(0, true, 1)
	if err != nil || len(areas) != 1 {
		t.Fatalf("reservation = %+v %v", areas, err)
	}
	a := areas[0]
	assertCoreError(t, s.ScratchCleanupSave(a.ID, a.CleanupToken, "../escape", false), "invalid_input")
	if err := s.ScratchCleanupSave(a.ID, a.CleanupToken, ".", true); err != nil {
		t.Fatal(err)
	}
	assertCoreError(t, s.ScratchCleanupSave(a.ID, a.CleanupToken, "nested", false), "conflict")
	active := mustCreateTask(t, s, CreateSpec{Title: "Restored active reference"})
	n, _ := taskNumber(active.ID)
	if _, err := s.DB.Exec("INSERT INTO scratch_refs(task_id,area_id) VALUES(?,?)", n, a.ID); err != nil {
		t.Fatal(err)
	}
	assertCoreError(t, s.ScratchCleanupFinish(a.ID, a.CleanupToken), "conflict")
}

func TestScratchChangesDoNotRefreshUnrelatedAreas(t *testing.T) {
	s := newTaskTestStore(t)
	first := mustCreateTask(t, s, CreateSpec{Title: "Changed work"})
	other := mustCreateTask(t, s, CreateSpec{Title: "Unrelated work"})
	const otherArea = "A11111111111111111111111111111111"
	if err := s.ScratchCreate(first.ID, "", testScratchID); err != nil {
		t.Fatal(err)
	}
	if err := s.ScratchCreate(other.ID, "", otherArea); err != nil {
		t.Fatal(err)
	}
	if _, err := s.DB.Exec(`CREATE TRIGGER unrelated_scratch BEFORE UPDATE ON scratch_areas
 WHEN OLD.id='` + otherArea + `' BEGIN SELECT RAISE(ABORT,'unrelated scratch was updated'); END;`); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Cancel(first.ID, "", "Finished"); err != nil {
		t.Fatal(err)
	}
	current := mustCreateTask(t, s, CreateSpec{Title: "Continue using scratch"})
	if err := s.ScratchAttach(current.ID, "", testScratchID); err != nil {
		t.Fatal(err)
	}
	for _, reserve := range []bool{false, true} {
		areas, err := s.ScratchCleanupCandidates(0, reserve, 1)
		if err != nil || len(areas) != 0 {
			t.Fatalf("active scratch changed during cleanup: %v %v", areas, err)
		}
	}
	children, err := s.Evolve(current.ID, "", current.Revision, EvolveSpec{Summary: "Fresh remaining work", Replacements: []CreateSpec{{Title: "Remaining work", Brief: "Finish the work using its scratch"}}})
	if err != nil {
		t.Fatal(err)
	}
	child := mustClaimTask(t, s, children[0].ID, "worker")
	mustCompleteTask(t, s, child)
	areas, err := s.ScratchCleanupCandidates(0, true, 1)
	if err != nil || len(areas) != 1 || areas[0].ID != testScratchID {
		t.Fatalf("affected scratch did not become eligible: %v %v", areas, err)
	}
}
