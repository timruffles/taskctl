package main

import (
	"fmt"
	"strconv"
	"strings"
	"time"
)

type options struct {
	pos    []string
	values map[string][]string
}

var booleans = map[string]bool{"json": true, "help": true, "once": true, "resume": true, "codex": true, "all": true, "dry-run": true}
var repeated = map[string]bool{"tag": true, "metadata": true, "prerequisite": true}

func parse(args []string) (options, error) {
	o := options{values: map[string][]string{}}
	for i := 0; i < len(args); i++ {
		a := args[i]
		if a == "--" {
			o.pos = append(o.pos, args[i+1:]...)
			break
		}
		if a == "-h" {
			a = "--help"
		}
		if !strings.HasPrefix(a, "--") {
			if strings.HasPrefix(a, "-") {
				return o, usage("short flags are not supported: " + a)
			}
			o.pos = append(o.pos, a)
			continue
		}
		k, v, has := strings.Cut(strings.TrimPrefix(a, "--"), "=")
		if k == "" {
			return o, usage("empty flag")
		}
		if booleans[k] {
			if !has {
				v = "true"
			}
			if v != "true" && v != "false" {
				return o, usage("--" + k + " must be true or false")
			}
		} else if !has {
			if i+1 >= len(args) || strings.HasPrefix(args[i+1], "--") {
				return o, usage("--" + k + " requires a value")
			}
			i++
			v = args[i]
		}
		if _, ok := o.values[k]; ok && !repeated[k] {
			return o, usage("--" + k + " may only be specified once")
		}
		o.values[k] = append(o.values[k], v)
	}
	return o, nil
}
func (o options) get(k string) string {
	v := o.values[k]
	if len(v) == 0 {
		return ""
	}
	return v[len(v)-1]
}
func (o options) has(k string) bool { _, ok := o.values[k]; return ok }
func (o options) yes(k string) bool { return o.get(k) == "true" }
func (o options) require(k string) (string, error) {
	v := o.get(k)
	if strings.TrimSpace(v) == "" {
		return "", usage("--" + k + " is required")
	}
	return v, nil
}
func (o options) allow(keys ...string) error {
	allowed := map[string]bool{"json": true, "help": true, "project": true}
	for _, k := range keys {
		allowed[k] = true
	}
	for k := range o.values {
		if !allowed[k] {
			return usage("unknown or unsupported flag --" + k)
		}
	}
	return nil
}
func (o options) number(k string, def int64) (int64, error) {
	if !o.has(k) {
		return def, nil
	}
	n, e := strconv.ParseInt(o.get(k), 10, 64)
	if e != nil || n < 0 {
		return 0, usage("--" + k + " must be a nonnegative integer")
	}
	return n, nil
}
func (o options) duration(k string, def time.Duration) (time.Duration, error) {
	if !o.has(k) {
		return def, nil
	}
	d, e := time.ParseDuration(o.get(k))
	if e != nil || d <= 0 {
		return 0, usage("--" + k + " must be a positive Go duration, such as 30s or 2h")
	}
	return d, nil
}
func (o options) position(n int) (string, error) {
	if len(o.pos) != n+1 {
		return "", usage(fmt.Sprintf("expected exactly %d arguments after the command", n-1))
	}
	return o.pos[n], nil
}
func (o options) noID() error {
	if len(o.pos) != 2 {
		return usage("this command takes no task ID or positional arguments")
	}
	return nil
}

type exitError struct {
	code    int
	message string
}

func (e *exitError) Error() string { return e.message }
func usage(s string) error         { return &exitError{2, s} }

func jsonRequested(args []string) bool {
	requested := false
	for _, arg := range args {
		if arg == "--" {
			break
		}
		if arg == "--json" || arg == "--json=true" {
			requested = true
		}
		if arg == "--json=false" {
			requested = false
		}
	}
	return requested
}
