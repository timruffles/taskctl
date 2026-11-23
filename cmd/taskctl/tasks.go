package main

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
	"text/tabwriter"

	scratchpkg "taskctl/internal/scratch"
	"taskctl/internal/store"
)

type listItem struct {
	ID             string   `json:"id"`
	Title          string   `json:"title"`
	Status         string   `json:"status"`
	OriginID       string   `json:"origin_id"`
	Owner          string   `json:"owner,omitempty"`
	Tags           []string `json:"tags"`
	LastActivityAt string   `json:"last_activity_at,omitempty"`
	Revision       int64    `json:"revision"`
}

func (a *app) taskResult(t store.Task) error {
	if a.o.yes("json") {
		return a.json(t)
	}
	fmt.Fprintf(a.out, "%s  %s  %s  revision=%d\n", t.ID, t.Status, clean(t.Title), t.Revision)
	if t.ClaimID != "" {
		fmt.Fprintf(a.out, "owner=%s claim=%s\n", t.Owner, t.ClaimID)
	}
	return nil
}
func (a *app) tasks() error {
	op := a.o.pos[1]
	switch op {
	case "available", "in-progress", "blocked", "list":
		return a.listTasks(op)
	case "create":
		if err := a.o.allow("title", "brief", "brief-file", "tag", "metadata", "prerequisite", "request-id"); err != nil {
			return err
		}
		if err := a.o.noID(); err != nil {
			return err
		}
		if a.o.has("request-id") && a.o.get("request-id") == "" {
			return usage("--request-id must not be empty")
		}
		title, err := a.o.require("title")
		if err != nil {
			return err
		}
		brief, err := a.readText("brief")
		if err != nil {
			return err
		}
		meta := map[string]string{}
		for _, v := range a.o.values["metadata"] {
			k, v, ok := strings.Cut(v, "=")
			if !ok || k == "" {
				return usage("metadata must be KEY=VALUE")
			}
			if _, exists := meta[k]; exists {
				return usage("duplicate metadata key " + k)
			}
			meta[k] = v
		}
		t, err := a.s.CreateWithRequest(store.CreateSpec{Title: title, Brief: brief, Tags: a.o.values["tag"], Metadata: meta, Prerequisites: a.o.values["prerequisite"]}, a.o.get("request-id"))
		if err != nil {
			return err
		}
		return a.taskResult(t)
	case "subscriptions":
		if err := a.o.allow("session", "limit", "offset"); err != nil {
			return err
		}
		if err := a.o.noID(); err != nil {
			return err
		}
		limit, offset, err := a.pageOptions()
		if err != nil {
			return err
		}
		items, more, err := a.s.SubscriptionsPage(a.o.get("session"), limit, offset)
		if err != nil {
			return err
		}
		if a.o.yes("json") {
			return a.json(map[string]any{"subscriptions": items, "more": more, "next_offset": offset + len(items)})
		}
		for _, s := range items {
			fmt.Fprintf(a.out, "%s  %s  session=%s resume=%t\n", s.ID, s.TaskID, s.Session, s.ResumeIdle)
		}
		a.printNextPage(more, offset+len(items))
		return nil
	case "watch", "wait":
		return a.watch(op == "wait")
	}
	id, err := a.o.position(2)
	if err != nil {
		return err
	}
	claim := a.o.get("claim")
	switch op {
	case "show":
		if err = a.o.allow(); err != nil {
			return err
		}
		t, e := a.s.Get(id)
		if e != nil {
			return e
		}
		if a.o.yes("json") {
			areas, more, e := a.s.ScratchListPage(id, a.cfg.ListLimit, 0)
			if e != nil {
				return e
			}
			refs := make([]scratchpkg.Result, 0, len(areas))
			for _, area := range areas {
				refs = append(refs, a.scratchResult(area))
			}
			return a.json(struct {
				store.Task
				Scratch           []scratchpkg.Result `json:"scratch"`
				ScratchMore       bool                `json:"scratch_more"`
				ScratchNextOffset int                 `json:"scratch_next_offset"`
			}{t, refs, more, len(refs)})
		}
		if e = a.taskResult(t); e != nil {
			return e
		}
		fmt.Fprintf(a.out, "origin=%s", t.OriginID)
		if t.ParentID != "" {
			fmt.Fprintf(a.out, " predecessor=%s", t.ParentID)
		}
		fmt.Fprintln(a.out)
		if len(t.Tags) > 0 {
			fmt.Fprintln(a.out, "tags:", strings.Join(t.Tags, ", "))
		}
		if len(t.Prerequisites) > 0 {
			fmt.Fprintln(a.out, "prerequisites:", strings.Join(t.Prerequisites, ", "))
		}
		fmt.Fprintln(a.out, "\n"+t.Brief)
		if t.Progress != "" {
			fmt.Fprintln(a.out, "\nlatest progress:", t.Progress)
			if t.ProgressNoteID != "" {
				fmt.Fprintf(a.out, "note=%s created=%s\n", t.ProgressNoteID, t.ProgressAt)
			}
		}
		if t.Result != "" {
			fmt.Fprintln(a.out, "\nresult:", t.Result)
		}
		if len(t.Metadata) > 0 {
			b, _ := json.Marshal(t.Metadata)
			fmt.Fprintln(a.out, "metadata:", string(b))
		}
		areas, more, e := a.s.ScratchListPage(id, a.cfg.ListLimit, 0)
		if e != nil {
			return e
		}
		for _, x := range areas {
			fmt.Fprintf(a.out, "scratch: %s  %s\n", x.ID, a.scratchPath(x.ID))
		}
		if more {
			fmt.Fprintf(a.out, "more scratch references; use scratch list %s --offset %d\n", id, len(areas))
		}
		return nil
	case "history":
		if err = a.o.allow("limit", "offset"); err != nil {
			return err
		}
		limit, offset, e := a.pageOptions()
		if e != nil {
			return e
		}
		items, more, e := a.s.HistoryPage(id, limit, offset)
		if e != nil {
			return e
		}
		if a.o.yes("json") {
			return a.json(map[string]any{"tasks": items, "more": more, "next_offset": offset + len(items)})
		}
		for _, t := range items {
			fmt.Fprintf(a.out, "%s  %s  %s\n%s\n\n", t.ID, t.Status, t.Title, t.Brief)
		}
		a.printNextPage(more, offset+len(items))
		return nil
	case "claim":
		if err = a.o.allow("session"); err != nil {
			return err
		}
		session, e := a.o.require("session")
		if e != nil {
			return e
		}
		t, e := a.s.Claim(id, session)
		if e != nil {
			return e
		}
		return a.taskResult(t)
	case "release":
		if err = a.o.allow("claim"); err != nil {
			return err
		}
		if _, err = a.o.require("claim"); err != nil {
			return err
		}
		t, e := a.s.Release(id, claim)
		if e != nil {
			return e
		}
		return a.taskResult(t)
	case "takeover":
		if err = a.o.allow("expected-claim", "session", "reason"); err != nil {
			return err
		}
		old, e := a.o.require("expected-claim")
		if e != nil {
			return e
		}
		session, e := a.o.require("session")
		if e != nil {
			return e
		}
		reason, e := a.o.require("reason")
		if e != nil {
			return e
		}
		t, e := a.s.Takeover(id, old, session, reason)
		if e != nil {
			return e
		}
		return a.taskResult(t)
	case "progress":
		return a.progress(id)
	case "heartbeat":
		if err = a.o.allow("claim"); err != nil {
			return err
		}
		if e := a.s.Heartbeat(id, claim); e != nil {
			return e
		}
		return a.ok(map[string]any{"task_id": id, "recorded": true}, "")
	case "complete":
		if err = a.o.allow("claim", "summary", "summary-file"); err != nil {
			return err
		}
		text, e := a.readText("summary")
		if e != nil {
			return e
		}
		t, e := a.s.Complete(id, claim, text)
		if e != nil {
			return e
		}
		return a.taskResult(t)
	case "cancel":
		if err = a.o.allow("claim", "reason"); err != nil {
			return err
		}
		reason, e := a.o.require("reason")
		if e != nil {
			return e
		}
		t, e := a.s.Cancel(id, claim, reason)
		if e != nil {
			return e
		}
		return a.taskResult(t)
	case "edit":
		if err = a.o.allow("claim", "revision", "title", "brief", "brief-file"); err != nil {
			return err
		}
		rev, e := a.revision()
		if e != nil {
			return e
		}
		var title, brief *string
		if a.o.has("title") {
			v := a.o.get("title")
			title = &v
		}
		if a.o.has("brief") || a.o.has("brief-file") {
			v, e := a.readText("brief")
			if e != nil {
				return e
			}
			brief = &v
		}
		if title == nil && brief == nil {
			return usage("edit requires --title or --brief/--brief-file")
		}
		t, e := a.s.Edit(id, claim, rev, title, brief)
		if e != nil {
			return e
		}
		return a.taskResult(t)
	case "evolve", "supersede":
		if err = a.o.allow("claim", "revision", "file"); err != nil {
			return err
		}
		rev, e := a.revision()
		if e != nil {
			return e
		}
		var spec store.EvolveSpec
		if e = a.readJSONFile(&spec); e != nil {
			return e
		}
		tasks, e := a.s.Evolve(id, claim, rev, spec)
		if e != nil {
			return e
		}
		if a.o.yes("json") {
			return a.json(tasks)
		}
		fmt.Fprintln(a.out, id, "evolved")
		for _, t := range tasks {
			if e = a.taskResult(t); e != nil {
				return e
			}
		}
		return nil
	case "tag", "untag":
		if err = a.o.allow("claim", "tag"); err != nil {
			return err
		}
		if len(a.o.values["tag"]) != 1 {
			return usage("tag/untag requires exactly one --tag")
		}
		if e := a.s.Tag(id, claim, a.o.get("tag"), op == "untag"); e != nil {
			return e
		}
		return a.ok(map[string]string{"task_id": id, "tag": a.o.get("tag"), "operation": op}, id+" "+op+" "+a.o.get("tag"))
	case "metadata":
		if err = a.o.allow("claim", "key", "value"); err != nil {
			return err
		}
		key, e := a.o.require("key")
		if e != nil {
			return e
		}
		if !a.o.has("value") {
			return usage("--value is required")
		}
		if e = a.s.MetadataSet(id, claim, key, a.o.get("value")); e != nil {
			return e
		}
		return a.ok(map[string]string{"task_id": id, "key": key, "value": a.o.get("value")}, id+" metadata "+key+" set")
	case "depend", "undepend":
		if err = a.o.allow("claim", "prerequisite"); err != nil {
			return err
		}
		if len(a.o.values["prerequisite"]) != 1 {
			return usage("depend/undepend requires exactly one --prerequisite")
		}
		pr := a.o.get("prerequisite")
		if e := a.s.AddPrerequisite(id, claim, pr, op == "undepend"); e != nil {
			return e
		}
		return a.ok(map[string]string{"task_id": id, "prerequisite": pr, "operation": op}, id+" "+op+" "+pr)
	case "subscribe":
		if err = a.o.allow("session", "resume"); err != nil {
			return err
		}
		session, e := a.o.require("session")
		if e != nil {
			return e
		}
		sub, e := a.s.Subscribe(id, session, a.o.yes("resume"))
		if e != nil {
			return e
		}
		return a.ok(sub, sub.ID+"  task="+sub.TaskID+" session="+sub.Session)
	case "unsubscribe":
		if err = a.o.allow("session"); err != nil {
			return err
		}
		session, e := a.o.require("session")
		if e != nil {
			return e
		}
		if e = a.s.Unsubscribe(id, session); e != nil {
			return e
		}
		return a.ok(map[string]string{"subscription_id": id, "status": "inactive"}, id+" unsubscribed")
	}
	return usage("unknown tasks command " + op)
}
func (a *app) revision() (int64, error) {
	if !a.o.has("revision") {
		return 0, usage("--revision is required; use tasks show to read it")
	}
	n, e := a.o.number("revision", 0)
	if e != nil {
		return 0, e
	}
	if n == 0 {
		return 0, usage("revision must be positive")
	}
	return n, nil
}
func (a *app) readJSONFile(v any) error {
	path, err := a.o.require("file")
	if err != nil {
		return err
	}
	r := a.in
	if path != "-" {
		f, e := os.Open(path)
		if e != nil {
			return e
		}
		defer f.Close()
		r = f
	}
	d := json.NewDecoder(io.LimitReader(r, 1024*1024+1))
	d.DisallowUnknownFields()
	if err = d.Decode(v); err != nil {
		return usage("invalid JSON: " + err.Error())
	}
	var extra any
	if err = d.Decode(&extra); err != io.EOF {
		return usage("expected exactly one JSON object")
	}
	return nil
}
func (a *app) pageOptions() (int, int, error) {
	limit, err := a.o.number("limit", int64(a.cfg.ListLimit))
	if err != nil {
		return 0, 0, err
	}
	if limit < 1 || limit > 200 {
		return 0, 0, usage("limit must be 1..200")
	}
	offset, err := a.o.number("offset", 0)
	if err != nil {
		return 0, 0, err
	}
	if int64(int(offset)) != offset {
		return 0, 0, usage("offset is too large")
	}
	return int(limit), int(offset), nil
}

func (a *app) printNextPage(more bool, offset int) {
	if more {
		fmt.Fprintln(a.out, "more results; use --offset "+strconv.Itoa(offset))
	}
}

func (a *app) listTasks(op string) error {
	if err := a.o.allow("tag", "exclude-tag", "where", "limit", "offset", "status"); err != nil {
		return err
	}
	if err := a.o.noID(); err != nil {
		return err
	}
	limit, err := a.o.number("limit", int64(a.cfg.ListLimit))
	if err != nil {
		return err
	}
	if limit < 1 || limit > 200 {
		return usage("limit must be 1..200")
	}
	offset, err := a.o.number("offset", 0)
	if err != nil {
		return err
	}
	filter := op
	if op == "in-progress" {
		filter = "in_progress"
	}
	if op == "list" {
		filter = a.o.get("status")
		if filter == "" {
			filter = "current"
		}
	} else if a.o.has("status") {
		return usage("--status is only supported by tasks list")
	}
	var match func(store.Task) (bool, error)
	if a.o.has("where") {
		match, err = compileTaskFilter(a.o.get("where"))
		if err != nil {
			return err
		}
	}
	items, total, err := a.s.ListMatching(filter, a.o.values["tag"], a.o.values["exclude-tag"], int(limit), int(offset), match)
	if err != nil {
		return err
	}
	rows := make([]listItem, 0, len(items))
	for _, t := range items {
		r := listItem{ID: t.ID, Title: t.Title, Status: t.Status, OriginID: t.OriginID, Tags: t.Tags, Revision: t.Revision}
		if t.Status == "in_progress" {
			r.Owner = t.Owner
			r.LastActivityAt = t.LastActivityAt
		}
		rows = append(rows, r)
	}
	omitted := total - int(offset) - len(rows)
	if omitted < 0 {
		omitted = 0
	}
	if a.o.yes("json") {
		return a.json(map[string]any{"project_id": a.p.ID, "tasks": rows, "total": total, "omitted": omitted, "next_offset": int(offset) + len(rows)})
	}
	w := tabwriter.NewWriter(a.out, 0, 4, 2, ' ', 0)
	fmt.Fprintf(w, "PROJECT %s  %s\n", a.p.ID, clean(a.p.Name))
	fmt.Fprintln(w, "ID\tSTATUS\tTITLE\tTAGS\tORIGIN\tOWNER\tLAST ACTIVITY")
	for _, t := range rows {
		fmt.Fprintf(w, "%s\t%s\t%s\t%s\t%s\t%s\t%s\n", t.ID, t.Status, clean(t.Title), strings.Join(t.Tags, ","), t.OriginID, t.Owner, t.LastActivityAt)
	}
	if err = w.Flush(); err != nil {
		return err
	}
	if omitted > 0 {
		fmt.Fprintln(a.out, strconv.Itoa(omitted)+" more; use --offset "+strconv.Itoa(int(offset)+len(rows)))
	}
	return nil
}
