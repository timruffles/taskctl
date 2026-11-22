package codex

import (
	"bytes"
	"fmt"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"taskctl/internal/store"
)

func TestChaosHookLargeInitialSnapshotAndEventsRemainDeliverable(t *testing.T) {
	s := hookStore(t)
	original, err := s.Create(store.CreateSpec{Title: "original"})
	if err != nil {
		t.Fatal(err)
	}
	children, err := s.Supersede(original.ID, "", original.Revision, store.SupersedeSpec{Summary: "three active branches", Replacements: []store.CreateSpec{
		{Title: strings.Repeat("\"", 180), Brief: "first branch"},
		{Title: strings.Repeat("\"", 180), Brief: "second branch"},
		{Title: strings.Repeat("\"", 180), Brief: "third branch"},
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
	sub, err := s.Subscribe(original.ID, "watcher", false)
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 5; i++ {
		child := children[i%len(children)]
		if _, err = s.AddNote(child.ID, child.ClaimID, strings.Repeat("\"", 120), strings.NewReader(strings.Repeat("\"", 400))); err != nil {
			t.Fatal(err)
		}
		if err = s.MetadataSet(child.ID, child.ClaimID, "checkpoint", fmt.Sprint(i)); err != nil {
			t.Fatal(err)
		}
	}
	for attempt := 0; attempt < 3; attempt++ {
		var out bytes.Buffer
		err = Handle(s, strings.NewReader(`{"hook_event_name":"PostToolUse","session_id":"watcher","tool_name":"exec_command"}`), &out, Options{EventLimit: 5, MaxContextBytes: 3000})
		if err != nil {
			t.Fatalf("valid pending state cannot ever fit default CLI context budget: %v", err)
		}
		context := hookContext(t, out.Bytes(), "PostToolUse")
		if len(context) > 3000 {
			t.Fatalf("oversized context: %d", len(context))
		}
		token, through := currentDelivery(t, s, sub)
		if token == "" {
			t.Fatal("large valid batch made no delivery progress")
		}
		if err = s.Acknowledge(sub.ID, "watcher", token, through); err != nil {
			t.Fatal(err)
		}
		left, err := s.Offer(sub.ID, "watcher", 5, time.Minute)
		if err != nil {
			t.Fatal(err)
		}
		if left.Token == "" {
			return
		}
		if err = s.ReleaseDelivery(sub.ID, left.Token); err != nil {
			t.Fatal(err)
		}
	}
	t.Fatal("bounded deliveries did not drain after finite retries")
}

func TestChaosHookMinimumBudgetShrinksPageWithoutSkippingEvents(t *testing.T) {
	s := hookStore(t)
	task := hookTask(t, s, "bounded event storm", "worker")
	sub, err := s.Subscribe(task.ID, "watcher", false)
	if err != nil {
		t.Fatal(err)
	}
	initial, err := s.Offer(sub.ID, "watcher", 5, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if err = s.Acknowledge(sub.ID, "watcher", initial.Token, initial.Through); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 25; i++ {
		if err = s.MetadataSet(task.ID, task.ClaimID, fmt.Sprintf("checkpoint-%d", i), strings.Repeat("long metadata ", 50)); err != nil {
			t.Fatal(err)
		}
	}
	expected, err := s.Events(task.ID, sub.Cursor, 100)
	if err != nil {
		t.Fatal(err)
	}
	cursor := sub.Cursor
	pages := 0
	for pages < 25 {
		var out bytes.Buffer
		if err := Handle(s, strings.NewReader(`{"hook_event_name":"PostToolUse","session_id":"watcher","tool_name":"exec_command"}`), &out, Options{EventLimit: 20, MaxContextBytes: 512}); err != nil {
			t.Fatal(err)
		}
		if out.Len() == 0 {
			break
		}
		context := hookContext(t, out.Bytes(), "PostToolUse")
		if len(context) > 512 {
			t.Fatalf("context budget exceeded: %d", len(context))
		}
		token, through := currentDelivery(t, s, sub)
		if token == "" || through <= cursor {
			t.Fatal("bounded hook made no delivery progress")
		}
		for _, event := range expected.Events {
			if event.ID > cursor && event.ID <= through && !strings.Contains(context, fmt.Sprintf("E%d %s %s", event.ID, event.TaskID, event.Kind)) {
				t.Fatalf("ACK range includes an event absent from compact summary: event=%d context=%s", event.ID, context)
			}
		}
		if err = s.Acknowledge(sub.ID, "watcher", token, through); err != nil {
			t.Fatal(err)
		}
		cursor = through
		pages++
	}
	if cursor != expected.Events[len(expected.Events)-1].ID || pages < 2 {
		t.Fatalf("wrong drained range/pages: cursor=%d pages=%d", cursor, pages)
	}
}

func TestChaosConcurrentHooksAfterLostOutputAndExpiry(t *testing.T) {
	path := filepath.Join(t.TempDir(), "tasks.sqlite")
	s, err := store.Open(path, "hook-chaos")
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	var nanos atomic.Int64
	nanos.Store(time.Date(2026, 11, 21, 12, 0, 0, 0, time.UTC).UnixNano())
	clock := func() time.Time { return time.Unix(0, nanos.Load()) }
	s.Now = clock
	var subs []store.Subscription
	for i := 0; i < 6; i++ {
		task := hookTask(t, s, fmt.Sprintf("worker branch %d", i), "worker")
		sub, err := s.Subscribe(task.ID, "watcher", false)
		if err != nil {
			t.Fatal(err)
		}
		subs = append(subs, sub)
	}
	connections := []*store.Store{s}
	for i := 1; i < 8; i++ {
		other, err := store.Open(path, s.ProjectID)
		if err != nil {
			t.Fatal(err)
		}
		other.Now = clock
		connections = append(connections, other)
	}
	defer func() {
		for _, other := range connections[1:] {
			other.Close()
		}
	}()
	run := func(event string) []string {
		start := make(chan struct{})
		type result struct {
			output string
			err    error
		}
		results := make(chan result, len(connections))
		var wg sync.WaitGroup
		for _, connection := range connections {
			wg.Add(1)
			go func(connection *store.Store) {
				defer wg.Done()
				<-start
				var out bytes.Buffer
				input := fmt.Sprintf(`{"hook_event_name":%q,"session_id":"watcher","tool_name":"exec_command"}`, event)
				err := Handle(connection, strings.NewReader(input), &out, Options{Lease: time.Minute, MaxContextBytes: 3000})
				results <- result{out.String(), err}
			}(connection)
		}
		close(start)
		wg.Wait()
		close(results)
		outputs := []string{}
		for result := range results {
			if result.err != nil {
				t.Fatal(result.err)
			}
			if result.output != "" {
				context := hookContext(t, []byte(result.output), event)
				if len(context) > 3000 {
					t.Fatal("concurrent output budget exceeded")
				}
				outputs = append(outputs, context)
			}
		}
		return outputs
	}
	first := run("PostToolUse")
	old := map[string]string{}
	for _, sub := range subs {
		token, _ := currentDelivery(t, s, sub)
		if token == "" {
			t.Fatalf("contending hooks left subscription unoffered: %s", sub.ID)
		}
		old[sub.ID] = token
		count := 0
		for _, out := range first {
			if strings.Contains(out, token) {
				count++
			}
		}
		if count != 1 {
			t.Fatalf("same offer delivered by %d concurrent hooks", count)
		}
	}
	_ = run("SessionStart")
	nanos.Add(int64(2 * time.Minute))
	replay := run("PostToolUse")
	for _, sub := range subs {
		token, through := currentDelivery(t, s, sub)
		if token == "" || token == old[sub.ID] {
			t.Fatalf("lost output did not obtain fresh lease: %s", sub.ID)
		}
		count := 0
		for _, out := range replay {
			if strings.Contains(out, token) {
				count++
			}
		}
		if count != 1 {
			t.Fatalf("replay duplicated across %d hooks", count)
		}
		if err := s.Acknowledge(sub.ID, "watcher", old[sub.ID], through); store.ErrorCode(err) != "conflict" {
			t.Fatalf("late old hook acknowledged new offer: %v", err)
		}
		if err := s.Acknowledge(sub.ID, "watcher", token, through); err != nil {
			t.Fatal(err)
		}
	}
	if outputs := run("PostToolUse"); len(outputs) != 0 {
		t.Fatalf("acknowledged subscriptions made %d unchanged hooks noisy", len(outputs))
	}
}
