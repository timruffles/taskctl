package main

import (
	"fmt"
	"path/filepath"
	"time"

	scratchpkg "taskctl/internal/scratch"
	"taskctl/internal/store"
)

func (a *app) scratchPath(id string) string { return filepath.Join(a.p.Scratch, id) }
func (a *app) scratch() error {
	m := scratchpkg.Manager{Store: a.s, Root: a.p.Scratch}
	op := a.o.pos[1]
	if op == "cleanup" {
		if err := a.o.allow("grace", "dry-run", "limit"); err != nil {
			return err
		}
		if err := a.o.noID(); err != nil {
			return err
		}
		def, _ := time.ParseDuration(a.cfg.ScratchGrace)
		grace, err := a.o.duration("grace", def)
		if err != nil {
			return err
		}
		limit, err := a.o.number("limit", 100)
		if err != nil {
			return err
		}
		if limit < 1 || limit > 1000 {
			return usage("limit must be 1..1000")
		}
		rows, err := m.Cleanup(grace, a.o.yes("dry-run"), int(limit))
		if err != nil {
			return err
		}
		if a.o.yes("json") {
			return a.json(rows)
		}
		for _, r := range rows {
			verb := r.State
			fmt.Fprintf(a.out, "%s %s\n", verb, r.Area.ID)
		}
		return nil
	}
	id, err := a.o.position(2)
	if err != nil {
		return err
	}
	switch op {
	case "create":
		if err = a.o.allow("claim"); err != nil {
			return err
		}
		r, err := m.Create(id, a.o.get("claim"))
		if err != nil {
			return err
		}
		return a.ok(r, r.Path+"\n"+r.Guidance)
	case "list":
		if err = a.o.allow("limit", "offset"); err != nil {
			return err
		}
		limit, offset, err := a.pageOptions()
		if err != nil {
			return err
		}
		areas, more, err := a.s.ScratchListPage(id, limit, offset)
		if err != nil {
			return err
		}
		rows := []scratchpkg.Result{}
		for _, x := range areas {
			rows = append(rows, a.scratchResult(x))
		}
		if a.o.yes("json") {
			return a.json(map[string]any{"areas": rows, "more": more, "next_offset": offset + len(rows)})
		}
		for _, r := range rows {
			status := r.State
			fmt.Fprintf(a.out, "%s %s %s\n", r.Area.ID, status, r.Path)
		}
		a.printNextPage(more, offset+len(rows))
		return nil
	case "attach":
		if err = a.o.allow("area", "claim"); err != nil {
			return err
		}
		area, err := a.o.require("area")
		if err != nil {
			return err
		}
		if err = a.s.ScratchAttach(id, a.o.get("claim"), area); err != nil {
			return err
		}
		return a.ok(map[string]string{"task_id": id, "area_id": area, "path": a.scratchPath(area)}, id+" attached "+area)
	}
	return usage("unknown scratch command " + op)
}
func (a *app) opportunisticCleanup() {
	grace, err := time.ParseDuration(a.cfg.ScratchGrace)
	if err != nil {
		return
	}
	m := scratchpkg.Manager{Store: a.s, Root: a.p.Scratch}
	_, _ = m.Cleanup(grace, false, 1)
}

func (a *app) scratchResult(area store.ScratchArea) scratchpkg.Result {
	state := "retained"
	if area.CleanupToken != "" {
		state = "deleting"
	}
	if area.RemovedAt != "" {
		state = "removed"
	}
	return scratchpkg.Result{Area: area, Path: a.scratchPath(area.ID), State: state}
}
