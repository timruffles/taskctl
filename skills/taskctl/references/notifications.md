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

For another harness, `subscriptions watch SUB --session SESSION --exec /absolute/program` sends the delivery as JSON on that executable's stdin. A successful callback is automatically acknowledged; acknowledge only once, through either the callback watcher or the consumer. `--exec` takes an executable path, not a shell command line. Arrange for callback success to mean its handoff is durable.

For a human's terminal or an intentionally blocking orchestration step, use `tasks watch T1` or `tasks wait T1 --timeout 10m`. `tasks watch T1 --once --json` reads one current snapshot. Pass `--after EVENT_ID --once` to read an event page instead. `wait` returns exit 0 for completed work, 4 for cancellation, and 5 for timeout.
