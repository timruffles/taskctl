package cli

import (
	"fmt"
	"os"
	"path/filepath"

	"taskctl/internal/project"
	"taskctl/internal/store"
)

func (a *app) backup() error {
	if a.o.has("file") == a.o.has("directory") {
		return usage("backup requires exactly one of --file or --directory")
	}
	flag := "file"
	if a.o.has("directory") {
		flag = "directory"
	}
	path, err := a.o.require(flag)
	if err != nil {
		return err
	}
	if path == "-" {
		return usage("backup requires a filesystem path")
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return err
	}
	if flag == "directory" {
		err = a.s.BackupDirectory(abs)
	} else {
		err = a.s.Backup(abs)
	}
	if err != nil {
		return err
	}
	return a.ok(map[string]string{flag: abs, "project_id": a.p.ID}, "backed up "+abs)
}

func (a *app) restore() error {
	if a.o.has("project") {
		return usage("restore creates a new project; omit --project")
	}
	if a.o.has("file") == a.o.has("directory") {
		return usage("restore requires exactly one of --file or --directory")
	}
	flag := "file"
	if a.o.has("directory") {
		flag = "directory"
	}
	path, err := a.o.require(flag)
	if err != nil {
		return err
	}
	if path == "-" {
		return usage("restore requires a filesystem path")
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
	if flag == "directory" {
		err = s.RestoreDirectory(path)
	} else {
		err = s.Restore(path)
	}
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
