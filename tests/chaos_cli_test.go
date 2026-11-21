package tests

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func TestChaosMissingOrEmptyDatabaseDoesNotBecomeEmptyFrontier(t *testing.T) {
	for _, damage := range []string{"missing", "empty"} {
		t.Run(damage, func(t *testing.T) {
			f := newFixture(t)
			f.create("Committed work must not disappear silently")
			path := filepath.Join(f.root, "data", "taskctl", "projects", f.project, "tasks.sqlite")
			if damage == "missing" {
				if err := os.Remove(path); err != nil {
					t.Fatal(err)
				}
			} else {
				if err := os.Truncate(path, 0); err != nil {
					t.Fatal(err)
				}
			}
			result := f.run("", "tasks", "list", "--json")
			if result.code == 0 || result.stdout != "" {
				t.Fatalf("damaged database was reported as success: %+v", result)
			}
			var errorBody struct {
				Error struct {
					Code string `json:"code"`
				} `json:"error"`
			}
			if err := json.Unmarshal([]byte(result.stderr), &errorBody); err != nil || errorBody.Error.Code == "" {
				t.Fatalf("missing structured failure: %s", result.stderr)
			}
			if damage == "missing" {
				if _, err := os.Stat(path); !os.IsNotExist(err) {
					t.Fatalf("missing database recreated: %v", err)
				}
			} else {
				st, err := os.Stat(path)
				if err != nil || st.Size() != 0 {
					t.Fatalf("empty database initialized: %v %+v", err, st)
				}
			}
		})
	}
}

func TestChaosCreateRequestIDSurvivesDiscardedResponse(t *testing.T) {
	f := newFixture(t)
	args := []string{"tasks", "create", "--title", "Only one task", "--brief", "One durable intent", "--request-id", "lost-response-42", "--json"}
	_ = f.must(args...)
	var retried task
	f.read(f.must(args...), &retried)
	var list taskList
	f.read(f.must("tasks", "list", "--status", "all", "--json"), &list)
	if list.Total != 1 || retried.ID != list.Tasks[0].ID {
		t.Fatalf("retry duplicated work: %+v %+v", retried, list)
	}
	result := f.run("", "tasks", "create", "--title", "Changed intent", "--request-id", "lost-response-42", "--json")
	if result.code != 3 {
		t.Fatalf("reused key silently changed intent: %+v", result)
	}
	for _, key := range []string{"", " ", "bad\nkey"} {
		result = f.run("", "tasks", "create", "--title", "Must not create", "--request-id", key, "--json")
		if result.code != 2 {
			t.Fatalf("invalid request key %q was not rejected: %+v", key, result)
		}
	}
	f.read(f.must("tasks", "list", "--status", "all", "--json"), &list)
	if list.Total != 1 {
		t.Fatalf("invalid request key created work: %+v", list)
	}
}
