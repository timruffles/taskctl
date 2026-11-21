# Receive task updates

Use durable subscriptions for agent notifications. A subscription belongs to its named session and follows the watched task through replacements. The agent runtime launches workers and delivers context; taskctl records state and produces updates.

## Subscribe, consume, acknowledge

```sh
taskctl tasks subscribe T1 --session SESSION --json
taskctl subscriptions pending SUB --session SESSION --limit 5 --json
```

Use the returned subscription ID as `SUB`. A new subscription offers a current snapshot; later offers include changes. Empty output means no offer. Read a nonempty delivery's snapshot and events, then acknowledge its exact `token` and `through`:

```sh
taskctl subscriptions acknowledge SUB --session SESSION --token TOKEN --through EVENT_ID
```

Record any required follow-up before acknowledging. If `more` is true, acknowledge this batch and request the next. A page may stop at its byte budget before its event-count limit. Use only the offered range, never an independently observed latest event ID.

Acknowledgement commits consumption. Delivery is at least once: replays can occur after a crash or an expired lease. Make external side effects idempotent by event ID. A delivery lease expiring does not release a task's ownership. If an old token fails, obtain a fresh offer before acknowledging.

Recover subscription IDs with:

```sh
taskctl tasks subscriptions --session SESSION --json
```

## Active Codex sessions

Generate the adapter configuration from the selected project:

```sh
taskctl integrations codex config
```

This prints asynchronous `PostToolUse` and `SessionStart` configuration; it does not install it. Merge the output into the installed Codex version's hook configuration using its [hook documentation](https://learn.chatgpt.com/docs/hooks). Preserve existing hooks.

The adapter selects `agent_id` when supplied, otherwise `session_id`. Use the same identity for task claims and subscriptions. Hook activity refreshes owned tickets. Quiet `PostToolUse` calls produce no output; pending changes add bounded context with an exact acknowledgement command. Process that context, then run its acknowledgement. `SessionStart` can restore a current snapshot.

An active hook reaches session safe points; it cannot by itself resume an idle session. Hook and queue support depend on the installed Codex version. The project's adapter and harness tests do not establish that a particular native Codex installation delivers hooks correctly; verify it with a disposable task before relying on it.

## Idle sessions and external watchers

For a Codex version with `codex queue`, use its thread ID as the recipient session:

```sh
taskctl tasks subscribe T1 --session CODEX_THREAD_ID --resume --json
taskctl subscriptions watch SUB --session CODEX_THREAD_ID --codex
```

Run the long-lived watcher under the harness or a user service, outside the agent's blocking command. Check the installed version's [queue documentation](https://learn.chatgpt.com/docs/developer-commands?surface=cli). Progress alone does not request a resumed turn; completion, cancellation, available work and inactivity can. Queue acceptance advances transport state, while the agent still needs to consume and acknowledge the pending delivery.

For another harness, `subscriptions watch SUB --session SESSION --exec /absolute/program` sends the delivery as JSON on that executable's stdin. A successful callback is automatically acknowledged; acknowledge only once, through either the callback watcher or the consumer. `--exec` takes an executable path, not a shell command line. Arrange for callback success to mean its handoff is durable.

For a human's terminal or an intentionally blocking orchestration step, use `tasks watch T1` or `tasks wait T1 --timeout 10m`. `tasks watch T1 --once --json` reads one current snapshot. Pass `--after EVENT_ID --once` to read an event page instead. `wait` returns exit 0 for completed work, 4 for cancellation, and 5 for timeout.

Watchers and active hooks detect inactivity, with a default 30-minute threshold. One event is emitted per claim/activity period. Heartbeats and ordinary activity keep a task quiet; silence or an inactivity event alone does not prove an agent has died. Use the recovery reference before takeover.
