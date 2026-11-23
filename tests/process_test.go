package tests

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestTaskListFiltersWithCELBeforePagination(t *testing.T) {
	f := newFixture(t)
	f.create("Advisor behavior review", "--tag", "human")
	first := f.create("Advisor layout", "--metadata", "beads_id=asc-42")
	second := f.create("Advisor behavior")
	f.create("Planet layout")
	f.create("Advisor behavior follow-up", "--prerequisite", first.ID)

	const filter = `search(title, "ADVISOR") && (metadata.exists(k, k == "beads_id" && metadata[k] == "asc-42") || search(brief, "behavior"))`
	for offset, want := range []string{first.ID, second.ID} {
		var page struct {
			Tasks []task `json:"tasks"`
			Total int    `json:"total"`
		}
		f.read(f.must("tasks", "available", "--where", filter, "--exclude-tag", "human",
			"--limit", "1", "--offset", fmt.Sprint(offset), "--json"), &page)
		if len(page.Tasks) != 1 || page.Tasks[0].ID != want || page.Total != 2 {
			t.Fatalf("offset %d: expected %s and two matches, got %+v", offset, want, page)
		}
	}
}

func TestTaskListRejectsInvalidCEL(t *testing.T) {
	f := newFixture(t)
	for _, expression := range []string{`unknown == "x"`, `title`} {
		result := f.run("", "tasks", "list", "--where", expression, "--json")
		if result.code != 2 || result.stdout != "" || !strings.Contains(result.stderr, "--where") {
			t.Fatalf("expected usage error for %q, got %+v", expression, result)
		}
	}
}

func TestTaskListExcludesTagsBeforePagination(t *testing.T) {
	f := newFixture(t)
	f.create("human review", "--tag", "queue", "--tag", "human")
	first := f.create("first agent task", "--tag", "queue")
	f.create("epic", "--tag", "queue", "--tag", "epic")
	second := f.create("second agent task", "--tag", "queue")
	f.create("other work")

	for _, view := range []string{"available", "list"} {
		t.Run(view, func(t *testing.T) {
			for offset, want := range []string{first.ID, second.ID} {
				var page struct {
					Tasks      []task `json:"tasks"`
					Total      int    `json:"total"`
					Omitted    int    `json:"omitted"`
					NextOffset int    `json:"next_offset"`
				}
				f.read(f.must("tasks", view, "--tag", "queue", "--exclude-tag", "human",
					"--exclude-tag", "epic", "--limit", "1", "--offset", fmt.Sprint(offset), "--json"), &page)
				if len(page.Tasks) != 1 || page.Tasks[0].ID != want ||
					page.Total != 2 || page.Omitted != 1-offset || page.NextOffset != offset+1 {
					t.Fatalf("offset %d: expected task %s and pagination across two matches, got %+v", offset, want, page)
				}
			}
		})
	}
}

func TestIndependentProcessesClaimExactlyOnce(t *testing.T) {
	f := newFixture(t)
	created := f.create("one contested assignment")
	results := race(8, func(i int) commandResult {
		return f.run("", "tasks", "claim", created.ID, "--session", fmt.Sprintf("worker-%d", i), "--json")
	})
	winners := 0
	var winner task
	for _, result := range results {
		if result.code == 0 {
			winners++
			f.read(result, &winner)
		} else {
			f.requireConflict(result)
		}
	}
	if winners != 1 {
		t.Fatalf("8 independent processes produced %d successful claims", winners)
	}
	stored := f.get(created.ID)
	if stored.Status != "in_progress" || stored.ClaimID == "" || stored.ClaimID != winner.ClaimID || stored.Owner != winner.Owner {
		t.Fatalf("persisted claim differs from the sole winner: stored=%+v winner=%+v", stored, winner)
	}
}

func TestStaleOwnerCannotWriteAfterTakeover(t *testing.T) {
	f := newFixture(t)
	created := f.create("recover an interrupted worker")
	first := f.claim(created.ID, "first-session")
	var replacement task
	f.read(f.must("tasks", "takeover", created.ID, "--expected-claim", first.ClaimID, "--session", "second-session", "--reason", "Confirmed the first session stopped.", "--json"), &replacement)
	if replacement.ClaimID == first.ClaimID || replacement.ClaimID == "" {
		t.Fatal("takeover did not issue a fresh fencing token")
	}
	staleOperations := [][]string{
		{"tasks", "progress", created.ID, "--claim", first.ClaimID, "--text", "stale progress", "--json"},
		{"tasks", "heartbeat", created.ID, "--claim", first.ClaimID, "--json"},
		{"tasks", "complete", created.ID, "--claim", first.ClaimID, "--summary", "stale completion", "--json"},
		{"tasks", "release", created.ID, "--claim", first.ClaimID, "--json"},
		{"tasks", "takeover", created.ID, "--expected-claim", first.ClaimID, "--session", "third-session", "--reason", "An out-of-date observer attempted takeover.", "--json"},
	}
	for _, args := range staleOperations {
		f.requireConflict(f.run("", args...))
	}
	stored := f.get(created.ID)
	if stored.ClaimID != replacement.ClaimID || stored.Owner != "second-session" || stored.Status != "in_progress" {
		t.Fatalf("a stale operation changed the current claim: %+v", stored)
	}
	if strings.Contains(stored.Progress, "stale progress") || strings.Contains(stored.Result, "stale completion") {
		t.Fatalf("stale worker content reached storage: %+v", stored)
	}
	f.complete(replacement)
	if f.get(created.ID).Status != "completed" {
		t.Fatal("current owner could not complete after stale writes were rejected")
	}
}

func TestConcurrentBriefEditsRejectLostUpdate(t *testing.T) {
	f := newFixture(t)
	created := f.create("edit a shared brief")
	results := race(2, func(i int) commandResult {
		return f.run("", "tasks", "edit", created.ID, "--revision", fmt.Sprint(created.Revision), "--brief", fmt.Sprintf("replacement brief %d", i), "--json")
	})
	winners := 0
	var winner task
	for _, result := range results {
		if result.code == 0 {
			winners++
			f.read(result, &winner)
		} else {
			f.requireConflict(result)
		}
	}
	stored := f.get(created.ID)
	if winners != 1 || stored.Brief != winner.Brief || stored.Revision <= created.Revision {
		t.Fatalf("revision check failed: winners=%d stored=%+v winner=%+v", winners, stored, winner)
	}
}

func TestEvolutionPreservesFrontierLineageAndPrerequisites(t *testing.T) {
	f := newFixture(t)
	original := f.create("deliver the whole feature", "--tag", "feature")
	dependent := f.create("integrate the delivered feature", "--prerequisite", original.ID)
	claimed := f.claim(original.ID, "planner")
	spec := map[string]any{
		"summary": "Completed the investigation; the two implementation steps are now explicit.",
		"replacements": []map[string]any{
			{"key": "implementation", "title": "implement the feature", "brief": "Implement the agreed feature and record verification."},
			{"key": "verification", "title": "verify the feature", "brief": "Independently verify the completed implementation.", "prerequisites": []string{"@implementation"}},
		},
	}
	path := writeSpec(t, f.root, "replacement.json", spec)
	f.must("tasks", "evolve", original.ID, "--claim", claimed.ClaimID, "--revision", fmt.Sprint(claimed.Revision), "--file", path, "--json")
	if current := f.get(original.ID); current.Status != "evolved" || current.Brief != original.Brief {
		t.Fatalf("evolution did not preserve the predecessor: %+v", current)
	}
	available := f.list("available")
	if len(available.Tasks) != 1 || available.Tasks[0].Title != "implement the feature" {
		t.Fatalf("wrong available frontier after replacement: %+v", available)
	}
	implementation := f.get(available.Tasks[0].ID)
	if implementation.OriginID != original.ID || implementation.ParentID != original.ID {
		t.Fatalf("successor lost its lineage: %+v", implementation)
	}
	if !containsTask(f.list("blocked").Tasks, dependent.ID) {
		t.Fatal("original dependent was unblocked by evolution alone")
	}
	f.complete(f.claim(implementation.ID, "implementer"))
	available = f.list("available")
	if len(available.Tasks) != 1 || available.Tasks[0].Title != "verify the feature" {
		t.Fatalf("downstream work became available too early: %+v", available)
	}
	verification := f.get(available.Tasks[0].ID)
	if verification.OriginID != original.ID || verification.ParentID != original.ID {
		t.Fatalf("second successor lost its lineage: %+v", verification)
	}
	pending := f.run("", "tasks", "wait", original.ID, "--timeout", "50ms", "--json")
	if pending.code != 5 {
		t.Fatalf("wait treated partial successor completion as final: exit=%d stdout=%q stderr=%q", pending.code, pending.stdout, pending.stderr)
	}
	f.complete(f.claim(verification.ID, "verifier"))
	f.must("tasks", "wait", original.ID, "--timeout", "1s", "--json")
	if !containsTask(f.list("available").Tasks, dependent.ID) {
		t.Fatal("dependent did not become available after every successor completed")
	}
	if f.get(original.ID).Status != "evolved" {
		t.Fatal("following the outcome rewrote the immutable predecessor state")
	}
}

func TestCyclicEvolutionRollsBackAllReplacements(t *testing.T) {
	f := newFixture(t)
	original := f.create("original objective")
	dependent := f.create("downstream objective", "--prerequisite", original.ID)
	claimed := f.claim(original.ID, "planner")
	before := f.list("list").Total
	spec := map[string]any{
		"summary": "Attempting an invalid plan.",
		"replacements": []map[string]any{
			{"key": "valid", "title": "valid first replacement", "brief": "An otherwise valid replacement must not leak through rollback."},
			{"key": "cycle", "title": "cyclic second replacement", "brief": "This creates a cycle through the original task.", "prerequisites": []string{dependent.ID}},
		},
	}
	path := writeSpec(t, f.root, "cyclic.json", spec)
	result := f.run("", "tasks", "evolve", original.ID, "--claim", claimed.ClaimID, "--revision", fmt.Sprint(claimed.Revision), "--file", path, "--json")
	if result.code == 0 {
		t.Fatal("effective dependency cycle was accepted")
	}
	after := f.get(original.ID)
	if after.Status != claimed.Status || after.ClaimID != claimed.ClaimID || after.Revision != claimed.Revision {
		t.Fatalf("failed evolution changed the predecessor: before=%+v after=%+v", claimed, after)
	}
	if total := f.list("list").Total; total != before {
		t.Fatalf("failed evolution leaked replacements: before=%d after=%d", before, total)
	}
	f.complete(claimed)
	if !containsTask(f.list("available").Tasks, dependent.ID) {
		t.Fatal("failed evolution left hidden dependency or lineage changes")
	}
}

func TestWaitDistinguishesCompletionCancellationAndTimeout(t *testing.T) {
	f := newFixture(t)
	completed := f.complete(f.claim(f.create("successful work").ID, "worker"))
	f.must("tasks", "wait", completed.ID, "--timeout", "1s", "--json")
	cancelled := f.create("abandoned work")
	f.must("tasks", "cancel", cancelled.ID, "--reason", "No longer required.", "--json")
	if result := f.run("", "tasks", "wait", cancelled.ID, "--timeout", "1s", "--json"); result.code != 4 {
		t.Fatalf("cancelled wait returned %d instead of 4: %q %q", result.code, result.stdout, result.stderr)
	}
	pending := f.create("unfinished work")
	if result := f.run("", "tasks", "wait", pending.ID, "--timeout", "50ms", "--json"); result.code != 5 {
		t.Fatalf("unfinished wait returned %d instead of 5: %q %q", result.code, result.stdout, result.stderr)
	}
}

func TestWaitingProcessDoesNotBlockCompletion(t *testing.T) {
	f := newFixture(t)
	created := f.create("finish work while another process waits")
	waited := make(chan commandResult, 1)
	go func() {
		waited <- f.run("", "tasks", "wait", created.ID, "--timeout", "5s", "--json")
	}()
	select {
	case result := <-waited:
		t.Fatalf("wait returned before completion: exit=%d stdout=%q stderr=%q", result.code, result.stdout, result.stderr)
	case <-time.After(100 * time.Millisecond):
	}
	f.complete(f.claim(created.ID, "worker"))
	select {
	case result := <-waited:
		if result.code != 0 {
			t.Fatalf("wait did not observe concurrent completion: exit=%d stdout=%q stderr=%q", result.code, result.stdout, result.stderr)
		}
	case <-time.After(6 * time.Second):
		t.Fatal("waiting process did not terminate after concurrent completion")
	}
}

func TestWatchDrainsCompletionCommittedDuringCallback(t *testing.T) {
	f := newFixture(t)
	created := f.create("complete during delivery of an earlier event")
	f.claim(created.ID, "worker")
	logPath := filepath.Join(f.root, "callback-events.jsonl")
	helper := f.callback(logPath)
	f.must("tasks", "watch", created.ID, "--after", "0", "--exec", helper, "--timeout", "5s", "--interval", "10ms", "--json")
	if actual := f.get(created.ID); actual.Status != "completed" {
		t.Fatalf("callback did not complete the task: %+v", actual)
	}
	file, err := os.Open(logPath)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	seenCompletion := false
	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		var message struct {
			Event event `json:"event"`
		}
		if err := json.Unmarshal(scanner.Bytes(), &message); err != nil {
			t.Fatal(err)
		}
		seenCompletion = seenCompletion || message.Event.Kind == "completed"
	}
	if err := scanner.Err(); err != nil {
		t.Fatal(err)
	}
	if !seenCompletion {
		t.Fatal("watch exited after observing the terminal state without delivering its completion event to the callback")
	}
}

func TestDeliveryClaimIsExclusiveAcrossProcesses(t *testing.T) {
	f := newFixture(t)
	created := f.create("deliver a completion notification")
	subscription := f.subscribe(created.ID, "observer")
	f.acknowledge(subscription, "observer", f.offer(subscription, "observer"))
	f.complete(f.claim(created.ID, "worker"))
	results := race(2, func(int) commandResult {
		return f.run("", "subscriptions", "pending", subscription, "--session", "observer", "--limit", "100", "--json")
	})
	winners := 0
	var winner delivery
	for _, result := range results {
		if result.code == 3 {
			f.requireConflict(result)
			continue
		}
		if result.code != 0 {
			t.Fatalf("pending delivery failed: exit=%d stderr=%q", result.code, result.stderr)
		}
		if strings.TrimSpace(result.stdout) != "" {
			winners++
			f.read(result, &winner)
		}
	}
	if winners != 1 || winner.Token == "" {
		t.Fatalf("two consumers received %d deliveries", winners)
	}
	foundCompletion := false
	seen := make(map[int64]bool)
	for _, event := range winner.Events {
		if seen[event.ID] {
			t.Fatalf("delivery contains duplicate event ID %d", event.ID)
		}
		seen[event.ID] = true
		foundCompletion = foundCompletion || event.Kind == "completed"
	}
	if !foundCompletion {
		t.Fatalf("completion event was not delivered: %+v", winner.Events)
	}
	f.acknowledge(subscription, "observer", winner)
	if result := f.must("subscriptions", "pending", subscription, "--session", "observer", "--json"); result.stdout != "" {
		t.Fatalf("acknowledged completion was offered again: %q", result.stdout)
	}
}

func TestStaleDeliveryAcknowledgementCannotConsumeNewOffer(t *testing.T) {
	f := newFixture(t)
	created := f.create("retry unacknowledged completion")
	subscription := f.subscribe(created.ID, "observer")
	f.acknowledge(subscription, "observer", f.offer(subscription, "observer"))
	f.complete(f.claim(created.ID, "worker"))
	first := f.offer(subscription, "observer", "--lease", "100ms")
	time.Sleep(150 * time.Millisecond)
	second := f.offer(subscription, "observer")
	if second.Token == first.Token {
		t.Fatal("expired delivery token was reused")
	}
	if second.Through != first.Through {
		t.Fatalf("unacknowledged events disappeared during retry: first=%d second=%d", first.Through, second.Through)
	}
	f.requireConflict(f.run("", "subscriptions", "acknowledge", subscription, "--session", "observer", "--token", first.Token, "--through", fmt.Sprint(first.Through), "--json"))
	f.acknowledge(subscription, "observer", second)
	if result := f.must("subscriptions", "pending", subscription, "--session", "observer", "--json"); result.stdout != "" {
		t.Fatalf("successful acknowledgement did not drain delivery: %q", result.stdout)
	}
}

func TestFailedSubscriptionCallbackRetriesWithoutLosingEvents(t *testing.T) {
	f := newFixture(t)
	created := f.create("retry a failed completion callback")
	subscription := f.subscribe(created.ID, "observer")
	f.acknowledge(subscription, "observer", f.offer(subscription, "observer"))
	f.complete(f.claim(created.ID, "worker"))
	logPath := filepath.Join(f.root, "delivery-attempts.jsonl")
	helper := f.callback(logPath)
	f.env = append(f.env, "TASKCTL_QA_FAIL_ONCE="+filepath.Join(f.root, "failed-once"))
	args := []string{"subscriptions", "watch", subscription, "--session", "observer", "--exec", helper, "--once", "--json"}
	if result := f.run("", args...); result.code == 0 {
		t.Fatal("subscription watch ignored callback failure")
	}
	f.must(args...)
	file, err := os.Open(logPath)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	var attempts []delivery
	scanner := bufio.NewScanner(file)
	for scanner.Scan() {
		var offered delivery
		if err := json.Unmarshal(scanner.Bytes(), &offered); err != nil {
			t.Fatal(err)
		}
		attempts = append(attempts, offered)
	}
	if err := scanner.Err(); err != nil {
		t.Fatal(err)
	}
	if len(attempts) != 2 {
		t.Fatalf("failed callback was not immediately retried: %d callback attempts", len(attempts))
	}
	if attempts[0].Token == attempts[1].Token || attempts[0].Through != attempts[1].Through {
		t.Fatalf("retry did not obtain a fresh claim on the same events: first=%+v second=%+v", attempts[0], attempts[1])
	}
	firstIDs := make(map[int64]bool)
	for _, event := range attempts[0].Events {
		firstIDs[event.ID] = true
	}
	completed := false
	for _, event := range attempts[1].Events {
		if !firstIDs[event.ID] {
			t.Fatalf("retry changed its event batch: unexpected event %d", event.ID)
		}
		delete(firstIDs, event.ID)
		completed = completed || event.Kind == "completed"
	}
	if !completed || len(firstIDs) != 0 {
		t.Fatalf("callback retry lost completion or earlier events: completed=%t missing=%v", completed, firstIDs)
	}
	if result := f.must("subscriptions", "pending", subscription, "--session", "observer", "--json"); result.stdout != "" {
		t.Fatalf("successful retry did not acknowledge its events: %q", result.stdout)
	}
}

func TestQuietHookProducesNoOutputAfterAcknowledgement(t *testing.T) {
	f := newFixture(t)
	created := f.create("quiet observer")
	subscription := f.subscribe(created.ID, "observer")
	f.acknowledge(subscription, "observer", f.offer(subscription, "observer"))
	input := `{"hook_event_name":"PostToolUse","session_id":"observer","tool_name":"shell","tool_input":{},"tool_response":{}}`
	for i := 0; i < 2; i++ {
		result := f.run(input, "integrations", "codex", "hook")
		if result.code != 0 || result.stdout != "" || result.stderr != "" {
			t.Fatalf("quiet hook emitted noise: exit=%d stdout=%q stderr=%q", result.code, result.stdout, result.stderr)
		}
	}
}

func TestCompactListsOmitTaskBodies(t *testing.T) {
	f := newFixture(t)
	created := f.create("inspect compact lists")
	claimed := f.claim(created.ID, "worker")
	f.must("tasks", "progress", created.ID, "--claim", claimed.ClaimID, "--text", "PRIVATE_FULL_PROGRESS_SENTINEL", "--json")
	result := f.must("tasks", "in-progress", "--json")
	var listed struct {
		Tasks []map[string]json.RawMessage `json:"tasks"`
	}
	f.read(result, &listed)
	if len(listed.Tasks) != 1 {
		t.Fatalf("expected one compact row, got %d", len(listed.Tasks))
	}
	for _, key := range []string{"brief", "metadata", "progress", "result"} {
		if _, present := listed.Tasks[0][key]; present {
			t.Errorf("compact list unexpectedly includes %q", key)
		}
	}
	if strings.Contains(result.stdout, "PRIVATE_FULL_PROGRESS_SENTINEL") || strings.Contains(result.stdout, created.Brief) {
		t.Fatal("compact list contains task body text")
	}
}

func TestParseErrorsHonorTrailingJSONFlag(t *testing.T) {
	f := newFixture(t)
	created := f.create("report malformed command arguments")
	result := f.run("", "tasks", "claim", created.ID, "--session", "--json")
	if result.code != 2 || result.stdout != "" {
		t.Fatalf("malformed command must fail with usage exit 2 and no stdout: exit=%d stdout=%q stderr=%q", result.code, result.stdout, result.stderr)
	}
	var response struct {
		Error struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal([]byte(result.stderr), &response); err != nil || response.Error.Code != "usage" || response.Error.Message == "" {
		t.Fatalf("parse error did not honor the trailing JSON flag: %q", result.stderr)
	}
}

func TestBackupCannotReplaceLiveDatabaseThroughDirectoryAlias(t *testing.T) {
	f := newFixture(t)
	created := f.create("preserve live data during backup")
	var project struct {
		Database string `json:"database"`
	}
	f.read(f.must("projects", "show", "--json"), &project)
	alias := filepath.Join(f.root, "database-alias")
	if err := os.Symlink(filepath.Dir(project.Database), alias); err != nil {
		t.Fatal(err)
	}
	result := f.run("", "backup", "--file", filepath.Join(alias, filepath.Base(project.Database)), "--json")
	if result.code == 0 {
		t.Fatal("backup replaced the live SQLite database through a directory alias")
	}
	if stored := f.get(created.ID); stored.Title != created.Title || stored.Brief != created.Brief {
		t.Fatalf("rejected backup damaged the existing task: %+v", stored)
	}
}

func writeSpec(t *testing.T, root, name string, spec any) string {
	t.Helper()
	data, err := json.Marshal(spec)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(root, name)
	if err := os.WriteFile(path, data, 0600); err != nil {
		t.Fatal(err)
	}
	return path
}
