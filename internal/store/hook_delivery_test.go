package store

import (
	"fmt"
	"io"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func hookDeliveryTask(t *testing.T, s *Store, title string) Task {
	t.Helper()
	task := notificationTask(t, s, title)
	task, err := s.Claim(task.ID, "worker")
	if err != nil {
		t.Fatal(err)
	}
	return task
}

func TestHookDeliveryIgnoresNoteBacklogWithoutClaimingOrConsumingIt(t *testing.T) {
	s, _ := notificationStore(t)
	task := hookDeliveryTask(t, s, "quiet notes")
	sub := notificationSubscribe(t, s, task.ID, "watcher")
	before, err := loadSubscription(s.DB, sub.ID)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		if _, err := s.AddNote(task.ID, task.ClaimID, fmt.Sprintf("saved note %d", i), strings.NewReader(strings.Repeat("note body\n", 20000))); err != nil {
			t.Fatal(err)
		}
	}
	n, _ := taskNumber(task.ID)
	if _, err := s.DB.Exec(`WITH RECURSIVE n(i) AS (VALUES(1) UNION ALL SELECT i+1 FROM n WHERE i<5000)
 INSERT INTO events(task_id,kind,message,data,created_at)
 SELECT ?,'progress',printf('checkpoint %d',i),'null',? FROM n`, n, timestamp(s.Now())); err != nil {
		t.Fatal(err)
	}
	indexHookFixtureNotes(t, s, n)
	for i := 0; i < 3; i++ {
		candidates, err := s.HookSubscriptions("watcher", false, 6)
		if err != nil || len(candidates) != 0 {
			t.Fatalf("note-only work became a hook candidate: %+v %v", candidates, err)
		}
		delivery, err := s.OfferHookSummary(sub.ID, "watcher", 1, time.Minute)
		if err != nil || delivery.Token != "" || len(delivery.Events) != 0 || delivery.More || delivery.Through != sub.Cursor {
			t.Fatalf("quiet notes acquired a delivery or advanced the cursor: %+v %v", delivery, err)
		}
		after, err := loadSubscription(s.DB, sub.ID)
		if err != nil || after != before {
			t.Fatalf("quiet note check changed subscription state: %+v %v", after, err)
		}
	}
	full, err := s.OfferSummary(sub.ID, "watcher", 5, time.Minute)
	if err != nil || len(full.Events) != 5 || full.Events[0].Kind != "progress" {
		t.Fatalf("explicit pending delivery lost note history: %+v %v", full, err)
	}
	if err := s.ReleaseDelivery(sub.ID, full.Token); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Complete(task.ID, task.ClaimID, "Finished"); err != nil {
		t.Fatal(err)
	}
	delivery, err := s.OfferHookSummary(sub.ID, "watcher", 1, time.Minute)
	if err != nil || len(delivery.Events) != 1 || delivery.Events[0].Kind != "completed" || delivery.More {
		t.Fatalf("completion was hidden by the note backlog: %+v %v", delivery, err)
	}
	if len(delivery.LatestProgress) != 1 || delivery.LatestProgress[0].Message != "checkpoint 5000" || delivery.LatestProgress[0].ID > delivery.Through {
		t.Fatalf("wrong bounded latest note: %+v", delivery.LatestProgress)
	}
	if err := s.Acknowledge(sub.ID, "watcher", delivery.Token, delivery.Through); err != nil {
		t.Fatal(err)
	}
	page, err := s.Events(task.ID, sub.Cursor, 5)
	if err != nil || len(page.Events) != 5 || page.Events[0].Kind != "progress" || !page.More {
		t.Fatalf("hook acknowledgement removed pullable note history: %+v %v", page, err)
	}
}

func TestHookDeliveryFencesLatestTitlesAndReplaysUnacknowledgedEvents(t *testing.T) {
	s, _ := notificationStore(t)
	now := time.Date(2026, 11, 22, 12, 0, 0, 0, time.UTC)
	s.Now = func() time.Time { return now }
	root := notificationTask(t, s, "original")
	children, err := s.Evolve(root.ID, "", root.Revision, EvolveSpec{Summary: "two branches", Replacements: []CreateSpec{
		{Title: "one", Brief: "first branch"}, {Title: "two", Brief: "second branch"},
	}})
	if err != nil {
		t.Fatal(err)
	}
	for i := range children {
		children[i], err = s.Claim(children[i].ID, "worker")
		if err != nil {
			t.Fatal(err)
		}
	}
	sub := notificationSubscribe(t, s, root.ID, "watcher")
	addNote := func(task Task, title string) {
		t.Helper()
		if _, err := s.AddNote(task.ID, task.ClaimID, title, strings.NewReader("body never belongs in hook context")); err != nil {
			t.Fatal(err)
		}
	}
	addNote(children[0], "first branch before event")
	addNote(children[1], "second branch before event")
	if err := s.MetadataSet(children[0].ID, children[0].ClaimID, "worktree", "/first"); err != nil {
		t.Fatal(err)
	}
	addNote(children[0], "first branch after event")
	if _, err := s.Complete(children[1].ID, children[1].ClaimID, "Second finished"); err != nil {
		t.Fatal(err)
	}
	addNote(children[0], "first branch latest")
	if _, err := s.Complete(children[0].ID, children[0].ClaimID, "First finished"); err != nil {
		t.Fatal(err)
	}
	page, err := s.Events(root.ID, sub.Cursor, 100)
	if err != nil {
		t.Fatal(err)
	}
	var actionable []Event
	for _, event := range page.Events {
		if event.Kind != "progress" {
			actionable = append(actionable, event)
		}
	}
	wantTitles := []string{"first branch before event", "second branch before event", "first branch latest"}
	if len(actionable) != len(wantTitles) {
		t.Fatalf("unexpected fixture actions: %+v", actionable)
	}
	for i, want := range actionable {
		delivery, err := s.OfferHookSummary(sub.ID, "watcher", 1, time.Minute)
		if err != nil || len(delivery.Events) != 1 || delivery.Events[0].ID != want.ID || delivery.Through != want.ID || delivery.More != (i+1 < len(actionable)) {
			t.Fatalf("page %d skipped or hid an event: %+v %v", i, delivery, err)
		}
		if len(delivery.LatestProgress) != 1 || delivery.LatestProgress[0].TaskID != want.TaskID || delivery.LatestProgress[0].Message != wantTitles[i] || delivery.LatestProgress[0].ID > delivery.Through {
			t.Fatalf("page %d leaked newer or unrelated note state: %+v", i, delivery.LatestProgress)
		}
		if i == 0 {
			oldToken := delivery.Token
			if _, err := s.OfferHookSummary(sub.ID, "watcher", 1, time.Minute); ErrorCode(err) != "delivery_busy" {
				t.Fatalf("active lease allowed a duplicate offer: %v", err)
			}
			now = now.Add(2 * time.Minute)
			replay, err := s.OfferHookSummary(sub.ID, "watcher", 1, time.Minute)
			if err != nil || replay.Token == oldToken || replay.Through != delivery.Through || !reflect.DeepEqual(replay.Events, delivery.Events) || !reflect.DeepEqual(replay.LatestProgress, delivery.LatestProgress) {
				t.Fatalf("lease expiry changed the replay range/context: %+v %v", replay, err)
			}
			if err := s.Acknowledge(sub.ID, "watcher", oldToken, delivery.Through); ErrorCode(err) != "conflict" {
				t.Fatalf("stale hook acknowledged replay: %v", err)
			}
			delivery = replay
			if err := s.Acknowledge(sub.ID, "watcher", delivery.Token, actionable[1].ID); ErrorCode(err) != "invalid_acknowledgement" {
				t.Fatalf("ACK consumed an omitted actionable event: %v", err)
			}
		}
		if err := s.Acknowledge(sub.ID, "watcher", delivery.Token, delivery.Through); err != nil {
			t.Fatal(err)
		}
	}
}

func TestHookInitialSnapshotTitlesRespectOfferedCursor(t *testing.T) {
	s, _ := notificationStore(t)
	task := hookDeliveryTask(t, s, "initial snapshot")
	if _, err := s.AddNote(task.ID, task.ClaimID, "before subscription", strings.NewReader("old body")); err != nil {
		t.Fatal(err)
	}
	sub, err := s.Subscribe(task.ID, "watcher", false)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := s.AddNote(task.ID, task.ClaimID, "after subscription", strings.NewReader("new body")); err != nil {
		t.Fatal(err)
	}
	delivery, err := s.OfferHookSummary(sub.ID, "watcher", 1, time.Minute)
	if err != nil || delivery.Snapshot == nil || len(delivery.Snapshot.Tasks) != 1 || delivery.Snapshot.Tasks[0].Progress != "before subscription" || delivery.Through != sub.Cursor || len(delivery.Events) != 0 {
		t.Fatalf("initial hook snapshot crossed its cursor: %+v %v", delivery, err)
	}
	current, err := s.SnapshotSummary(task.ID)
	if err != nil || current.Tasks[0].Progress != "after subscription" {
		t.Fatalf("current resume snapshot lost the latest title: %+v %v", current, err)
	}
}

func TestHookDeletedNotesStayQuietAndDoNotReviveOlderTitles(t *testing.T) {
	s, _ := notificationStore(t)
	task := hookDeliveryTask(t, s, "deleted note")
	sub := notificationSubscribe(t, s, task.ID, "watcher")
	if _, err := s.AddNote(task.ID, task.ClaimID, "older stale checkpoint", strings.NewReader("old body")); err != nil {
		t.Fatal(err)
	}
	note, err := s.AddNote(task.ID, task.ClaimID, "removed checkpoint", strings.NewReader("deleted body"))
	if err != nil {
		t.Fatal(err)
	}
	if err := s.DeleteNote(note.ID, task.ClaimID); err != nil {
		t.Fatal(err)
	}
	candidates, err := s.HookSubscriptions("watcher", false, 6)
	if err != nil || len(candidates) != 0 {
		t.Fatalf("note deletion became a hook candidate: %+v %v", candidates, err)
	}
	delivery, err := s.OfferHookSummary(sub.ID, "watcher", 1, time.Minute)
	if err != nil || delivery.Token != "" {
		t.Fatalf("note deletion acquired a delivery: %+v %v", delivery, err)
	}
	if _, err := s.Complete(task.ID, task.ClaimID, "Finished"); err != nil {
		t.Fatal(err)
	}
	delivery, err = s.OfferHookSummary(sub.ID, "watcher", 1, time.Minute)
	if err != nil || len(delivery.Events) != 1 || delivery.Events[0].Kind != "completed" || len(delivery.LatestProgress) != 0 {
		t.Fatalf("completion revived a tombstone or stale title: %+v %v", delivery, err)
	}
	current, err := s.SnapshotSummary(task.ID)
	if err != nil || current.Tasks[0].Progress != "" {
		t.Fatalf("resume revived a deleted note: %+v %v", current, err)
	}
	page, err := s.Events(task.ID, sub.Cursor, 100)
	if err != nil || len(page.Events) != 4 || page.Events[2].Kind != "note_deleted" {
		t.Fatalf("quiet deletion lost its audit event: %+v %v", page, err)
	}
}

func TestHookIndexUpgradesPreviousQuietPredicate(t *testing.T) {
	s, path := notificationStore(t)
	if _, err := s.DB.Exec(`DROP INDEX events_hook_task_id;
 CREATE INDEX events_hook_task_id ON events(task_id,id) WHERE kind<>'progress'`); err != nil {
		t.Fatal(err)
	}
	if current, err := schemaCurrent(s.DB); err != nil || current {
		t.Fatalf("old hook predicate was treated as current: %t %v", current, err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := OpenExisting(path, s.ProjectID)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	if current, err := schemaCurrent(reopened.DB); err != nil || !current {
		t.Fatalf("hook index was not upgraded: %t %v", current, err)
	}
}

func TestHookEventQuerySkipsHundredThousandRoutineNotes(t *testing.T) {
	s, _ := notificationStore(t)
	task := hookDeliveryTask(t, s, "large routine backlog")
	sub := notificationSubscribe(t, s, task.ID, "watcher")
	n, _ := taskNumber(task.ID)
	for _, kind := range []string{"progress", "note_deleted"} {
		if _, err := s.DB.Exec(`WITH RECURSIVE n(i) AS (VALUES(1) UNION ALL SELECT i+1 FROM n WHERE i<100000)
 INSERT INTO events(task_id,kind,message,data,created_at) SELECT ?,?,'checkpoint','null',? FROM n`, n, kind, timestamp(s.Now())); err != nil {
			t.Fatal(err)
		}
	}
	indexHookFixtureNotes(t, s, n)
	start := time.Now()
	candidates, err := s.HookSubscriptions("watcher", false, 6)
	if err != nil || len(candidates) != 0 {
		t.Fatalf("routine backlog is not quiet: %+v %v", candidates, err)
	}
	t.Logf("quiet candidate query over 100000 notes + 100000 deletion events: %s", time.Since(start))
	if _, err := s.Complete(task.ID, task.ClaimID, "Finished"); err != nil {
		t.Fatal(err)
	}
	start = time.Now()
	page, err := readHookEvents(s.DB, n, sub.Cursor, 1)
	if err != nil || len(page.Events) != 1 || page.Events[0].Kind != "completed" || page.More {
		t.Fatalf("completion not reachable through routine backlog: %+v %v", page, err)
	}
	t.Logf("completion page query: %s", time.Since(start))
	start = time.Now()
	delivery, err := s.OfferHookSummary(sub.ID, "watcher", 1, time.Minute)
	if err != nil || len(delivery.Events) != 1 || delivery.Events[0].Kind != "completed" || len(delivery.LatestProgress) != 1 || delivery.LatestProgress[0].Message != "checkpoint" {
		t.Fatalf("complete hook offer failed with a large routine backlog: %+v %v", delivery, err)
	}
	t.Logf("complete hook offer including cursor-bounded title lookup: %s", time.Since(start))
	rows, err := s.DB.Query("EXPLAIN QUERY PLAN "+selectedHookEvents+"SELECT id FROM selected", n, sub.Cursor, 2, 2)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	indexed := false
	for rows.Next() {
		var id, parent, unused int
		var detail string
		if err := rows.Scan(&id, &parent, &unused, &detail); err != nil {
			t.Fatal(err)
		}
		indexed = indexed || strings.Contains(detail, "events_hook_task_id (task_id=? AND id>?)")
		t.Log(detail)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	if !indexed {
		t.Fatal("hook query did not seek through the partial event index")
	}
}

func indexHookFixtureNotes(t *testing.T, s *Store, task int64) {
	t.Helper()
	if _, err := s.DB.Exec(`INSERT INTO notes(id,task_id,event_id,title,author,claim_id,created_at)
 SELECT printf('N%032x',e.id),e.task_id,e.id,e.message,'worker','',e.created_at
 FROM events e WHERE e.task_id=? AND e.kind='progress'
 AND NOT EXISTS(SELECT 1 FROM notes n WHERE n.event_id=e.id)`, task); err != nil {
		t.Fatal(err)
	}
}

type hookBlockingNoteReader struct {
	reader  io.Reader
	started chan struct{}
	resume  <-chan struct{}
	once    sync.Once
}

func (r *hookBlockingNoteReader) Read(p []byte) (int, error) {
	r.once.Do(func() {
		close(r.started)
		<-r.resume
	})
	return r.reader.Read(p)
}

func TestNotePublicationCannotRegressConcurrentHeartbeat(t *testing.T) {
	s, _ := notificationStore(t)
	var clock atomic.Int64
	clock.Store(time.Date(2026, 11, 22, 12, 0, 0, 0, time.UTC).UnixNano())
	s.Now = func() time.Time { return time.Unix(0, clock.Load()).UTC() }
	task := hookDeliveryTask(t, s, "streaming note")
	resume := make(chan struct{})
	var resumed sync.Once
	release := func() { resumed.Do(func() { close(resume) }) }
	defer release()
	reader := &hookBlockingNoteReader{reader: strings.NewReader("streamed body"), started: make(chan struct{}), resume: resume}
	type result struct {
		note Note
		err  error
	}
	done := make(chan result, 1)
	go func() {
		note, err := s.AddNote(task.ID, task.ClaimID, "published note", reader)
		done <- result{note: note, err: err}
	}()
	select {
	case <-reader.started:
	case <-time.After(5 * time.Second):
		t.Fatal("note did not start streaming")
	}
	clock.Add(int64(10 * time.Minute))
	if err := s.Heartbeat(task.ID, task.ClaimID); err != nil {
		t.Fatal(err)
	}
	want := timestamp(s.Now())
	release()
	select {
	case result := <-done:
		if result.err != nil || result.note.CreatedAt != want {
			t.Fatalf("note creation used its stream-start time: %+v %v", result.note, result.err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("note did not finish publishing")
	}
	current, err := s.Get(task.ID)
	if err != nil || current.LastActivityAt != want || current.UpdatedAt != want {
		t.Fatalf("note publication regressed activity: %+v %v", current, err)
	}
}
