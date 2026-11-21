# taskctl

A local Go CLI for a small group of coding agents. SQLite owns task state, claims, events and subscriptions. There is no server and no Git dependency.

The default view is the work remaining now. A substantial change replaces a task with fresh, self-contained tasks and marks the original `superseded`. History remains discoverable through `parent_id` and `origin_id`.

## Build

Go 1.26 or newer; Linux and macOS. SQLite is embedded through `modernc.org/sqlite`; no C toolchain or SQLite installation is required.

```sh
go build -buildvcs=false -trimpath -o bin/taskctl ./cmd/taskctl
go test ./...
```

Put `taskctl` on `PATH` for the examples and for commands included in queue wake messages. The binary reports `0.2.0`. Existing schema-version-1 SQLite projects remain compatible; missing additive indexes are installed on open.

## Start a project

```sh
taskctl projects create --name my-project --path "$PWD"
taskctl tasks create --title 'Implement parser' --brief-file parser-task.md --tag parser
taskctl tasks available
taskctl tasks claim T1 --session agent-1 --json
```

A successful claim returns a `claim_id`. Use that exact token for subsequent owner operations:

```sh
taskctl tasks progress T1 --claim CLAIM --text 'Tokenizer complete; error cases remain'
taskctl tasks heartbeat T1 --claim CLAIM
taskctl tasks complete T1 --claim CLAIM --summary 'Parser and error-case tests pass'
```

`claim` atomically changes a ready pending task to `in_progress`. A concurrent claimant loses with exit 3. All owner mutations check the current token. Repeating `claim` is a conflict, including for the same session. Read `tasks show` if a command's output was lost.

`release` returns claimed work to pending. `takeover` requires the exact previous claim, a new session and a reason. Inactivity never silently releases ownership. A lease expiring only releases a **notification delivery claim**, never a task claim.

For creation that may be retried after a lost command response, provide a stable request ID:

```sh
taskctl tasks create --title 'Implement parser' --brief-file parser-task.md --request-id parser-v1 --json
```

Repeating that key with equivalent input returns the same task without creating another task or event. Reusing the key with different input is a conflict. Keys are project-local, persist across process restarts, and are not preserved when restoring into a new project. Creation without a request ID creates a new task on every successful call. If another mutation loses its response, inspect current state before retrying; a failed transport does not prove the transaction failed.

## Keep the current work crisp

When a significant chunk is finished, record what changed and replace the task with the actual remaining work. Do not keep extending a stale brief. The following is an alternative to completing T1: start from a still-claimed, unfinished task. A completed task cannot be superseded.

```json
{
  "summary": "Tokenizer implemented and tested in parser/tokenize.go. Error handling and integration remain.",
  "replacements": [
    {
      "key": "errors",
      "title": "Handle malformed tokens",
      "brief": "Tokenizer is complete in parser/tokenize.go. Add typed errors for unterminated strings and invalid escapes; add table tests. The public token API stays unchanged."
    },
    {
      "key": "integration",
      "title": "Integrate the tokenizer",
      "brief": "Use the tokenizer from parser/tokenize.go in Parse. Preserve typed errors and verify the existing parser fixtures.",
      "prerequisites": ["@errors"]
    }
  ],
  "continue_with": "errors"
}
```

```sh
taskctl tasks show T1 --json
taskctl tasks supersede T1 --claim CLAIM --revision REVISION --file remaining.json
taskctl tasks in-progress
taskctl tasks available
taskctl tasks history T1
```

Supersession, replacement creation, inherited references and events commit together. The old task becomes immutable. Each replacement needs a brief that stands on its own. Tags, metadata, prerequisites and scratch references are inherited. Additional replacement prerequisites can refer to new siblings with `@key`.

`continue_with` is optional. It transfers the current owner to one ready replacement and returns a **new** claim token. Without it every replacement is pending. An old token cannot mutate a successor.

A prerequisite or watch on an original task follows all its replacement branches. The original succeeds only when every branch completes. Cancellation is a distinct outcome; other unfinished branches remain visible. A dependent task becomes available only after all prerequisite outcomes are completed. Cycles across both prerequisites and replacements are rejected atomically.

## Cheap discovery, then detail

```sh
taskctl tasks available --tag parser --limit 10 --json
taskctl tasks in-progress --json
taskctl tasks blocked
taskctl tasks list --status all --limit 20 --offset 20
taskctl tasks show T3 --json
taskctl tasks history T3 --limit 20 --offset 0 --json
taskctl tasks subscriptions --session coordinator --limit 20 --json
taskctl scratch list T3 --limit 20 --json
```

Task lists default to 20 rows, cap at 200, and report `total`, `omitted` and `next_offset`. They exclude briefs, metadata, results, progress text and claim tokens. In-progress rows include owner and last activity.

History, subscription and scratch lists also default to 20 rows and cap at 200. Their JSON envelopes contain `tasks`, `subscriptions` or `areas`, plus `more` and `next_offset`. When `more` is true, pass that offset to the next call. History pages include the original lineage's definitions, links, progress and results.

`show` includes the current brief, latest progress, result and metadata. Its scratch references are limited to one page, with `scratch_more` and `scratch_next_offset`; continue through `scratch list ID --offset N`. These pagination envelopes replace the unbounded arrays returned before 0.2.0.

Tags are plain strings. Repeated `--tag` filters mean AND. Lifecycle state and ownership are separate fields.

```sh
taskctl tasks tag T3 --tag tests
taskctl tasks metadata T3 --key worktree --value /absolute/worktree
taskctl tasks metadata T3 --key agent_session_id --value SESSION
taskctl tasks depend T3 --prerequisite T2
```

Pass `--claim` when modifying in-progress work. `edit` and `supersede` also require the revision read from the task. A small correction can use `edit`; substantial completed work belongs in a supersession summary and a fresh remaining brief.

## Watch and subscribe

Foreground watches stream an initial current snapshot and subsequent events, following replacements:

```sh
taskctl tasks watch T1 --json
taskctl tasks watch T1 --once --json
taskctl tasks watch T1 --after 42 --exec /absolute/handler --timeout 30m
taskctl tasks wait T1 --timeout 30m
```

`watch --once` without `--after` returns one current snapshot. With `--after`, it performs one event-page check; resume from the last emitted cursor to read further pages. Full event pages target at most 1 MiB of stored event payload as well as their event-count limit. One oversized event is allowed so it can be delivered without blocking the cursor permanently. Snapshots contain at most 20 task summaries and report omitted rows.

A task-watch callback receives one snapshot or event envelope on stdin. It runs without a shell, with no arguments, a 30-second execution timeout, and no held database transaction. Handler stdout and stderr are forwarded to stderr. A failed callback stops the watcher before advancing its in-memory cursor. Resume with the last successfully processed event ID. `wait` exits 0 for completed, 4 for cancelled and 5 for timeout. Neither command assumes that silence means success.

Use a subscription when a cursor must survive a consumer restart:

```sh
taskctl tasks subscribe T1 --session coordinator --json
taskctl subscriptions pending SUB --session coordinator --json
taskctl subscriptions acknowledge SUB --session coordinator --token TOKEN --through EVENT_ID
taskctl subscriptions watch SUB --session coordinator --exec /absolute/handler
```

Subscriptions, event cursors, offered ranges and delivery tokens live in SQLite. Only one consumer holds a subscription's delivery lease at a time (default five minutes). An explicit ACK advances the cursor. Expired or superseded delivery tokens cannot ACK another consumer's batch. A subscription callback receives the complete Delivery batch on stdin, including its event array and any initial snapshot, rather than a single task-watch event. It has the same 30-second timeout and ACKs only after exit 0. Delivery is at least once: make external callback effects idempotent using the event ID. No claim of exactly-once external effects is made.

New subscriptions include an ACK-required current snapshot, so subscribing to already completed work still produces a notification. `pending` prints nothing when there is nothing to deliver. Progress updates stay available; bounded Codex summaries coalesce adjacent updates without skipping their event IDs.

An event cursor beyond the database's current high-water mark is rejected. After restoring a backup, recreate subscriptions in the new project rather than reusing an old database's cursor.

Watchers scan for inactivity once a second, using a default 30-minute activity threshold. Active Codex hooks also check after refreshing the executing owner’s activity. One inactivity event is emitted per claim/activity period. Heartbeats are silent and do not change the task revision. Inactivity is a reason to inspect the task, not proof that a process has died.

## Codex

```sh
taskctl integrations codex config > taskctl-hooks.json
```

This prints configuration for asynchronous `PostToolUse` and `SessionStart` command hooks with an absolute executable and explicit project. Merge it into your Codex hook configuration; the command does not modify your configuration. See the [official hooks documentation](https://learn.chatgpt.com/docs/hooks).

The adapter records activity using `agent_id` when present, otherwise `session_id`. Claim work using that same identity. With no pending changes, `PostToolUse` emits nothing. With changes, it returns bounded `additionalContext` and an explicit ACK command. `SessionStart` restores a compact current snapshot. Defaults are five events and 3,000 context bytes; `integrations codex hook` accepts `--limit`, `--lease` and `--max-context-bytes`.

Hooks and Codex queue wakes read bounded summaries directly from SQLite: they limit text fields and omit unused event data before loading a batch. Large snapshots or progress text are compacted further to fit the context budget, with omitted detail explicitly indicated. Event identities and the exact acknowledgement range remain available. Explicit event reads and callback deliveries retain full event content. If a delivery token expires before the agent acknowledges it, request a fresh batch with `subscriptions pending`; do not treat an old token as authority over a newer offer.

Asynchronous hooks deliver context at active-session safe points; they do not themselves resume an idle session. Idle resumption is separate and opt-in:

```sh
taskctl tasks subscribe T1 --session CODEX_THREAD_ID --resume --json
taskctl subscriptions watch SUB --session CODEX_THREAD_ID --codex
```

Run this watcher outside the agent's blocking tool call, for example under a user service. It uses `codex queue --thread ... --message ... --resume`; see [Codex developer commands](https://learn.chatgpt.com/docs/developer-commands?surface=cli). Progress alone never queues a turn. Completion, cancellation, newly available work and inactivity can queue a wake. The queue cursor is independent of the acknowledgement cursor, so unacknowledged progress cannot hide later completion.

Queue acceptance records transport progress; it does not acknowledge context. The wake instructs the agent to read pending updates and ACK them. The adapter does not bypass Codex's pause or interruption handling. Automatic delivery depends on the installed Codex version and its hook/queue support. The V1 test report distinguishes fake-transport tests from the real Luna CLI trial.

## Scratch and storage

```sh
taskctl scratch create T3 --claim CLAIM --json
taskctl scratch list T3
taskctl scratch attach T4 --area AREA
taskctl scratch cleanup --dry-run
taskctl scratch cleanup
```

Scratch is out of the repository, tool-owned and disposable. Important output goes in the repository, with a reference in the task. A scratch area remains while any pending or in-progress task references it. Replacement tasks inherit references. After its last current reference disappears, the default grace is seven days. Cleanup only acts on registered tool-owned areas; metadata paths and attached worktrees are never deletion targets. Automatic cleanup is bounded and runs after selected task mutations; large areas can require multiple passes or explicit cleanup calls. Mutations refresh only the affected scratch references in the same transaction. Cleanup selects indexed eligible areas in bounded batches, without refreshing every area first.

| Data | Location |
| --- | --- |
| Database and project manifest | `$XDG_DATA_HOME/taskctl/projects/PROJECT/` |
| Disposable scratch | `$XDG_STATE_HOME/taskctl/scratch/PROJECT/` |
| Optional defaults | `$XDG_CONFIG_HOME/taskctl/config.json` |
| Repository/worktree selector | `.taskctl.json` containing the project ID |

XDG defaults are `~/.local/share`, `~/.local/state` and `~/.config`. Project selection is `--project`, then `TASKCTL_PROJECT`, then the nearest ancestor `.taskctl.json`. Attach every worktree to the same project ID with `projects attach`. The marker contains an ID, not an absolute database path. Use a local filesystem; network filesystems and multi-host coordination are outside V1.

Normal commands open an existing, validated database. Missing or empty storage fails with `storage_missing` or `storage_invalid`; it cannot silently become an empty backlog. Full-disk, I/O and read-only failures retain distinct operational error codes. Only explicit project creation or restore initializes a database.

Optional configuration:

```json
{"inactivity":"30m","scratch_grace":"168h","list_limit":20}
```

## SQLite backup and restore

```sh
taskctl backup --file tasks-2026-11-21.sqlite
taskctl restore --file tasks-2026-11-21.sqlite --name recovered-project
```

`backup` creates a consistent live SQLite snapshot using `VACUUM INTO`. It syncs the completed staging file and publishes it atomically, refusing an existing destination. It includes durable task/history data and subscription state, but omits scratch file contents. The resulting SQLite file can be stored in Git or another backup system. Do not copy only the live main database file while WAL writes may be outstanding.

`restore` accepts a SQLite backup and creates a **new** project. It holds one read-only source snapshot and validates and inserts one row at a time into the application's own schema. Schema, field, reference, lineage and dependency-graph validation must all pass before the destination transaction commits. Source triggers and other schema objects are not imported. A failed restore rolls back its changes.

Restore releases old active claims, clears subscriptions and creation request receipts, and preserves scratch-reference history marked unavailable. Recreate subscriptions and attach the new project to the intended worktree. There is no restore-over-live-database operation.

JSON archive export and restore were removed in 0.2.0. Command output with `--json` and supersession-plan JSON remain supported.

## Data size and memory

Ordinary operations query the affected tasks, prerequisites or watched scope. Recording progress does not scan historical task definitions. Availability events are written with the task mutation, and completing a replacement still notifies dependents of its ancestors.

Pure reads use read snapshots without reserving the SQLite writer lock. Opening an up-to-date database skips the schema write transaction. Availability and graph checks use indexed SQL and disk-backed temporary storage where needed.

Pages and restore buffers avoid loading an entire history or database into Go objects. Memory still depends on the requested page, its largest records, and SQLite's working sets; a single task's full brief, metadata or event payload can be large. Exact list counts and requests covering large replacement trees still require work proportional to the relevant data. See `docs/PERFORMANCE.md` for measured scaling and limits.

## Command rules

- Exact operation names and flags; no abbreviation, implicit next-task claim or overloaded status-setting command.
- Flags may appear after task IDs. Unknown flags and unknown supersession JSON fields fail.
- `--json` writes machine data to stdout and structured errors to stderr. Empty pending/hook results produce zero bytes.
- Exit codes: 0 success; 1 operational failure; 2 invalid usage/input; 3 conflict or busy; 4 cancelled watched work; 5 timeout; 130 interrupted watch.
- Tasks use `T1` IDs within an explicitly selected project. Subscriptions and scratch areas have random IDs.
- Task changes and their events share short SQLite transactions. WAL, foreign keys, a five-second busy timeout and full synchronous commits are enabled. No database transaction spans waiting or an external callback.

See `docs/DESIGN.md` and the ADRs for the reasoning. `docs/VALIDATION.md` records the initial V1 verification. `docs/CHAOS_VALIDATION.md` records the recovery tests, defects found, and hooked Luna chaos trial.
