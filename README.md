# taskctl

A local task CLI for a small group of coding agents. Agents claim work, record useful progress and replace stale task definitions with the work that actually remains. You use the same CLI to see what is running, what can start and what is blocked.

One Go binary. `tasks.sqlite` stores tasks, ownership, note titles and notification cursors; a separate `notes.sqlite` holds long Markdown bodies. Storage lives outside the repository, so agents in separate worktrees can share a project. Git is optional.

## Install

Download the archive for your platform from [GitHub Releases](https://github.com/timruffles/taskctl/releases), check it against `SHA256SUMS` and extract `taskctl` into a directory on your `PATH`. Releases include Linux and macOS binaries for both amd64 and arm64 plus a matching source archive with vendored dependencies.

Build from this checkout with Go 1.26 or newer:

```sh
mkdir -p "$HOME/.local/bin"
go build -buildvcs=false -trimpath -o "$HOME/.local/bin/taskctl" ./cmd/taskctl
export PATH="$HOME/.local/bin:$PATH"
taskctl version
```

Linux and macOS are supported. SQLite is embedded; running taskctl needs neither a SQLite installation nor a C toolchain.

Passing `main` builds also publish all four binary archives in **Actions → CI**. These development artifacts are retained for 14 days.

## Use it with agents

### 1. Create one project for the work

From the repository your agents will work on:

```sh
taskctl projects create --name csv-import --path "$PWD"
taskctl projects show
```

The first command creates a database and writes a `.taskctl.json` selector in this directory. Keep that local selector out of Git. Later commands find it from the current directory or an ancestor.

Create a few concrete tasks. These examples assume a new project, where the returned IDs are `T1`, `T2` and `T3`:

```sh
taskctl tasks create --title 'Read CSV input' --tag import \
  --brief 'Implement CSV reading in package input. Return records with named fields and useful errors for malformed rows. Add parser tests.'
taskctl tasks create --title 'Add import command' --tag import \
  --brief 'Add the import subcommand and --help text. Accept an input file path and report usage errors. Keep CSV parsing in package input.'
taskctl tasks create --title 'Verify CSV import end to end' --tag import \
  --brief 'Connect the import command to package input. Test a valid file, malformed CSV and a missing path; document one working invocation.' \
  --prerequisite T1 --prerequisite T2
taskctl tasks available
```

Two agents can start `T1` and `T2`. `T3` becomes available after both prerequisites succeed. A task brief should say what remains, where to work and what evidence will establish completion.

For separate worktrees, attach each existing directory to the **same project ID** returned by `projects show`:

```sh
taskctl projects attach --project PROJECT_ID --path /absolute/path/to/agent-worktree
```

You can also select a project explicitly with `taskctl --project PROJECT_ID ...` or `TASKCTL_PROJECT`. Selection order is the flag, environment variable, then directory selector.

### 2. Give agents the taskctl skill

Use [skills/taskctl/SKILL.md](skills/taskctl/SKILL.md) as the agent entry point. Install the entire `skills/taskctl/` directory, including its references, in your agent's project skill directory. Alternatively, give the agent the absolute path to `SKILL.md` and ask it to read that file.

A worker prompt can be this small:

> Read the taskctl skill at `/absolute/path/to/taskctl/skills/taskctl/SKILL.md`. Use project `PROJECT_ID`. Pick one available task tagged `import`, claim it using your session identity, and implement it in your assigned worktree. Keep its current progress and remaining work accurate. Complete it with the checks you actually ran.

taskctl coordinates task ownership; your agent harness starts agents and gives them worktrees or file boundaries. A database claim does not prevent two processes editing the same files.

### 3. Let each worker claim its own task

The agent starts with a short list and loads the chosen task's details:

```sh
taskctl tasks available --tag import --limit 10 --json
taskctl tasks show T1 --json
taskctl tasks claim T1 --session AGENT_SESSION_ID --json
```

A successful claim returns a `claim_id`. Use that exact value wherever `CLAIM_TOKEN` appears below. Concurrent claims are safe: one succeeds and the other receives a conflict. A list is only a snapshot; successful `claim` establishes ownership.

With Codex hooks, the session identity must match the hook's `agent_id` when present, otherwise its `session_id`. With another harness, assign each worker a distinct identity and use it consistently.

Agents can attach useful references without putting them in every list:

```sh
taskctl tasks metadata T1 --claim CLAIM_TOKEN --key worktree --value /absolute/agent-worktree
taskctl tasks metadata T1 --claim CLAIM_TOKEN --key agent_session_id --value AGENT_SESSION_ID
```

## See what remains

Start with these commands while agents work:

| What you want to know | Command |
| --- | --- |
| All unfinished current tasks | `taskctl tasks list` |
| Work an agent can claim now | `taskctl tasks available` |
| Work currently claimed, its owner and last activity | `taskctl tasks in-progress` |
| Pending work waiting on prerequisites | `taskctl tasks blocked` |
| Current work in one area | `taskctl tasks list --tag import` |
| A task's brief, latest progress, prerequisites and result | `taskctl tasks show T1` |
| Recent progress-note titles | `taskctl notes list T1 --limit 10` |
| One note's full Markdown body | `taskctl notes read NOTE_ID` |
| Original and replacement definitions | `taskctl tasks history T1` |
| A snapshot of an original task's overall outcome | `taskctl tasks watch T1 --once` |
| Completed work | `taskctl tasks list --status completed` |

The default list contains pending and in-progress tasks. Completed, cancelled and evolved definitions stay in history. The `ORIGIN` column points back to the top-level original; `show` also names the immediate predecessor. You can inspect old intent without making it the default view.

Lists default to 20 rows and print a continuation command when more remain:

```sh
taskctl tasks list --tag import --limit 20 --offset 20
taskctl tasks show T1 --json
```

`--json` gives agents and scripts structured output. Task lists include `total`, `omitted` and `next_offset`; `total` counts all matching tasks, including those beyond the current page. History, subscription and scratch lists use `more` and `next_offset`. Notes are newest first, with `more` and `next_before`; pass that event ID as `--before` for older entries. Lists cap at 200 rows and omit large briefs and note bodies. `notes read` streams raw Markdown and does not accept `--json`.

Repeated tags mean AND: `--tag import --tag tests` selects tasks carrying both tags. Repeat `--exclude-tag` to omit tasks carrying any excluded tag. For example, `taskctl tasks available --exclude-tag human --exclude-tag epic` shows the agent queue. Tag filters apply before pagination. Add a tag with `tasks tag ID --tag TAG`, supplying `--claim` if the task is in progress.

Use `--where` on `tasks list`, `available`, `blocked` or `in-progress` for a [CEL](https://cel.dev/) expression that returns a boolean. Status and tag flags narrow the candidates first. CEL filters those candidates before pagination, including the reported total.

```sh
taskctl tasks available --where 'search(title, "advisor") && !("human" in tags)'
taskctl tasks list --status all --where '"beads_id" in metadata && metadata["beads_id"] == "asc-42"'
```

- `id`, `title`, `brief`, `status`, `owner`, `origin_id` and `parent_id` are strings. A missing owner or parent is an empty string.
- `tags` and `prerequisites` are lists of strings. Prerequisites are task IDs.
- `metadata` maps string keys to string values. Use `"key" in metadata` before reading an optional key.
- `search(text, query)` matches a case-insensitive literal substring. It accepts either `title` or `brief`, or any other string expression.

Expressions can inspect briefs and metadata while the list output stays compact. Progress note bodies are loaded separately with `notes read`.

## Keep progress and task definitions current

Record a checkpoint when something meaningful changes:

```sh
taskctl tasks progress T1 --claim CLAIM_TOKEN \
  --title 'Valid CSV rows parse; parser tests pass. Malformed-row errors remain.'
```

Titles must fit on one line and contain at most 120 characters. Longer titles are rejected. Add optional Markdown evidence, reproduction steps or investigation details with `--body-file FILE`; `--body-file -` streams from stdin. The new command returns the note's ID and metadata, never its body.

`tasks show` displays only the latest progress title and its note reference. Load more only when needed:

```sh
taskctl notes list T1 --limit 10
taskctl notes show NOTE_ID --json
taskctl notes read NOTE_ID
taskctl notes delete NOTE_ID --claim CLAIM_TOKEN
```

The note index, task views and notification events do not contain long bodies. Markdown bodies live in a separate `notes.sqlite` database, in indexed 64 KiB chunks. A read loads only the requested note, with bounded buffering; normal task operations do not read its chunks. Bodies are durable and are not scratch-cleanup targets. Important decisions and acceptance conditions belong in the current task brief or the repository, so they remain visible after a later checkpoint replaces the title.

Deleting a note removes its title and body while keeping a small deletion record for historical references. Supply the current task claim while the task is in progress. Deleting the current note clears the latest progress display; an older title is not substituted. Freed body pages can be reused, but deletion does not promise to shrink the database file. There is no compaction command.

Existing `progress --text` and `--text-file` invocations still work. Input that is already a valid title becomes a title-only note; other text is preserved as a body with a derived title. Legacy `--text-file` retains its 1 MiB input limit; use `--title` and streaming `--body-file` for larger bodies. Evolution leaves old notes with the original task; carry essential facts into the handoff and successor briefs.

A heartbeat only refreshes activity and is silent by default:

```sh
taskctl tasks heartbeat T1 --claim CLAIM_TOKEN
```

Hooks can handle heartbeats. Agents still need to record what they achieved, what remains and any useful paths or decisions. Activity alone cannot tell you whether the work is advancing.

### Evolve a task as the remaining work changes

Use `tasks evolve` when substantial progress or a discovery makes the current brief stale. It retires that definition and creates **new task IDs** describing the remaining work, including newly discovered work. Preserve completed work, verification and useful repository paths in the summary. For the still-claimed `T1`, save this as `remaining.json`:

```json
{
  "summary": "CSV parsing is implemented in input/csv.go; valid-row tests pass. Malformed-row errors remain.",
  "replacements": [
    {
      "key": "errors",
      "title": "Report malformed CSV rows",
      "brief": "Extend input/csv.go to return errors with row numbers for malformed CSV and missing required fields. Preserve valid-row parsing. Add cases to input/csv_test.go and run the parser tests."
    }
  ],
  "continue_with": "errors"
}
```

Read the current revision, then use it with the current claim:

```sh
taskctl tasks show T1 --json
taskctl tasks evolve T1 --claim CLAIM_TOKEN --revision REVISION --file remaining.json
taskctl tasks in-progress
taskctl tasks available
```

The replacements become the current tasks; `T1`'s definition becomes immutable and remains discoverable as `evolved`. Its old claim cannot change either definition. `continue_with` names a ready replacement by its unique plan key, claims it for the same worker and returns a **new claim token**. Use the returned task ID and token from then on. Omit `continue_with` to leave replacements pending for other agents.

Only the current claimer can evolve in-progress work; original authorship grants no extra authority. Pending tasks need the current revision but no claim. If the revision conflicts, reread the task and rebuild the plan with any intervening changes.

Tags, metadata, prerequisites and scratch references carry forward. Each new brief must stand on its own. A prerequisite or subscription on `T1` follows its replacements, so `T3` keeps waiting for the remaining CSV work. An original succeeds only when every replacement branch completes; its reported state remains `evolved`.

For a small correction, use `tasks edit ID --revision REVISION --brief-file FILE`, with `--claim` when owned. Editing keeps the same task ID. When work is finished, record the actual result and verification:

```sh
taskctl tasks complete TASK_ID --claim CLAIM_TOKEN --summary 'Implemented row-numbered errors; parser tests pass.'
```

### Recover interrupted work deliberately

Inspect `tasks in-progress`, then `tasks show ID`. If a worker is handing off unfinished work, record a checkpoint and release its claim:

```sh
taskctl tasks release TASK_ID --claim CLAIM_TOKEN
```

For a crashed worker, confirm its process has stopped or isolate its worktree before taking over:

```sh
taskctl tasks takeover TASK_ID --expected-claim OLD_CLAIM_TOKEN \
  --session REPLACEMENT_SESSION_ID --reason 'Previous worker exited; inspected its worktree and remaining changes.'
```

Takeover returns a new token. Old tokens can no longer change the task, but taskctl cannot stop an old process writing files. Inactivity never releases ownership automatically.

If you return to find another worker owns the task, stop working on it, read the current state and coordinate a handoff. The current owner can release it, then you can claim it with a fresh token. Copying their token would bypass that ownership handoff.

If a command's response is lost, read current state before retrying. For task creation that may be retried, supply a stable `--request-id`; equivalent input with the same key returns the existing task.

## Watch work and notify agents

For a human terminal or a script waiting on a result:

```sh
taskctl tasks watch T1
taskctl tasks wait T1 --timeout 30m
```

These follow replacements. `watch` streams updates; `wait` blocks until the overall outcome is known or the timeout expires. Exit status is 0 for completed, 4 for cancelled and 5 for timeout.

For notifications that survive consumer restarts, create a subscription:

```sh
taskctl tasks subscribe T1 --session coordinator --json
taskctl subscriptions pending SUBSCRIPTION_ID --session coordinator
```

`pending` includes the exact acknowledgement command. Read the batch, then acknowledge that token and event range. When nothing is pending it prints nothing. SQLite holds the cursor and exclusive delivery lease; expired deliveries can be offered again. Callback handlers can use `subscriptions watch SUBSCRIPTION_ID --session coordinator --exec /absolute/handler`, and should make effects idempotent by event ID.

Run long-lived watchers in a separate terminal or supervised process. Use asynchronous hooks to deliver changes to a working agent without holding its tool call open.

### Codex hooks

From an attached project, generate the hook configuration:

```sh
taskctl integrations codex config > taskctl-hooks.json
```

Merge the generated entries into your Codex hook configuration, such as the project's `.codex/hooks.json`; this command only prints configuration. See the [Codex hooks documentation](https://learn.chatgpt.com/docs/hooks). The generated commands include the absolute taskctl executable and selected project ID, so regenerate them if either changes.

The adapter uses asynchronous `PostToolUse` and `SessionStart` hooks. `PostToolUse` refreshes the executing worker's activity; ordinary progress notes remain silent. Other task changes arrive as bounded context with an explicit ACK command and at most one latest progress title per included task. `SessionStart` restores a compact current snapshot. Note bodies require an explicit read. Subscribe using the receiving agent's hook identity.

With a watcher or hook running, the default inactivity threshold is 30 minutes. An inactivity event means inspect the worker; it does not establish that the process crashed. Recent activity suppresses repeat inactivity reports.

Waking an idle Codex thread is a separate opt-in integration:

```sh
taskctl tasks subscribe T1 --session CODEX_THREAD_ID --resume --json
taskctl subscriptions watch SUBSCRIPTION_ID --session CODEX_THREAD_ID --codex
```

This uses `codex queue --thread ... --message ... --resume`; run the watcher outside the waiting agent. Queue acceptance leaves the notification pending until the agent reads and acknowledges it. Progress alone does not queue a wake. Availability depends on your installed Codex version supporting these [developer commands](https://learn.chatgpt.com/docs/developer-commands?surface=cli). Native Codex delivery has not been validated end to end; the [chaos report](docs/CHAOS_VALIDATION.md) distinguishes the hook-enabled agent harness tests from native-client coverage.

## Scratch, storage and backups

Agents can create disposable storage linked to a task:

```sh
taskctl scratch create TASK_ID --claim CLAIM_TOKEN
taskctl scratch list TASK_ID
taskctl scratch cleanup --dry-run
taskctl scratch cleanup
```

Keep important code, results and decisions in the repository, and link to them from the task. Scratch remains while a current task references it, including inherited references after evolution. Once no current task needs it, the default cleanup grace is seven days. Cleanup runs in bounded passes after selected mutations or through `scratch cleanup`. Attached worktrees and metadata paths are never cleanup targets.

| Data | Location |
| --- | --- |
| Database and project manifest | `$XDG_DATA_HOME/taskctl/projects/PROJECT_ID/` |
| Markdown note bodies | `$XDG_DATA_HOME/taskctl/projects/PROJECT_ID/notes.sqlite` |
| Disposable scratch | `$XDG_STATE_HOME/taskctl/scratch/PROJECT_ID/` |
| Optional configuration | `$XDG_CONFIG_HOME/taskctl/config.json` |

The XDG defaults are `~/.local/share`, `~/.local/state` and `~/.config`. Use a local filesystem shared by the local agents; multi-host and network-filesystem coordination are outside the supported scope.

Back up the task database and its Markdown body database together:

```sh
taskctl backup --directory taskctl-backup
taskctl restore --directory taskctl-backup --name recovered-project
```

Use a fresh directory for each backup; an existing destination is never overwritten. A full backup contains `tasks.sqlite`, a `notes.sqlite` snapshot when bodies are present, and a `COMPLETE` marker written last. Restore rejects an incomplete directory. You can store the directory in Git or another backup system. Use `backup` rather than separately copying live databases while agents write to them.

Restore creates a **new project** and leaves the source intact. It preserves task history and notes, releases old active claims, and clears subscriptions and creation request receipts. Attach the new project ID to the intended directory and recreate subscriptions. Scratch contents are not included.

`backup --file FILE.sqlite` remains available when the store has no live note bodies; it rejects a backup that would omit one. `restore --file FILE.sqlite --name NAME` accepts old SQLite backups and migrates their progress text into the current note format. A current-format database that references external bodies requires a full directory restore. JSON archives and compaction are not supported.

## Development and reference

[CI](.github/workflows/ci.yml) runs root unit, process and chaos tests, both example modules and `go vet` on Linux and macOS. Tests use the race detector. Pull requests, pushes to `main`, version tags and manual runs trigger checks. Successful main and version tag runs build all four binary archives.

Use `taskctl --help` for exact operation names and flags. Commands reject unknown flags. With `--json`, data goes to stdout and structured errors to stderr. Exit codes are 0 success, 1 operational failure, 2 invalid input, 3 conflict or busy, 4 cancelled watched work, 5 timeout, and 130 interrupted watch.

Version 0.4.0 names the replacement operation `evolve` and reports its state and event kind as `evolved`. The old `supersede` command and `superseded` status filter remain accepted. JSON consumers matching those output strings should use `evolved`; existing SQLite stores, backups and running old watchers remain compatible. See the [compatibility decision](docs/adr/0008-evolve-public-names.md).

- [Agent skill](skills/taskctl/SKILL.md): activation conditions and links to instructions for the operation at hand.
- [Design](docs/DESIGN.md) and [decisions](docs/adr/): states, concurrency and notification semantics.
- [Performance](docs/PERFORMANCE.md): measured scaling, memory use and limits.
- [Beads comparison](docs/BEADS_COMPARISON.md): same-host CLI latency, memory and concurrent operations.
- [Naming evaluation](docs/EVOLVE_EVALUATION.md): fresh Luna readers of `supersede` and `evolve` README variants.
- [Note storage validation](docs/NOTES_VALIDATION.md): streaming memory measurements, migration and process-kill tests.
- [Validation](docs/VALIDATION.md) and [chaos testing](docs/CHAOS_VALIDATION.md): test evidence and remaining limits.
- [Changelog](CHANGELOG.md): changes between versions.

## Releasing

Push a version tag such as `v0.5.1`. CI runs the tests before publishing all four binary archives, their matching source archive and SHA-256 checksums. Released binaries report the tag version without its leading `v`.

## License

Copyright © 2026 Tim Ruffles. taskctl is licensed under the [GNU Affero General Public License, version 3 only](LICENSE) (`AGPL-3.0-only`). Dependencies retain their own licenses. Binary archives include their third-party notices.
