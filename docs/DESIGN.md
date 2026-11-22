# taskctl: 0.3.0 design

Date: 2026-11-22

`taskctl` is a Go command-line tool for a single-digit number of coding agents working locally. This document records the current design, including recovery, bounded data access and the separate progress-note body store. See the README for exact command grammar, PERFORMANCE.md for scaling measurements, VALIDATION.md for the initial trial, and CHAOS_VALIDATION.md for the historical failure tests and hooked Luna recovery exercise.

## Accepted decisions

### Current work and history

Everyday views show the current frontier of work. When substantial progress or changed understanding makes a task definition stale, an agent replaces it with one or more fresh tasks. The old task becomes superseded and remains discoverable through history links. Agents decide when replacement is useful; there is no progress score or automatic judgement of substantive progress.

Each replacement has a self-contained brief describing the remaining work, relevant completed work and decisions, and useful references. Reading ancestors is optional. The original intention remains traceable.

Completion watches follow replacements and provide progress updates. Success means all remaining branches completed. Cancellation is distinct from success and is reported to the watcher; other branches continue normally. Superseded records stay superseded and need no separate maintenance.

### Availability and ownership

An available task describes current unfinished work, is unclaimed, and has no unfinished prerequisites. Prerequisites are optional and follow replacement work when necessary. Availability is derived rather than maintained as another flag.

A claim checks availability, assigns one owning agent session and marks the task in progress atomically. Two different sessions racing for the same task get one success and one clear conflict. Listing is a snapshot; only a successful claim grants ownership. Explicit release removes ownership; reassignment invalidates previous ownership. Parallel work uses separate tasks.

These tokens fence taskctl mutations. File isolation remains the execution harness's responsibility: separate worktrees or restricted writable paths prevent a resumed worker from editing another worker's files. Confirm termination before explicit takeover; inactivity alone is insufficient.

### Activity

A coarse last-activity signal suppresses inactivity reminders while activity is recent. Agents track their own progress. The tool does not need fine-grained productivity, progress or agent-health classifications. V1 uses a configurable 30-minute inactivity threshold and one event per silence period. Heartbeats do not produce events or change revisions. Inactivity never expires task ownership. Deliberate pauses can retain ownership or explicitly release it.

### Progress notes

A progress note has a required single-line title of at most 120 Unicode characters, an optional Markdown body, a task and event ID, its author and claim, and a timestamp. New titles over the limit are rejected. The current claim is checked before publication. Task views expose only the latest title and note reference. Note lists are paged, newest first, using an event-ID cursor; bodies are read explicitly.

`tasks.sqlite` holds note metadata and the latest title. A separate `notes.sqlite` stores Markdown bodies in indexed 64 KiB chunks. The body byte length and SHA-256 support validation and bounded streaming without file-position indexes or scanning older notes. Body publication precedes its main-database reference; interrupted publication can leave unreferenced body data, rather than a successful task update that points to an unfinished body. Reads, deletion and complete backups coordinate with publication.

Deletion clears the note's visible text and body, retaining a small deletion record. Deleting the latest note clears current progress rather than selecting an older title. Freed pages remain available for reuse; physical database compaction is not part of this release. Note bodies are durable and are not subject to scratch cleanup. Supersession does not copy note history into new tasks: essential facts belong in a deliberate handoff and self-contained successor briefs.

Legacy `progress --text` and `--text-file` remain supported. Input that is already a valid title creates a title-only note; other text is preserved as a body with a derived title. Legacy file input is buffered with a 1 MiB cap; the new `--body-file` path streams. Upgrading a schema-version-1 store moves its historical progress text into the new format. Explicit note reads stream Markdown; `--json` is available for note metadata, not the body stream.

### Projects, storage and backups

Each project has a stable ID and a task database outside the repository, shared across its worktrees. Long note bodies live in a second database in the same project directory. Local configuration selects the project, with an explicit `--project` override. Markdown briefs live in the task store. A complete backup directory contains both SQLite snapshots and can be stored in Git or another backup system; normal operation does not require Git. JSON archives were removed in 0.2.0.

Normal commands open existing validated storage and fail explicitly when it is missing or invalid. Opening an up-to-date store does not start a schema write transaction; missing additive objects are installed when required. Only explicit creation or restore initializes a database. Optional creation request IDs make retries after lost responses safe by committing the normalized input receipt, task and events together. Reusing a key with different input conflicts; a new restored project starts without the old operational receipts.

Single-file backup uses SQLite `VACUUM INTO`, syncs the staging file and publishes it atomically without replacing an existing destination. It rejects stores with live note bodies. Full directory backup coordinates body publication and deletion while taking snapshots of both databases; an incomplete backup is not a valid restore source. Restore creates a new project, validates source fields and references, and streams rows into the application's own schemas. Old claims are released, subscriptions and request receipts are cleared, and scratch history remains marked unavailable. Source schema objects and scratch file contents are not imported. Legacy SQLite-only backups remain accepted and their progress is migrated.

Locations follow XDG:

| Content | Location |
| --- | --- |
| Project database, history and subscriptions | `$XDG_DATA_HOME/taskctl/projects/<project-id>/` |
| Markdown note bodies | `$XDG_DATA_HOME/taskctl/projects/<project-id>/notes.sqlite` |
| User configuration | `$XDG_CONFIG_HOME/taskctl/config.json` |
| Managed scratch | `$XDG_STATE_HOME/taskctl/scratch/<project-id>/` |

Use the XDG defaults when variables are absent or empty, and ignore relative XDG paths as invalid. Keep managed scratch out of the cache directory because it may hold context required by current tasks. [XDG Base Directory Specification](https://specifications.freedesktop.org/basedir/)

### Scratch

Scratch is outside repositories and linked to tasks. Keep it while any current task references it, including successors of its creator. Once no current task references it, a configurable grace period makes it eligible for cleanup. Inactivity alone never triggers deletion. Important code and results belong in the repository. V1 uses a seven-day grace and bounded opportunistic cleanup after selected task mutations, plus an explicit cleanup command. A permanent deleting marker and a separately fenced cleanup lease prevent an area being reattached during partial cleanup. Filesystem locks prevent overlapping cleaners; path confinement and ownership markers protect unrelated files. A task mutation refreshes eligibility only for its affected areas in the same transaction. Cleanup reads indexed eligible candidates in bounded batches, without a global refresh.

### Tags, metadata and context cost

Tags are plain labels. Named metadata fields cover worktrees and agent/session identities; additional key/value metadata is supported. Status and ownership are dedicated properties.

| Command | Default content |
| --- | --- |
| `tasks available` | ID, title, tags, original task ID |
| `tasks in-progress` | The same, plus owner and last activity |
| `tasks show ID` | Current brief, latest progress title and note ID, prerequisites, metadata and a page of scratch references |
| `tasks history ID` | A page of definitions from the original lineage, including links, progress and results |
| `notes list ID` | A page of note titles and metadata, with a `next_before` cursor |
| `notes read NOTE_ID` | One explicitly requested Markdown body, streamed |

Task lists default to 20 and cap at 200, with exact totals and continuation offsets. History, subscription and scratch lists use the same limits and return an explicit `more` flag and `next_offset`. `tasks show --json` supplies `scratch_more` and `scratch_next_offset` for its scratch page. Details and history are requested progressively rather than returned in every list. JSON envelope changes in 0.2.0 are intentional CLI changes.

### Watching

`tasks watch ID` runs as an ordinary foreground process, with JSON output and optional callback support. It observes stored events, follows replacements and reports progress, completion, cancellation and inactivity. A saved cursor permits resumption. State changes and corresponding events are recorded together. Immediate external notification requires a running observer, but ordinary task commands do not.

`watch --once` without a cursor returns one current snapshot; with `--after`, it performs one event-page check. Full event pages have a count limit and a 1 MiB stored-payload target. One larger event is allowed to guarantee delivery can advance. Snapshots contain at most 20 task rows and disclose omitted rows. Event retrieval seeks the watched task IDs using their event index, selects a bounded set of IDs in global event order, then loads payloads.

### Work and memory bounds

Availability traverses only the prerequisites of the tasks being considered and the replacement descendants needed to resolve them. Progress, claims, takeovers, edits, tags and metadata do not perform a database-wide availability comparison. Mutations that can make work ready select the affected candidates and insert availability events in the same transaction. Completing a leaf includes dependencies on its superseded ancestors, including tasks outside the watched subtree.

Normal dependency changes check reachability from the proposed edge. Restore's full-graph check uses indexed SQLite temporary tables and topological removal, without retaining a graph-sized Go map or recursive Go stack. Temporary storage uses files when it grows beyond SQLite's working cache.

Row streaming and paged reads bound the number of records retained by Go. Note body input and output use bounded chunks, and ordinary task operations do not load them. This is not a constant-memory guarantee for all commands: an individual requested task can have a large brief or metadata, full event delivery can include one oversized non-progress record, and SQLite needs working and temporary storage. Exact totals and broad replacement scopes still cost time proportional to the relevant data. No persistent derived-outcome cache is introduced.

## Codex notification research

The user requested subscriptions that inform an agent efficiently without blocking its work. The following product behaviour informed the implementation. The adapter and queue transport have protocol/fake-executable tests; native automatic delivery inside the user’s Codex installation has not been exercised.

- Codex supports asynchronous command hooks. `PostToolUse` can return `hookSpecificOutput.additionalContext`, which is delivered at the next safe point in an active turn. It does not start a new turn when the session is idle. Up to eight background hooks run concurrently per session, and session shutdown discards undelivered hook output. [Hooks documentation](https://learn.chatgpt.com/docs/hooks#how-background-hooks-run)
- `SessionStart` can supply context on startup or resume. For a native subagent's `PostToolUse`, `session_id` identifies the shared root session and `agent_id` identifies the executing child thread. Attribute child activity using `agent_id` when present, otherwise use `session_id` for the root. Handle `SubagentStart` and `SubagentStop` as lifecycle events about their named child. [Hooks documentation](https://learn.chatgpt.com/docs/hooks), [stable-release identity accessors](https://github.com/openai/codex/blob/rust-v0.171.0/codex-rs/core/src/session/session.rs), [hook construction](https://github.com/openai/codex/blob/rust-v0.171.0/codex-rs/core/src/hook_runtime.rs).
- `codex queue --thread <UUID> --message <TEXT>` queues text for an existing session. `--resume` attempts to load an unloaded session through the running Codex daemon or selected server. It can warn about a failed resume while still exiting successfully. Queue acceptance is not evidence that the model consumed a notification. [CLI documentation](https://learn.chatgpt.com/docs/developer-commands?surface=cli#codex-queue)
- Queued input is for later turns rather than active-turn context. A user-interrupted thread can retain a paused queue; preserve that user stop. [Maintainer explanation of next-turn semantics](https://github.com/openai/codex/issues/58965), [maintainer confirmation of interruption behaviour](https://github.com/openai/codex/issues/58421), [tracked visibility limitation](https://github.com/openai/codex/issues/56914).

The locally inspected executable is `codex-cli 0.172.0-alpha.2`; this does not establish the user's version. The official changelog lists stable `0.171.0` on 20 November 2026. Integration must check supported capabilities and must not depend on private session database formats. [Changelog](https://learn.chatgpt.com/docs/changelog)

### Implemented adapter

Keep subscriptions and pending events in our store. An asynchronous `PostToolUse` handler performs a short check for relevant pending updates and records coarse activity where ownership matches. Ordinary progress notes do not trigger context; other task changes produce a compact summary with task IDs, event IDs and an explicit acknowledgement command. Details remain available through `tasks show`, `tasks history --json` and explicit note reads. `SessionStart` restores a concise current snapshot.

This is cheap local polling at hook boundaries, not a model repeatedly calling a waiting tool. It provides no guaranteed latency during a long uninterrupted model request or tool call. Do not launch a persistent watcher for every tool invocation.

The adapter defaults to five events and a maximum of 3,000 context bytes. Its delivery query skips progress events and includes at most the latest note title per affected task within the offered event range. Acknowledgement covers that range; later events remain pending. Progress-only hook checks neither acquire a delivery lease nor acknowledge history. Explicit event reads still include progress history. Heartbeats generate no model-visible events. The hook refreshes the executing owner’s activity before detecting silence in other tasks, so hook-only subscribers can learn of inactivity without a separate watcher.

Hooks and Codex queue watchers fetch bounded text summaries and omit unused event data in SQL before constructing context. They compact large snapshots or progress text further with omitted detail disclosed, preserving event identities and ACK ranges. Full callback consumers retain complete event content. Subscription selection uses the session index and bounded results; quiet subscriptions do not each open a delivery write transaction. Initial queued snapshots also preserve events created since subscription. Cursors beyond the current database high-water mark fail rather than silently skipping future work.

Hook output is not a durable acknowledgement of model consumption. Keep events available until explicit acknowledgement, tolerate duplicate delivery using stable event IDs, and refresh current state on resume. `subscriptions pending` offers a batch with a random delivery token and a through-event ID. `subscriptions acknowledge` requires both. A five-minute lease prevents concurrent offers; expired tokens are fenced.

An external subscription watcher can enqueue a compact wake for idle resumption only when both the subscription has `--resume` and the watcher has `--codex`. Completion, cancellation, newly available work and inactivity are eligible; progress alone is not. Queue scanning and accepted wake-up watermarks are distinct from the ACK cursor. Queuing never consumes context, and Codex’s user-interruption handling is preserved. A subscription remains until explicitly unsubscribed.

## Current implementation choices

- `modernc.org/sqlite` embeds SQLite. WAL, full synchronous commits, foreign keys, a five-second busy timeout and short immediate write transactions are enabled. Consistent pure reads use deferred transactions, and large temporary SQL structures can spill to disk.
- Claims use random fencing tokens. Edits and supersession also compare the expected revision. Supersession input is a strict JSON object; ownership transfer is explicit through `continue_with`.
- An original’s completion outcome follows all replacement branches; cancellation is distinct. Effective dependency cycles include replacement edges.
- Lists default to 20 and cap at 200. Tags use AND filters; replacement tasks inherit tags, metadata, prerequisites and scratch references.
- Session and worktree references are string metadata; they never grant ownership or cause worktree deletion.
- Task changes and their notifications share the same commit. Subscriptions, acknowledgement cursors, offered event ranges and delivery locks are database concepts. No transaction spans waiting, a callback or a Codex command.
- Backups use SQLite snapshots. Full directories contain both task metadata and note bodies; a single task-database snapshot is accepted only when no live bodies would be omitted. Restore validates source data and resets operational state.
- Database schema version 2 introduces progress notes. Version-1 projects and backups migrate their progress text; older binaries reject the upgraded schema. Opening a future unknown schema fails.
- Explicit waiting uses exit 0 for completed, 4 for cancelled and 5 for timeout. No default progress percentage, priority system, server, daemon requirement or Git synchronisation protocol is included.

See ADR 0004 for durable notification delivery and the independent queue cursor, ADR 0005 for uncertain command outcomes and recovery boundaries, ADR 0006 for the earlier SQLite-only backup format and bounded data access, and ADR 0007 for separate Markdown note bodies.
