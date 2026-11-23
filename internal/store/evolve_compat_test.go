package store

import (
	"bytes"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func assertEvolveStorage(t *testing.T, q Queryer, id string) {
	t.Helper()
	n, err := taskNumber(id)
	if err != nil {
		t.Fatal(err)
	}
	var version, status string
	var legacyEvents, renamedEvents int
	if err := q.QueryRow(`SELECT
 (SELECT value FROM store_info WHERE key='schema_version'),
 (SELECT status FROM tasks WHERE id=?),
 (SELECT COUNT(*) FROM events WHERE task_id=? AND kind='superseded'),
 (SELECT COUNT(*) FROM events WHERE kind='evolved')`, n, n).Scan(&version, &status, &legacyEvents, &renamedEvents); err != nil {
		t.Fatal(err)
	}
	if version != "2" || status != "superseded" || legacyEvents != 1 || renamedEvents != 0 {
		t.Fatalf("durable compatibility changed: schema=%s status=%s legacy events=%d renamed events=%d", version, status, legacyEvents, renamedEvents)
	}
}

func assertEvolveEvent(t *testing.T, events []Event, id string) Event {
	t.Helper()
	var found Event
	count := 0
	for _, event := range events {
		if event.Kind == "superseded" {
			t.Fatalf("legacy spelling leaked into an event: %+v", event)
		}
		if event.TaskID == id && event.Kind == "evolved" {
			found = event
			count++
		}
	}
	if count != 1 {
		t.Fatalf("got %d evolution events for %s: %+v", count, id, events)
	}
	return found
}

func TestEvolvePublicNamesPreserveStoredProtocolAndClaims(t *testing.T) {
	s := newTaskTestStore(t)
	spec := CreateSpec{Title: "Implement the parser", Brief: "Keep the literal word superseded in this original brief."}
	original, err := s.CreateWithRequest(spec, "evolve-original")
	if err != nil {
		t.Fatal(err)
	}
	original = mustClaimTask(t, s, original.ID, "worker")
	dependent := mustCreateTask(t, s, CreateSpec{Title: "Review the completed parser", Prerequisites: []string{original.ID}})
	type deliveryCase struct {
		name  string
		offer func(string, string, int, time.Duration) (Delivery, error)
		sub   Subscription
	}
	deliveries := []deliveryCase{{name: "full", offer: s.Offer}, {name: "summary", offer: s.OfferSummary}, {name: "hook", offer: s.OfferHookSummary}}
	for i := range deliveries {
		deliveries[i].sub, err = s.Subscribe(original.ID, deliveries[i].name, false)
		if err != nil {
			t.Fatal(err)
		}
	}
	const summary = "The old parser is superseded in code; error handling and integration tests remain."
	children, err := s.Evolve(original.ID, original.ClaimID, original.Revision, EvolveSpec{
		Summary: summary, ContinueWith: "errors",
		Replacements: []CreateSpec{
			{Key: "errors", Title: "Verify parser errors", Brief: "Add malformed-row handling and verify parser tests."},
			{Key: "integration", Title: "Verify integration", Brief: "Add integration tests for the parser command."},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(children) != 2 || children[0].Status != "in_progress" || children[0].ClaimID == "" || children[0].ClaimID == original.ClaimID || children[0].Owner != original.Owner || children[1].Status != "pending" {
		t.Fatalf("continuation lost ownership fencing: %+v", children)
	}
	assertEvolveStorage(t, s.DB, original.ID)
	retired, err := s.Get(original.ID)
	if err != nil || retired.Status != "evolved" || retired.Brief != spec.Brief || retired.Result != summary || retired.ClaimID != "" {
		t.Fatalf("public original lost its historical definition: %+v %v", retired, err)
	}
	for _, filter := range []string{"evolved", "superseded"} {
		listed, total, err := s.List(filter, nil, nil, 20, 0)
		if err != nil || total != 1 || len(listed) != 1 || listed[0].ID != original.ID || listed[0].Status != "evolved" {
			t.Fatalf("filter %q returned %+v total=%d err=%v", filter, listed, total, err)
		}
	}
	history, more, err := s.HistoryPage(children[0].ID, 20, 0)
	if err != nil || more || len(history) != 3 || history[0].ID != original.ID || history[0].Status != "evolved" || children[0].ParentID != original.ID || children[0].OriginID != original.ID {
		t.Fatalf("public history lost lineage: %+v more=%t err=%v", history, more, err)
	}
	retried, err := s.CreateWithRequest(spec, "evolve-original")
	if err != nil || retried.ID != original.ID || retried.Status != "evolved" {
		t.Fatalf("idempotent creation returned a stale public status: %+v %v", retried, err)
	}
	page, err := s.Events(original.ID, 0, 100)
	if err != nil || page.More {
		t.Fatalf("event page: %+v %v", page, err)
	}
	event := assertEvolveEvent(t, page.Events, original.ID)
	if event.Message != summary {
		t.Fatalf("public naming rewrote historical text: %q", event.Message)
	}
	for _, test := range deliveries {
		t.Run(test.name, func(t *testing.T) {
			delivery, err := test.offer(test.sub.ID, test.name, 100, time.Minute)
			if err != nil || delivery.Token == "" || delivery.More || delivery.Snapshot == nil || delivery.Snapshot.Outcome != "pending" || len(delivery.Snapshot.Tasks) != 2 {
				t.Fatalf("unexpected delivery: %+v %v", delivery, err)
			}
			if got := assertEvolveEvent(t, delivery.Events, original.ID); got.ID != event.ID {
				t.Fatalf("event identity changed between delivery paths: %+v", got)
			}
			for _, task := range delivery.Snapshot.Tasks {
				if task.ID == original.ID || task.Status == "superseded" || task.Status == "evolved" {
					t.Fatalf("retired task entered the current frontier: %+v", task)
				}
			}
			if err := s.Acknowledge(test.sub.ID, test.name, delivery.Token, delivery.Through); err != nil {
				t.Fatal(err)
			}
		})
	}
	_, err = s.Complete(original.ID, original.ClaimID, "stale completion")
	assertCoreError(t, err, "conflict")
	_, err = s.Progress(children[0].ID, original.ClaimID, "stale continuation")
	assertCoreError(t, err, "conflict")
	mustCompleteTask(t, s, children[0])
	if outcome, err := s.Outcome(original.ID); err != nil || outcome != "pending" {
		t.Fatalf("unfinished successor was treated as completed: %q %v", outcome, err)
	}
	_, err = s.Claim(dependent.ID, "reviewer")
	assertCoreError(t, err, "blocked")
	mustCompleteTask(t, s, mustClaimTask(t, s, children[1].ID, "tester"))
	if outcome, err := s.Outcome(original.ID); err != nil || outcome != "completed" {
		t.Fatalf("finished successors did not complete the outcome: %q %v", outcome, err)
	}
	mustClaimTask(t, s, dependent.ID, "reviewer")
	retired, err = s.Get(original.ID)
	if err != nil || retired.Status != "evolved" {
		t.Fatalf("aggregate completion rewrote historical state: %+v %v", retired, err)
	}
	assertEvolveStorage(t, s.DB, original.ID)
}

func TestEvolveBackupRoundTripKeepsLegacyStorageAndNoteBodies(t *testing.T) {
	s := newTaskTestStore(t)
	original := mustClaimTask(t, s, mustCreateTask(t, s, CreateSpec{Title: "Original intention", Brief: "Original work."}).ID, "worker")
	body := "# Evidence\n\nThe previous implementation was superseded; preserve this text exactly.\n"
	note, err := s.AddNote(original.ID, original.ClaimID, "Implementation verified; integration remains", strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	original, err = s.Get(original.ID)
	if err != nil {
		t.Fatal(err)
	}
	children, err := s.Evolve(original.ID, original.ClaimID, original.Revision, EvolveSpec{
		Summary: "Implementation is verified; finish integration.", ContinueWith: "integration",
		Replacements: []CreateSpec{{Key: "integration", Title: "Finish integration", Brief: "Wire the verified implementation into the command and test it."}},
	})
	if err != nil {
		t.Fatal(err)
	}
	directory := filepath.Join(t.TempDir(), "backup")
	if err := s.BackupDirectory(directory); err != nil {
		t.Fatal(err)
	}
	assertEvolveStorage(t, backupTestRead(t, filepath.Join(directory, "tasks.sqlite")), original.ID)
	restored := newTaskTestStore(t)
	if err := restored.RestoreDirectory(directory); err != nil {
		t.Fatal(err)
	}
	assertEvolveStorage(t, restored.DB, original.ID)
	retired, err := restored.Get(original.ID)
	if err != nil || retired.Status != "evolved" || retired.ProgressNoteID != note.ID || retired.Progress != note.Title {
		t.Fatalf("restore lost historical state or note reference: %+v %v", retired, err)
	}
	var actual bytes.Buffer
	if err := restored.WriteNoteBody(note.ID, &actual); err != nil || actual.String() != body {
		t.Fatalf("restore changed the original note body: %q %v", actual.String(), err)
	}
	child, err := restored.Get(children[0].ID)
	if err != nil || child.Status != "pending" || child.ClaimID != "" || child.Owner != "" || child.ProgressNoteID != "" || child.ParentID != original.ID || child.OriginID != original.ID {
		t.Fatalf("restore lost frontier or retained an active claim: %+v %v", child, err)
	}
	_, err = restored.Complete(child.ID, children[0].ClaimID, "stale restored claim")
	assertCoreError(t, err, "conflict")
	if outcome, err := restored.Outcome(original.ID); err != nil || outcome != "pending" {
		t.Fatalf("restored unfinished work was treated as completed: %q %v", outcome, err)
	}
	page, err := restored.Events(original.ID, 0, 100)
	if err != nil || page.More {
		t.Fatalf("restored event page: %+v %v", page, err)
	}
	assertEvolveEvent(t, page.Events, original.ID)
}
