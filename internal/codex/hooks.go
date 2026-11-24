package codex

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"strconv"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"taskctl/internal/store"
)

type Options struct {
	Executable      string
	EventLimit      int
	Lease           time.Duration
	Inactivity      time.Duration
	MaxContextBytes int
}

type hookInput struct {
	SessionID     string `json:"session_id"`
	AgentID       string `json:"agent_id"`
	HookEventName string `json:"hook_event_name"`
	ToolName      string `json:"tool_name"`
}

type hookOutput struct {
	Specific struct {
		Event   string `json:"hookEventName"`
		Context string `json:"additionalContext"`
	} `json:"hookSpecificOutput"`
}

func shellQuote(value string) string {
	return "'" + strings.ReplaceAll(value, "'", "'\"'\"'") + "'"
}

func validIdentity(value string) bool {
	return strings.TrimSpace(value) != "" && len(value) <= 1024 && !strings.ContainsFunc(value, unicode.IsControl)
}

func normalizeOptions(options Options) (Options, error) {
	if options.Executable == "" {
		options.Executable = "taskctl"
	}
	if strings.ContainsAny(options.Executable, "\x00\n\r") {
		return options, errors.New("executable must be a single path or command name")
	}
	if options.EventLimit == 0 {
		options.EventLimit = 5
	}
	if options.EventLimit < 1 || options.EventLimit > 20 {
		return options, errors.New("Codex event limit must be between 1 and 20")
	}
	if options.Lease == 0 {
		options.Lease = 5 * time.Minute
	}
	if options.Lease <= 0 || options.Lease > 24*time.Hour {
		return options, errors.New("Codex delivery lease must be positive and no longer than 24 hours")
	}
	if options.Inactivity == 0 {
		options.Inactivity = 30 * time.Minute
	}
	if options.Inactivity < 0 {
		return options, errors.New("Codex inactivity age must be positive")
	}
	if options.MaxContextBytes == 0 {
		options.MaxContextBytes = 6000
	}
	if options.MaxContextBytes < 512 || options.MaxContextBytes > 12000 {
		return options, errors.New("Codex context limit must be between 512 and 12000 bytes")
	}
	return options, nil
}

func Handle(s *store.Store, input io.Reader, output io.Writer, options Options) error {
	options, err := normalizeOptions(options)
	if err != nil {
		return err
	}
	limited := &io.LimitedReader{R: input, N: 16*1024*1024 + 1}
	decoder := json.NewDecoder(limited)
	var hook hookInput
	if err := decoder.Decode(&hook); err != nil {
		return fmt.Errorf("decode Codex hook: %w", err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		return errors.New("Codex hook input must contain exactly one JSON object")
	}
	if limited.N == 0 {
		return errors.New("Codex hook input exceeds 16 MiB")
	}
	if !validIdentity(hook.SessionID) || (hook.AgentID != "" && !validIdentity(hook.AgentID)) {
		return errors.New("Codex hook requires a valid session_id and, when present, agent_id")
	}
	if hook.HookEventName != "PostToolUse" && hook.HookEventName != "SessionStart" {
		return errors.New("supported Codex hook events are PostToolUse and SessionStart")
	}
	if hook.HookEventName == "PostToolUse" && strings.TrimSpace(hook.ToolName) == "" {
		return errors.New("PostToolUse requires tool_name")
	}
	owner := hook.SessionID
	if hook.AgentID != "" {
		owner = hook.AgentID
	}
	if _, err := s.TouchOwned(owner); err != nil {
		return err
	}
	if _, err := s.DetectInactivity(options.Inactivity); err != nil {
		return err
	}
	subscriptions, err := s.HookSubscriptions(owner, hook.HookEventName == "SessionStart", 6)
	if err != nil {
		return err
	}
	var claims []store.Delivery
	written := false
	defer func() {
		if !written {
			for _, delivery := range claims {
				_ = s.ReleaseDelivery(delivery.Subscription.ID, delivery.Token)
			}
		}
	}()
	var context strings.Builder
	remaining := options.EventLimit
	groups := 0
	truncated := false
	for _, subscription := range subscriptions {
		if remaining == 0 || groups == 5 {
			truncated = true
			break
		}
		delivery, err := s.OfferHookSummary(subscription.ID, owner, remaining, options.Lease)
		if err != nil {
			if store.ErrorCode(err) == "not_found" {
				continue
			}
			if store.ErrorCode(err) != "delivery_busy" {
				return err
			}
			if hook.HookEventName != "SessionStart" {
				continue
			}
			delivery = store.Delivery{Subscription: subscription}
		}
		var group string
		var resumed *store.Snapshot
		if hook.HookEventName == "SessionStart" {
			snapshot, err := s.SnapshotSummary(subscription.TaskID)
			if err != nil {
				if delivery.Token != "" {
					_ = s.ReleaseDelivery(subscription.ID, delivery.Token)
				}
				return err
			}
			resumed = &snapshot
			delivery.Snapshot = resumed
			delivery.LatestProgress = nil
		}
		if delivery.Token == "" {
			if resumed == nil {
				continue
			}
			group = formatSnapshot(*resumed)
		} else {
			group = formatDelivery(delivery, options.Executable, s.ProjectID, owner)
		}
		if context.Len()+len(group) > options.MaxContextBytes {
			if delivery.Token == "" {
				group = formatCompactSnapshot(*resumed)
			} else {
				group = formatCompactDelivery(delivery, options.Executable, s.ProjectID, owner)
				for context.Len() == 0 && len(group) > options.MaxContextBytes && len(delivery.Events) > 1 {
					if err := s.ReleaseDelivery(subscription.ID, delivery.Token); err != nil {
						return err
					}
					delivery, err = s.OfferHookSummary(subscription.ID, owner, max(1, len(delivery.Events)/2), options.Lease)
					if err != nil {
						if store.ErrorCode(err) == "delivery_busy" || store.ErrorCode(err) == "not_found" {
							break
						}
						return err
					}
					if resumed != nil {
						delivery.Snapshot = resumed
						delivery.LatestProgress = nil
					}
					group = formatCompactDelivery(delivery, options.Executable, s.ProjectID, owner)
				}
				if err != nil || delivery.Token == "" {
					continue
				}
			}
		}
		if context.Len()+len(group) > options.MaxContextBytes {
			if delivery.Token != "" {
				if err := s.ReleaseDelivery(subscription.ID, delivery.Token); err != nil {
					return err
				}
			}
			if context.Len() == 0 {
				return errors.New("notification cannot fit the configured context limit; increase max-context-bytes")
			}
			truncated = true
			break
		}
		context.WriteString(group)
		groups++
		if delivery.Token != "" {
			claims = append(claims, delivery)
			remaining -= len(delivery.Events)
		}
	}
	if context.Len() == 0 {
		return nil
	}
	footer := "Additional subscription state is omitted from this summary; pending deliveries remain in the database.\n"
	if truncated && context.Len()+len(footer) <= options.MaxContextBytes {
		context.WriteString(footer)
	}
	var result hookOutput
	result.Specific.Event = hook.HookEventName
	result.Specific.Context = context.String()
	if err := json.NewEncoder(output).Encode(result); err != nil {
		return err
	}
	written = true
	return nil
}

func shortText(text string, limit int) string {
	text = strings.Join(strings.FieldsFunc(text, func(r rune) bool { return unicode.IsSpace(r) || unicode.IsControl(r) }), " ")
	if len(text) <= limit {
		return text
	}
	end := limit - len("…")
	for end > 0 && !utf8.RuneStart(text[end]) {
		end--
	}
	return text[:end] + "…"
}

func formatSnapshot(snapshot store.Snapshot) string {
	var out strings.Builder
	fmt.Fprintf(&out, "Current %s: %s", snapshot.TaskID, snapshot.Outcome)
	if snapshot.Available {
		out.WriteString("; work is available")
	}
	out.WriteString(".\n")
	shown := 0
	for _, task := range snapshot.Tasks {
		if shown == 3 {
			break
		}
		fmt.Fprintf(&out, "- %s %s %q", task.ID, task.Status, shortText(task.Title, 120))
		if task.Available {
			out.WriteString(" (available)")
		}
		if task.Owner != "" {
			fmt.Fprintf(&out, " (owner %q)", shortText(task.Owner, 64))
		}
		if task.Progress != "" {
			fmt.Fprintf(&out, "; latest note: %q", shortText(task.Progress, 480))
		}
		if (task.Status == "completed" || task.Status == "cancelled") && task.Result != "" {
			fmt.Fprintf(&out, "; result: %q", shortText(task.Result, 160))
		}
		for _, blocker := range task.ResourceBlockers {
			fmt.Fprintf(&out, "; waiting for resource %q held by %s", shortText(blocker.Resource, 120), blocker.Holder)
			if blocker.LastActivityAt != "" {
				fmt.Fprintf(&out, " (holder last active %s)", blocker.LastActivityAt)
			}
		}
		out.WriteByte('\n')
		shown++
	}
	if omitted := snapshot.Omitted + len(snapshot.Tasks) - shown; omitted > 0 {
		fmt.Fprintf(&out, "- %d further branches; read the task for details.\n", omitted)
	}
	if snapshot.Truncated {
		out.WriteString("Task text is shortened; use tasks show ID for complete details.\n")
	}
	return out.String()
}

func formatDelivery(delivery store.Delivery, executable, project, owner string) string {
	var out strings.Builder
	fmt.Fprintf(&out, "Task notifications for %s (%s):\n", delivery.Subscription.TaskID, delivery.Subscription.ID)
	if delivery.Snapshot != nil {
		out.WriteString(formatSnapshot(*delivery.Snapshot))
	}
	shortened := false
	for _, event := range delivery.Events {
		shortened = shortened || event.Truncated
		fmt.Fprintf(&out, "- E%d %s %s: %q\n", event.ID, event.TaskID, event.Kind, shortText(event.Message, 240))
	}
	for _, event := range delivery.LatestProgress {
		shown := false
		if delivery.Snapshot != nil {
			for _, task := range delivery.Snapshot.Tasks[:min(3, len(delivery.Snapshot.Tasks))] {
				shown = shown || task.ID == event.TaskID
			}
		}
		if !shown {
			fmt.Fprintf(&out, "- %s latest note (E%d): %q\n", event.TaskID, event.ID, shortText(event.Message, 480))
		}
	}
	if shortened {
		fmt.Fprintf(&out, "Event text is shortened; read full events with tasks watch %s --after %d --once --json.\n", delivery.Subscription.TaskID, delivery.Subscription.Cursor)
	}
	if delivery.More {
		out.WriteString("More events are pending; this acknowledgement covers only the displayed page.\n")
	}
	out.WriteString(acknowledgementCommand(delivery, executable, project, owner))
	return out.String()
}

func acknowledgementCommand(delivery store.Delivery, executable, project, owner string) string {
	return "After reading, acknowledge this delivery:\n" + shellQuote(executable) + " --project " + shellQuote(project) + " subscriptions acknowledge " + shellQuote(delivery.Subscription.ID) +
		" --session " + shellQuote(owner) + " --token " + shellQuote(delivery.Token) + " --through " + strconv.FormatInt(delivery.Through, 10) + "\n"
}

func formatCompactSnapshot(snapshot store.Snapshot) string {
	var out strings.Builder
	fmt.Fprintf(&out, "Current %s: %s; available=%t.\n", snapshot.TaskID, snapshot.Outcome, snapshot.Available)
	shown := min(3, len(snapshot.Tasks))
	for _, task := range snapshot.Tasks[:shown] {
		fmt.Fprintf(&out, "- %s %s\n", task.ID, task.Status)
	}
	if omitted := snapshot.Omitted + len(snapshot.Tasks) - shown; omitted > 0 {
		fmt.Fprintf(&out, "%d further branches.\n", omitted)
	}
	out.WriteString("Details omitted; use tasks show ID.\n")
	return out.String()
}

func formatCompactDelivery(delivery store.Delivery, executable, project, owner string) string {
	var out strings.Builder
	fmt.Fprintf(&out, "Updates for %s:\n", delivery.Subscription.TaskID)
	if delivery.Snapshot != nil {
		out.WriteString(formatCompactSnapshot(*delivery.Snapshot))
	}
	for _, event := range delivery.Events {
		fmt.Fprintf(&out, "E%d %s %s\n", event.ID, event.TaskID, event.Kind)
	}
	if delivery.Snapshot == nil {
		out.WriteString("Details omitted; use tasks show ID.\n")
	}
	if delivery.More {
		out.WriteString("More events remain pending.\n")
	}
	out.WriteString(acknowledgementCommand(delivery, executable, project, owner))
	return out.String()
}

func Config(executable, projectID string) ([]byte, error) {
	if !filepath.IsAbs(executable) || strings.ContainsAny(executable, "\x00\n\r") {
		return nil, errors.New("Codex config requires an absolute executable path")
	}
	if !validIdentity(projectID) {
		return nil, errors.New("Codex config requires a valid project ID")
	}
	command := shellQuote(executable) + " --project " + shellQuote(projectID) + " integrations codex hook"
	type commandHook struct {
		Type    string `json:"type"`
		Command string `json:"command"`
		Async   bool   `json:"async"`
		Timeout int    `json:"timeout"`
	}
	type matcher struct {
		Hooks []commandHook `json:"hooks"`
	}
	config := struct {
		Hooks map[string][]matcher `json:"hooks"`
	}{Hooks: map[string][]matcher{}}
	for _, event := range []string{"SessionStart", "PostToolUse"} {
		config.Hooks[event] = []matcher{{Hooks: []commandHook{{Type: "command", Command: command, Async: true, Timeout: 10}}}}
	}
	return json.MarshalIndent(config, "", "  ")
}
