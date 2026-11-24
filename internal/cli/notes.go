package cli

import (
	"fmt"
	"io"
	"os"

	"taskctl/internal/store"
)

func (a *app) progress(id string) error {
	claim, err := a.o.require("claim")
	if err != nil {
		return err
	}
	legacy := a.o.has("text") || a.o.has("text-file")
	if legacy {
		if a.o.has("title") || a.o.has("body-file") {
			return usage("use --title with optional --body-file, or legacy --text/--text-file")
		}
		text, err := a.readText("text")
		if err != nil {
			return err
		}
		task, err := a.s.Progress(id, claim, text)
		if err != nil {
			return err
		}
		return a.taskResult(task)
	}
	title, err := a.o.require("title")
	if err != nil {
		return err
	}
	var body io.Reader
	if a.o.has("body-file") {
		path, err := a.o.require("body-file")
		if err != nil {
			return err
		}
		if path == "-" {
			body = a.in
		} else {
			file, err := os.Open(path)
			if err != nil {
				return err
			}
			defer file.Close()
			body = file
		}
	}
	note, err := a.s.AddNote(id, claim, title, body)
	if err != nil {
		return err
	}
	return a.noteResult(note)
}

func (a *app) notes() error {
	op := a.command
	id := a.args[0]
	switch op {
	case "list":
		limit, err := a.o.number("limit", int64(a.cfg.ListLimit))
		if err != nil {
			return err
		}
		if limit < 1 || limit > 200 {
			return usage("--limit must be 1..200")
		}
		before, err := a.o.number("before", 0)
		if err != nil {
			return err
		}
		if a.o.has("before") && before == 0 {
			return usage("--before must be a positive event ID")
		}
		notes, more, err := a.s.NotesPage(id, int(limit), before)
		if err != nil {
			return err
		}
		var next int64
		if len(notes) != 0 {
			next = notes[len(notes)-1].EventID
		}
		if a.o.yes("json") {
			return a.json(map[string]any{"notes": notes, "more": more, "next_before": next})
		}
		for _, note := range notes {
			fmt.Fprintf(a.out, "%s  %s  %s\n", note.ID, note.CreatedAt, note.Title)
		}
		if more {
			fmt.Fprintf(a.out, "more notes; use notes list %s --before %d\n", id, next)
		}
		return nil
	case "show":
		note, err := a.s.GetNote(id)
		if err != nil {
			return err
		}
		return a.noteResult(note)
	case "read":
		if a.o.yes("json") {
			return usage("notes read streams Markdown; omit --json, or use notes show --json for metadata")
		}
		return a.s.WriteNoteBody(id, a.out)
	case "delete":
		if err := a.s.DeleteNote(id, a.o.get("claim")); err != nil {
			return err
		}
		return a.ok(map[string]string{"note_id": id, "status": "deleted"}, "deleted "+id)
	default:
		return usage("unknown notes command " + op)
	}
}

func (a *app) noteResult(note store.Note) error {
	if a.o.yes("json") {
		return a.json(note)
	}
	fmt.Fprintf(a.out, "%s  task=%s  %s\n", note.ID, note.TaskID, note.Title)
	fmt.Fprintf(a.out, "author=%s created=%s body_bytes=%d\n", note.Author, note.CreatedAt, note.BodyBytes)
	if note.DeletedAt != "" {
		fmt.Fprintln(a.out, "deleted:", note.DeletedAt)
	} else if note.BodyBytes > 0 {
		fmt.Fprintf(a.out, "read body: notes read %s\n", note.ID)
	}
	return nil
}
