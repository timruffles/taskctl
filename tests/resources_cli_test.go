package tests

import (
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
)

func TestResourceClaimsAreExclusiveAndFenced(t *testing.T) {
	f := newFixture(t)
	f.must("resources", "create", "unity-main")
	first := f.create("Verify the globe", "--requires-resource", "unity-main")
	second := f.create("Verify the ship designer")
	f.must("tasks", "require-resource", second.ID, "unity-main")
	candidates := []task{first, second}
	results := race(len(candidates), func(i int) commandResult {
		return f.run("", "tasks", "claim", candidates[i].ID, "--session", "qa-"+candidates[i].ID, "--json")
	})
	var holder task
	var waiter task
	for i, result := range results {
		switch result.code {
		case 0:
			if holder.ID != "" {
				t.Fatal("two tasks claimed the same editor")
			}
			f.read(result, &holder)
		case 3:
			waiter = candidates[i]
		default:
			t.Fatalf("unexpected claim result: %+v", result)
		}
	}
	if holder.ID == "" || waiter.ID == "" {
		t.Fatalf("expected one holder and one blocked task: %+v", results)
	}
	blocked := f.must("tasks", "blocked").stdout
	if !strings.Contains(blocked, waiter.ID+" waits for resource unity-main held by "+holder.ID) {
		t.Fatalf("blocked list did not explain the conflict: %s", blocked)
	}
	var takeover task
	f.read(f.must("tasks", "takeover", holder.ID, "--expected-claim", holder.ClaimID, "--session", "replacement", "--reason", "Previous worker stopped", "--json"), &takeover)
	if result := f.run("", "resources", "release", "unity-main", "--claim", holder.ClaimID); result.code != 3 {
		t.Fatalf("stale claim released a transferred hold: %+v", result)
	}
	var beforeRelease struct {
		Cursor int64 `json:"cursor"`
	}
	f.read(f.must("tasks", "watch", waiter.ID, "--once", "--json"), &beforeRelease)
	f.must("resources", "release", "unity-main", "--claim", takeover.ClaimID)
	if events := f.must("tasks", "watch", waiter.ID, "--after", fmt.Sprint(beforeRelease.Cursor), "--once").stdout; !strings.Contains(events, " available ") {
		t.Fatalf("waiting tasks weren't notified when the resource became free: %s", events)
	}
	next := f.claim(waiter.ID, "next worker")
	if result := f.run("", "resources", "release", "unity-main", "--claim", takeover.ClaimID); result.code != 3 {
		t.Fatalf("old holder released a new holder's resource: %+v", result)
	}
	f.complete(takeover)
	f.complete(next)
	if output := f.must("resources", "list").stdout; !strings.Contains(output, "unity-main  available") {
		t.Fatalf("completion did not release the resource: %s", output)
	}
}

func TestMultipleResourcesAreClaimedTogetherAndRestoredWithoutHolds(t *testing.T) {
	f := newFixture(t)
	f.must("resources", "create", "editor")
	f.must("resources", "create", "player")
	holder := f.claim(f.create("Check player", "--requires-resource", "player").ID, "player worker")
	both := f.create("Check editor and player", "--requires-resource", "editor", "--requires-resource", "player")
	if result := f.run("", "tasks", "claim", both.ID, "--session", "qa"); result.code != 3 {
		t.Fatalf("claim should fail while player is held: %+v", result)
	}
	editor := f.claim(f.create("Check editor", "--requires-resource", "editor").ID, "editor worker")
	f.must("tasks", "cancel", editor.ID, "--claim", editor.ClaimID, "--reason", "No longer needed")
	f.must("tasks", "release", holder.ID, "--claim", holder.ClaimID)
	both = f.claim(both.ID, "qa")

	path := filepath.Join(f.root, "resources.sqlite")
	f.must("backup", "--file", path)
	var restored struct {
		ID string `json:"id"`
	}
	unselected := *f
	unselected.project = ""
	f.read(unselected.must("restore", "--file", path, "--name", "restored-qa", "--json"), &restored)
	unselected.project = restored.ID
	if output := unselected.must("resources", "list").stdout; !strings.Contains(output, "editor  available") || !strings.Contains(output, "player  available") {
		t.Fatalf("restore did not clear both live holds: %s", output)
	}
	both = unselected.claim(both.ID, "restored worker")
	if output := unselected.must("resources", "list").stdout; strings.Count(output, "held by "+both.ID) != 2 {
		t.Fatalf("restore did not preserve both resource requirements: %s", output)
	}
}

func TestEvolutionTransfersResourceToContinuingClaim(t *testing.T) {
	f := newFixture(t)
	f.must("resources", "create", "unity-main")
	original := f.claim(f.create("Verify UI", "--requires-resource", "unity-main").ID, "tester")
	plan := writeSpec(t, f.root, "qa.json", json.RawMessage(`{
		"summary": "Split the verification pass",
		"continue_with": "globe",
		"replacements": [
			{"key": "globe", "title": "Verify globe", "brief": "Check the globe view"},
			{"key": "ship", "title": "Verify ship", "brief": "Check the ship view"}
		]
	}`))
	var replacements []task
	f.read(f.must("tasks", "evolve", original.ID, "--claim", original.ClaimID,
		"--revision", fmt.Sprint(original.Revision), "--file", plan, "--json"), &replacements)
	if len(replacements) != 2 {
		t.Fatalf("expected two replacements: %+v", replacements)
	}
	if result := f.run("", "resources", "release", "unity-main", "--claim", original.ClaimID); result.code != 3 {
		t.Fatalf("original claim released the replacement's resource: %+v", result)
	}
	if result := f.run("", "tasks", "claim", replacements[1].ID, "--session", "next tester"); result.code != 3 {
		t.Fatalf("sibling claimed the editor while the continuation held it: %+v", result)
	}
	f.complete(replacements[0])
	f.claim(replacements[1].ID, "next tester")
}
