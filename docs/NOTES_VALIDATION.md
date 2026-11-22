# taskctl 0.3.0: note storage validation

Date: 22 November 2026.

Notes now keep short titles and coordination metadata in `tasks.sqlite`, with optional Markdown bodies in `notes.sqlite`. Bodies are stored and read in chunks of at most 64 KiB. Existing progress text migrates into that format. [ADR 0007](adr/0007-separate-markdown-note-bodies.md) explains the transaction ordering and backup design.

## Failure and concurrency tests

The complete project suite passes, including race-detector runs. The two example modules also pass under `-race`; `go vet` passes and a macOS arm64 binary cross-compiles. Focused tests cover:

- Eight independent store connections publishing notes without losing a body, event or latest-title update.
- Claim takeover during a paused body stream: the old writer cannot publish its note.
- Heartbeats during slow body input: committing the note cannot move activity time backwards.
- Input failure, immutable body IDs, missing body databases, malformed chunks, checksum mismatch and short output writes.
- Lossless migration of historical notes and current progress that never reached an event; reopening does not repeat migration.
- Complete backup/restore, empty bodies, truncated files, missing members, symlinks, invalid schemas and source-trigger exclusion.
- Quiet routine notes, actionable event delivery, current titles on resume, cursor-fenced titles, lease replay and stale ACK rejection.

Three deterministic subprocess scenarios were repeated ten times under `-race`, for **30 actual SIGKILLs**:

1. Kill after the first body-chunk INSERT, before its transaction commits. No task, note, event or partial body is published; a subsequent write succeeds.
2. Kill after the body commits, before its task reference is published. Only an unreferenced body remains. An orphan-only backup omits the body database; restoring a mixed backup imports only referenced bodies.
3. Hold the body database's writer transaction, observe a deletion tombstone commit in the task database, then kill the blocked deleter. Retry removes the remaining body without reviving older progress or duplicating the deletion event.

These exercise process death at known transaction boundaries. They do not simulate a physical power failure or establish guarantees about faulty disk firmware. SQLite `FULL` synchronisation and explicit file/directory syncing provide the durability mechanism.

## Memory and elapsed time

Fresh CLI processes on a shared Linux host; Go 1.27.4, GOGC=100, no race instrumentation or memory cap. Each row is one diagnostic sample. Peak RSS includes Go and SQLite working memory; it is not a latency percentile or a strict memory bound. Filesystem caches were not flushed.

| Operation | Body size | Elapsed | Peak RSS |
| --- | ---: | ---: | ---: |
| Write note | 16 MiB | 158 ms | 12.5 MiB |
| Write note | 128 MiB | 1,418 ms | 14.0 MiB |
| Read note | 16 MiB | 29 ms | 14.9 MiB |
| Read note | 128 MiB | 329 ms | 34.0 MiB |
| Upgrade legacy note | 16 MiB | 164 ms | 21.9 MiB |
| Upgrade legacy note | 128 MiB | 1,544 ms | 23.2 MiB |
| Show task with large note | 16 MiB | 6.1 ms | 9.0 MiB |
| Show task with large note | 128 MiB | 7.2 ms | 9.1 MiB |

The migration fixture stores the same large text in its old task row and progress event. Both copies become one body with the expected hash. Bodies are generated and checked incrementally; the diagnostic runner does not feed a body-sized string to the CLI. Routine task output remains 581 bytes in both streaming fixtures.

With 100,000 note records and 100,000 deletion events, an individual warm store-level diagnostic measured 0.52 ms for the quiet hook candidate query and 0.88 ms for an actionable offer including its latest title and delivery lease. Query-plan checks verify indexed seeks for actionable events and latest note titles. These figures exclude process startup.

## Reproduction

```sh
go test -race -count=1 -timeout=15m ./...
go test -race -count=10 -timeout=2m ./internal/store \
  -run '^TestNoteCrash(During|After)'
go build -o /tmp/taskctl ./cmd/taskctl
go build -o /tmp/taskctl-measure ./tests/perf
python3 tests/perf/notes_scale.py --root /absolute/fresh-notes-check \
  --binary /tmp/taskctl --measure /tmp/taskctl-measure
```

The script requires Python 3.11 or newer for SQLite incremental BLOB fixture creation. Use a disposable directory with roughly 2 GiB free; it keeps its reproducible fixtures and measurements. It tests body sizes of 16 and 128 MiB by default.

No compaction or automatic orphan collection is included. Deleted pages can be reused without immediately shrinking the SQLite file. A crash can leave unreferenced body rows or an unpublished initialisation file; these cannot create published references to unfinished bodies. Existing broad task metadata, briefs and non-progress event records retain their earlier memory characteristics, described in [the 0.2.0 performance report](PERFORMANCE.md).
