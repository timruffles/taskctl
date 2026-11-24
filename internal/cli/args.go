package cli

import (
	"strconv"
	"strings"
	"time"
)

type options struct {
	values map[string][]string
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
