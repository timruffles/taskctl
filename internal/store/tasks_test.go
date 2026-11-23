package store

import (
	"database/sql"
	"path/filepath"
	"reflect"
	"sync"
	"testing"
	"time"
)

func newTaskTestStore(t *testing.T) *Store {
	t.Helper()
	s, err := Open(filepath.Join(t.TempDir(), "tasks.sqlite"), "core-tests")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

func mustCreateTask(t *testing.T, s *Store, spec CreateSpec) Task {
	t.Helper()
	task, err := s.Create(spec)
	if err != nil {
		t.Fatal(err)
	}
	return task
}

func mustClaimTask(t *testing.T, s *Store, id, owner string) Task {
	t.Helper()
	task, err := s.Claim(id, owner)
	if err != nil {
		t.Fatal(err)
	}
	return task
}

func mustCompleteTask(t *testing.T, s *Store, task Task) Task {
	t.Helper()
	completed, err := s.Complete(task.ID, task.ClaimID, "Verified the work")
	if err != nil {
		t.Fatal(err)
	}
	return completed
}

func coreEventCount(t *testing.T, s *Store) int {
	t.Helper()
	var count int
	if err := s.DB.QueryRow("SELECT COUNT(*) FROM events").Scan(&count); err != nil {
		t.Fatal(err)
	}
	return count
}

func attachCoreScratch(t *testing.T, s *Store, taskID int64) {
	t.Helper()
	if _, err := s.DB.Exec("INSERT INTO scratch_areas(id,created_at) VALUES('A1',?)", timestamp(s.Now())); err != nil {
		t.Fatal(err)
	}
	if _, err := s.DB.Exec("INSERT INTO scratch_refs(task_id,area_id) VALUES(?,'A1')", taskID); err != nil {
		t.Fatal(err)
	}
}

func assertCoreError(t *testing.T, err error, code string) {
	t.Helper()
	if err == nil || ErrorCode(err) != code {
		t.Fatalf("error = %v; want code %s", err, code)
	}
}

func TestCoreClaimsAcrossIndependentConnections(t *testing.T) {
	path := filepath.Join(t.TempDir(), "tasks.sqlite")
	const clients = 8
	stores := make([]*Store, clients)
	for i := range stores {
		var err error
		stores[i], err = Open(path, "claim-race")
		if err != nil {
			t.Fatal(err)
		}
		defer stores[i].Close()
	}
	task := mustCreateTask(t, stores[0], CreateSpec{Title: "One owner"})
	start := make(chan struct{})
	results := make(chan error, clients)
	var wg sync.WaitGroup
	for _, s := range stores {
		wg.Add(1)
		go func(s *Store) {
			defer wg.Done()
			<-start
			_, err := s.Claim(task.ID, "same-session")
			results <- err
		}(s)
	}
	close(start)
	wg.Wait()
	close(results)
	winners := 0
	for err := range results {
		if err == nil {
			winners++
		} else {
			assertCoreError(t, err, "conflict")
		}
	}
	if winners != 1 {
		t.Fatalf("successful claims = %d; want 1", winners)
	}
	if got := coreEventCount(t, stores[0]); got != 3 {
		t.Fatalf("events = %d; want created, available and one claimed", got)
	}
}

func TestCoreEveryOwnerMutationRejectsReplacedClaim(t *testing.T) {
	s := newTaskTestStore(t)
	original := mustClaimTask(t, s, mustCreateTask(t, s, CreateSpec{Title: "Owned task"}).ID, "worker-a")
	original, err := s.Progress(original.ID, original.ClaimID, "Checkpoint before handoff")
	if err != nil {
		t.Fatal(err)
	}
	dependency := mustCreateTask(t, s, CreateSpec{Title: "Dependency"})
	current, err := s.Takeover(original.ID, original.ClaimID, "worker-b", "Worker A explicitly stopped")
	if err != nil {
		t.Fatal(err)
	}
	if current.ClaimID == original.ClaimID || current.Progress != original.Progress {
		t.Fatalf("takeover lost fencing or checkpoint: %+v", current)
	}
	before := coreEventCount(t, s)
	changedTitle := "Stale change"
	cases := map[string]func() error{
		"progress":  func() error { _, err := s.Progress(original.ID, original.ClaimID, "stale"); return err },
		"heartbeat": func() error { return s.Heartbeat(original.ID, original.ClaimID) },
		"complete":  func() error { _, err := s.Complete(original.ID, original.ClaimID, "stale"); return err },
		"cancel":    func() error { _, err := s.Cancel(original.ID, original.ClaimID, "stale"); return err },
		"release":   func() error { _, err := s.Release(original.ID, original.ClaimID); return err },
		"takeover":  func() error { _, err := s.Takeover(original.ID, original.ClaimID, "worker-c", "stale"); return err },
		"edit": func() error {
			_, err := s.Edit(original.ID, original.ClaimID, current.Revision, &changedTitle, nil)
			return err
		},
		"tag":          func() error { return s.Tag(original.ID, original.ClaimID, "stale", false) },
		"metadata":     func() error { return s.MetadataSet(original.ID, original.ClaimID, "notes", "stale") },
		"prerequisite": func() error { return s.AddPrerequisite(original.ID, original.ClaimID, dependency.ID, false) },
		"evolve": func() error {
			_, err := s.Evolve(original.ID, original.ClaimID, current.Revision, EvolveSpec{Summary: "stale", Replacements: []CreateSpec{{Title: "Replacement", Brief: "Independent remaining work"}}})
			return err
		},
	}
	for name, action := range cases {
		t.Run(name, func(t *testing.T) { assertCoreError(t, action(), "conflict") })
	}
	after, err := s.Get(original.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(current, after) || before != coreEventCount(t, s) {
		t.Fatal("a rejected stale owner changed task state or events")
	}
}

func TestCoreHeartbeatDoesNotAdvanceRevisionOrExpireClaims(t *testing.T) {
	s := newTaskTestStore(t)
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	s.Now = func() time.Time { return now }
	task := mustClaimTask(t, s, mustCreateTask(t, s, CreateSpec{Title: "Long calculation"}).ID, "worker")
	count := coreEventCount(t, s)
	now = now.Add(24 * time.Hour)
	_, err := s.Claim(task.ID, "another-worker")
	assertCoreError(t, err, "conflict")
	if err := s.Heartbeat(task.ID, task.ClaimID); err != nil {
		t.Fatal(err)
	}
	after, err := s.Get(task.ID)
	if err != nil {
		t.Fatal(err)
	}
	if after.Revision != task.Revision || after.UpdatedAt != task.UpdatedAt || after.LastActivityAt != timestamp(now) || coreEventCount(t, s) != count {
		t.Fatalf("heartbeat changed revision/events or missed activity: %+v", after)
	}
}

func TestCoreReleasePreservesProgressAndInvalidatesOldClaim(t *testing.T) {
	s := newTaskTestStore(t)
	task := mustClaimTask(t, s, mustCreateTask(t, s, CreateSpec{Title: "Resumable"}).ID, "worker")
	task, err := s.Progress(task.ID, task.ClaimID, "Result saved in scratch A1")
	if err != nil {
		t.Fatal(err)
	}
	released, err := s.Release(task.ID, task.ClaimID)
	if err != nil {
		t.Fatal(err)
	}
	if released.Status != "pending" || released.ClaimID != "" || released.Owner != "" || released.Progress != task.Progress {
		t.Fatalf("bad release: %+v", released)
	}
	assertCoreError(t, s.Tag(task.ID, task.ClaimID, "old-owner", false), "conflict")
	reclaimed := mustClaimTask(t, s, task.ID, "worker")
	if reclaimed.ClaimID == task.ClaimID {
		t.Fatal("released claim was reused")
	}
}

func TestCoreEvolveTransfersContextAndFollowsAllReplacements(t *testing.T) {
	s := newTaskTestStore(t)
	prerequisite := mustCompleteTask(t, s, mustClaimTask(t, s, mustCreateTask(t, s, CreateSpec{Title: "Design"}).ID, "planner"))
	original := mustCreateTask(t, s, CreateSpec{Title: "Implementation", Brief: "Original full scope", Tags: []string{"feature", "backend"}, Metadata: map[string]string{"component": "store", "piece": "original"}, Prerequisites: []string{prerequisite.ID}})
	original = mustClaimTask(t, s, original.ID, "worker")
	original, err := s.Progress(original.ID, original.ClaimID, "Storage foundation implemented")
	if err != nil {
		t.Fatal(err)
	}
	originalNumber, _ := taskNumber(original.ID)
	attachCoreScratch(t, s, originalNumber)
	dependent := mustCreateTask(t, s, CreateSpec{Title: "Integration review", Prerequisites: []string{original.ID}})
	children, err := s.Evolve(original.ID, original.ClaimID, original.Revision, EvolveSpec{
		Summary:      "Storage foundation is complete; finish transactions and verification",
		ContinueWith: "transactions",
		Replacements: []CreateSpec{
			{Key: "transactions", Title: "Finish transactions", Brief: "Implement atomic transaction checks on the completed foundation", Tags: []string{"concurrency"}, Metadata: map[string]string{"piece": "transactions"}, Prerequisites: []string{prerequisite.ID}},
			{Key: "verification", Title: "Verify transactions", Brief: "Test the transaction API and retain results in scratch A1", Prerequisites: []string{"@transactions"}},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(children) != 2 || children[0].Owner != "worker" || children[0].ClaimID == original.ClaimID || children[0].ClaimID == "" || children[1].Status != "pending" {
		t.Fatalf("unexpected replacements: %+v", children)
	}
	if !reflect.DeepEqual(children[0].Tags, []string{"backend", "concurrency", "feature"}) || children[0].Metadata["component"] != "store" || children[0].Metadata["piece"] != "transactions" {
		t.Fatalf("inheritance failed: %+v", children[0])
	}
	if !reflect.DeepEqual(children[0].Prerequisites, []string{prerequisite.ID}) || !reflect.DeepEqual(children[1].Prerequisites, []string{prerequisite.ID, children[0].ID}) {
		t.Fatalf("incorrect prerequisites: %+v", children)
	}
	for _, child := range children {
		if child.OriginID != original.ID || child.ParentID != original.ID {
			t.Fatalf("lineage missing: %+v", child)
		}
		var refs int
		n, _ := taskNumber(child.ID)
		if err := s.DB.QueryRow("SELECT COUNT(*) FROM scratch_refs WHERE task_id=? AND area_id='A1'", n).Scan(&refs); err != nil || refs != 1 {
			t.Fatalf("scratch reference not inherited: count=%d err=%v", refs, err)
		}
	}
	old, err := s.Get(original.ID)
	if err != nil {
		t.Fatal(err)
	}
	if old.Status != "evolved" || old.ClaimID != "" || old.Progress != original.Progress || old.Result == "" {
		t.Fatalf("original history was lost: %+v", old)
	}
	_, err = s.Complete(original.ID, original.ClaimID, "stale completion")
	assertCoreError(t, err, "conflict")
	_, err = s.Claim(dependent.ID, "reviewer")
	assertCoreError(t, err, "blocked")
	mustCompleteTask(t, s, children[0])
	if outcome, err := s.Outcome(original.ID); err != nil || outcome != "pending" {
		t.Fatalf("partial replacement outcome = %q, %v", outcome, err)
	}
	mustCompleteTask(t, s, mustClaimTask(t, s, children[1].ID, "tester"))
	if outcome, err := s.Outcome(original.ID); err != nil || outcome != "completed" {
		t.Fatalf("all replacement outcome = %q, %v", outcome, err)
	}
	mustClaimTask(t, s, dependent.ID, "reviewer")
	old, _ = s.Get(original.ID)
	if old.Status != "evolved" {
		t.Fatal("aggregate completion rewrote historical superseded status")
	}
	history, more, err := s.HistoryPage(children[1].ID, 20, 0)
	if err != nil || more || len(history) != 3 || history[0].ID != original.ID {
		t.Fatalf("lineage history = %+v, %v", history, err)
	}
	var unreferenced sql.NullString
	if err := s.DB.QueryRow("SELECT unreferenced_at FROM scratch_areas WHERE id='A1'").Scan(&unreferenced); err != nil || !unreferenced.Valid {
		t.Fatalf("scratch not released after final successor: %+v %v", unreferenced, err)
	}
}

func TestCoreEvolveFailureRollsBackTasksEventsAndOwnership(t *testing.T) {
	tests := []struct {
		name string
		spec func(Task, Task) EvolveSpec
		code string
	}{
		{"unknown sibling", func(a, b Task) EvolveSpec {
			return EvolveSpec{Summary: "Split", Replacements: []CreateSpec{{Title: "Piece", Brief: "Remaining work", Prerequisites: []string{"@missing"}}}}
		}, "invalid_input"},
		{"sibling cycle", func(a, b Task) EvolveSpec {
			return EvolveSpec{Summary: "Split", Replacements: []CreateSpec{{Key: "a", Title: "A", Brief: "A work", Prerequisites: []string{"@b"}}, {Key: "b", Title: "B", Brief: "B work", Prerequisites: []string{"@a"}}}}
		}, "conflict"},
		{"ancestor cycle", func(a, b Task) EvolveSpec {
			return EvolveSpec{Summary: "Split", Replacements: []CreateSpec{{Title: "Piece", Brief: "Remaining work", Prerequisites: []string{a.ID}}}}
		}, "conflict"},
		{"downstream cycle", func(a, b Task) EvolveSpec {
			return EvolveSpec{Summary: "Split", Replacements: []CreateSpec{{Title: "Piece", Brief: "Remaining work", Prerequisites: []string{b.ID}}}}
		}, "conflict"},
		{"blocked continuation", func(a, b Task) EvolveSpec {
			return EvolveSpec{Summary: "Split", ContinueWith: "b", Replacements: []CreateSpec{{Key: "a", Title: "A", Brief: "A work"}, {Key: "b", Title: "B", Brief: "B work", Prerequisites: []string{"@a"}}}}
		}, "blocked"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			s := newTaskTestStore(t)
			a := mustClaimTask(t, s, mustCreateTask(t, s, CreateSpec{Title: "Original"}).ID, "worker")
			b := mustCreateTask(t, s, CreateSpec{Title: "Downstream", Prerequisites: []string{a.ID}})
			before := coreEventCount(t, s)
			_, err := s.Evolve(a.ID, a.ClaimID, a.Revision, test.spec(a, b))
			assertCoreError(t, err, test.code)
			after, err := s.Get(a.ID)
			if err != nil || !reflect.DeepEqual(a, after) {
				t.Fatalf("failed evolution changed original: %+v, %v", after, err)
			}
			_, total, err := s.List("all", nil, nil, 20, 0)
			if err != nil || total != 2 || coreEventCount(t, s) != before {
				t.Fatalf("failed evolution leaked tasks/events: total=%d err=%v", total, err)
			}
		})
	}
}

func TestCoreDependenciesUseReplacementEdgesForCycleDetection(t *testing.T) {
	s := newTaskTestStore(t)
	a := mustCreateTask(t, s, CreateSpec{Title: "Original"})
	children, err := s.Evolve(a.ID, "", a.Revision, EvolveSpec{Summary: "Replanned", Replacements: []CreateSpec{{Title: "Successor", Brief: "Remaining work"}}})
	if err != nil {
		t.Fatal(err)
	}
	b := mustCreateTask(t, s, CreateSpec{Title: "Downstream", Prerequisites: []string{a.ID}})
	before := coreEventCount(t, s)
	assertCoreError(t, s.AddPrerequisite(children[0].ID, "", b.ID, false), "conflict")
	child, err := s.Get(children[0].ID)
	if err != nil || len(child.Prerequisites) != 0 || coreEventCount(t, s) != before {
		t.Fatalf("effective cycle was not rolled back: %+v %v", child, err)
	}
}

func TestCoreNestedCancellationDominatesPendingSibling(t *testing.T) {
	s := newTaskTestStore(t)
	a := mustCreateTask(t, s, CreateSpec{Title: "Original"})
	children, err := s.Evolve(a.ID, "", a.Revision, EvolveSpec{Summary: "Split", Replacements: []CreateSpec{{Title: "A", Brief: "A scope"}, {Title: "B", Brief: "B scope"}}})
	if err != nil {
		t.Fatal(err)
	}
	grandchildren, err := s.Evolve(children[0].ID, "", children[0].Revision, EvolveSpec{Summary: "Refine", Replacements: []CreateSpec{{Title: "A1", Brief: "A1 scope"}, {Title: "A2", Brief: "A2 scope"}}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Cancel(grandchildren[1].ID, "", "No longer needed"); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{a.ID, children[0].ID, grandchildren[1].ID} {
		if outcome, err := s.Outcome(id); err != nil || outcome != "cancelled" {
			t.Fatalf("outcome(%s) = %q, %v", id, outcome, err)
		}
	}
	if outcome, err := s.Outcome(children[1].ID); err != nil || outcome != "pending" {
		t.Fatalf("sibling outcome = %q, %v", outcome, err)
	}
}

func TestCoreEventFailureRollsBackCompletionAndScratchRelease(t *testing.T) {
	s := newTaskTestStore(t)
	task := mustClaimTask(t, s, mustCreateTask(t, s, CreateSpec{Title: "Atomic event"}).ID, "worker")
	n, _ := taskNumber(task.ID)
	attachCoreScratch(t, s, n)
	if _, err := s.DB.Exec(`CREATE TRIGGER refuse_completion BEFORE INSERT ON events WHEN NEW.kind='completed' BEGIN SELECT RAISE(ABORT,'simulated event failure'); END;`); err != nil {
		t.Fatal(err)
	}
	before := coreEventCount(t, s)
	if _, err := s.Complete(task.ID, task.ClaimID, "All tests passed"); err == nil {
		t.Fatal("completion succeeded despite event failure")
	}
	after, err := s.Get(task.ID)
	if err != nil || !reflect.DeepEqual(task, after) || coreEventCount(t, s) != before {
		t.Fatalf("completion escaped rollback: %+v %v", after, err)
	}
	var unreferenced sql.NullString
	if err := s.DB.QueryRow("SELECT unreferenced_at FROM scratch_areas WHERE id='A1'").Scan(&unreferenced); err != nil || unreferenced.Valid {
		t.Fatalf("scratch became reclaimable: %+v %v", unreferenced, err)
	}
}

func TestCoreRevisionChecksAndTerminalImmutability(t *testing.T) {
	s := newTaskTestStore(t)
	task := mustCreateTask(t, s, CreateSpec{Title: "Planning", Brief: "Keep this text"})
	if err := s.Tag(task.ID, "", "planned", false); err != nil {
		t.Fatal(err)
	}
	title := "Stale edit"
	_, err := s.Edit(task.ID, "", task.Revision, &title, nil)
	assertCoreError(t, err, "conflict")
	_, err = s.Evolve(task.ID, "", task.Revision, EvolveSpec{Summary: "Stale replan", Replacements: []CreateSpec{{Title: "Replacement", Brief: "Remaining work"}}})
	assertCoreError(t, err, "conflict")
	claimed := mustClaimTask(t, s, task.ID, "worker")
	completed := mustCompleteTask(t, s, claimed)
	_, err = s.Cancel(task.ID, "", "Too late")
	assertCoreError(t, err, "conflict")
	_, err = s.Edit(task.ID, "", completed.Revision, &title, nil)
	assertCoreError(t, err, "conflict")
	assertCoreError(t, s.Tag(task.ID, "", "late", false), "conflict")
	assertCoreError(t, s.MetadataSet(task.ID, "", "note", "late"), "conflict")
}

func TestCoreListFiltersPaginationAndProjection(t *testing.T) {
	s := newTaskTestStore(t)
	a := mustCreateTask(t, s, CreateSpec{Title: "Available", Brief: "Do not include in lists", Tags: []string{"x", "y"}, Metadata: map[string]string{"large": "Do not include in lists"}})
	b := mustCreateTask(t, s, CreateSpec{Title: "Blocked", Tags: []string{"x"}, Prerequisites: []string{a.ID}})
	c := mustClaimTask(t, s, mustCreateTask(t, s, CreateSpec{Title: "Owned", Tags: []string{"x", "y"}}).ID, "worker")
	if _, err := s.Progress(c.ID, c.ClaimID, "Do not include in lists"); err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		filter string
		ids    []string
	}{
		{"available", []string{a.ID}}, {"blocked", []string{b.ID}}, {"in_progress", []string{c.ID}}, {"current", []string{a.ID, b.ID, c.ID}},
	} {
		items, total, err := s.List(test.filter, nil, nil, 20, 0)
		if err != nil || total != len(test.ids) || len(items) != len(test.ids) {
			t.Fatalf("list %s: %v total=%d err=%v", test.filter, items, total, err)
		}
		for i, item := range items {
			if item.ID != test.ids[i] || item.Brief != "" || item.Progress != "" || item.Result != "" || len(item.Metadata) != 0 {
				t.Fatalf("wrong projection in %s: %+v", test.filter, item)
			}
		}
	}
	items, total, err := s.List("current", []string{"x", "y"}, nil, 1, 1)
	if err != nil || total != 2 || len(items) != 1 || items[0].ID != c.ID {
		t.Fatalf("AND tags/pagination = %+v total=%d err=%v", items, total, err)
	}
	items, total, err = s.List("all", nil, nil, 20, 99)
	if err != nil || total != 3 || len(items) != 0 {
		t.Fatalf("empty page lost total: %+v total=%d err=%v", items, total, err)
	}
}

func TestCoreInputValidationAndMutationNoOps(t *testing.T) {
	s := newTaskTestStore(t)
	for _, spec := range []CreateSpec{{Title: "\n"}, {Title: "bad\nline"}, {Title: "bad\x1btitle"}, {Title: "Valid", Metadata: map[string]string{"status": "completed"}}, {Title: "Valid", Metadata: map[string]string{"claim_id": "fake"}}} {
		_, err := s.Create(spec)
		assertCoreError(t, err, "invalid_input")
	}
	task := mustCreateTask(t, s, CreateSpec{Title: "Valid", Tags: []string{"tag"}, Metadata: map[string]string{"key": "value"}})
	before := coreEventCount(t, s)
	if err := s.Tag(task.ID, "", "tag", false); err != nil {
		t.Fatal(err)
	}
	if err := s.MetadataSet(task.ID, "", "key", "value"); err != nil {
		t.Fatal(err)
	}
	if err := s.Tag(task.ID, "", "absent", true); err != nil {
		t.Fatal(err)
	}
	after, err := s.Get(task.ID)
	if err != nil || after.Revision != task.Revision || coreEventCount(t, s) != before {
		t.Fatalf("idempotent no-ops created changes: %+v %v", after, err)
	}
	for _, id := range []string{"1", "T0", "T01", "T-1", "T1x"} {
		_, err := s.Get(id)
		assertCoreError(t, err, "invalid_id")
	}
	_, err = s.Get("T999")
	assertCoreError(t, err, "not_found")
}

func subscribeCoreTask(t *testing.T, s *Store, id string) Subscription {
	t.Helper()
	subscription, err := s.Subscribe(id, "watcher", false)
	if err != nil {
		t.Fatal(err)
	}
	delivery, err := s.Offer(subscription.ID, "watcher", 20, time.Minute)
	if err != nil || delivery.Snapshot == nil {
		t.Fatalf("initial offer = %+v, %v", delivery, err)
	}
	if err := s.Acknowledge(subscription.ID, "watcher", delivery.Token, delivery.Through); err != nil {
		t.Fatal(err)
	}
	return subscription
}

func TestCoreAvailabilityNotifiesBlockedTaskSubscriber(t *testing.T) {
	s := newTaskTestStore(t)
	a := mustCreateTask(t, s, CreateSpec{Title: "Prerequisite outside subscriber subtree"})
	b := mustCreateTask(t, s, CreateSpec{Title: "Waiting task", Prerequisites: []string{a.ID}})
	subscription := subscribeCoreTask(t, s, b.ID)
	mustCompleteTask(t, s, mustClaimTask(t, s, a.ID, "worker"))
	delivery, err := s.Offer(subscription.ID, "watcher", 20, time.Minute)
	if err != nil || len(delivery.Events) != 1 || delivery.Events[0].Kind != "available" || delivery.Events[0].TaskID != b.ID {
		t.Fatalf("subscriber missed ready transition: %+v, %v", delivery, err)
	}
	if err := s.Acknowledge(subscription.ID, "watcher", delivery.Token, delivery.Through); err != nil {
		t.Fatal(err)
	}
	if err := s.Tag(a.ID, "", "late", false); ErrorCode(err) != "conflict" {
		t.Fatalf("terminal edit unexpectedly allowed: %v", err)
	}
	if err := s.Tag(b.ID, "", "ready", false); err != nil {
		t.Fatal(err)
	}
	delivery, err = s.Offer(subscription.ID, "watcher", 20, time.Minute)
	if err != nil || len(delivery.Events) != 1 || delivery.Events[0].Kind != "tags_changed" {
		t.Fatalf("unchanged availability was re-emitted: %+v, %v", delivery, err)
	}
}

func TestCoreAvailabilityWaitsForEveryReplacementBranch(t *testing.T) {
	s := newTaskTestStore(t)
	a := mustCreateTask(t, s, CreateSpec{Title: "Original prerequisite"})
	b := mustCreateTask(t, s, CreateSpec{Title: "Waiting task", Prerequisites: []string{a.ID}})
	subscription := subscribeCoreTask(t, s, b.ID)
	children, err := s.Evolve(a.ID, "", a.Revision, EvolveSpec{Summary: "Split remaining work", Replacements: []CreateSpec{{Title: "First", Brief: "First scope"}, {Title: "Second", Brief: "Second scope"}}})
	if err != nil {
		t.Fatal(err)
	}
	mustCompleteTask(t, s, mustClaimTask(t, s, children[0].ID, "worker-a"))
	delivery, err := s.Offer(subscription.ID, "watcher", 20, time.Minute)
	if err != nil || delivery.Token != "" || len(delivery.Events) != 0 {
		t.Fatalf("availability arrived before all branches completed: %+v %v", delivery, err)
	}
	mustCompleteTask(t, s, mustClaimTask(t, s, children[1].ID, "worker-b"))
	delivery, err = s.Offer(subscription.ID, "watcher", 20, time.Minute)
	if err != nil || len(delivery.Events) != 1 || delivery.Events[0].Kind != "available" || delivery.Events[0].TaskID != b.ID {
		t.Fatalf("all-branches availability missing: %+v %v", delivery, err)
	}
	items, total, err := s.List("available", nil, nil, 20, 0)
	if err != nil || total != 1 || len(items) != 1 || items[0].ID != b.ID {
		t.Fatalf("list disagrees with availability event: %+v total=%d err=%v", items, total, err)
	}
}

func TestCoreAvailableEventsOnCreateDependencyRemovalAndRelease(t *testing.T) {
	s := newTaskTestStore(t)
	a := mustCreateTask(t, s, CreateSpec{Title: "Prerequisite"})
	b := mustCreateTask(t, s, CreateSpec{Title: "Blocked", Prerequisites: []string{a.ID}})
	page, err := s.Events(a.ID, 0, 20)
	if err != nil || len(page.Events) != 2 || page.Events[0].Kind != "created" || page.Events[1].Kind != "available" {
		t.Fatalf("initial availability missing: %+v %v", page, err)
	}
	if err := s.AddPrerequisite(b.ID, "", a.ID, true); err != nil {
		t.Fatal(err)
	}
	claimed := mustClaimTask(t, s, b.ID, "worker")
	if _, err := s.Release(b.ID, claimed.ClaimID); err != nil {
		t.Fatal(err)
	}
	page, err = s.Events(b.ID, 0, 20)
	if err != nil {
		t.Fatal(err)
	}
	kinds := []string{}
	for _, event := range page.Events {
		kinds = append(kinds, event.Kind)
	}
	if !reflect.DeepEqual(kinds, []string{"created", "prerequisite_changed", "available", "claimed", "released", "available"}) {
		t.Fatalf("availability transition sequence = %v", kinds)
	}
}

func TestCoreAvailabilityEventFailureRollsBackPrerequisiteCompletion(t *testing.T) {
	s := newTaskTestStore(t)
	a := mustClaimTask(t, s, mustCreateTask(t, s, CreateSpec{Title: "Prerequisite"}).ID, "worker")
	b := mustCreateTask(t, s, CreateSpec{Title: "Blocked", Prerequisites: []string{a.ID}})
	if _, err := s.DB.Exec(`CREATE TRIGGER refuse_availability BEFORE INSERT ON events WHEN NEW.kind='available' BEGIN SELECT RAISE(ABORT,'simulated availability event failure'); END;`); err != nil {
		t.Fatal(err)
	}
	before := coreEventCount(t, s)
	if _, err := s.Complete(a.ID, a.ClaimID, "Finished"); err == nil {
		t.Fatal("completion committed without its dependent availability event")
	}
	after, err := s.Get(a.ID)
	if err != nil || !reflect.DeepEqual(a, after) || coreEventCount(t, s) != before {
		t.Fatalf("failed availability event escaped rollback: %+v, %v", after, err)
	}
	_, err = s.Claim(b.ID, "watcher")
	assertCoreError(t, err, "blocked")
}
