package codex

import (
	"bytes"
	"fmt"
	"strings"
	"testing"
	"time"

	"taskctl/internal/store"
)

func TestHookReachesPendingWorkBehindManyQuietSubscriptions(t *testing.T) {
	s := hookStore(t)
	now := time.Date(2026, 11, 21, 12, 0, 0, 0, time.UTC).Format("2006-01-02T15:04:05.000000000Z")
	if _, err := s.DB.Exec(`WITH RECURSIVE n(i) AS (VALUES(1) UNION ALL SELECT i+1 FROM n WHERE i<1000)
 INSERT INTO tasks(id,title,status,origin_id,created_at,updated_at) SELECT i,'quiet','pending',i,?,? FROM n`, now, now); err != nil {
		t.Fatal(err)
	}
	if _, err := s.DB.Exec(`INSERT INTO subscriptions(id,task_id,session,cursor,snapshot_pending,queue_snapshot_pending,created_at,updated_at)
 SELECT 'Squiet'||id,id,'watcher',0,0,0,?,? FROM tasks`, now, now); err != nil {
		t.Fatal(err)
	}
	var pending []store.Subscription
	for i := 0; i < 7; i++ {
		task := hookTask(t, s, fmt.Sprintf("active %d", i), "worker")
		sub, err := s.Subscribe(task.ID, "watcher", false)
		if err != nil {
			t.Fatal(err)
		}
		pending = append(pending, sub)
	}
	seen := map[string]bool{}
	for pass, want := range []int{5, 2} {
		var output bytes.Buffer
		event := "PostToolUse"
		if pass == 0 {
			event = "SessionStart"
		}
		input := fmt.Sprintf(`{"hook_event_name":%q,"session_id":"watcher","tool_name":"exec_command"}`, event)
		if err := Handle(s, strings.NewReader(input), &output, Options{MaxContextBytes: 6000}); err != nil {
			t.Fatal(err)
		}
		context := hookContext(t, output.Bytes(), event)
		count := 0
		for _, sub := range pending {
			token, through := currentDelivery(t, s, sub)
			if token == "" {
				continue
			}
			if seen[sub.ID] || !strings.Contains(context, token) {
				t.Fatal("hook lost candidate fairness or held an undisplayed claim")
			}
			if err := s.Acknowledge(sub.ID, "watcher", token, through); err != nil {
				t.Fatal(err)
			}
			seen[sub.ID] = true
			count++
		}
		if count != want {
			t.Fatalf("pass %d delivered %d subscriptions, want %d", pass, count, want)
		}
	}
	var output bytes.Buffer
	if err := Handle(s, strings.NewReader(`{"hook_event_name":"PostToolUse","session_id":"watcher","tool_name":"exec_command"}`), &output, Options{}); err != nil || output.Len() != 0 {
		t.Fatalf("fully acknowledged subscriptions are not quiet: %v %s", err, output.String())
	}
}

func TestHookDisclosesShortenedLargeEventText(t *testing.T) {
	s := hookStore(t)
	task := hookTask(t, s, "large event", "worker")
	sub, err := s.Subscribe(task.ID, "watcher", false)
	if err != nil {
		t.Fatal(err)
	}
	initial, err := s.Offer(sub.ID, "watcher", 5, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Acknowledge(sub.ID, "watcher", initial.Token, initial.Through); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Complete(task.ID, task.ClaimID, strings.Repeat("Large completion result ", 10000)); err != nil {
		t.Fatal(err)
	}
	var output bytes.Buffer
	if err := Handle(s, strings.NewReader(`{"hook_event_name":"PostToolUse","session_id":"watcher","tool_name":"exec_command"}`), &output, Options{}); err != nil {
		t.Fatal(err)
	}
	context := hookContext(t, output.Bytes(), "PostToolUse")
	if len(context) > 6000 || !strings.Contains(context, "Event text is shortened") || !strings.Contains(context, "tasks watch "+task.ID+" --after ") {
		t.Fatalf("large event summary is not bounded and explicit: %s", context)
	}
}
