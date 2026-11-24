package cli

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/spf13/cobra"
)

type commandSpec struct {
	use     string
	short   string
	flags   string
	cleanup bool
}

func newRoot(version string) *cobra.Command {
	root := commandGroup("taskctl", "Track local tasks and coordinate work")
	root.Long = root.Short + ". Select a project with --project, TASKCTL_PROJECT, or a .taskctl.json file."
	root.Version = version
	root.SilenceErrors = true
	root.SilenceUsage = true
	root.SetFlagErrorFunc(func(_ *cobra.Command, err error) error { return usage(err.Error()) })

	values := map[string][]string{}
	addFlags(root.PersistentFlags(), values, "project json")

	add := func(parent *cobra.Command, action handler, needsStore bool, specs ...commandSpec) {
		for _, spec := range specs {
			cmd := newCommand(spec, values, func(a *app) error {
				return a.run(action, needsStore, spec.cleanup)
			})
			parent.AddCommand(cmd)
		}
	}
	group := func(parent *cobra.Command, name, summary string) *cobra.Command {
		cmd := commandGroup(name, summary)
		parent.AddCommand(cmd)
		return cmd
	}

	projects := group(root, "projects", "Create, attach, or inspect a project")
	add(projects, (*app).projects, false,
		commandSpec{"create", "Create a project", "name path", false},
		commandSpec{"attach", "Attach a checkout to a project", "path", false},
		commandSpec{"show", "Show the selected project", "", false},
	)

	tasks := group(root, "tasks", "Create, update, and inspect tasks")
	add(tasks, (*app).tasks, true,
		commandSpec{"create", "Create a task", "title brief brief-file tag metadata prerequisite request-id", true},
		commandSpec{"available", "List tasks ready to claim", "tag exclude-tag where limit offset", false},
		commandSpec{"in-progress", "List claimed tasks", "tag exclude-tag where limit offset", false},
		commandSpec{"blocked", "List tasks with unmet prerequisites", "tag exclude-tag where limit offset", false},
		commandSpec{"list", "List tasks by status or filter", "status tag exclude-tag where limit offset", false},
		commandSpec{"show TASK_ID", "Show a task and its latest progress", "", false},
		commandSpec{"history TASK_ID", "Show a task's predecessor and replacement history", "limit offset", false},
		commandSpec{"claim TASK_ID", "Claim an available task", "session", false},
		commandSpec{"release TASK_ID", "Return a claimed task to pending", "claim", true},
		commandSpec{"takeover TASK_ID", "Transfer a claim to another session", "expected-claim session reason", false},
		commandSpec{"progress TASK_ID", "Record progress on a claimed task", "claim title body-file text text-file", false},
		commandSpec{"heartbeat TASK_ID", "Update the activity time of a claim", "claim", false},
		commandSpec{"complete TASK_ID", "Complete a claimed task", "claim summary summary-file", true},
		commandSpec{"cancel TASK_ID", "Cancel a task", "claim reason", true},
		commandSpec{"reopen TASK_ID", "Return a completed or cancelled task to pending", "reason", false},
		commandSpec{"evolve TASK_ID", "Replace a task with one or more new tasks", "claim revision file", true},
		commandSpec{"edit TASK_ID", "Edit a task while keeping its ID", "claim revision title brief brief-file", false},
		commandSpec{"tag TASK_ID", "Add a task tag", "claim tag", false},
		commandSpec{"untag TASK_ID", "Remove a task tag", "claim tag", false},
		commandSpec{"metadata TASK_ID", "Set a task metadata value", "claim key value", false},
		commandSpec{"depend TASK_ID", "Add a prerequisite", "claim prerequisite", false},
		commandSpec{"undepend TASK_ID", "Remove a prerequisite", "claim prerequisite", false},
		commandSpec{"watch TASK_ID", "Stream task events", "after timeout interval exec once inactivity", false},
		commandSpec{"wait TASK_ID", "Wait for a terminal task outcome", "timeout interval inactivity", false},
		commandSpec{"subscribe TASK_ID", "Subscribe a session to task events", "session resume", false},
		commandSpec{"subscriptions", "List task subscriptions", "session limit offset", false},
		commandSpec{"unsubscribe SUBSCRIPTION_ID", "Deactivate a task subscription", "session", false},
	)
	evolve, _, _ := tasks.Find([]string{"evolve"})
	evolve.Aliases = []string{"supersede"}

	subscriptions := group(root, "subscriptions", "Read, acknowledge, and watch subscription deliveries")
	add(subscriptions, (*app).subscriptions, true,
		commandSpec{"pending SUBSCRIPTION_ID", "Lease pending events for delivery", "session limit lease", false},
		commandSpec{"acknowledge SUBSCRIPTION_ID", "Acknowledge delivered events", "session token through", false},
		commandSpec{"watch SUBSCRIPTION_ID", "Watch for subscription deliveries", "session exec codex codex-executable once timeout interval limit lease inactivity", false},
	)

	scratch := group(root, "scratch", "Manage disposable files associated with tasks")
	add(scratch, (*app).scratch, true,
		commandSpec{"create TASK_ID", "Create a scratch area for a task", "claim", false},
		commandSpec{"attach TASK_ID", "Attach an existing scratch area to a task", "area claim", false},
		commandSpec{"list TASK_ID", "List scratch areas attached to a task", "limit offset", false},
		commandSpec{"cleanup", "Remove expired, unreferenced scratch areas", "grace dry-run limit", false},
	)

	notes := group(root, "notes", "Inspect and manage task progress notes")
	add(notes, (*app).notes, true,
		commandSpec{"list TASK_ID", "List progress notes for a task", "limit before", false},
		commandSpec{"show NOTE_ID", "Show note metadata", "", false},
		commandSpec{"read NOTE_ID", "Write a note's Markdown body to stdout", "", false},
		commandSpec{"delete NOTE_ID", "Delete a progress note", "claim", false},
	)

	codex := group(group(root, "integrations", "Configure integrations"), "codex", "Configure and run Codex hooks")
	add(codex, (*app).integrations, true,
		commandSpec{"config", "Print Codex hook configuration", "executable", false},
		commandSpec{"hook", "Handle a Codex hook request from stdin", "executable limit lease max-context-bytes", false},
	)
	add(root, (*app).backup, true,
		commandSpec{"backup", "Back up the selected project", "directory file", false},
	)
	add(root, (*app).restore, false,
		commandSpec{"restore", "Restore a backup into a new project", "directory file name path", false},
	)
	root.AddCommand(newCommand(commandSpec{use: "version", short: "Show the taskctl version"}, values, func(a *app) error {
		if a.o.yes("json") {
			return json.NewEncoder(a.out).Encode(map[string]string{"version": version})
		}
		_, err := fmt.Fprintln(a.out, "taskctl", version)
		return err
	}))
	return root
}

func commandGroup(use, short string) *cobra.Command {
	return &cobra.Command{
		Use: use, Short: short,
		Args: usageArgs(cobra.NoArgs),
		RunE: func(cmd *cobra.Command, _ []string) error {
			return cmd.Help()
		},
	}
}

func newCommand(spec commandSpec, values map[string][]string, action handler) *cobra.Command {
	cmd := &cobra.Command{
		Use: spec.use, Short: spec.short,
		Args: usageArgs(cobra.ExactArgs(len(strings.Fields(spec.use)) - 1)),
		RunE: func(cmd *cobra.Command, args []string) error {
			return action(&app{command: cmd.Name(), args: args, o: options{values: values}, in: cmd.InOrStdin(), out: cmd.OutOrStdout()})
		},
	}
	addFlags(cmd.Flags(), values, spec.flags)
	return cmd
}

func usageArgs(validate cobra.PositionalArgs) cobra.PositionalArgs {
	return func(cmd *cobra.Command, args []string) error {
		if err := validate(cmd, args); err != nil {
			return usage(err.Error())
		}
		return nil
	}
}
