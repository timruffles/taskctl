# V1 validation and Luna trial

Date: 21 November 2026. Version: `0.1.0`. Built with Go 1.27.4 and `modernc.org/sqlite` 1.62.0.

## Automated verification

`go test -race -p 1 -json ./...` passed across all six packages: **69 top-level tests**, including **15 separate-process CLI tests**. `go vet ./...` passed. The independent-process harness builds a normal CLI executable; the race detector instruments the Go test packages. A serialized build with `GOGC=30` and `GOMEMLIMIT=700MiB` avoided an earlier compiler memory-limit failure while instrumenting generated SQLite code.

| Area | Evidence |
| --- | --- |
| Exclusive ownership | Eight CLI processes compete for one task; exactly one succeeds. Old tokens fail after takeover. |
| Concurrent definition changes | Competing revision-checked edits cannot overwrite each other. |
| Atomic supersession | Failed replacement/dependency/event insertion rolls back all changes. Cycles include replacement edges. |
| Derived availability | A subscriber receives availability when an external prerequisite finishes, including every branch of a superseded prerequisite. |
| Durable notifications | Concurrent offers have one owner; stale and expired ACKs fail; failed callbacks can retry without losing events. |
| Queue transport | A completion behind more than one page of unacknowledged progress is reached. Progress alone stays quiet. Queue acceptance never advances the ACK cursor. |
| Hook behaviour | Native-child identity, literal command quoting, bounded context, quiet unchanged hooks, current-state restoration, and inactivity deduplication. |
| Scratch | Active/inherited references, grace, exclusive cleanup claims, bounded batches, restart recovery, symlinks, ownership markers and worktree preservation. |
| Backup/restore | Deterministic exports, WAL-inclusive SQLite snapshots, no backup overwrite, claim invalidation on restore, dependency validation and rollback. |
| CLI behaviour | JSON parse errors, compact paginated lists, cancellation/timeouts and a live waiter that permits another process to complete work. |

The eight-way claim race, competing edit race and competing delivery race each passed ten additional repetitions in independent QA.

Three defects found during verification have direct regressions: a completion committed inside an earlier callback could be missed before watch exited; a parse error before a trailing `--json` produced plain text; and an export path through a symlinked parent could overwrite the live database. All are fixed.

## Actual two-agent trial

Two agents ran with the **`gpt-6-luna` model**, using the built `taskctl` executable against the same project database. They built the small Go project under `examples/textstats`. The coordinator supplied a shared contract and non-overlapping source-file ownership.

The agents used compact discovery, `show`, atomic claims, session metadata, progress updates and completion summaries. The core agent also created scratch and replaced its task while retaining ownership of the remaining verification work through a fresh token.

| Task | Work | Predecessor | Final state |
| --- | --- | --- | --- |
| T1 | Original library and CLI intention | — | superseded |
| T2 | Implement and test Analyze | T1 | superseded |
| T5 | Verify Analyze edge cases | T2 | completed |
| T3 | Implement and test the CLI | T1 | completed |
| T4 | Verify the integrated project; prerequisite T1 | — | completed |

`tasks in-progress --json` captured both real owners and their T1 origin in **502 bytes**, without briefs, metadata or full history. The foreground watch observed T2 being replaced by T5, its new claim and completion, and the independent CLI branch’s completion. It then reported T1’s completed outcome. T1 and T2 remained historical superseded records.

T4 was blocked until T3 and T5 had both finished. It then appeared in `tasks available`, was claimed by the coordinator, and was completed after integration verification. The final current-task list was empty. Raw evidence is in `examples/trial-evidence/`, including the watch event stream, actual task export and captured lists.

### Demo verification

The library’s table tests cover empty/ASCII input, Unicode whitespace and code points, newline counts, CRLF and invalid UTF-8. The CLI tests cover input selection, JSON output, help, usage errors and read errors. The coordinator ran `go test -race ./...` successfully for both demo packages and exercised the built executable against five additional fixtures:

- Unicode stdin produced `{"lines":2,"words":2,"runes":6}`.
- Empty stdin produced zero counts.
- `-- -input.txt` read a real leading-hyphen filename.
- A missing file produced exit 1 with stderr and no successful JSON.
- Two filenames produced exit 2 with stderr and no successful JSON.

One Luna agent corrected its own rune-count expectation during implementation. Neither reported a task-CLI blocker. The trial used no Git repository, demonstrating that ordinary coordination does not require Git.

### Notifications and housekeeping against the trial database

After the agents completed, synthetic Codex `PostToolUse` inputs were passed to the real adapter. It delivered two outstanding batches of **1,027 and 989 context bytes**, both below the 3,000-byte default. Their explicit ACK commands succeeded. The next unchanged hook invocation emitted **zero bytes**.

The core agent’s scratch reference was inherited by T5 and retained through active work. After all work was complete, an explicit shortened grace allowed a dry-run and actual cleanup. The area was removed, its reference history remained, and no repository files were affected.

Two consecutive exports were byte-identical. A live SQLite backup was restored into a different project; all five tasks and the completed original outcome were recovered.

## What was and was not exercised

The real Luna agents used the CLI and SQLite store. Hooks and Codex queue integration were tested through synthetic hook input and a fake queue executable, including failure paths. **Native automatic delivery and idle resumption inside the user’s Codex client were not exercised, and no user hook configuration was changed.** Actual client behaviour depends on its hook/queue support and pause handling.

Linux amd64 was built and executed. The macOS arm64 binary was cross-compiled successfully; it was not executed on macOS. Filesystem cleanup is implemented for Linux/macOS/BSD and returns an explicit unsupported error on other platforms.

The cleanup budget is cooperative: 256 entries or approximately 50 ms per call, checked between filesystem operations. A blocking syscall cannot be interrupted by that budget. Notification transport is at least once; callback effects need event-ID deduplication when repetition matters. Inactivity signals silence, not a proven process crash, and never transfers task ownership.
