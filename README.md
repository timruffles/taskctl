# taskctl

A local task CLI for a small group of coding agents. Agents claim work, record useful progress and replace stale task definitions with the work that actually remains. You use the same CLI to see what is running, what can start and what is blocked.

One Go binary. SQLite stores tasks, ownership, history and notification cursors. Storage lives outside the repository, so agents in separate worktrees can share a project. Git is optional.

## Install

Build from this checkout with Go 1.26 or newer:

```sh
mkdir -p "$HOME/.local/bin"
go build -buildvcs=false -trimpath -o "$HOME/.local/bin/taskctl" ./cmd/taskctl
export PATH="$HOME/.local/bin:$PATH"
taskctl version
```

Linux and macOS are supported. SQLite is embedded; running taskctl needs neither a SQLite installation nor a C toolchain.

Passing `main` builds also publish `taskctl-linux-amd64.tar.gz` and `taskctl-darwin-arm64.tar.gz` in **Actions → CI**. Each archive contains the executable `taskctl`; extract it into a directory on your `PATH`. Artifacts are retained for 14 days.

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
| Original and replacement definitions | `taskctl tasks history T1` |
| A snapshot of an original task's overall outcome | `taskctl tasks watch T1 --once` |
| Completed work | `taskctl tasks list --status completed` |

The default list contains pending and in-progress tasks. Completed, cancelled and superseded definitions stay in history. The `ORIGIN` column points back to the top-level original; `show` also names the immediate predecessor. You can inspect old intent without making it the default view.

Lists default to 20 rows and print a continuation command when more remain:

```sh
taskctl tasks list --tag import --limit 20 --offset 20
taskctl tasks show T1 --json
```

`--json` gives agents and scripts structured output. Task lists include `total`, `omitted` and `next_offset`; `total` counts all matching tasks, including those beyond the current page. History, subscription and scratch lists use `more` and `next_offset`. Lists cap at 200 rows, and omit large briefs and progress text; use `show` for detail.

Repeated tags mean AND: `--tag import --tag tests` selects tasks carrying both tags. Add a tag with `tasks tag ID --tag TAG`, supplying `--claim` if the task is in progress.

## Keep progress and task definitions current

Record a checkpoint when something meaningful changes:

```sh
taskctl tasks progress T1 --claim CLAIM_TOKEN \
  --text 'Valid CSV rows parse; parser tests pass. Malformed-row errors remain.'
```

`show` displays the latest checkpoint. A heartbeat only refreshes activity and is silent by default:

```sh
taskctl tasks heartbeat T1 --claim CLAIM_TOKEN
```

Hooks can handle heartbeats. Agents still need to record what they achieved, what remains and any useful paths or decisions. Activity alone cannot tell you whether the work is advancing.

### Replace a task when a substantial chunk is finished

If the original brief now describes mostly completed work, supersede it with a crisp description of what remains. For the still-claimed `T1`, save this as `remaining.json`:

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
taskctl tasks supersede T1 --claim CLAIM_TOKEN --revision REVISION --file remaining.json
taskctl tasks in-progress
taskctl tasks available
```

The replacement becomes the current task; `T1` remains discoverable as `superseded`. `continue_with` claims the ready replacement for the same worker and returns a **new claim token**. Use the returned task ID and token from then on. Omit `continue_with` to leave replacements pending for other agents.

Tags, metadata, prerequisites and scratch references carry forward. Each new brief must stand on its own. A prerequisite or subscription on `T1` follows its replacements, so `T3` keeps waiting for the remaining CSV work. An original succeeds only when every replacement branch completes; its stored state remains `superseded`.

For a small correction, use `tasks edit ID --revision REVISION --brief-file FILE`, with `--claim` when owned. When work is finished, record the actual result and verification:

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

The adapter uses asynchronous `PostToolUse` and `SessionStart` hooks. `PostToolUse` refreshes the executing worker's activity and stays silent when there are no updates. Notifications arrive as bounded context with an explicit ACK command; `SessionStart` can restore a compact current snapshot. Subscribe using the receiving agent's hook identity.

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

Keep important code, results and decisions in the repository, and link to them from the task. Scratch remains while a current task references it, including inherited references after supersession. Once no current task needs it, the default cleanup grace is seven days. Cleanup runs in bounded passes after selected mutations or through `scratch cleanup`. Attached worktrees and metadata paths are never cleanup targets.

| Data | Location |
| --- | --- |
| Database and project manifest | `$XDG_DATA_HOME/taskctl/projects/PROJECT_ID/` |
| Disposable scratch | `$XDG_STATE_HOME/taskctl/scratch/PROJECT_ID/` |
| Optional configuration | `$XDG_CONFIG_HOME/taskctl/config.json` |

The XDG defaults are `~/.local/share`, `~/.local/state` and `~/.config`. Use a local filesystem shared by the local agents; multi-host and network-filesystem coordination are outside the supported scope.

Back up to a consistent SQLite snapshot:

```sh
taskctl backup --file taskctl-backup.sqlite
taskctl restore --file taskctl-backup.sqlite --name recovered-project
```

Use a fresh filename for each backup; an existing file is never overwritten. You can store the resulting snapshot in Git or any backup system. Use `backup` rather than copying a live database file, which may have outstanding WAL writes.

Restore creates a **new project** and leaves the source intact. It preserves task history, releases old active claims, and clears subscriptions and creation request receipts. Attach the new project ID to the intended directory and recreate subscriptions. Scratch contents are not included. Backups are SQLite-only; `--json` remains available for command output.

## Development and reference

[CI](.github/workflows/ci.yml) runs root unit, process and chaos tests, both example modules and `go vet` on Linux and macOS. Tests use the race detector. Pull requests, pushes to `main` and manual runs trigger checks; successful main runs build the two binary artifacts.

Use `taskctl --help` for exact operation names and flags. Commands reject unknown flags. With `--json`, data goes to stdout and structured errors to stderr. Exit codes are 0 success, 1 operational failure, 2 invalid input, 3 conflict or busy, 4 cancelled watched work, 5 timeout, and 130 interrupted watch.

- [Agent skill](skills/taskctl/SKILL.md): activation conditions and links to instructions for the operation at hand.
- [Design](docs/DESIGN.md) and [decisions](docs/adr/): states, concurrency and notification semantics.
- [Performance](docs/PERFORMANCE.md): measured scaling, memory use and limits.
- [Validation](docs/VALIDATION.md) and [chaos testing](docs/CHAOS_VALIDATION.md): test evidence and remaining limits.
- [Changelog](CHANGELOG.md): changes between versions.
