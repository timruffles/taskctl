package project

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func isolate(t *testing.T) string {
	t.Helper()
	root := t.TempDir()
	for _, kind := range []string{"DATA", "STATE", "CONFIG"} {
		t.Setenv("XDG_"+kind+"_HOME", filepath.Join(root, kind))
	}
	t.Setenv("TASKCTL_PROJECT", "")
	return root
}

func TestProjectResolutionAcrossWorktrees(t *testing.T) {
	root := isolate(t)
	p, err := Create("Project")
	if err != nil {
		t.Fatal(err)
	}
	other, err := Create("Other")
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"main", "worktree"} {
		path := filepath.Join(root, name)
		if err = os.MkdirAll(filepath.Join(path, "nested"), 0700); err != nil {
			t.Fatal(err)
		}
		if err = Attach(p, path); err != nil {
			t.Fatal(err)
		}
		if err = Attach(p, path); err != nil {
			t.Fatal(err)
		}
		found, err := Resolve("", filepath.Join(path, "nested"))
		if err != nil || found.ID != p.ID || found.Database != p.Database {
			t.Fatalf("resolve: %+v %v", found, err)
		}
		if err = Attach(other, path); err == nil {
			t.Fatal("attachment silently replaced another project")
		}
	}
	t.Setenv("TASKCTL_PROJECT", other.ID)
	found, err := Resolve("", filepath.Join(root, "main"))
	if err != nil || found.ID != other.ID {
		t.Fatalf("env precedence: %+v %v", found, err)
	}
	found, err = Resolve(p.ID, root)
	if err != nil || found.ID != p.ID {
		t.Fatalf("explicit precedence: %+v %v", found, err)
	}
	if _, err = Load("../../bad"); err == nil {
		t.Fatal("unsafe project ID accepted")
	}
	if filepath.Dir(p.Database) != filepath.Join(root, "DATA", "taskctl", "projects", p.ID) || p.Scratch != filepath.Join(root, "STATE", "taskctl", "scratch", p.ID) {
		t.Fatalf("XDG locations: %+v", p)
	}
}

func TestConfigurationValidation(t *testing.T) {
	isolate(t)
	c, err := Defaults()
	if err != nil || c.ListLimit != 20 || c.Inactivity != "30m" || c.ScratchGrace != "168h" {
		t.Fatalf("defaults: %+v %v", c, err)
	}
	dir := Base("CONFIG")
	if err = os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "config.json")
	for _, input := range []string{`{"list_limit":201}`, `{"inactivity":"0s"}`, `{"scratch_grace":"bad"}`, `{"unknown":1}`, `{} {}`} {
		if err = os.WriteFile(path, []byte(input), 0600); err != nil {
			t.Fatal(err)
		}
		if _, err = Defaults(); err == nil {
			t.Errorf("invalid config accepted: %s", input)
		}
	}
	b, _ := json.Marshal(Config{ListLimit: 12, Inactivity: "15m", ScratchGrace: "72h"})
	if err = os.WriteFile(path, b, 0600); err != nil {
		t.Fatal(err)
	}
	c, err = Defaults()
	if err != nil || c.ListLimit != 12 || c.Inactivity != "15m" {
		t.Fatalf("config: %+v %v", c, err)
	}
}

func TestConfigurationFileReadIsBounded(t *testing.T) {
	isolate(t)
	path := filepath.Join(t.TempDir(), "oversized.json")
	if err := os.WriteFile(path, []byte(strings.Repeat(" ", 1024*1024)+"{}"), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := readConfigFile(path); err == nil || !strings.Contains(err.Error(), "exceeds 1 MiB") {
		t.Fatalf("oversized config read: %v", err)
	}
}
