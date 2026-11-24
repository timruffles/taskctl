package cli

import (
	"fmt"
	"strings"

	"github.com/spf13/cobra"
	"github.com/spf13/pflag"
)

// pflag accepts a following flag as a separated value. Catch that case
// before it can become task data. Explicit --flag=--value remains valid.
func checkFlagValues(root *cobra.Command, args []string) error {
	cmd, _, err := root.Find(args)
	if err != nil {
		return nil // Cobra reports command lookup errors.
	}
	for i, arg := range args {
		if arg == "--" {
			break
		}
		name, explicit := strings.CutPrefix(arg, "--")
		if !explicit || strings.Contains(name, "=") || i+1 == len(args) || !strings.HasPrefix(args[i+1], "--") {
			continue
		}
		flag := cmd.Flag(name)
		if flag != nil && flag.NoOptDefVal == "" {
			return usage(arg + " requires a value")
		}
	}
	return nil
}

func addFlags(flags *pflag.FlagSet, values map[string][]string, names string) {
	for _, name := range strings.Fields(names) {
		spec := flagSpecs[name]
		flag := flags.VarPF(&optionValue{name: name, spec: spec, values: values}, name, "", spec.help)
		if spec.kind == "bool" {
			flag.NoOptDefVal = "true"
		}
		flag.Hidden = name == "text" || name == "text-file"
	}
}

type flagSpec struct {
	kind   string
	help   string
	repeat bool
}

var flagSpecs = map[string]flagSpec{
	"after":             {"int", "Start reading after this event ID", false},
	"area":              {"string", "Scratch area ID", false},
	"before":            {"int", "Return notes before this event ID", false},
	"body-file":         {"string", "Read a Markdown body from a file (use - for stdin)", false},
	"brief":             {"string", "Task brief", false},
	"brief-file":        {"string", "Read the task brief from a file (use - for stdin)", false},
	"claim":             {"string", "Current claim token", false},
	"codex":             {"bool", "Queue deliveries for Codex", false},
	"codex-executable":  {"string", "Path to the Codex executable", false},
	"directory":         {"string", "Backup directory containing the project databases", false},
	"dry-run":           {"bool", "Preview scratch cleanup", false},
	"exclude-tag":       {"string", "Exclude tasks with this tag (repeatable)", true},
	"exec":              {"string", "Invoke a program with each event as JSON on stdin", false},
	"executable":        {"string", "Path to the taskctl executable", false},
	"expected-claim":    {"string", "Current claim token expected by the caller", false},
	"file":              {"string", "Input or output file", false},
	"grace":             {"duration", "Minimum age of an unreferenced scratch area", false},
	"inactivity":        {"duration", "Time before a claim is considered inactive", false},
	"interval":          {"duration", "Polling interval", false},
	"json":              {"bool", "Write machine-readable output and errors where supported", false},
	"key":               {"string", "Metadata key", false},
	"lease":             {"duration", "Delivery lease duration", false},
	"limit":             {"int", "Maximum number of results", false},
	"max-context-bytes": {"int", "Maximum bytes of context to return to Codex", false},
	"metadata":          {"string", "Task metadata as KEY=VALUE (repeatable)", true},
	"name":              {"string", "Project name", false},
	"offset":            {"int", "Number of results to skip", false},
	"once":              {"bool", "Exit after one polling cycle", false},
	"path":              {"string", "Checkout path", false},
	"prerequisite":      {"string", "Prerequisite task ID", true},
	"project":           {"string", "Select a project by ID", false},
	"reason":            {"string", "Reason to record in the task's event history", false},
	"request-id":        {"string", "Retry key for idempotent creation", false},
	"requires-resource": {"string", "Exclusive resource required at claim time (repeatable)", true},
	"resume":            {"bool", "Allow Codex to resume an idle session", false},
	"revision":          {"int", "Expected task revision from tasks show", false},
	"session":           {"string", "Session identifier", false},
	"status":            {"string", "Task status filter (default current)", false},
	"summary":           {"string", "Completion summary", false},
	"summary-file":      {"string", "Read a completion summary from a file (use - for stdin)", false},
	"tag":               {"string", "Task tag", true},
	"text":              {"string", "Legacy progress text", false},
	"text-file":         {"string", "Legacy progress text file", false},
	"through":           {"int", "Last event ID to acknowledge", false},
	"timeout":           {"duration", "Maximum time to wait", false},
	"title":             {"string", "Title", false},
	"token":             {"string", "Delivery token", false},
	"value":             {"string", "Metadata value", false},
	"where":             {"string", "CEL expression to filter tasks", false},
}

// optionValue preserves explicit empty values and rejects duplicate scalar flags.
type optionValue struct {
	name   string
	spec   flagSpec
	values map[string][]string
}

func (v *optionValue) String() string { return "" }
func (v *optionValue) Type() string   { return v.spec.kind }
func (v *optionValue) Set(value string) error {
	if len(v.values[v.name]) > 0 && !v.spec.repeat {
		return fmt.Errorf("--%s may only be specified once", v.name)
	}
	if v.spec.kind == "bool" && value != "true" && value != "false" {
		return fmt.Errorf("--%s must be true or false", v.name)
	}
	v.values[v.name] = append(v.values[v.name], value)
	return nil
}
