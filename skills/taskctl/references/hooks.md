# Configure hooks and idle wakes

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

Watchers and active hooks detect inactivity, with a default 30-minute threshold. One event is emitted per claim/activity period. Heartbeats and ordinary activity keep a task quiet; silence or an inactivity event alone does not prove an agent has died. Read [recovery](recovery.md) before takeover.
