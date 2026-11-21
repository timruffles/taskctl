package store

import (
	"path/filepath"
	"reflect"
	"sync"
	"testing"
	"time"
)

func notificationStore(t *testing.T) (*Store, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "tasks.sqlite")
	s, err := Open(path, "notification-tests")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s, path
}

func notificationTask(t *testing.T, s *Store, title string) Task {
	t.Helper()
	task, err := s.Create(CreateSpec{Title: title, Brief: "Complete " + title})
	if err != nil {
		t.Fatal(err)
	}
	return task
}

func notificationSubscribe(t *testing.T, s *Store, taskID, session string) Subscription {
	t.Helper()
	sub, err := s.Subscribe(taskID, session, false)
	if err != nil {
		t.Fatal(err)
	}
	delivery, err := s.Offer(sub.ID, session, 5, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if delivery.Snapshot == nil || delivery.Token == "" {
		t.Fatalf("new subscription lacks initial snapshot: %+v", delivery)
	}
	if err := s.Acknowledge(sub.ID, session, delivery.Token, delivery.Through); err != nil {
		t.Fatal(err)
	}
	return sub
}

func TestSubscriptionAlreadyTerminalSnapshotPersistsAndAcknowledgesOnce(t *testing.T) {
	s, path := notificationStore(t)
	task := notificationTask(t, s, "finished before subscribing")
	claimed, err := s.Claim(task.ID, "worker")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Complete(task.ID, claimed.ClaimID, "All acceptance checks passed"); err != nil {
		t.Fatal(err)
	}
	sub, err := s.Subscribe(task.ID, "watcher", false)
	if err != nil {
		t.Fatal(err)
	}
	reopened, err := Open(path, s.ProjectID)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	delivery, err := reopened.Offer(sub.ID, "watcher", 5, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if delivery.Snapshot == nil || delivery.Snapshot.Outcome != "completed" || len(delivery.Events) != 0 {
		t.Fatalf("unexpected terminal initial delivery: %+v", delivery)
	}
	if len(delivery.Snapshot.Tasks) != 1 || delivery.Snapshot.Tasks[0].Result != "All acceptance checks passed" {
		t.Fatalf("snapshot lost completion context: %+v", delivery.Snapshot)
	}
	if err := s.Acknowledge(sub.ID, "watcher", delivery.Token, delivery.Through); err != nil {
		t.Fatal(err)
	}
	if err := reopened.Acknowledge(sub.ID, "watcher", delivery.Token, delivery.Through); err != nil {
		t.Fatalf("identical acknowledgement must be idempotent: %v", err)
	}
	again, err := reopened.Offer(sub.ID, "watcher", 5, time.Minute)
	if err != nil || again.Token != "" || again.Snapshot != nil || len(again.Events) != 0 {
		t.Fatalf("acknowledged terminal snapshot repeated: %+v, %v", again, err)
	}
	duplicate, err := s.Subscribe(task.ID, "watcher", true)
	if err != nil || duplicate.ID != sub.ID || !duplicate.ResumeIdle || duplicate.SnapshotPending {
		t.Fatalf("resubscription reset durable state: %+v, %v", duplicate, err)
	}
}

func TestSubscriptionDeliveryClaimsAreAtomicAndFencedAcrossConnections(t *testing.T) {
	s, path := notificationStore(t)
	now := time.Date(2026, 11, 21, 12, 0, 0, 0, time.UTC)
	s.Now = func() time.Time { return now }
	task := notificationTask(t, s, "race")
	sub, err := s.Subscribe(task.ID, "watcher", false)
	if err != nil {
		t.Fatal(err)
	}
	other, err := Open(path, s.ProjectID)
	if err != nil {
		t.Fatal(err)
	}
	defer other.Close()
	other.Now = s.Now
	start := make(chan struct{})
	type result struct {
		delivery Delivery
		err      error
	}
	results := make(chan result, 2)
	var workers sync.WaitGroup
	for _, connection := range []*Store{s, other} {
		workers.Add(1)
		go func(connection *Store) {
			defer workers.Done()
			<-start
			delivery, err := connection.Offer(sub.ID, "watcher", 5, time.Minute)
			results <- result{delivery, err}
		}(connection)
	}
	close(start)
	workers.Wait()
	close(results)
	var winner Delivery
	successes, conflicts := 0, 0
	for result := range results {
		if result.err == nil && result.delivery.Token != "" {
			winner = result.delivery
			successes++
		} else if ErrorCode(result.err) == "delivery_busy" {
			conflicts++
		} else {
			t.Fatalf("unexpected concurrent offer: %+v, %v", result.delivery, result.err)
		}
	}
	if successes != 1 || conflicts != 1 {
		t.Fatalf("offers were not exclusive: successes=%d conflicts=%d", successes, conflicts)
	}
	if err := s.Acknowledge(sub.ID, "wrong-session", winner.Token, winner.Through); ErrorCode(err) != "conflict" {
		t.Fatalf("wrong owner could acknowledge: %v", err)
	}
	if err := s.Acknowledge(sub.ID, "watcher", winner.Token, winner.Through+1); ErrorCode(err) != "invalid_acknowledgement" {
		t.Fatalf("unoffered cursor accepted: %v", err)
	}
	now = now.Add(time.Minute)
	if err := s.Acknowledge(sub.ID, "watcher", winner.Token, winner.Through); ErrorCode(err) != "conflict" {
		t.Fatalf("expired claim could acknowledge: %v", err)
	}
	retry, err := other.Offer(sub.ID, "watcher", 5, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if retry.Token == winner.Token || retry.Through != winner.Through || retry.Snapshot == nil {
		t.Fatalf("expired delivery was not retried: old=%+v new=%+v", winner, retry)
	}
	if err := s.Acknowledge(sub.ID, "watcher", winner.Token, winner.Through); ErrorCode(err) != "conflict" {
		t.Fatalf("stale token advanced newer claim: %v", err)
	}
	if err := s.ReleaseDelivery(sub.ID, winner.Token); ErrorCode(err) != "conflict" {
		t.Fatalf("stale release cleared newer claim: %v", err)
	}
	if err := other.Acknowledge(sub.ID, "watcher", retry.Token, retry.Through); err != nil {
		t.Fatal(err)
	}
}

func TestSubscriptionBoundedPagesRetryWithoutSkippingEvents(t *testing.T) {
	s, _ := notificationStore(t)
	task := notificationTask(t, s, "paged")
	claimed, err := s.Claim(task.ID, "worker")
	if err != nil {
		t.Fatal(err)
	}
	sub := notificationSubscribe(t, s, task.ID, "watcher")
	for i := 0; i < 7; i++ {
		if _, err := s.Progress(task.ID, claimed.ClaimID, "Meaningful progress"); err != nil {
			t.Fatal(err)
		}
		_ = notificationTask(t, s, "unrelated")
	}
	expected, err := s.Events(task.ID, sub.Cursor, 100)
	if err != nil {
		t.Fatal(err)
	}
	first, err := s.Offer(sub.ID, "watcher", 2, time.Minute)
	if err != nil || len(first.Events) != 2 || !first.More {
		t.Fatalf("first bounded offer: %+v, %v", first, err)
	}
	state, _, err := s.SubscriptionsPage("watcher", 200, 0)
	if err != nil || state[0].Cursor != sub.Cursor {
		t.Fatalf("attempted delivery advanced cursor: %+v %v", state, err)
	}
	if err := s.ReleaseDelivery(sub.ID, first.Token); err != nil {
		t.Fatal(err)
	}
	queued, err := s.OfferQueue(sub.ID, "watcher", 2, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.MarkQueued(sub.ID, "watcher", queued.Token, queued.Through); err != nil {
		t.Fatal(err)
	}
	if err := s.ReleaseDelivery(sub.ID, queued.Token); err != nil {
		t.Fatal(err)
	}
	state, _, err = s.SubscriptionsPage("watcher", 200, 0)
	if err != nil || state[0].Cursor != sub.Cursor || state[0].QueuedThrough != first.Through {
		t.Fatalf("queue receipt consumed events: %+v %v", state, err)
	}
	var got []int64
	for {
		delivery, err := s.Offer(sub.ID, "watcher", 2, time.Minute)
		if err != nil {
			t.Fatal(err)
		}
		if delivery.Token == "" {
			break
		}
		if len(delivery.Events) > 2 {
			t.Fatal("offer exceeded page limit")
		}
		for _, event := range delivery.Events {
			got = append(got, event.ID)
		}
		if err := s.Acknowledge(sub.ID, "watcher", delivery.Token, delivery.Through); err != nil {
			t.Fatal(err)
		}
	}
	want := make([]int64, len(expected.Events))
	for i, event := range expected.Events {
		want[i] = event.ID
	}
	if !reflect.DeepEqual(got, want) || len(got) != 7 {
		t.Fatalf("event page skipped or duplicated: got=%v want=%v", got, want)
	}
}

func TestSubscriptionScopeFollowsOnlyTargetDescendantsAndKeepsCancellation(t *testing.T) {
	s, _ := notificationStore(t)
	original := notificationTask(t, s, "original")
	children, err := s.Supersede(original.ID, "", original.Revision, SupersedeSpec{
		Summary:      "Split independent remaining work",
		Replacements: []CreateSpec{{Title: "A", Brief: "Finish branch A"}, {Title: "B", Brief: "Finish branch B"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	sub := notificationSubscribe(t, s, children[0].ID, "watcher")
	grandchildren, err := s.Supersede(children[0].ID, "", children[0].Revision, SupersedeSpec{
		Summary:      "A now needs two remaining pieces",
		Replacements: []CreateSpec{{Title: "A1", Brief: "Finish A1"}, {Title: "A2", Brief: "Finish A2"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Cancel(grandchildren[0].ID, "", "No longer wanted"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Cancel(children[1].ID, "", "Sibling outside subscription"); err != nil {
		t.Fatal(err)
	}
	page, err := s.Events(children[0].ID, sub.Cursor, 100)
	if err != nil {
		t.Fatal(err)
	}
	seenDescendant := false
	for _, event := range page.Events {
		if event.TaskID == children[1].ID || event.TaskID == original.ID {
			t.Fatalf("subscription escaped target subtree: %+v", event)
		}
		seenDescendant = seenDescendant || event.TaskID == grandchildren[0].ID
	}
	if !seenDescendant {
		t.Fatal("replacement descendant events missing")
	}
	snapshot, _, err := s.Current(children[0].ID)
	if err != nil || snapshot.Outcome != "cancelled" || len(snapshot.Tasks) != 2 {
		t.Fatalf("cancellation lost through replacement tree: %+v %v", snapshot, err)
	}
	remaining, err := s.Get(grandchildren[1].ID)
	if err != nil || remaining.Status != "pending" {
		t.Fatalf("cancellation incorrectly stopped remaining branch: %+v %v", remaining, err)
	}
}

func TestInactivityDeduplicatesSilenceAndDoesNotChangeOwnership(t *testing.T) {
	s, _ := notificationStore(t)
	now := time.Date(2026, 11, 21, 12, 0, 0, 0, time.UTC)
	s.Now = func() time.Time { return now }
	task := notificationTask(t, s, "heartbeat")
	claimed, err := s.Claim(task.ID, "child")
	if err != nil {
		t.Fatal(err)
	}
	now = now.Add(31 * time.Minute)
	if n, err := s.DetectInactivity(30 * time.Minute); err != nil || n != 1 {
		t.Fatalf("first silence event: %d %v", n, err)
	}
	if n, err := s.DetectInactivity(30 * time.Minute); err != nil || n != 0 {
		t.Fatalf("duplicate silence event: %d %v", n, err)
	}
	if n, err := s.TouchOwned("parent"); err != nil || n != 0 {
		t.Fatalf("parent touched child activity: %d %v", n, err)
	}
	current, err := s.Get(task.ID)
	if err != nil || current.Owner != "child" || current.ClaimID != claimed.ClaimID || current.Revision != claimed.Revision {
		t.Fatalf("silence altered ownership: %+v %v", current, err)
	}
	if n, err := s.TouchOwned("child"); err != nil || n != 1 {
		t.Fatalf("owner heartbeat failed: %d %v", n, err)
	}
	if n, err := s.DetectInactivity(30 * time.Minute); err != nil || n != 0 {
		t.Fatalf("recent activity raised notification: %d %v", n, err)
	}
	now = now.Add(31 * time.Minute)
	if n, err := s.DetectInactivity(30 * time.Minute); err != nil || n != 1 {
		t.Fatalf("new silence period missing: %d %v", n, err)
	}
	taken, err := s.Takeover(task.ID, claimed.ClaimID, "replacement", "Original worker was stopped")
	if err != nil {
		t.Fatal(err)
	}
	now = now.Add(time.Minute)
	if n, err := s.TouchOwned("child"); err != nil || n != 0 {
		t.Fatalf("old owner touched reassigned task: %d %v", n, err)
	}
	current, err = s.Get(task.ID)
	if err != nil || current.LastActivityAt != taken.LastActivityAt || current.ClaimID != taken.ClaimID {
		t.Fatalf("stale activity overwrote new claim: %+v %v", current, err)
	}
}

func TestQueueScanFindsCompletionBeyondUnacknowledgedProgressPages(t *testing.T) {
	s, _ := notificationStore(t)
	task := notificationTask(t, s, "queue progress")
	claimed, err := s.Claim(task.ID, "worker")
	if err != nil {
		t.Fatal(err)
	}
	sub := notificationSubscribe(t, s, task.ID, "watcher")
	for i := 0; i < 7; i++ {
		if _, err := s.Progress(task.ID, claimed.ClaimID, "Progress before completion"); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := s.Complete(task.ID, claimed.ClaimID, "Finished all work"); err != nil {
		t.Fatal(err)
	}
	var accepted int64
	pages := 0
	for {
		delivery, err := s.OfferQueue(sub.ID, "watcher", 2, time.Minute)
		if err != nil {
			t.Fatal(err)
		}
		if delivery.Token == "" {
			break
		}
		pages++
		if delivery.Mode != "queue" || delivery.Snapshot != nil || len(delivery.Events) > 2 {
			t.Fatalf("unexpected queue page: %+v", delivery)
		}
		if err := s.Acknowledge(sub.ID, "watcher", delivery.Token, delivery.Through); ErrorCode(err) != "conflict" {
			t.Fatalf("queue token consumed notices: %v", err)
		}
		if _, err := s.Offer(sub.ID, "watcher", 2, time.Minute); ErrorCode(err) != "delivery_busy" {
			t.Fatalf("queue and hook delivered concurrently: %v", err)
		}
		completed := false
		for _, event := range delivery.Events {
			completed = completed || event.Kind == "completed"
		}
		if completed {
			accepted = delivery.Through
			err = s.MarkQueued(sub.ID, "watcher", delivery.Token, delivery.Through)
		} else {
			err = s.AdvanceQueueScan(sub.ID, "watcher", delivery.Token, delivery.Through)
		}
		if err != nil {
			t.Fatal(err)
		}
		if err := s.ReleaseDelivery(sub.ID, delivery.Token); err != nil {
			t.Fatal(err)
		}
		if !completed {
			state, _, err := s.SubscriptionsPage("watcher", 200, 0)
			if err != nil || state[0].QueuedThrough != 0 || state[0].Cursor != sub.Cursor {
				t.Fatalf("scanned progress pretended queue acceptance/ACK: %+v %v", state, err)
			}
		}
	}
	if pages != 4 || accepted == 0 {
		t.Fatalf("completion starved behind pending progress: pages=%d accepted=%d", pages, accepted)
	}
	state, _, err := s.SubscriptionsPage("watcher", 200, 0)
	if err != nil || state[0].Cursor != sub.Cursor || state[0].QueuedThrough != accepted || state[0].QueueCursor != accepted {
		t.Fatalf("queue scan changed wrong cursors: %+v %v", state, err)
	}
	delivery, err := s.Offer(sub.ID, "watcher", 100, time.Minute)
	if err != nil || len(delivery.Events) != 8 || delivery.Through != accepted {
		t.Fatalf("queue transport consumed actual delivery: %+v %v", delivery, err)
	}
	if err := s.MarkQueued(sub.ID, "watcher", delivery.Token, delivery.Through); ErrorCode(err) != "conflict" {
		t.Fatalf("normal delivery token advanced queue cursor: %v", err)
	}
}

func TestQueueAlreadyTerminalSnapshotWakesOnceWithoutAcknowledging(t *testing.T) {
	s, _ := notificationStore(t)
	task := notificationTask(t, s, "terminal queue")
	claimed, err := s.Claim(task.ID, "worker")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Complete(task.ID, claimed.ClaimID, "Already done"); err != nil {
		t.Fatal(err)
	}
	sub, err := s.Subscribe(task.ID, "watcher", true)
	if err != nil {
		t.Fatal(err)
	}
	queued, err := s.OfferQueue(sub.ID, "watcher", 1, time.Minute)
	if err != nil || queued.Token == "" || queued.Snapshot == nil || queued.Snapshot.Outcome != "completed" || queued.Through != sub.Cursor {
		t.Fatalf("missing current terminal queue snapshot: %+v %v", queued, err)
	}
	if err := s.MarkQueued(sub.ID, "watcher", queued.Token, queued.Through); err != nil {
		t.Fatal(err)
	}
	if err := s.ReleaseDelivery(sub.ID, queued.Token); err != nil {
		t.Fatal(err)
	}
	again, err := s.OfferQueue(sub.ID, "watcher", 1, time.Minute)
	if err != nil || again.Token != "" {
		t.Fatalf("accepted queue snapshot repeated: %+v %v", again, err)
	}
	delivery, err := s.Offer(sub.ID, "watcher", 1, time.Minute)
	if err != nil || delivery.Snapshot == nil || delivery.Snapshot.Outcome != "completed" || delivery.Token == "" {
		t.Fatalf("queue acceptance consumed initial context: %+v %v", delivery, err)
	}
}

func TestSnapshotAvailabilityIncludesBranchesOutsideBoundedRows(t *testing.T) {
	s, _ := notificationStore(t)
	blocker := notificationTask(t, s, "unfulfilled prerequisite")
	original := notificationTask(t, s, "many branches")
	replacements := make([]CreateSpec, 25)
	for i := range replacements {
		replacements[i] = CreateSpec{Title: "blocked branch", Brief: "Complete after prerequisite", Prerequisites: []string{blocker.ID}}
	}
	replacements[24] = CreateSpec{Title: "available branch", Brief: "Ready now"}
	if _, err := s.Supersede(original.ID, "", original.Revision, SupersedeSpec{Summary: "Decompose remaining work", Replacements: replacements}); err != nil {
		t.Fatal(err)
	}
	snapshot, _, err := s.Current(original.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !snapshot.Available || len(snapshot.Tasks) != 20 || snapshot.Omitted != 5 {
		t.Fatalf("aggregate availability lost omitted branch: %+v", snapshot)
	}
	for _, task := range snapshot.Tasks {
		if task.Available {
			t.Fatalf("fixture did not exercise omitted readiness: %+v", task)
		}
	}
}

func TestSubscriptionListMayReadAllSessionsWithoutGrantingOwnership(t *testing.T) {
	s, _ := notificationStore(t)
	task := notificationTask(t, s, "list subscriptions")
	one, err := s.Subscribe(task.ID, "one", false)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.Subscribe(task.ID, "two", false); err != nil {
		t.Fatal(err)
	}
	all, _, err := s.SubscriptionsPage("", 200, 0)
	if err != nil || len(all) != 2 {
		t.Fatalf("unfiltered subscriptions: %+v %v", all, err)
	}
	filtered, _, err := s.SubscriptionsPage("one", 200, 0)
	if err != nil || len(filtered) != 1 || filtered[0].ID != one.ID {
		t.Fatalf("filtered subscriptions: %+v %v", filtered, err)
	}
	if _, err := s.Offer(one.ID, "", 5, time.Minute); ErrorCode(err) != "conflict" {
		t.Fatalf("empty read filter granted write ownership: %v", err)
	}
}
