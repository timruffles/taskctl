package main

import (
	"fmt"
	"os"
	"path/filepath"

	"taskctl/internal/project"
	"taskctl/internal/store"
)

func (a *app) backup() error {
	if len(a.o.pos) != 1 {
		return usage("backup takes no positional arguments")
	}
	if err := a.o.allow("file"); err != nil {
		return err
	}
	path, err := a.o.require("file")
	if err != nil {
		return err
	}
	if path == "-" {
		return usage("backup requires a file path")
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return err
	}
	if err = a.s.Backup(abs); err != nil {
		return err
	}
	return a.ok(map[string]string{"file": abs, "project_id": a.p.ID}, "backed up "+abs)
}

func (a *app) restore() error {
	if len(a.o.pos) != 1 {
		return usage("restore takes no positional arguments")
	}
	if err := a.o.allow("file", "name", "path"); err != nil {
		return err
	}
	if a.o.has("project") {
		return usage("restore creates a new project; omit --project")
	}
	path, err := a.o.require("file")
	if err != nil {
		return err
	}
	name, err := a.o.require("name")
	if err != nil {
		return err
	}
	p, err := project.Create(name)
	if err != nil {
		return err
	}
	s, err := store.Open(p.Database, p.ID)
	if err != nil {
		dir, _ := project.Dir(p.ID)
		_ = os.RemoveAll(dir)
		return err
	}
	err = s.Restore(path)
	closeErr := s.Close()
	if err != nil {
		dir, _ := project.Dir(p.ID)
		_ = os.RemoveAll(dir)
		return err
	}
	if closeErr != nil {
		return closeErr
	}
	if a.o.has("path") {
		if err = project.Attach(p, a.o.get("path")); err != nil {
			return fmt.Errorf("restored project %s but attachment failed: %w", p.ID, err)
		}
	}
	return a.ok(p, "restored "+p.ID+"  "+p.Name+"\nActive claims were released. Recreate subscriptions. Scratch contents are not in backups.")
}
