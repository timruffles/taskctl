package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"taskctl/internal/codex"
	"taskctl/internal/project"
	"taskctl/internal/store"
)

const version = "0.2.0"
const help = `taskctl 0.2.0 — local task tracking

Select a project with --project ID, TASKCTL_PROJECT, or a .taskctl.json file.
Flags may follow positional IDs. All read commands support --json.

projects create --name NAME [--path DIR]
projects attach --project ID --path DIR
projects show [--project ID]
tasks create --title TITLE --brief TEXT [--tag TAG] [--prerequisite T1] [--request-id KEY]
tasks available | in-progress | blocked | list [--tag TAG] [--limit N] [--offset N]
tasks show T1
tasks history T1 [--limit N] [--offset N]
tasks claim T1 --session SESSION
tasks release T1 --claim TOKEN
tasks takeover T1 --expected-claim TOKEN --session SESSION --reason TEXT
tasks progress T1 --claim TOKEN --text TEXT
tasks heartbeat T1 --claim TOKEN
tasks complete T1 --claim TOKEN --summary TEXT
tasks cancel T1 --reason TEXT [--claim TOKEN]
tasks supersede T1 --revision N --file PLAN.json [--claim TOKEN]
tasks edit T1 --revision N [--title TITLE] [--brief-file FILE] [--claim TOKEN]
tasks tag T1 --tag TAG | untag T1 --tag TAG [--claim TOKEN]
tasks metadata T1 --key KEY --value VALUE [--claim TOKEN]
tasks depend T1 --prerequisite T2 | undepend T1 --prerequisite T2 [--claim TOKEN]
tasks watch T1 [--after EVENT_ID] [--timeout 10m] [--exec PROGRAM] [--once]
tasks wait T1 [--timeout 10m]
tasks subscribe T1 --session SESSION [--resume]
tasks subscriptions [--session SESSION] [--limit N] [--offset N]
tasks unsubscribe SUB --session SESSION
subscriptions pending SUB --session SESSION [--limit N] [--lease 5m]
subscriptions acknowledge SUB --session SESSION --token TOKEN --through EVENT_ID
subscriptions watch SUB --session SESSION [--exec PROGRAM | --codex] [--once]
scratch create T1 | attach T1 --area AREA
scratch list T1 [--limit N] [--offset N]
scratch cleanup [--grace 168h] [--dry-run]
backup --file FILE.sqlite | restore --file FILE.sqlite --name NAME
integrations codex config | hook
version | --help

Human output is compact. --json emits structured output; JSON errors go to stderr.
Lists and history are paged: default 20, maximum 200. Backups use SQLite only.
Exit codes: 0 success, 1 other failure, 2 usage, 3 conflict, 4 cancelled, 5 timeout.
Supersede files contain {"summary":"...","replacements":[{"key":"a","title":"...","brief":"..."}],"continue_with":"a"}.
Replacement prerequisites may refer to new siblings as @key. Repeated --tag filters mean AND.
Scratch is disposable. Put important work in the repository and reference it in a task.
`

type app struct {
	o   options
	out io.Writer
	in  io.Reader
	p   project.Project
	s   *store.Store
	cfg project.Config
}

func main() {
	o, err := parse(os.Args[1:])
	if err != nil && jsonRequested(os.Args[1:]) {
		o.values["json"] = []string{"true"}
	}
	if err == nil {
		err = run(o, os.Stdin, os.Stdout)
	}
	if err == nil {
		return
	}
	code := 1
	name := store.ErrorCode(err)
	var x *exitError
	if errors.As(err, &x) {
		code = x.code
		if code == 2 {
			name = "usage"
		} else if code == 4 {
			name = "cancelled"
		} else if code == 5 {
			name = "timeout"
		}
	}
	if strings.HasPrefix(name, "invalid_") {
		code = 2
	}
	if name == "already_claimed" || name == "stale_claim" || name == "revision_conflict" || name == "conflict" || name == "busy" || name == "blocked" || name == "delivery_busy" || name == "stale_delivery" {
		code = 3
	}
	if o.yes("json") {
		_ = json.NewEncoder(os.Stderr).Encode(map[string]any{"error": map[string]string{"code": name, "message": err.Error()}})
	} else {
		fmt.Fprintln(os.Stderr, "error:", err)
	}
	os.Exit(code)
}
func run(o options, in io.Reader, out io.Writer) error {
	if o.yes("help") || len(o.pos) == 0 {
		fmt.Fprint(out, help)
		return nil
	}
	if o.pos[0] == "version" {
		if err := o.allow(); err != nil {
			return err
		}
		if len(o.pos) != 1 {
			return usage("version takes no arguments")
		}
		if o.yes("json") {
			return json.NewEncoder(out).Encode(map[string]string{"version": version})
		}
		fmt.Fprintln(out, "taskctl", version)
		return nil
	}
	switch o.pos[0] {
	case "projects", "restore", "backup", "tasks", "subscriptions", "scratch", "integrations":
	default:
		return usage("unknown command " + o.pos[0])
	}
	a := &app{o: o, in: in, out: out}
	cfg, err := project.Defaults()
	if err != nil {
		return err
	}
	a.cfg = cfg
	if o.pos[0] == "projects" {
		return a.projects()
	}
	if o.pos[0] == "restore" {
		return a.restore()
	}
	cwd, err := os.Getwd()
	if err != nil {
		return err
	}
	p, err := project.Resolve(o.get("project"), cwd)
	if err != nil {
		return err
	}
	a.p = p
	s, err := store.OpenExisting(p.Database, p.ID)
	if err != nil {
		return err
	}
	a.s = s
	defer s.Close()
	if o.pos[0] == "backup" {
		return a.backup()
	}
	if len(o.pos) < 2 {
		return usage("a subcommand is required")
	}
	switch o.pos[0] {
	case "tasks":
		err = a.tasks()
	case "subscriptions":
		err = a.subscriptions()
	case "scratch":
		err = a.scratch()
	case "integrations":
		err = a.integrations()
	default:
		return usage("unknown command " + o.pos[0])
	}
	if err == nil && o.pos[0] == "tasks" {
		switch o.pos[1] {
		case "complete", "cancel", "supersede", "create", "release":
			a.opportunisticCleanup()
		}
	}
	return err
}
func (a *app) json(v any) error { return json.NewEncoder(a.out).Encode(v) }
func (a *app) ok(v any, human string) error {
	if a.o.yes("json") {
		return a.json(v)
	}
	if human != "" {
		fmt.Fprintln(a.out, human)
	}
	return nil
}
func (a *app) projects() error {
	if len(a.o.pos) < 2 {
		return usage("projects requires create, attach or show")
	}
	switch a.o.pos[1] {
	case "create":
		if err := a.o.allow("name", "path"); err != nil {
			return err
		}
		if err := a.o.noID(); err != nil {
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
			return err
		}
		s.Close()
		if a.o.has("path") {
			if err = project.Attach(p, a.o.get("path")); err != nil {
				return err
			}
		}
		return a.ok(p, p.ID+"  "+p.Name)
	case "attach":
		if err := a.o.allow("path"); err != nil {
			return err
		}
		if err := a.o.noID(); err != nil {
			return err
		}
		id, err := a.o.require("project")
		if err != nil {
			return err
		}
		path, err := a.o.require("path")
		if err != nil {
			return err
		}
		p, err := project.Load(id)
		if err != nil {
			return err
		}
		if err = project.Attach(p, path); err != nil {
			return err
		}
		return a.ok(p, "attached "+path+" to "+p.ID)
	case "show":
		if err := a.o.allow(); err != nil {
			return err
		}
		if err := a.o.noID(); err != nil {
			return err
		}
		cwd, err := os.Getwd()
		if err != nil {
			return err
		}
		p, err := project.Resolve(a.o.get("project"), cwd)
		if err != nil {
			return err
		}
		return a.ok(p, p.ID+"  "+p.Name+"\ndatabase: "+p.Database+"\nscratch: "+p.Scratch)
	}
	return usage("unknown projects command " + a.o.pos[1])
}
func (a *app) integrations() error {
	if len(a.o.pos) != 3 || a.o.pos[1] != "codex" {
		return usage("integrations codex requires config or hook")
	}

	exe := a.o.get("executable")
	if exe == "" {
		var err error
		exe, err = os.Executable()
		if err != nil {
			return err
		}
	}
	exe, err := filepath.Abs(exe)
	if err != nil {
		return err
	}
	switch a.o.pos[2] {
	case "config":
		if err := a.o.allow("executable"); err != nil {
			return err
		}
		b, err := codex.Config(exe, a.p.ID)
		if err != nil {
			return err
		}
		_, err = a.out.Write(append(b, '\n'))
		return err
	case "hook":
		if err := a.o.allow("executable", "limit", "lease", "max-context-bytes"); err != nil {
			return err
		}
		maxContext, err := a.o.number("max-context-bytes", 3000)
		if err != nil {
			return err
		}
		if maxContext < 512 || maxContext > 12000 {
			return usage("max-context-bytes must be 512..12000")
		}
		n, err := a.o.number("limit", 5)
		if err != nil || n < 1 || n > 20 {
			if err != nil {
				return err
			}
			return usage("limit must be 1..20")
		}
		lease, err := a.o.duration("lease", 5*time.Minute)
		if err != nil {
			return err
		}
		inactivity, _ := time.ParseDuration(a.cfg.Inactivity)
		return codex.Handle(a.s, a.in, a.out, codex.Options{Executable: exe, EventLimit: int(n), Lease: lease, MaxContextBytes: int(maxContext), Inactivity: inactivity})
	}
	return usage("unknown Codex integration command")
}
func (a *app) readText(flag string) (string, error) {
	fileflag := flag + "-file"
	if a.o.has(flag) && a.o.has(fileflag) {
		return "", usage("use either --" + flag + " or --" + fileflag)
	}
	if a.o.has(fileflag) {
		var r io.Reader
		if a.o.get(fileflag) == "-" {
			r = a.in
		} else {
			f, err := os.Open(a.o.get(fileflag))
			if err != nil {
				return "", err
			}
			defer f.Close()
			r = f
		}
		b, err := io.ReadAll(io.LimitReader(r, 1024*1024+1))
		if len(b) > 1024*1024 {
			return "", usage("text input exceeds 1 MiB")
		}
		return string(b), err
	}
	return a.o.get(flag), nil
}
func clean(s string) string { return strings.Join(strings.Fields(s), " ") }
