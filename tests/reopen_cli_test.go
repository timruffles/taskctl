package tests

import (
	"os/exec"
	"strings"
	"testing"
)

func TestReopenCompletedTaskBlocksDependentsUntilCompletedAgain(t *testing.T) {
	f := newFixture(t)
	claimed := f.claim(f.create("Verify import behavior").ID, "reviewer")
	completed := f.complete(claimed)
	dependent := f.create("Publish verified import", "--prerequisite", completed.ID)

	var reopened task
	f.read(f.must("tasks", "reopen", completed.ID, "--reason", "Verification missed empty input", "--json"), &reopened)
	if reopened.ID != completed.ID || reopened.Status != "pending" || reopened.Result != "" {
		t.Fatalf("unexpected reopened task: %+v", reopened)
	}
	blocked := f.list("blocked").Tasks
	if len(blocked) != 1 || blocked[0].ID != dependent.ID {
		t.Fatalf("dependent should wait for reopened prerequisite: %+v", blocked)
	}

	events := f.must("tasks", "watch", reopened.ID, "--after", "0", "--once").stdout
	for _, message := range []string{completed.Result, "Verification missed empty input"} {
		if !strings.Contains(events, message) {
			t.Errorf("missing %q from task events: %s", message, events)
		}
	}

	reclaimed := f.claim(reopened.ID, "next reviewer")
	if result := f.run("", "tasks", "complete", reopened.ID, "--claim", claimed.ClaimID, "--summary", "Old claim"); result.code != 3 {
		t.Fatalf("old claim should be rejected after reopening: %+v", result)
	}
	f.complete(reclaimed)
	f.claim(dependent.ID, "publisher")
}

func TestReopenCancelledTaskWithUnmetPrerequisite(t *testing.T) {
	f := newFixture(t)
	prerequisite := f.create("Prepare input")
	task := f.create("Review input", "--prerequisite", prerequisite.ID)
	if result := f.run("", "tasks", "reopen", task.ID); result.code != 3 {
		t.Fatalf("reopening a pending task should conflict: %+v", result)
	}
	f.must("tasks", "cancel", task.ID, "--reason", "Review deferred")
	f.must("tasks", "reopen", task.ID)
	if blocked := f.list("blocked").Tasks; len(blocked) != 1 || blocked[0].ID != task.ID {
		t.Fatalf("cancelled task should reopen as blocked: %+v", blocked)
	}
	f.complete(f.claim(prerequisite.ID, "preparer"))
	f.claim(task.ID, "reviewer")
}

func TestCommandHelpWithoutProject(t *testing.T) {
	for _, args := range [][]string{{"tasks", "reopen", "--help"}, {"help", "tasks", "reopen"}} {
		cmd := exec.Command(binaryPath, args...)
		cmd.Dir = t.TempDir()
		cmd.Env = []string{"TASKCTL_PROJECT=missing"}
		output, err := cmd.CombinedOutput()
		help := string(output)
		if err != nil || !strings.Contains(help, "taskctl tasks reopen TASK_ID") ||
			!strings.Contains(help, "--reason") || strings.Contains(help, "--claim") {
			t.Errorf("%v should show command-specific help without opening a project: %v\n%s", args, err, help)
		}
	}
}
