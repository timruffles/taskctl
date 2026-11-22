package tests

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLunaEvolutionPlans(t *testing.T) {
	for _, name := range []string{"a1", "a2", "a3", "b1", "b2", "b3", "final"} {
		t.Run(name, func(t *testing.T) {
			data, err := os.ReadFile(filepath.Join("evals", "evolve", "responses", name+".json"))
			if err != nil {
				t.Fatal(err)
			}
			var response struct {
				Answers []struct {
					ID      string          `json:"id"`
					Command string          `json:"command"`
					Plan    json.RawMessage `json:"plan"`
				} `json:"answers"`
			}
			if err := json.Unmarshal(data, &response); err != nil {
				t.Fatal(err)
			}
			var command string
			var plan json.RawMessage
			for _, answer := range response.Answers {
				if answer.ID == "remaining" {
					command, plan = answer.Command, answer.Plan
				}
			}
			args := strings.Fields(command)
			if len(args) != 10 || args[0] != "taskctl" || args[1] != "tasks" || args[3] != "T7" || args[4] != "--claim" || args[5] != "C7" || args[6] != "--revision" || args[7] != "12" || args[8] != "--file" {
				t.Fatalf("unexpected generated command: %q", command)
			}
			if args[2] != "evolve" && args[2] != "supersede" {
				t.Fatalf("wrong operation: %q", command)
			}
			f := newFixture(t)
			original := f.claim(f.create("CSV import").ID, "reader")
			dependent := f.create("review CSV import", "--prerequisite", original.ID)
			file := writeSpec(t, f.root, "remaining.json", plan)
			var children []task
			// Bind the scenario's illustrative identity and claim to this isolated project.
			f.read(f.must("tasks", args[2], original.ID, "--claim", original.ClaimID, "--revision", fmt.Sprint(original.Revision), "--file", file, "--json"), &children)
			if len(children) != 2 || children[0].Status != "in_progress" || children[1].Status != "pending" || children[0].ClaimID == "" || children[0].ClaimID == original.ClaimID {
				t.Fatalf("generated plan did not leave the intended frontier: %+v", children)
			}
			for _, child := range children {
				if child.ID == original.ID || child.OriginID != original.ID || child.ParentID != original.ID {
					t.Fatalf("generated plan lost identity or lineage: %+v", child)
				}
			}
			for _, status := range []string{"evolved", "superseded"} {
				var listed taskList
				f.read(f.must("tasks", "list", "--status", status, "--json"), &listed)
				if len(listed.Tasks) != 1 || listed.Tasks[0].ID != original.ID || listed.Tasks[0].Status != "evolved" {
					t.Fatalf("status filter %q: %+v", status, listed)
				}
			}
			f.requireConflict(f.run("", "tasks", "progress", children[0].ID, "--claim", original.ClaimID, "--title", "Stale worker", "--json"))
			f.complete(children[0])
			f.requireConflict(f.run("", "tasks", "claim", dependent.ID, "--session", "reviewer", "--json"))
			f.complete(f.claim(children[1].ID, "tester"))
			f.must("tasks", "wait", original.ID, "--timeout", "1s", "--json")
			f.claim(dependent.ID, "reviewer")
			if retired := f.get(original.ID); retired.Status != "evolved" || retired.Brief != original.Brief {
				t.Fatalf("original definition changed: %+v", retired)
			}
		})
	}
}
