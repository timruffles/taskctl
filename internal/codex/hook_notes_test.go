package codex

import (
	"bytes"
	"fmt"
	"strings"
	"testing"
	"time"

	"taskctl/internal/store"
)

func acknowledgedHookSubscription(t *testing.T, s *store.Store, taskID string) store.Subscription {
	t.Helper()
	sub, err := s.Subscribe(taskID, "watcher", false)
	if err != nil {
		t.Fatal(err)
	}
	delivery, err := s.Offer(sub.ID, "watcher", 5, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Acknowledge(sub.ID, "watcher", delivery.Token, delivery.Through); err != nil {
		t.Fatal(err)
	}
	return sub
}

func TestHookNotesStayQuietAndResumeShowsOnlyLatestTitles(t *testing.T) {
	s := hookStore(t)
	root, err := s.Create(store.CreateSpec{Title: "import data"})
	if err != nil {
		t.Fatal(err)
	}
	children, err := s.Evolve(root.ID, "", root.Revision, store.EvolveSpec{Summary: "two branches", Replacements: []store.CreateSpec{
		{Title: "parser", Brief: "parse fields"}, {Title: "validator", Brief: "validate fields"},
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
	sub := acknowledgedHookSubscription(t, s, root.ID)
	latest := []string{strings.Repeat("界", 120), "Blank-column validation is complete"}
	for i := 0; i < 6; i++ {
		child := children[i%2]
		title := fmt.Sprintf("old checkpoint %d", i)
		if i >= 4 {
			title = latest[i%2]
		}
		if _, err := s.AddNote(child.ID, child.ClaimID, title, strings.NewReader(strings.Repeat("NOTE_BODY_MUST_STAY_OUT_OF_HOOK_CONTEXT\n", 10000))); err != nil {
			t.Fatal(err)
		}
	}
	postToolUse := `{"hook_event_name":"PostToolUse","session_id":"watcher","tool_name":"exec_command"}`
	for i := 0; i < 3; i++ {
		var out bytes.Buffer
		if err := Handle(s, strings.NewReader(postToolUse), &out, Options{}); err != nil || out.Len() != 0 {
			t.Fatalf("ordinary notes produced hook output: %s %v", out.String(), err)
		}
		if token, _ := currentDelivery(t, s, sub); token != "" {
			t.Fatal("ordinary notes acquired a delivery lease")
		}
	}
	state, _, err := s.SubscriptionsPage("watcher", 20, 0)
	if err != nil || len(state) != 1 || state[0].Cursor != sub.Cursor {
		t.Fatalf("quiet hooks silently acknowledged notes: %+v %v", state, err)
	}
	var out bytes.Buffer
	if err := Handle(s, strings.NewReader(`{"hook_event_name":"SessionStart","session_id":"watcher","source":"resume"}`), &out, Options{}); err != nil {
		t.Fatal(err)
	}
	context := hookContext(t, out.Bytes(), "SessionStart")
	for _, title := range latest {
		if strings.Count(context, title) != 1 {
			t.Fatalf("resume did not show exactly one latest title per branch: %s", context)
		}
	}
	for _, forbidden := range []string{"old checkpoint", "NOTE_BODY_MUST_STAY_OUT_OF_HOOK_CONTEXT", "--token"} {
		if strings.Contains(context, forbidden) {
			t.Fatalf("resume leaked history, bodies or a needless lease: %s", context)
		}
	}
	if len(context) > 6000 {
		t.Fatal("resume exceeded the context budget")
	}
	page, err := s.Events(root.ID, sub.Cursor, 100)
	if err != nil || len(page.Events) != 6 {
		t.Fatalf("quiet hooks removed pullable note history: %+v %v", page, err)
	}
}

func TestHookInterleavedNotesCannotHideCompletionOrCrossAckCursor(t *testing.T) {
	s := hookStore(t)
	now := time.Date(2026, 11, 22, 12, 0, 0, 0, time.UTC)
	s.Now = func() time.Time { return now }
	task := hookTask(t, s, "notification boundaries", "worker")
	sub := acknowledgedHookSubscription(t, s, task.ID)
	addNote := func(title string) {
		t.Helper()
		if _, err := s.AddNote(task.ID, task.ClaimID, title, strings.NewReader(strings.Repeat("BODY_SHOULD_NOT_BE_INJECTED\n", 10000))); err != nil {
			t.Fatal(err)
		}
	}
	addNote("before inactivity")
	now = now.Add(2 * time.Minute)
	if count, err := s.DetectInactivity(time.Minute); err != nil || count != 1 {
		t.Fatalf("fixture inactivity: count=%d err=%v", count, err)
	}
	addNote("after inactivity")
	if _, err := s.Complete(task.ID, task.ClaimID, "All checks pass"); err != nil {
		t.Fatal(err)
	}
	page, err := s.Events(task.ID, sub.Cursor, 100)
	if err != nil || len(page.Events) != 4 {
		t.Fatalf("unexpected event fixture: %+v %v", page, err)
	}
	input := `{"hook_event_name":"PostToolUse","session_id":"watcher","tool_name":"exec_command"}`
	if err := Handle(s, strings.NewReader(input), failingWriter{}, Options{EventLimit: 1}); err == nil {
		t.Fatal("failed output reported success")
	}
	if token, _ := currentDelivery(t, s, sub); token != "" {
		t.Fatal("failed output left a delivery claimed")
	}
	for i, want := range []struct {
		kind, title, absent string
		through             int64
	}{
		{"inactive", "before inactivity", "after inactivity", page.Events[1].ID},
		{"completed", "after inactivity", "before inactivity", page.Events[3].ID},
	} {
		var out bytes.Buffer
		if err := Handle(s, strings.NewReader(input), &out, Options{EventLimit: 1}); err != nil {
			t.Fatal(err)
		}
		context := hookContext(t, out.Bytes(), "PostToolUse")
		if !strings.Contains(context, task.ID+" "+want.kind) || strings.Count(context, want.title) != 1 || strings.Contains(context, want.absent) || strings.Contains(context, "BODY_SHOULD_NOT_BE_INJECTED") {
			t.Fatalf("delivery %d leaked note history or missed an event: %s", i, context)
		}
		token, through := currentDelivery(t, s, sub)
		if through != want.through {
			t.Fatalf("delivery acknowledged beyond shown event: got %d want %d", through, want.through)
		}
		if err := s.Acknowledge(sub.ID, "watcher", token, through); err != nil {
			t.Fatal(err)
		}
	}
	var out bytes.Buffer
	if err := Handle(s, strings.NewReader(input), &out, Options{}); err != nil || out.Len() != 0 {
		t.Fatalf("acknowledged completion repeated: %s %v", out.String(), err)
	}
}

func TestHookResumeRefreshesLatestTitleEvenWithPendingEventPage(t *testing.T) {
	s := hookStore(t)
	task := hookTask(t, s, "resumed work", "worker")
	sub := acknowledgedHookSubscription(t, s, task.ID)
	if _, err := s.AddNote(task.ID, task.ClaimID, "old checkpoint", strings.NewReader("old body")); err != nil {
		t.Fatal(err)
	}
	if err := s.MetadataSet(task.ID, task.ClaimID, "worktree", "/worktrees/task"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.AddNote(task.ID, task.ClaimID, "current checkpoint", strings.NewReader("current body")); err != nil {
		t.Fatal(err)
	}
	page, err := s.Events(task.ID, sub.Cursor, 100)
	if err != nil || len(page.Events) != 3 {
		t.Fatalf("unexpected fixture events: %+v %v", page, err)
	}
	var out bytes.Buffer
	if err := Handle(s, strings.NewReader(`{"hook_event_name":"SessionStart","session_id":"watcher"}`), &out, Options{EventLimit: 1}); err != nil {
		t.Fatal(err)
	}
	context := hookContext(t, out.Bytes(), "SessionStart")
	if strings.Count(context, "current checkpoint") != 1 || strings.Contains(context, "old checkpoint") || strings.Contains(context, "current body") {
		t.Fatalf("resume used event history instead of current title: %s", context)
	}
	_, through := currentDelivery(t, s, sub)
	if through != page.Events[1].ID {
		t.Fatalf("current snapshot advanced the actionable ACK range: got %d want %d", through, page.Events[1].ID)
	}
}
