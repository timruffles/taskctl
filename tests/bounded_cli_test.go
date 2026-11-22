package tests

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestDetailListsReportContinuation(t *testing.T) {
	f := newFixture(t)
	original := f.create("Original scope")
	replacements := make([]map[string]string, 25)
	for i := range replacements {
		replacements[i] = map[string]string{"title": fmt.Sprintf("Remaining task %d", i), "brief": "Self-contained remaining scope"}
	}
	plan := writeSpec(t, f.root, "remaining.json", map[string]any{"summary": "Split remaining work", "replacements": replacements})
	f.must("tasks", "evolve", original.ID, "--revision", fmt.Sprint(original.Revision), "--file", plan, "--json")
	var history struct {
		Tasks      []task `json:"tasks"`
		More       bool   `json:"more"`
		NextOffset int    `json:"next_offset"`
	}
	f.read(f.must("tasks", "history", original.ID, "--json"), &history)
	if len(history.Tasks) != 20 || !history.More || history.NextOffset != 20 {
		t.Fatalf("history first page: %+v", history)
	}
	f.read(f.must("tasks", "history", original.ID, "--offset", "20", "--json"), &history)
	if len(history.Tasks) != 6 || history.More || history.NextOffset != 26 {
		t.Fatalf("history final page: %+v", history)
	}
	for i := 2; i <= 26; i++ {
		f.must("tasks", "subscribe", fmt.Sprintf("T%d", i), "--session", "observer", "--json")
	}
	var subscriptions struct {
		Items      []any `json:"subscriptions"`
		More       bool  `json:"more"`
		NextOffset int   `json:"next_offset"`
	}
	f.read(f.must("tasks", "subscriptions", "--session", "observer", "--json"), &subscriptions)
	if len(subscriptions.Items) != 20 || !subscriptions.More || subscriptions.NextOffset != 20 {
		t.Fatalf("subscription first page: %+v", subscriptions)
	}
	f.read(f.must("tasks", "subscriptions", "--session", "observer", "--offset", "20", "--json"), &subscriptions)
	if len(subscriptions.Items) != 5 || subscriptions.More || subscriptions.NextOffset != 25 {
		t.Fatalf("subscription final page: %+v", subscriptions)
	}
	for i := 0; i < 21; i++ {
		f.must("scratch", "create", "T2", "--json")
	}
	var scratch struct {
		Areas      []any `json:"areas"`
		More       bool  `json:"more"`
		NextOffset int   `json:"next_offset"`
	}
	f.read(f.must("scratch", "list", "T2", "--json"), &scratch)
	if len(scratch.Areas) != 20 || !scratch.More || scratch.NextOffset != 20 {
		t.Fatalf("scratch first page: %+v", scratch)
	}
	f.read(f.must("scratch", "list", "T2", "--offset", "20", "--json"), &scratch)
	if len(scratch.Areas) != 1 || scratch.More || scratch.NextOffset != 21 {
		t.Fatalf("scratch final page: %+v", scratch)
	}
	var shown struct {
		Scratch []any `json:"scratch"`
		More    bool  `json:"scratch_more"`
	}
	f.read(f.must("tasks", "show", "T2", "--json"), &shown)
	if len(shown.Scratch) != 20 || !shown.More {
		t.Fatalf("task detail omitted pagination marker: %+v", shown)
	}
	for _, args := range [][]string{
		{"tasks", "history", "T1", "--limit", "201"},
		{"tasks", "subscriptions", "--limit", "0"},
		{"scratch", "list", "T2", "--offset", "-1"},
	} {
		if result := f.run("", args...); result.code != 2 {
			t.Fatalf("invalid pagination accepted: %v -> %+v", args, result)
		}
	}
}

func TestSQLiteOnlyCLI(t *testing.T) {
	f := newFixture(t)
	created := f.create("Preserved in SQLite")
	if result := f.run("", "export", "--json"); result.code != 2 || result.stdout != "" || !strings.Contains(result.stderr, "unknown command export") {
		t.Fatalf("removed export command: %+v", result)
	}
	jsonPath := filepath.Join(f.root, "old-export.json")
	if err := os.WriteFile(jsonPath, []byte(`{"format":"taskctl-export","schema_version":1,"project_id":"old","tables":{}}`), 0600); err != nil {
		t.Fatal(err)
	}
	unselected := *f
	unselected.project = ""
	if result := unselected.run("", "restore", "--file", jsonPath, "--name", "rejected-json", "--json"); result.code != 2 || !strings.Contains(result.stderr, "SQLite backup") {
		t.Fatalf("JSON restore was not rejected clearly: %+v", result)
	}
	backup := filepath.Join(f.root, "backup.sqlite")
	f.must("backup", "--file", backup, "--json")
	var restored struct {
		ID string `json:"id"`
	}
	f.read(unselected.must("restore", "--file", backup, "--name", "restored", "--json"), &restored)
	var got task
	unselected.project = restored.ID
	f.read(unselected.must("tasks", "show", created.ID, "--json"), &got)
	if restored.ID == f.project || got.Title != created.Title {
		t.Fatalf("SQLite round trip: project=%s task=%+v", restored.ID, got)
	}
}

func TestWatchOnceWithoutCursorReturnsOneSnapshot(t *testing.T) {
	f := newFixture(t)
	created := f.create("Current task")
	output := f.must("tasks", "watch", created.ID, "--once", "--json").stdout
	if strings.Count(output, "\n") != 1 || !strings.Contains(output, `"type":"snapshot"`) {
		t.Fatalf("once returned more than the current snapshot: %s", output)
	}
}
