package codex

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"

	"taskctl/internal/store"
)

func hookStore(t *testing.T) *store.Store {
	t.Helper()
	s, err := store.Open(filepath.Join(t.TempDir(), "tasks.sqlite"), "hook-tests")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

func hookTask(t *testing.T, s *store.Store, title, owner string) store.Task {
	t.Helper()
	task, err := s.Create(store.CreateSpec{Title: title, Brief: "Finish " + title})
	if err != nil {
		t.Fatal(err)
	}
	task, err = s.Claim(task.ID, owner)
	if err != nil {
		t.Fatal(err)
	}
	return task
}

func currentDelivery(t *testing.T, s *store.Store, subscription store.Subscription) (string, int64) {
	t.Helper()
	var token string
	var through int64
	if err := s.DB.QueryRow(`SELECT delivery_token,offered_through FROM subscriptions WHERE id=?`, subscription.ID).Scan(&token, &through); err != nil {
		t.Fatal(err)
	}
	return token, through
}

func hookContext(t *testing.T, output []byte, event string) string {
	t.Helper()
	var response hookOutput
	if err := json.Unmarshal(output, &response); err != nil {
		t.Fatalf("invalid hook output: %s (%v)", output, err)
	}
	if response.Specific.Event != event {
		t.Fatalf("wrong hook event: %+v", response)
	}
	return response.Specific.Context
}

func TestHookChildIdentityDurableAckAndQuietUnchangedOutput(t *testing.T) {
	s := hookStore(t)
	now := time.Date(2026, 11, 21, 12, 0, 0, 0, time.UTC)
	s.Now = func() time.Time { return now }
	parent := hookTask(t, s, "parent work", "parent")
	child := hookTask(t, s, "child work", "child")
	sub, err := s.Subscribe(child.ID, "child", false)
	if err != nil {
		t.Fatal(err)
	}
	now = now.Add(time.Minute)
	input := `{"hook_event_name":"PostToolUse","session_id":"parent","agent_id":"child","tool_name":"exec_command","future_upstream_field":true}`
	var output bytes.Buffer
	if err := Handle(s, strings.NewReader(input), &output, Options{}); err != nil {
		t.Fatal(err)
	}
	context := hookContext(t, output.Bytes(), "PostToolUse")
	token, through := currentDelivery(t, s, sub)
	if token == "" || !strings.Contains(context, token) || !strings.Contains(context, "--session 'child'") || !strings.Contains(context, "--through "+strconv.FormatInt(through, 10)) {
		t.Fatalf("missing usable child acknowledgement: %s", context)
	}
	parentAfter, _ := s.Get(parent.ID)
	childAfter, _ := s.Get(child.ID)
	if parentAfter.LastActivityAt != parent.LastActivityAt || childAfter.LastActivityAt == child.LastActivityAt || childAfter.Revision != child.Revision {
		t.Fatalf("hook activity attributed incorrectly: parent=%+v child=%+v", parentAfter, childAfter)
	}
	subs, _, err := s.SubscriptionsPage("child", 200, 0)
	if err != nil || !subs[0].SnapshotPending || subs[0].Cursor != sub.Cursor {
		t.Fatalf("hook output acknowledged itself: %+v, %v", subs, err)
	}
	output.Reset()
	if err := Handle(s, strings.NewReader(input), &output, Options{}); err != nil || output.Len() != 0 {
		t.Fatalf("busy delivery produced duplicate output: %s %v", output.String(), err)
	}
	if err := s.Acknowledge(sub.ID, "child", token, through); err != nil {
		t.Fatal(err)
	}
	if err := Handle(s, strings.NewReader(input), &output, Options{}); err != nil || output.Len() != 0 {
		t.Fatalf("unchanged PostToolUse produced output: %s %v", output.String(), err)
	}
	if err := Handle(s, strings.NewReader(`{"hook_event_name":"SessionStart","session_id":"parent","agent_id":"child","source":"resume"}`), &output, Options{}); err != nil {
		t.Fatal(err)
	}
	context = hookContext(t, output.Bytes(), "SessionStart")
	if !strings.Contains(context, "child work") || strings.Contains(context, "--token") {
		t.Fatalf("resume failed to restore acknowledged current state: %s", context)
	}
}

func TestHookCapsAndCoalescesProgressWithoutAdvancingUndisplayedEvents(t *testing.T) {
	s := hookStore(t)
	task := hookTask(t, s, "bounded context", "worker")
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
	for i := 0; i < 6; i++ {
		if _, err := s.Progress(task.ID, task.ClaimID, strconv.Itoa(i)+strings.Repeat("long useful progress context ", 100)); err != nil {
			t.Fatal(err)
		}
	}
	expected, err := s.Events(task.ID, sub.Cursor, 100)
	if err != nil {
		t.Fatal(err)
	}
	var received []int64
	for page := 0; page < 3; page++ {
		var output bytes.Buffer
		if err := Handle(s, strings.NewReader(`{"hook_event_name":"PostToolUse","session_id":"watcher","tool_name":"exec_command"}`), &output, Options{EventLimit: 2, MaxContextBytes: 2000}); err != nil {
			t.Fatal(err)
		}
		context := hookContext(t, output.Bytes(), "PostToolUse")
		if len(context) > 2000 || !strings.Contains(context, "(2 updates)") {
			t.Fatalf("context was not bounded and coalesced: %s", context)
		}
		token, through := currentDelivery(t, s, sub)
		received = append(received, through)
		if through != expected.Events[page*2+1].ID {
			t.Fatalf("page cursor skipped events: got %d wanted %d", through, expected.Events[page*2+1].ID)
		}
		if err := s.Acknowledge(sub.ID, "watcher", token, through); err != nil {
			t.Fatal(err)
		}
	}
	if len(received) != 3 {
		t.Fatal("missing progress pages")
	}
}

type failingWriter struct{}

func (failingWriter) Write([]byte) (int, error) { return 0, errors.New("output closed") }

func TestHookOutputFailureReleasesClaimWithoutConsumingNotice(t *testing.T) {
	s := hookStore(t)
	task := hookTask(t, s, "output failure", "worker")
	sub, err := s.Subscribe(task.ID, "watcher", false)
	if err != nil {
		t.Fatal(err)
	}
	err = Handle(s, strings.NewReader(`{"hook_event_name":"SessionStart","session_id":"watcher"}`), failingWriter{}, Options{})
	if err == nil {
		t.Fatal("output failure was ignored")
	}
	delivery, err := s.Offer(sub.ID, "watcher", 5, time.Minute)
	if err != nil || delivery.Snapshot == nil || delivery.Token == "" {
		t.Fatalf("failed output lost or locked notice: %+v %v", delivery, err)
	}
}

func TestHookRejectsMalformedIdentityBeforeUpdatingActivity(t *testing.T) {
	s := hookStore(t)
	for _, input := range []string{
		`{}`, `null`, `[]`, `{"hook_event_name":"Stop","session_id":"root"}`,
		`{"hook_event_name":"PostToolUse","session_id":"root"}`,
		`{"hook_event_name":"SessionStart","session_id":"root\nother"}`,
		`{"hook_event_name":"SessionStart","session_id":"root"} {}`,
	} {
		var out bytes.Buffer
		if err := Handle(s, strings.NewReader(input), &out, Options{}); err == nil || out.Len() != 0 {
			t.Fatalf("malformed hook accepted: %s (%v)", input, err)
		}
	}
}

func TestGeneratedConfigQuotesExecutableAndProjectAsLiteralShellArguments(t *testing.T) {
	executable := filepath.Join(t.TempDir(), "task ' $(printf SURPRISE)")
	if err := os.WriteFile(executable, []byte("#!/bin/sh\nprintf '%s\\n' \"$@\"\n"), 0700); err != nil {
		t.Fatal(err)
	}
	project := "project ' $(printf SURPRISE)"
	config, err := Config(executable, project)
	if err != nil {
		t.Fatal(err)
	}
	var parsed struct {
		Hooks map[string][]struct {
			Hooks []struct {
				Command string `json:"command"`
				Async   bool   `json:"async"`
			} `json:"hooks"`
		} `json:"hooks"`
	}
	if err := json.Unmarshal(config, &parsed); err != nil {
		t.Fatal(err)
	}
	if len(parsed.Hooks) != 2 || len(parsed.Hooks["SessionStart"]) != 1 || len(parsed.Hooks["PostToolUse"]) != 1 {
		t.Fatalf("unexpected generated hooks: %s", config)
	}
	for _, event := range []string{"SessionStart", "PostToolUse"} {
		hook := parsed.Hooks[event][0].Hooks[0]
		if !hook.Async {
			t.Fatal("generated hook blocks Codex")
		}
		output, err := exec.Command("sh", "-c", hook.Command).Output()
		if err != nil {
			t.Fatal(err)
		}
		got := strings.Split(strings.TrimSuffix(string(output), "\n"), "\n")
		want := []string{"--project", project, "integrations", "codex", "hook"}
		if !reflect.DeepEqual(got, want) {
			t.Fatalf("shell interpreted literal arguments: got %q want %q", got, want)
		}
	}
	if _, err := Config("relative/taskctl", "project"); err == nil {
		t.Fatal("relative executable accepted in hook config")
	}
}

func TestHookAloneDetectsSilentOtherWorkerOnceAndKeepsActiveOwnerQuiet(t *testing.T) {
	for _, test := range []struct {
		name    string
		options Options
		elapsed time.Duration
	}{
		{name: "default thirty minutes", elapsed: 31 * time.Minute},
		{name: "configured two minutes", options: Options{Inactivity: 2 * time.Minute}, elapsed: 3 * time.Minute},
	} {
		t.Run(test.name, func(t *testing.T) {
			s := hookStore(t)
			now := time.Date(2026, 11, 21, 12, 0, 0, 0, time.UTC)
			s.Now = func() time.Time { return now }
			silent := hookTask(t, s, "silent worker", "other-worker")
			active := hookTask(t, s, "active subscriber", "watcher")
			sub, err := s.Subscribe(silent.ID, "watcher", false)
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
			now = now.Add(test.elapsed)
			input := `{"hook_event_name":"PostToolUse","session_id":"watcher","tool_name":"exec_command"}`
			var output bytes.Buffer
			if err := Handle(s, strings.NewReader(input), &output, test.options); err != nil {
				t.Fatal(err)
			}
			context := hookContext(t, output.Bytes(), "PostToolUse")
			if !strings.Contains(context, silent.ID+" inactive") {
				t.Fatalf("hook-only subscription missed silent worker: %s", context)
			}
			page, err := s.Events(silent.ID, sub.Cursor, 100)
			if err != nil || len(page.Events) != 1 || page.Events[0].Kind != "inactive" {
				t.Fatalf("unexpected silence events: %+v %v", page, err)
			}
			token, through := currentDelivery(t, s, sub)
			if err := s.Acknowledge(sub.ID, "watcher", token, through); err != nil {
				t.Fatal(err)
			}
			for i := 0; i < 3; i++ {
				now = now.Add(test.elapsed)
				output.Reset()
				if err := Handle(s, strings.NewReader(input), &output, test.options); err != nil || output.Len() != 0 {
					t.Fatalf("same silence period/recent own activity produced noise: %s %v", output.String(), err)
				}
			}
			page, err = s.Events(silent.ID, sub.Cursor, 100)
			if err != nil || len(page.Events) != 1 {
				t.Fatalf("silence period was not deduplicated: %+v %v", page, err)
			}
			ownEvents, err := s.Events(active.ID, 0, 100)
			if err != nil {
				t.Fatal(err)
			}
			for _, event := range ownEvents.Events {
				if event.Kind == "inactive" {
					t.Fatalf("owner was scanned before its activity was refreshed: %+v", event)
				}
			}
			current, err := s.Get(silent.ID)
			if err != nil || current.Status != "in_progress" || current.ClaimID != silent.ClaimID {
				t.Fatalf("inactivity detection changed task ownership: %+v %v", current, err)
			}
		})
	}
}

func TestSessionStartRefreshesResumedChildBeforeInactivityScan(t *testing.T) {
	s := hookStore(t)
	now := time.Date(2026, 11, 21, 12, 0, 0, 0, time.UTC)
	s.Now = func() time.Time { return now }
	task := hookTask(t, s, "resumed child work", "child")
	sub, err := s.Subscribe(task.ID, "child", false)
	if err != nil {
		t.Fatal(err)
	}
	initial, err := s.Offer(sub.ID, "child", 5, time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if err := s.Acknowledge(sub.ID, "child", initial.Token, initial.Through); err != nil {
		t.Fatal(err)
	}
	now = now.Add(time.Hour)
	var output bytes.Buffer
	if err := Handle(s, strings.NewReader(`{"hook_event_name":"SessionStart","session_id":"parent","agent_id":"child","source":"resume"}`), &output, Options{}); err != nil {
		t.Fatal(err)
	}
	context := hookContext(t, output.Bytes(), "SessionStart")
	if !strings.Contains(context, "resumed child work") || strings.Contains(context, "inactive") {
		t.Fatalf("resume emitted false inactivity or lost current state: %s", context)
	}
	page, err := s.Events(task.ID, sub.Cursor, 100)
	if err != nil || len(page.Events) != 0 {
		t.Fatalf("resume generated a spurious inactivity event: %+v %v", page, err)
	}
	output.Reset()
	if err := Handle(s, strings.NewReader(`{"hook_event_name":"PostToolUse","session_id":"parent","agent_id":"child","tool_name":"exec_command"}`), &output, Options{}); err != nil || output.Len() != 0 {
		t.Fatalf("fresh resumed child produced notification noise: %s %v", output.String(), err)
	}
}
