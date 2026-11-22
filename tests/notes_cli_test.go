package tests

import (
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"unicode/utf8"
)

type note struct {
	ID        string `json:"id"`
	TaskID    string `json:"task_id"`
	EventID   int64  `json:"event_id"`
	Title     string `json:"title"`
	BodyBytes int64  `json:"body_bytes"`
	SHA256    string `json:"sha256"`
	DeletedAt string `json:"deleted_at"`
}

type notePage struct {
	Notes      []note `json:"notes"`
	More       bool   `json:"more"`
	NextBefore int64  `json:"next_before"`
}

func TestProgressNotesKeepBodiesOutOfNormalOutput(t *testing.T) {
	f := newFixture(t)
	task := f.claim(f.create("Stream a note").ID, "writer")
	body := strings.Repeat("## Body-only-marker\nUnicode: λ; evidence and reproduction steps.\n", 40000)
	title := strings.Repeat("é", 120)
	result := f.run(body, "tasks", "progress", task.ID, "--claim", task.ClaimID, "--title", title, "--body-file", "-", "--json")
	if result.code != 0 {
		t.Fatalf("streamed progress: %+v", result)
	}
	var created note
	f.read(result, &created)
	if created.TaskID != task.ID || created.Title != title || created.BodyBytes != int64(len(body)) || len(created.SHA256) != 64 {
		t.Fatalf("unexpected note metadata: %+v", created)
	}
	for _, args := range [][]string{
		{"tasks", "show", task.ID, "--json"},
		{"notes", "show", created.ID, "--json"},
		{"notes", "list", task.ID, "--json"},
		{"tasks", "watch", task.ID, "--after", "0", "--once", "--json"},
	} {
		output := f.must(args...).stdout
		if strings.Contains(output, "Body-only-marker") || len(output) > 10000 {
			t.Fatalf("%v exposed a body: %d bytes", args, len(output))
		}
	}
	var shown struct {
		Progress       string `json:"progress"`
		ProgressNoteID string `json:"progress_note_id"`
	}
	f.read(f.must("tasks", "show", task.ID, "--json"), &shown)
	if shown.Progress != title || shown.ProgressNoteID != created.ID {
		t.Fatalf("latest progress lacks note reference: %+v", shown)
	}
	if got := f.must("notes", "read", created.ID).stdout; got != body {
		t.Fatalf("streamed body mismatch: got %d bytes, want %d", len(got), len(body))
	}
	if result := f.run("", "notes", "read", created.ID, "--json"); result.code != 2 || result.stdout != "" {
		t.Fatalf("JSON body request was not rejected: %+v", result)
	}
}

func TestNotesValidationPagingAndDeletion(t *testing.T) {
	f := newFixture(t)
	task := f.claim(f.create("Manage notes").ID, "writer")
	for _, extra := range [][]string{
		{"--title", strings.Repeat("λ", 121)},
		{"--title", "first\nsecond"},
		{"--title", ""},
		{"--title", "New", "--text", "Old"},
		{"--body-file", "-"},
		{"--text", "Old", "--body-file", "-"},
	} {
		args := append([]string{"tasks", "progress", task.ID, "--claim", task.ClaimID, "--json"}, extra...)
		if result := f.run("ignored body", args...); result.code != 2 {
			t.Fatalf("%v should fail validation: %+v", extra, result)
		}
	}
	var created []note
	for _, title := range []string{"First checkpoint", "Second checkpoint", "Third checkpoint"} {
		var added note
		f.read(f.must("tasks", "progress", task.ID, "--claim", task.ClaimID, "--title", title, "--json"), &added)
		created = append(created, added)
	}
	var page notePage
	f.read(f.must("notes", "list", task.ID, "--limit", "2", "--json"), &page)
	if len(page.Notes) != 2 || !page.More || page.Notes[0].ID != created[2].ID || page.Notes[1].ID != created[1].ID || page.NextBefore != created[1].EventID {
		t.Fatalf("unexpected note page: %+v", page)
	}
	var tail notePage
	f.read(f.must("notes", "list", task.ID, "--limit", "2", "--before", strconv.FormatInt(page.NextBefore, 10), "--json"), &tail)
	if len(tail.Notes) != 1 || tail.More || tail.Notes[0].ID != created[0].ID {
		t.Fatalf("unexpected note tail: %+v", tail)
	}
	if result := f.run("", "notes", "delete", created[2].ID, "--claim", "wrong"); result.code != 3 {
		t.Fatalf("stale claim deleted a note: %+v", result)
	}
	f.must("notes", "delete", created[2].ID, "--claim", task.ClaimID)
	if got := f.get(task.ID).Progress; got != "" {
		t.Fatalf("deleting the latest note resurrected progress %q", got)
	}
	var deleted note
	f.read(f.must("notes", "show", created[2].ID, "--json"), &deleted)
	if deleted.DeletedAt == "" || deleted.Title != "" {
		t.Fatalf("expected a text-free deletion record: %+v", deleted)
	}
	if result := f.run("", "notes", "read", created[2].ID); result.code == 0 {
		t.Fatal("deleted body remained readable")
	}
}

func TestProgressLegacyTextPreservesLongBody(t *testing.T) {
	f := newFixture(t)
	task := f.claim(f.create("Legacy progress").ID, "writer")
	body := "A legacy checkpoint\n\n" + strings.Repeat("Long evidence. ", 100)
	f.must("tasks", "progress", task.ID, "--claim", task.ClaimID, "--text", body)
	var page notePage
	f.read(f.must("notes", "list", task.ID, "--limit", "1", "--json"), &page)
	if len(page.Notes) != 1 || utf8.RuneCountInString(page.Notes[0].Title) > 120 || strings.ContainsAny(page.Notes[0].Title, "\r\n") {
		t.Fatalf("legacy note was not indexed: %+v", page)
	}
	if got := f.must("notes", "read", page.Notes[0].ID).stdout; got != body {
		t.Fatal("legacy full text was not preserved")
	}
}

func TestProgressLegacyTextFileLimit(t *testing.T) {
	f := newFixture(t)
	task := f.claim(f.create("Legacy file input").ID, "writer")
	body := strings.Repeat("x", 1024*1024+1)
	path := filepath.Join(t.TempDir(), "evidence.md")
	if err := os.WriteFile(path, []byte(body), 0600); err != nil {
		t.Fatal(err)
	}
	result := f.run("", "tasks", "progress", task.ID, "--claim", task.ClaimID, "--text-file", path)
	if result.code != 2 || !strings.Contains(result.stderr, "exceeds 1 MiB") {
		t.Fatalf("oversized legacy text file was not rejected: %+v", result)
	}
	var page notePage
	f.read(f.must("notes", "list", task.ID, "--json"), &page)
	if len(page.Notes) != 0 || f.get(task.ID).Progress != "" {
		t.Fatal("rejected legacy file changed progress")
	}
	var created note
	f.read(f.must("tasks", "progress", task.ID, "--claim", task.ClaimID, "--title", "Evidence saved", "--body-file", path, "--json"), &created)
	if created.BodyBytes != int64(len(body)) || f.must("notes", "read", created.ID).stdout != body {
		t.Fatal("body-file did not preserve the larger file")
	}
	result = f.run(body[:1024*1024], "tasks", "progress", task.ID, "--claim", task.ClaimID, "--text-file", "-")
	if result.code != 0 {
		t.Fatalf("legacy input at the 1 MiB boundary failed: %+v", result)
	}
}
