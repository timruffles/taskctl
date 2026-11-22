package store

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math/rand"
	"os"
	"os/exec"
	"reflect"
	"strconv"
	"testing"
	"time"
)

func TestChaosInitialQueueOfferPreservesNewInactivityEvent(t *testing.T) {
	s, _ := notificationStore(t)
	now := time.Date(2026, 11, 21, 10, 0, 0, 0, time.UTC)
	s.Now = func() time.Time { return now }
	task := notificationTask(t, s, "worker stopped before queue watcher started")
	claimed, err := s.Claim(task.ID, "worker")
	if err != nil {
		t.Fatal(err)
	}
	sub, err := s.Subscribe(task.ID, "coordinator", true)
	if err != nil {
		t.Fatal(err)
	}
	now = now.Add(31 * time.Minute)
	if n, err := s.DetectInactivity(30 * time.Minute); err != nil || n != 1 {
		t.Fatalf("detect: %d %v", n, err)
	}
	delivery, err := s.OfferQueue(sub.ID, "coordinator", 5, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, event := range delivery.Events {
		found = found || event.TaskID == task.ID && event.Kind == "inactive"
	}
	if !found {
		t.Fatalf("first queue offer skipped the only inactivity notice: %+v", delivery)
	}
	if err := s.MarkQueued(sub.ID, "coordinator", delivery.Token, delivery.Through); err != nil {
		t.Fatal(err)
	}
	if err := s.ReleaseDelivery(sub.ID, delivery.Token); err != nil {
		t.Fatal(err)
	}
	if n, err := s.DetectInactivity(30 * time.Minute); err != nil || n != 0 {
		t.Fatalf("duplicate inactivity: %d %v", n, err)
	}
	pending, err := s.Offer(sub.ID, "coordinator", 100, time.Minute)
	if err != nil || len(pending.Events) != 1 || pending.Events[0].Kind != "inactive" {
		t.Fatalf("queue receipt lost agent context: %+v %v", pending, err)
	}
	current, err := s.Get(task.ID)
	if err != nil || current.ClaimID != claimed.ClaimID {
		t.Fatalf("silence changed task ownership: %+v %v", current, err)
	}
}

func TestChaosEventCursorMustBelongToCurrentDatabase(t *testing.T) {
	s, _ := notificationStore(t)
	task := notificationTask(t, s, "target")
	if _, err := s.Events(task.ID, 1<<50, 5); ErrorCode(err) != "invalid_cursor" {
		t.Fatalf("future cursor accepted: %v", err)
	}
	_ = notificationTask(t, s, "unrelated later task")
	var global int64
	if err := s.DB.QueryRow("SELECT MAX(id) FROM events").Scan(&global); err != nil {
		t.Fatal(err)
	}
	page, err := s.Events(task.ID, global, 5)
	if err != nil || len(page.Events) != 0 || page.Cursor != global {
		t.Fatalf("valid global cursor mistaken for invalid target cursor: %+v %v", page, err)
	}
}

func chaosScopeEvents(t *testing.T, s *Store, scope map[string]bool, after int64) []int64 {
	t.Helper()
	rows, err := s.DB.Query("SELECT id,task_id FROM events ORDER BY id")
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	result := []int64{}
	for rows.Next() {
		var id, task int64
		if err := rows.Scan(&id, &task); err != nil {
			t.Fatal(err)
		}
		if id > after && scope["T"+strconv.FormatInt(task, 10)] {
			result = append(result, id)
		}
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return result
}
func chaosDeliveryEvents(d Delivery) []int64 {
	ids := make([]int64, 0, len(d.Events))
	for _, e := range d.Events {
		ids = append(ids, e.ID)
	}
	return ids
}

func TestChaosSeededNotificationRecovery(t *testing.T) {
	for _, seed := range []int64{1, 2, 3, 5, 8, 13, 21, 34, 55, 89, 144, 233} {
		t.Run(fmt.Sprintf("seed_%d", seed), func(t *testing.T) {
			s, path := notificationStore(t)
			now := time.Date(2026, 11, 21, 10, 0, 0, 0, time.UTC)
			clock := func() time.Time { return now }
			s.Now = clock
			connections := []*Store{s}
			for i := 0; i < 2; i++ {
				other, err := Open(path, s.ProjectID)
				if err != nil {
					t.Fatal(err)
				}
				other.Now = clock
				connections = append(connections, other)
			}
			t.Cleanup(func() {
				for _, other := range connections[1:] {
					_ = other.Close()
				}
			})
			original := notificationTask(t, s, "original scope")
			sub, err := s.Subscribe(original.ID, "subscriber", true)
			if err != nil {
				t.Fatal(err)
			}
			first, err := s.Claim(original.ID, "worker")
			if err != nil {
				t.Fatal(err)
			}
			active := []Task{first}
			scope := map[string]bool{original.ID: true}
			cursor := sub.Cursor
			seen := []int64{}
			offers := []Delivery{}
			rng := rand.New(rand.NewSource(seed))
			counts := map[string]int{}
			const lease = 250 * time.Millisecond
			record := func() subscriptionRecord {
				r, err := loadSubscription(s.DB, sub.ID)
				if err != nil {
					t.Fatal(err)
				}
				return r
			}
			checkOffer := func(d Delivery, limit int) {
				want := chaosScopeEvents(t, s, scope, cursor)
				if len(want) > limit {
					want = want[:limit]
				}
				if !reflect.DeepEqual(chaosDeliveryEvents(d), want) {
					t.Fatalf("offer skipped or duplicated an unacknowledged event: got=%v want=%v cursor=%d", chaosDeliveryEvents(d), want, cursor)
				}
			}
			consume := func(d Delivery, connection *Store) {
				before := record()
				err := connection.Acknowledge(sub.ID, "subscriber", d.Token, d.Through)
				after := record()
				if err == nil {
					if before.token == d.Token && before.mode == "ack" {
						seen = append(seen, chaosDeliveryEvents(d)...)
						cursor = d.Through
						counts["ack"]++
					}
				} else if ErrorCode(err) != "conflict" {
					t.Fatalf("unexpected ACK failure: %v", err)
				}
				if after.Cursor != cursor {
					t.Fatalf("ACK cursor differs from consumed prefix: got=%d want=%d err=%v", after.Cursor, cursor, err)
				}
			}
			replace := func() {
				task := active[0]
				current, err := s.Get(task.ID)
				if err != nil {
					t.Fatal(err)
				}
				children, err := s.Evolve(current.ID, current.ClaimID, current.Revision, EvolveSpec{Summary: "completed portion retained; two branches remain", Replacements: []CreateSpec{{Key: "continue", Title: "continued branch", Brief: "Finish the current branch"}, {Key: "parallel", Title: "parallel branch", Brief: "Finish the independent branch"}}, ContinueWith: "continue"})
				if err != nil {
					t.Fatal(err)
				}
				children[1], err = s.Claim(children[1].ID, "parallel-worker")
				if err != nil {
					t.Fatal(err)
				}
				active = append(active[1:], children...)
				for _, child := range children {
					scope[child.ID] = true
				}
				counts["evolve"]++
			}
			for step := 0; step < 256; step++ {
				now = now.Add(time.Millisecond)
				if step == 48 || step == 144 {
					replace()
				}
				if step == 192 {
					task := active[len(active)-1]
					if _, err := s.Cancel(task.ID, task.ClaimID, "cancel one branch while others continue"); err != nil {
						t.Fatal(err)
					}
					active = active[:len(active)-1]
					counts["cancel"]++
				}
				connection := connections[rng.Intn(len(connections))]
				switch rng.Intn(8) {
				case 0:
					task := active[rng.Intn(len(active))]
					if _, err := s.Progress(task.ID, task.ClaimID, fmt.Sprintf("seed %d step %d", seed, step)); err != nil {
						t.Fatal(err)
					}
					counts["progress"]++
					if rng.Intn(4) == 0 {
						_ = notificationTask(t, s, "unrelated event stream")
					}
				case 1:
					limit := 1 + rng.Intn(7)
					d, err := connection.Offer(sub.ID, "subscriber", limit, lease)
					if ErrorCode(err) == "delivery_busy" {
						counts["busy"]++
						break
					}
					if err != nil {
						t.Fatal(err)
					}
					if d.Token != "" {
						checkOffer(d, limit)
						offers = append(offers, d)
						counts["offer"]++
					}
				case 2:
					if len(offers) > 0 {
						d := offers[rng.Intn(len(offers))]
						consume(d, connection)
						counts["delayed_ack_attempt"]++
					}
				case 3:
					now = now.Add(2 * time.Second)
					index := 1 + rng.Intn(2)
					if err := connections[index].Close(); err != nil {
						t.Fatal(err)
					}
					other, err := Open(path, s.ProjectID)
					if err != nil {
						t.Fatal(err)
					}
					other.Now = clock
					connections[index] = other
					counts["connection_crash_restart"]++
				case 4:
					before := record()
					d, err := connection.OfferQueue(sub.ID, "subscriber", 1+rng.Intn(7), lease)
					if ErrorCode(err) == "delivery_busy" {
						counts["busy"]++
						break
					}
					if err != nil {
						t.Fatal(err)
					}
					if d.Token == "" {
						break
					}
					offers = append(offers, d)
					if err := connection.Acknowledge(sub.ID, "subscriber", d.Token, d.Through); ErrorCode(err) != "conflict" {
						t.Fatalf("queue token acknowledged agent context: %v", err)
					}
					if rng.Intn(3) == 0 {
						now = now.Add(time.Second)
						counts["queue_acceptance_receipt_lost"]++
					} else {
						if rng.Intn(2) == 0 {
							err = connection.MarkQueued(sub.ID, "subscriber", d.Token, d.Through)
							counts["queue_receipt"]++
						} else {
							err = connection.AdvanceQueueScan(sub.ID, "subscriber", d.Token, d.Through)
							counts["queue_scan"]++
						}
						if err != nil {
							t.Fatal(err)
						}
						if rng.Intn(2) == 0 {
							if err := connection.ReleaseDelivery(sub.ID, d.Token); err != nil {
								t.Fatal(err)
							}
						}
					}
					if after := record(); after.Cursor != before.Cursor {
						t.Fatalf("queue operation consumed context: before=%d after=%d", before.Cursor, after.Cursor)
					}
				case 5:
					if len(offers) > 0 {
						d := offers[rng.Intn(len(offers))]
						before := record()
						err := connection.ReleaseDelivery(sub.ID, d.Token)
						after := record()
						if before.token != d.Token && ErrorCode(err) != "conflict" {
							t.Fatalf("stale release succeeded: %v", err)
						}
						if after.Cursor != cursor {
							t.Fatal("release advanced ACK cursor")
						}
						counts["release_attempt"]++
					}
				case 6:
					if len(offers) > 0 {
						d := offers[rng.Intn(len(offers))]
						if err := connection.Acknowledge(sub.ID, "wrong-session", d.Token, d.Through); ErrorCode(err) != "conflict" {
							t.Fatalf("wrong-session ACK accepted: %v", err)
						}
						if err := connection.Acknowledge(sub.ID, "subscriber", d.Token, d.Through+1); err == nil {
							t.Fatal("cursor outside offered batch acknowledged")
						}
						if record().Cursor != cursor {
							t.Fatal("invalid ACK changed cursor")
						}
						counts["invalid_ack_pair"]++
					}
				case 7:
					limit := 1 + rng.Intn(7)
					d, err := connection.Offer(sub.ID, "subscriber", limit, lease)
					if ErrorCode(err) == "delivery_busy" {
						counts["busy"]++
						break
					}
					if err != nil {
						t.Fatal(err)
					}
					if d.Token != "" {
						checkOffer(d, limit)
						offers = append(offers, d)
						consume(d, connection)
						if err := connection.Acknowledge(sub.ID, "subscriber", d.Token, d.Through); err != nil {
							t.Fatalf("lost ACK response cannot be retried immediately: %v", err)
						}
						counts["duplicate_ack"]++
					}
				}
				state := record()
				if state.Cursor != cursor {
					t.Fatalf("step %d: cursor skipped unconsumed data: %+v model=%d", step, state, cursor)
				}
				if state.QueuedThrough > state.QueueCursor {
					t.Fatalf("step %d: accepted queue beyond scan: %+v", step, state)
				}
				for _, task := range active {
					current, err := s.Get(task.ID)
					if err != nil || current.ClaimID != task.ClaimID || current.Owner != task.Owner {
						t.Fatalf("notification chaos changed task ownership at step %d: %+v %v", step, current, err)
					}
				}
			}
			for _, task := range active {
				if _, err := s.Complete(task.ID, task.ClaimID, "all remaining checks passed"); err != nil {
					t.Fatal(err)
				}
			}
			now = now.Add(2 * time.Second)
			for page := 0; page < 1000; page++ {
				d, err := s.Offer(sub.ID, "subscriber", 5, lease)
				if err != nil {
					t.Fatal(err)
				}
				if d.Token == "" {
					break
				}
				checkOffer(d, 5)
				consume(d, s)
				if page == 999 {
					t.Fatal("finite faults did not permit delivery to drain")
				}
			}
			want := chaosScopeEvents(t, s, scope, sub.Cursor)
			if !reflect.DeepEqual(seen, want) {
				t.Fatalf("consumed stream differs: got=%v want=%v", seen, want)
			}
			outcome, err := s.Outcome(original.ID)
			if err != nil || outcome != "cancelled" {
				t.Fatalf("partial cancellation corrupted aggregate outcome: %s %v", outcome, err)
			}
			var integrity string
			if err := s.DB.QueryRow("PRAGMA integrity_check").Scan(&integrity); err != nil || integrity != "ok" {
				t.Fatalf("integrity: %s %v", integrity, err)
			}
			counts["events_consumed"] = len(seen)
			t.Logf("seed=%d decisions=256 results=%v", seed, counts)
		})
	}
}

func TestChaosNotificationProcessHelper(t *testing.T) {
	if os.Getenv("TASKCTL_NOTIFICATION_CHAOS_CHILD") != "1" {
		t.Skip("subprocess helper")
	}
	s, err := Open(os.Getenv("TASKCTL_NOTIFICATION_CHAOS_DB"), "notification-tests")
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	now, err := time.Parse(time.RFC3339Nano, os.Getenv("TASKCTL_NOTIFICATION_CHAOS_NOW"))
	if err != nil {
		t.Fatal(err)
	}
	s.Now = func() time.Time { return now }
	var d Delivery
	if os.Getenv("TASKCTL_NOTIFICATION_CHAOS_MODE") == "queue" {
		d, err = s.OfferQueue(os.Getenv("TASKCTL_NOTIFICATION_CHAOS_SUB"), "subscriber", 2, time.Minute)
	} else {
		d, err = s.Offer(os.Getenv("TASKCTL_NOTIFICATION_CHAOS_SUB"), "subscriber", 2, time.Minute)
	}
	if err != nil {
		t.Fatal(err)
	}
	if os.Getenv("TASKCTL_NOTIFICATION_CHAOS_MODE") == "queue" {
		chaosRecordQueueEffect(t, os.Getenv("TASKCTL_NOTIFICATION_CHAOS_EFFECT"), d)
	}
	if err = json.NewEncoder(os.Stdout).Encode(d); err != nil {
		t.Fatal(err)
	}
	_, _ = io.Copy(io.Discard, os.Stdin)
}

func TestChaosKilledConsumerAfterCommittedOfferRedelivers(t *testing.T) {
	for _, mode := range []string{"ack", "queue"} {
		t.Run(mode, func(t *testing.T) {
			s, path := notificationStore(t)
			now := time.Date(2026, 11, 21, 11, 0, 0, 0, time.UTC)
			s.Now = func() time.Time { return now }
			task := notificationTask(t, s, "killed consumer")
			task, err := s.Claim(task.ID, "worker")
			if err != nil {
				t.Fatal(err)
			}
			sub, err := s.Subscribe(task.ID, "subscriber", true)
			if err != nil {
				t.Fatal(err)
			}
			for i := 0; i < 7; i++ {
				if _, err := s.Progress(task.ID, task.ClaimID, fmt.Sprintf("progress %d", i)); err != nil {
					t.Fatal(err)
				}
			}
			if _, err := s.Complete(task.ID, task.ClaimID, "completed before transport fault"); err != nil {
				t.Fatal(err)
			}
			executable, err := os.Executable()
			if err != nil {
				t.Fatal(err)
			}
			childContext, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			cmd := exec.CommandContext(childContext, executable, "-test.run=^TestChaosNotificationProcessHelper$")
			cmd.Env = append(os.Environ(), "TASKCTL_NOTIFICATION_CHAOS_CHILD=1", "TASKCTL_NOTIFICATION_CHAOS_DB="+path, "TASKCTL_NOTIFICATION_CHAOS_EFFECT="+path+".queue-effects", "TASKCTL_NOTIFICATION_CHAOS_SUB="+sub.ID, "TASKCTL_NOTIFICATION_CHAOS_MODE="+mode, "TASKCTL_NOTIFICATION_CHAOS_NOW="+now.Format(time.RFC3339Nano))
			stdout, err := cmd.StdoutPipe()
			if err != nil {
				t.Fatal(err)
			}
			stdin, err := cmd.StdinPipe()
			if err != nil {
				t.Fatal(err)
			}
			defer stdin.Close()
			if err = cmd.Start(); err != nil {
				t.Fatal(err)
			}
			waited := false
			t.Cleanup(func() {
				if !waited {
					_ = cmd.Process.Kill()
					_ = cmd.Wait()
				}
			})
			var lost Delivery
			if err = json.NewDecoder(stdout).Decode(&lost); err != nil {
				t.Fatal(err)
			}
			if lost.Token == "" {
				t.Fatal("helper did not commit delivery")
			}
			if err = cmd.Process.Kill(); err != nil {
				t.Fatal(err)
			}
			if err = cmd.Wait(); err == nil {
				t.Fatal("consumer survived SIGKILL")
			}
			waited = true
			before, err := loadSubscription(s.DB, sub.ID)
			if err != nil {
				t.Fatal(err)
			}
			if before.Cursor != sub.Cursor || before.QueuedThrough != 0 {
				t.Fatalf("killed process consumed unacknowledged state: %+v", before)
			}
			if _, err := s.Offer(sub.ID, "subscriber", 2, time.Minute); ErrorCode(err) != "delivery_busy" {
				t.Fatalf("crash erased durable delivery lease: %v", err)
			}
			now = now.Add(2 * time.Minute)
			var retry Delivery
			if mode == "queue" {
				retry, err = s.OfferQueue(sub.ID, "subscriber", 2, time.Minute)
			} else {
				retry, err = s.Offer(sub.ID, "subscriber", 2, time.Minute)
			}
			if err != nil {
				t.Fatal(err)
			}
			if retry.Token == lost.Token || retry.Snapshot == nil || !reflect.DeepEqual(chaosDeliveryEvents(retry), chaosDeliveryEvents(lost)) {
				t.Fatalf("crash did not replay exact first batch: old=%+v new=%+v", lost, retry)
			}
			if err := s.Acknowledge(sub.ID, "subscriber", lost.Token, lost.Through); ErrorCode(err) != "conflict" {
				t.Fatalf("killed consumer token remained usable: %v", err)
			}
			if mode == "queue" {
				effects, err := os.ReadFile(path + ".queue-effects")
				if err != nil {
					t.Fatal(err)
				}
				wantEffect := fmt.Sprintf("%s through %d\n", sub.ID, lost.Through)
				if string(effects) != wantEffect {
					t.Fatalf("queue did not accept before process death: %q", effects)
				}
				chaosRecordQueueEffect(t, path+".queue-effects", retry)
				effects, err = os.ReadFile(path + ".queue-effects")
				if err != nil || string(effects) != wantEffect+wantEffect {
					t.Fatalf("at-least-once queue retry was not observable: %q %v", effects, err)
				}
				if err := s.MarkQueued(sub.ID, "subscriber", lost.Token, lost.Through); ErrorCode(err) != "conflict" {
					t.Fatalf("stale transport receipt accepted: %v", err)
				}
				if err := s.MarkQueued(sub.ID, "subscriber", retry.Token, retry.Through); err != nil {
					t.Fatal(err)
				}
				if err := s.ReleaseDelivery(sub.ID, retry.Token); err != nil {
					t.Fatal(err)
				}
			} else {
				if err := s.Acknowledge(sub.ID, "subscriber", retry.Token, retry.Through); err != nil {
					t.Fatal(err)
				}
			}
			after, err := loadSubscription(s.DB, sub.ID)
			if err != nil {
				t.Fatal(err)
			}
			if mode == "queue" && after.Cursor != sub.Cursor {
				t.Fatal("queue receipt acknowledged model context")
			}
			var integrity string
			if err := s.DB.QueryRow("PRAGMA integrity_check").Scan(&integrity); err != nil || integrity != "ok" {
				t.Fatalf("integrity after consumer SIGKILL: %s %v", integrity, err)
			}
		})
	}
}

func chaosRecordQueueEffect(t *testing.T, path string, d Delivery) {
	t.Helper()
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0600)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = fmt.Fprintf(f, "%s through %d\n", d.Subscription.ID, d.Through); err != nil {
		f.Close()
		t.Fatal(err)
	}
	if err = f.Sync(); err != nil {
		f.Close()
		t.Fatal(err)
	}
	if err = f.Close(); err != nil {
		t.Fatal(err)
	}
}
