package store

import (
	"database/sql"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestEventPagesBoundBytesWithoutTruncatingRecords(t *testing.T) {
	s, _ := notificationStore(t)
	task := notificationTask(t, s, "payload pages")
	sub := notificationSubscribe(t, s, task.ID, "watcher")
	messages := []string{strings.Repeat("a", 600*1024), strings.Repeat("b", 600*1024), strings.Repeat("c", 2*1024*1024), "last"}
	ids := make([]int64, len(messages))
	if err := s.write(func(tx *sql.Tx) error {
		for i, message := range messages {
			id, err := s.emit(tx, 1, "progress", message, map[string]string{"retained": "exact"})
			if err != nil {
				return err
			}
			ids[i] = id
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	for i, want := range messages {
		delivery, err := s.Offer(sub.ID, "watcher", 100, time.Minute)
		if err != nil {
			t.Fatal(err)
		}
		if len(delivery.Events) != 1 || delivery.Events[0].Message != want || delivery.Through != ids[i] || delivery.Events[0].Truncated || delivery.More != (i < len(messages)-1) {
			t.Fatalf("page %d did not preserve byte boundary, payload or cursor: events=%d through=%d more=%t", i, len(delivery.Events), delivery.Through, delivery.More)
		}
		if err := s.Acknowledge(sub.ID, "watcher", delivery.Token, delivery.Through); err != nil {
			t.Fatal(err)
		}
	}
	delivery, err := s.Offer(sub.ID, "watcher", 100, time.Minute)
	if err != nil || delivery.Token != "" {
		t.Fatalf("drained page repeated: %v %v", delivery, err)
	}
}

func TestSummaryReadsBoundPayloadAndAdvertiseShortenedText(t *testing.T) {
	s, _ := notificationStore(t)
	task := notificationTask(t, s, "summary")
	sub, err := s.Subscribe(task.ID, "watcher", false)
	if err != nil {
		t.Fatal(err)
	}
	message := strings.Repeat("界", 100000)
	if _, err := s.DB.Exec(`UPDATE tasks SET title=?,progress=?,result=? WHERE id=1`, message, message, message); err != nil {
		t.Fatal(err)
	}
	if err := s.write(func(tx *sql.Tx) error {
		_, err := s.emit(tx, 1, "progress", message, map[string]string{"large": strings.Repeat("x", 2*1024*1024)})
		return err
	}); err != nil {
		t.Fatal(err)
	}
	delivery, err := s.OfferSummary(sub.ID, "watcher", 5, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if len(delivery.Events) != 1 || !delivery.Events[0].Truncated || len(delivery.Events[0].Message) > 4*1024 || string(delivery.Events[0].Data) != "null" {
		t.Fatalf("summary fetched complete event payload: events=%d", len(delivery.Events))
	}
	if delivery.Snapshot == nil || !delivery.Snapshot.Truncated || len(delivery.Snapshot.Tasks) != 1 {
		t.Fatalf("summary did not flag shortened snapshot: %+v", delivery.Snapshot)
	}
	item := delivery.Snapshot.Tasks[0]
	if len(item.Title) > 4*256 || len(item.Progress) > 4*1024 || len(item.Result) > 4*1024 {
		t.Fatal("summary fetched complete task payload")
	}
	if err := s.ReleaseDelivery(sub.ID, delivery.Token); err != nil {
		t.Fatal(err)
	}
	full, err := s.Offer(sub.ID, "watcher", 5, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if full.Through != delivery.Through || len(full.Events) != 1 || full.Events[0].Message != message || full.Events[0].Truncated || full.Snapshot.Tasks[0].Title != message {
		t.Fatal("summary changed replay or full delivery payload")
	}
}

func TestEventSelectionUsesTaskIndexAndMergesDescendants(t *testing.T) {
	s, _ := notificationStore(t)
	root := notificationTask(t, s, "root")
	children, err := s.Supersede(root.ID, "", root.Revision, SupersedeSpec{Summary: "split", Replacements: []CreateSpec{{Title: "one", Brief: "one"}, {Title: "two", Brief: "two"}}})
	if err != nil {
		t.Fatal(err)
	}
	unrelated := notificationTask(t, s, "unrelated")
	var cursor int64
	if err := s.DB.QueryRow(`SELECT MAX(id) FROM events`).Scan(&cursor); err != nil {
		t.Fatal(err)
	}
	want := []int64{}
	if err := s.write(func(tx *sql.Tx) error {
		for i := 0; i < 40; i++ {
			id, _ := taskNumber(children[i%2].ID)
			event, err := s.emit(tx, id, "progress", fmt.Sprint(i), nil)
			if err != nil {
				return err
			}
			want = append(want, event)
		}
		other, _ := taskNumber(unrelated.ID)
		_, err := tx.Exec(`WITH RECURSIVE n(i) AS (VALUES(1) UNION ALL SELECT i+1 FROM n WHERE i<10000)
 INSERT INTO events(task_id,kind,message,data,created_at) SELECT ?,'progress','','null',? FROM n`, other, timestamp(s.Now()))
		return err
	}); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < len(want); {
		page, err := s.Events(root.ID, cursor, 7)
		if err != nil {
			t.Fatal(err)
		}
		if len(page.Events) == 0 {
			t.Fatal("skipped descendant events")
		}
		for _, event := range page.Events {
			if i >= len(want) || event.ID != want[i] {
				t.Fatalf("global event order differs at %d: got %d", i, event.ID)
			}
			i++
		}
		cursor = page.Cursor
		if page.More != (i < len(want)) {
			t.Fatal("incorrect descendant page boundary")
		}
	}
	rows, err := s.DB.Query(`EXPLAIN QUERY PLAN `+selectedEvents+`SELECT id FROM selected ORDER BY id`, 1, cursor, 8, 8)
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
		indexed = indexed || strings.Contains(detail, "events_task_id (task_id=? AND id>?)")
		if strings.Contains(detail, "SCAN e") || strings.Contains(detail, "PRIMARY KEY (rowid>?)") {
			t.Fatalf("event query scans global history: %s", detail)
		}
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	if !indexed {
		t.Fatal("event query does not seek the scoped task index")
	}
}

func TestSubscriptionPagesAndHookCandidatesStayBounded(t *testing.T) {
	s, _ := notificationStore(t)
	for i := 0; i < 12; i++ {
		task := notificationTask(t, s, fmt.Sprintf("task %02d", i))
		notificationSubscribe(t, s, task.ID, "watcher")
	}
	all, more, err := s.SubscriptionsPage("watcher", 5, 0)
	if err != nil || len(all) != 5 || !more {
		t.Fatalf("first subscription page: %d %t %v", len(all), more, err)
	}
	last, more, err := s.SubscriptionsPage("watcher", 5, 10)
	if err != nil || len(last) != 2 || more {
		t.Fatalf("last subscription page: %d %t %v", len(last), more, err)
	}
	quiet, err := s.HookSubscriptions("watcher", false, 6)
	if err != nil || len(quiet) != 0 {
		t.Fatalf("quiet subscriptions became candidates: %d %v", len(quiet), err)
	}
	resume, err := s.HookSubscriptions("watcher", true, 6)
	if err != nil || len(resume) != 6 {
		t.Fatalf("resume candidates: %d %v", len(resume), err)
	}
	id, _ := taskNumber(last[1].TaskID)
	if err := s.write(func(tx *sql.Tx) error { _, err := s.emit(tx, id, "progress", "new", nil); return err }); err != nil {
		t.Fatal(err)
	}
	pending, err := s.HookSubscriptions("watcher", false, 6)
	if err != nil || len(pending) != 1 || pending[0].ID != last[1].ID {
		t.Fatalf("later active subscription was hidden behind quiet ones: %+v %v", pending, err)
	}
	delivery, err := s.Offer(pending[0].ID, "watcher", 5, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	pending, err = s.HookSubscriptions("watcher", false, 6)
	if err != nil || len(pending) != 0 {
		t.Fatalf("leased subscription remained a candidate: %+v %v", pending, err)
	}
	if err := s.ReleaseDelivery(delivery.Subscription.ID, delivery.Token); err != nil {
		t.Fatal(err)
	}
	for _, args := range [][2]int{{-1, 0}, {201, 0}, {1, -1}} {
		if _, _, err := s.SubscriptionsPage("watcher", args[0], args[1]); ErrorCode(err) != "invalid_limit" {
			t.Fatalf("invalid paging accepted: %v", err)
		}
	}
}

func BenchmarkNotificationBacklog(b *testing.B) {
	for _, count := range []int{100, 10000, 100000} {
		b.Run(fmt.Sprint(count), func(b *testing.B) {
			s, err := Open(filepath.Join(b.TempDir(), "backlog.sqlite"), "backlog")
			if err != nil {
				b.Fatal(err)
			}
			defer s.Close()
			task, err := s.Create(CreateSpec{Title: "large relevant backlog"})
			if err != nil {
				b.Fatal(err)
			}
			if _, err := s.DB.Exec(`WITH RECURSIVE n(i) AS (VALUES(1) UNION ALL SELECT i+1 FROM n WHERE i<?)
 INSERT INTO events(task_id,kind,message,data,created_at) SELECT 1,'progress','small','{}',? FROM n`, count, timestamp(s.Now())); err != nil {
				b.Fatal(err)
			}
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				page, err := s.Events(task.ID, 0, 20)
				if err != nil || len(page.Events) != 20 || !page.More {
					b.Fatalf("bounded event page: events=%d more=%t err=%v", len(page.Events), page.More, err)
				}
			}
			b.StopTimer()
		})
	}
}
