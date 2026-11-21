# taskctl 0.2.0: bounded data access and performance fixes

Date: 21 November 2026. This release applies the changes agreed after the [0.1.1 performance check](PERFORMANCE-0.1.1.md). JSON archive export and restore are removed. Backups and restores use SQLite only.

## Result

Routine task operations no longer depend on the amount of retired task history. The 100,000-task / one-million-event CLI fixture now handles progress in about 7 ms, compared with 450 ms before. A 1,000-definition replacement chain no longer causes a multi-second progress update. A 524.7 MiB SQLite database restores with 28.6 MiB peak process RSS.

These are diagnostic samples on a shared Linux host. The large improvements reflect removed scans and buffers; small millisecond differences should not be interpreted as stable speedups.

## What changed

### SQLite only, incremental restore

`backup --file FILE.sqlite` takes a consistent live SQLite snapshot with `VACUUM INTO`, syncs the completed staging file and atomically publishes it without replacing an existing destination. `restore --file FILE.sqlite --name NAME` opens a read-only source snapshot, validates each row, and inserts it with a prepared statement into a new project.

The implementation no longer constructs a whole-database `map[string][]map[string]any`. Reusable row buffers retain only the current record. Restoring old active claims emits reset events through SQL rather than collecting their IDs in Go. Whole-graph validation uses indexed SQLite temporary tables with file-backed temporary storage, replacing a graph-sized Go map and recursive Go stack.

All destination inserts, validation and ownership resets share one transaction. Type, timestamp, label, event-JSON, constraint, foreign-key, lineage and cycle validation remain. The destination uses taskctl's own schema; source triggers, views and other executable schema objects are never imported. Failure rolls back the destination. Old claims, subscriptions and creation request receipts are cleared; scratch history is retained as unavailable.

Existing schema-version-1 SQLite stores and backups remain supported. The `export` command and JSON archive input are removed. Structured command output and supersession-plan JSON remain supported.

### Scope queries and keep transactions short

Availability follows only the prerequisites and replacement branches relevant to the requested task. Progress, ownership reassignment, edits, tags and metadata no longer compute before/after sets of every ready task. Other mutations select their known affected tasks and emit availability events with SQL in the same transaction. Completing a leaf still notifies dependents of every superseded ancestor, including prerequisites outside the visible subtree.

Snapshot and event reads use deferred read transactions. Opening an up-to-date database no longer acquires a schema writer transaction. Missing additive schema objects still initialise atomically. Read regressions verify that existing-store opening, snapshots, events and details proceed while a different connection holds a write transaction, and that a read snapshot stays consistent across another writer's commit.

Scratch changes refresh only affected areas in the task or attachment transaction. Cleanup no longer updates every scratch record before selecting a bounded batch. Reference lookup indexes avoid repeated full-reference scans; ownership, leases and deletion fencing remain unchanged.

### Bound returned data and notification work

History, subscriptions and scratch lists now return pages: default 20, maximum 200, with explicit `more` and `next_offset`. Task detail includes one scratch page and continuation fields. `watch --once` without a cursor returns one current snapshot; it no longer constructs a second immediate snapshot.

Events use task-indexed ranges. At most one candidate page per scoped task is considered before merging by global event ID. Full event pages target 1 MiB of stored payload as well as the count limit. One oversized event is permitted so the cursor can always advance; full event data is never silently truncated.

Hooks and the Codex queue watcher fetch bounded summaries and omit unused event data before Go allocation. Explicit reads and callbacks still receive full payloads. The hook selects at most six candidate subscriptions in SQL rather than opening a transaction for every quiet subscription. SessionStart prioritises pending deliverable state. Token fencing, exact ACK ranges, replay and the independent queue cursor remain intact.

## Real CLI comparison

The flat fixture starts with 20 current tasks—five claimed and 15 pending with a completed prerequisite—and retired completed history. Each task has a 1 KiB brief, tag, metadata and ten moderately sized events. The largest database contains 100,000 tasks and 1,000,000 events, occupying 524.7 MiB. Ordinary cases use three fresh processes; deep-lineage and archive cases are single samples.

| Operation | 0.1.1 | 0.2.0 |
| --- | ---: | ---: |
| Available list, limit 20 | 334.9 ms | **5.7 ms** |
| Snapshot of one completed task | 261.6 ms | **6.3 ms** |
| Progress update | 449.8 ms | **7.0 ms** |
| Claim one task | 403.1 ms | **8.2 ms** |
| Progress at replacement depth 1,000 | 4,987.5 ms | **5.7 ms** |
| Quiet hook, 500,000 unrelated newer events | 59.3 ms | **5.7 ms** |
| Quiet hook, 1,000 subscriptions | 106.5 ms | **7.5 ms** |

At 100,000 tasks, available/snapshot/progress/claim peak RSS is now about 9.2–9.4 MiB, compared with 24–32 MiB before. Single-task reads and in-progress lists remain in the small single-digit millisecond range, with about 9 MiB RSS. The quiet hook with 1,000 subscriptions uses 9.8 MiB RSS.

Eight independent CLI processes updating different claimed tasks all succeeded. At 100,000 tasks, the burst fell from 4,274.4 ms to **47.5 ms**; individual operations ranged from 6.5 to 40.7 ms. The 1,000-task burst took 82.9 ms. These single burst samples demonstrate that large historical data no longer adds seconds of lock waiting; they do not establish a latency percentile.

## Backup and restore memory

| Tasks / events | Source DB size | Operation | Elapsed | Peak RSS |
| --- | ---: | --- | ---: | ---: |
| 10,000 / 100,000 | 52.3 MiB | backup | 0.17 s | **15.5 MiB** |
| 10,000 / 100,000 | 52.3 MiB | restore | 1.54 s | **27.6 MiB** |
| 100,000 / 1,000,000 | 524.7 MiB | backup | 1.33 s | **16.3 MiB** |
| 100,000 / 1,000,000 | 524.7 MiB | restore | 18.16 s | **28.6 MiB** |

The old SQLite restore at 10,000 tasks / 100,000 events took 5.28 s and 192.8 MiB RSS. The new matching-size restore took 1.54 s and 27.6 MiB. Increasing the new database tenfold raised peak RSS only to 28.6 MiB; time grows with the data being read, validated and written. Every measured restore passed integrity and row-count checks, cleared claims and removed subscriptions.

The former 577.8 MiB JSON-export case is eliminated by removing that operation. No JSON compatibility shim or whole-database JSON representation remains in production.

## Store benchmarks and remaining costs

All 64 flat store benchmark cases and six lineage cases passed. At 100,000 tasks with 20 current tasks, available listing fell 258.075→0.570 ms, one-task snapshot 184.134→0.413 ms, and progress 390.318→0.694 ms.

When almost all 100,000 tasks are current, exact available-list counting still costs **33.4 ms**. This is relevant-data work and remains linear. A one-leaf snapshot and progress remain about 0.39 ms and 0.57 ms respectively. Snapshot Go allocation fell 8,081,112→6,874 B/op; progress fell 13,778,962→12,226 B/op. These are allocated bytes, not peak or retained memory.

At replacement depth 1,000, store progress fell 4,839.030→0.938 ms and leaf snapshot 2,336.298→0.459 ms. The fixture includes a separate dependent on the original root, preventing SQLite from hiding the old expensive outcome path.

A separate indexed-event check reads 20 events from backlogs of 100, 10,000 and 100,000 events on the same task. Single samples were 0.367, 0.405 and 0.579 ms with about 18 KiB Go allocation. This checks that avoiding unrelated-event scans did not introduce scanning of the entire relevant backlog.

Memory is bounded by page contents, the largest requested record and SQLite's working sets, rather than by a whole-database Go representation. A very large individual task brief, aggregate metadata or event can still require substantial memory. Broad watched subtrees, exact list counts, validation and full restore must still process their relevant data. Events and request receipts remain durable history; no retention or pruning policy was added. This was not a memory-leak soak test.

## Verification

- Full `go test -race ./...`: **122 top-level tests, 277 including subtests**, across six packages. This includes eight randomized model seeds, 32 contention rounds and five rounds of each killed-writer scenario. No test or race-detector failure.
- Expanded state-machine oracle: **64 seeds × 160 operations = 10,240 operations**, 216,060 task-state checks, 216,060 outcome checks, 104,692 immutable-record checks and 10,240 availability-event checks. Completed in 29.56 seconds without race instrumentation.
- Final restore regressions were also run with the archive/backup chaos tests under `-race` after correcting owner validation: **13.806 seconds, passed**. A legal task owner longer than 1,024 bytes now round-trips; subscription sessions retain their existing limit.
- New regressions cover 1,000-deep ancestor notifications, duplicate/mixed graph edges, pagination, read snapshots during concurrent writes, bounded event pages, a 2 MiB unused queue payload followed by completion, and 1,000 quiet subscriptions before pending work.
- Restore checks include a backup above 64 MiB, malformed scalar/content/schema/graph/FK inputs, full-disk/read-only rollback, 32 truncations, concurrent live snapshots, source-trigger exclusion, source immutability and interrupted publication.
- Final static Linux release binary: **20 process tests passed**, including CLI concurrency, durable delivery, SQLite round-trip and paginated output. The macOS arm64 binary was cross-compiled; it was not run on macOS here.

An initial 64-seed race run was interrupted for runtime budgeting and superseded by the completed full default-seed race suite and separate expanded-model run above. The broad model checks are deterministic correctness checks; race instrumentation covers the full suite and concurrency tests. No timing assertion is used as a correctness gate.

Native Codex automatic delivery remains outside the verified boundary of this environment; the existing hook/queue protocol tests and installed-hook harness remain available. The earlier three-Luna live trial is recorded separately in CHAOS_VALIDATION.md and was not rerun for 0.2.0.

## Reproduction

Run on Linux amd64 with Go 1.27.4, the same bundled modernc SQLite 3.54.1 as the baseline, and an AMD EPYC 9V74 host exposing nine logical CPUs. Performance runs use GOGC=100, no GOMEMLIMIT and no race instrumentation. The Go supervisor records fresh-child elapsed time and Linux peak RSS; stdout goes to a file. Operating-system caches were not flushed. Timings use the normal unstripped Linux build; distributed binaries use `CGO_ENABLED=0`, `-trimpath` and stripped debug symbols. The final Linux release binary passed the process tests separately; compiler flags were not used to claim an additional speedup.

```sh
go test -run '^$' -bench '^BenchmarkStoreScale$' -benchtime=3x -benchmem ./internal/store
go test -run '^$' -bench '^BenchmarkLineageScale$' -benchtime=1x -benchmem ./internal/store
go test -run '^$' -bench '^BenchmarkNotificationBacklog$' -benchtime=1x -benchmem ./internal/store

go build -buildvcs=false -o bin/taskctl ./cmd/taskctl
go build -buildvcs=false -o /tmp/taskctl-perf-measure ./tests/perf
python3 tests/perf/cli_scale.py --root /absolute/fresh-perf-directory \
  --binary /absolute/taskctl/bin/taskctl --measure /tmp/taskctl-perf-measure
python3 tests/perf/check_edges.py /absolute/fresh-perf-directory \
  /absolute/taskctl/bin/taskctl /tmp/taskctl-perf-measure
```

Use an empty disposable directory with at least 3 GiB of free disk space for the CLI fixtures and transient restore/WAL files. Successful restored verification fixtures are removed after checking; the measured source fixtures and backup files remain. The evidence bundle contains raw before/after measurements and test logs, excluding regenerable databases.

Primary references: [SQLite VACUUM INTO](https://www.sqlite.org/lang_vacuum.html), [SQLite transactions](https://www.sqlite.org/lang_transaction.html), [SQLite isolation](https://www.sqlite.org/isolation.html), and [Go benchmark documentation](https://pkg.go.dev/testing#hdr-Benchmarks). SQLite's documented snapshot and transaction behaviour underpins the design; the timing and memory figures above are our measurements.
