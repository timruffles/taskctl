package cli

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

type app struct {
	command string
	args    []string
	o       options
	out     io.Writer
	in      io.Reader
	p       project.Project
	s       *store.Store
	cfg     project.Config
}

// Execute runs taskctl and returns its process exit code.
func Execute(args []string, in io.Reader, out, errOut io.Writer, version string) int {
	root := newRoot(version)
	root.SetArgs(append([]string{}, args...))
	root.SetIn(in)
	root.SetOut(out)
	root.SetErr(errOut)
	err := checkFlagValues(root, args)
	if err == nil {
		err = root.Execute()
	}
	if err == nil {
		return 0
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
	if jsonRequested(args) {
		_ = json.NewEncoder(os.Stderr).Encode(map[string]any{"error": map[string]string{"code": name, "message": err.Error()}})
	} else {
		fmt.Fprintln(errOut, "error:", err)
	}
	return code
}

type handler func(*app) error

func (a *app) run(action handler, needsStore, cleanup bool) error {
	cfg, err := project.Defaults()
	if err != nil {
		return err
	}
	a.cfg = cfg
	if !needsStore {
		return action(a)
	}
	cwd, err := os.Getwd()
	if err != nil {
		return err
	}
	p, err := project.Resolve(a.o.get("project"), cwd)
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
	err = action(a)
	if err == nil && cleanup {
		a.opportunisticCleanup()
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
	switch a.command {
	case "create":
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
	return usage("unknown projects command " + a.command)
}
func (a *app) integrations() error {

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
	switch a.command {
	case "config":
		b, err := codex.Config(exe, a.p.ID)
		if err != nil {
			return err
		}
		_, err = a.out.Write(append(b, '\n'))
		return err
	case "hook":
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
