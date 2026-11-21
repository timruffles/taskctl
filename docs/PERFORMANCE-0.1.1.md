# taskctl 0.1.1: quick performance and memory check

Date: 21 November 2026. This measures the released 0.1.1 implementation. The additions are reusable benchmarks and measurement tools; production behaviour was not changed.

## Findings

Small stores are fast and memory use for ordinary commands is modest. Three scaling problems deserve attention:

1. Availability and ordinary mutations perform project-wide outcome calculation. Historical completed tasks therefore slow current work. Long supersession chains make the global calculation quadratic.
2. JSON export/restore materializes the entire archive in memory. Export also permits files larger than the JSON restore path's 64 MiB input limit. The round-trip failure was reproduced.
3. Quiet notification work can grow with unrelated event history or accumulated subscriptions, even when the model receives no output.

## Real CLI timings

The dataset starts with 20 current tasks: five claimed and 15 pending, each pending task having a completed prerequisite. All other tasks are completed. Every task has a 1 KiB brief, a tag, metadata and ten moderately sized events. The largest dataset has 100,000 tasks and one million events, occupying 524.7 MiB on disk.

Values below are median elapsed time from three fresh CLI processes, including startup and output. These are diagnostic shared-host measurements, not stable percentile estimates.

| Operation | 1,000 tasks | 10,000 tasks | 100,000 tasks |
| --- | ---: | ---: | ---: |
| Show one task | 4.2 ms | 4.1 ms | 3.3 ms |
| List in progress | 7.5 ms | 3.5 ms | 3.7 ms |
| List available, limit 20 | 7.3 ms | 35.3 ms | 334.9 ms |
| Current snapshot of one completed task | 6.9 ms | 26.2 ms | 261.6 ms |
| Progress update | 12.4 ms | 43.9 ms | 449.8 ms |
| Claim one pending task | 9.5 ms | 44.2 ms | 403.1 ms |
| Caught-up PostToolUse hook | 5.6 ms | 5.4 ms | 5.8 ms |

At 100,000 tasks, peak process RSS was 8.4 MiB for a single read/in-progress list, 7.5 MiB for the caught-up hook, 31.6 MiB for available listing, and about 24–25 MiB for snapshot/progress/claim operations. Output limits bound the returned data but do not bound the work done by the SQL query.

An independent in-process Go benchmark produced the same scaling pattern. With 20 current tasks, progress rose from 0.92 ms at 100 total tasks to 390.32 ms at 100,000. The different absolute values reflect shorter payloads, one event per task, and no CLI startup/output.

When the current frontier itself grows to about 100,000 pending tasks, the store benchmark recorded:

| Operation | Time | Go allocation per operation |
| --- | ---: | ---: |
| Available list, limit 20 | 443.69 ms | 46.8 KiB |
| Snapshot of one leaf | 326.56 ms | 7.71 MiB / 200,234 allocations |
| Progress | 736.80 ms | 13.14 MiB / 400,029 allocations |

Go allocation counts do not represent peak RSS or retained memory. The separate process measurements include resident SQLite and Go memory.

## Replacement chains: the largest algorithmic problem

The fixture is one superseded chain ending in a claimed leaf, with one further pending task depending on the original root. That dependency matters: SQLite can skip outcome calculation in a fixture with no relevant prerequisites, concealing the problem.

| Definitions in chain | Total tasks | Store progress update | Store snapshot of the current leaf |
| --- | ---: | ---: | ---: |
| 100 | 101 | 36.76 ms | 17.05 ms |
| 300 | 301 | 330.01 ms | 168.11 ms |
| 1,000 | 1,001 | 4,839.03 ms | 2,336.30 ms |

These are single bounded diagnostic samples. A real CLI progress update at depth 1,000 took 4,987.55 ms and 24.2 MiB peak RSS. `tasks watch T1000 --once` took 4,794.72 ms: for a nonterminal task it currently builds the initial snapshot and then builds another snapshot in its first poll. Explicit full history of this 1,000-definition lineage took 37.13 ms and 23.4 MiB RSS.

The cause is `outcomeCTE` in `internal/store/tasks.go`. It starts from every task and expands descendants for every superseded ancestor. A chain with D definitions produces approximately D(D+1)/2 intermediate rows. The 1,000-definition fixture produces 500,501 rows including the separate dependent.

`taskWrite` calls `availableIDs` before and after mutations, including progress, takeover, claim, edits, tags and metadata. Most of these cannot make another task newly available. `snapshotWith` also builds the entire project's available-ID set before returning its bounded subtree view.

The bundled-driver query plan confirms a global task scan, recursive expansion and temporary grouping. This is an algorithmic problem; adding an index alone will not remove the quadratic expansion.

## Concurrent agents

Eight separate CLI processes updated eight different, validly claimed tasks at once. All completed successfully:

| Historical store size | Total burst elapsed | Fastest / slowest operation |
| --- | ---: | ---: |
| 1,000 tasks | 165 ms | 20 / 154 ms |
| 100,000 tasks | 4,274 ms | 463 / 4,256 ms |

The global calculations run inside write transactions, so their cost is also paid as waiting by other agents. Normal CLI opening performs a schema/identity transaction; several read-like methods also use immediate transactions. Slow calculations can therefore delay otherwise cheap commands. This test demonstrates the latency effect; it did not produce a concurrency correctness failure.

## Quiet hooks

A caught-up subscription with no large newer event tail stayed around 5–6 ms even with one million historical events. Two different shapes expose growth:

| Quiet-hook fixture | Median elapsed | Peak RSS |
| --- | ---: | ---: |
| No unrelated later events | 6.1 ms | 7.3 MiB |
| 10,000 unrelated later events | 7.1 ms | 8.8 MiB |
| 100,000 unrelated later events | 16.1 ms | 11.1 MiB |
| 500,000 unrelated later events | 59.3 ms | 11.6 MiB |
| One quiet subscription | 6.3 ms | 7.3 MiB |
| Ten quiet subscriptions | 9.0 ms | 7.6 MiB |
| 100 quiet subscriptions | 17.5 ms | 8.9 MiB |
| 1,000 quiet subscriptions | 106.5 ms | 14.4 MiB |

The event query's actual plan uses the global event ID range, then filters by task scope. It can repeatedly scan the same unrelated tail because an empty page leaves the cursor unchanged. The existing `(task_id,id)` index is not selected in that plan.

The hook also attempts one delivery transaction per quiet subscription. Its output/event limits cap delivered context, rather than the number of quiet subscriptions examined. The session query `WHERE (?='' OR session=?)` scans subscriptions; the equivalent bound `WHERE session=?` uses the session index. The report's query-plan evidence comes from the same bundled modernc driver, SQLite 3.54.1, rather than Python's SQLite planner.

## Archive memory and a round-trip defect

For 10,000 tasks / 100,000 original events, the database is 52.32 MiB and its JSON export is 63.43 MiB:

| Operation | Elapsed | Peak process RSS |
| --- | ---: | ---: |
| JSON export | 1.405 s | 577.8 MiB |
| SQLite backup | 0.148 s | 14.9 MiB |
| JSON restore | 3.374 s | 258.1 MiB |
| SQLite-format restore | 5.279 s | 192.8 MiB |

These heavier operations were measured once. JSON export constructs a `map[string][]map[string]any` for all tables and events before encoding. Both restore formats also materialize the archive. SQLite backup uses `VACUUM INTO` and avoids that Go object graph. Its low backup memory usage should not be confused with streaming restore.

**A successful JSON export can be too large for taskctl to restore.** Adding 13 characters to each event message increased the valid export to 67,815,214 bytes (64.67 MiB). Export succeeded. Restore returned exit 2 with `invalid_backup: unexpected EOF`, because `ReadArchive` wraps the input in a 64 MiB + 1 byte limited reader. The SQLite backup restored successfully.

This is a correctness issue as well as a performance issue. The size policy needs to be explicit and compatible across export and restore. Large JSON exports should not be trusted for round trips until it is corrected. SQLite backup is the lower-memory route available now, although its restore memory still grows with the archive.

The 100,000-task / one-million-event dataset was used for ordinary operations only; exporting that whole dataset was unnecessary after the smaller case established the allocation growth. No measured multi-gigabyte export claim is made.

## Recommended order of changes

1. **Fix the JSON export/restore size mismatch.** Make the supported size policy explicit and consistent; use streaming serialization/deserialization to address memory growth. Removing the read limit alone would not fix the memory problem.
2. **Skip availability comparison for mutations that cannot change readiness.** Progress, ownership reassignment, title/brief edits, tags and metadata should remain small indexed updates. Claims remove available work but cannot create newly available work.
3. **Scope outcome and snapshot queries to the requested work and required prerequisite roots.** Eliminate the all-ancestor closure. First try scoped queries; a persistent cache adds invalidation complexity and is not necessary as the first optimisation.
4. **Bound quiet notification work.** Test task-indexed event retrieval against broad subtrees, or add a separate scanned-through position that does not acknowledge unseen events. Use the session index and batch quiet checks. Preserve offer-token/ACK fencing and fairness.
5. **Use deferred read transactions where no mutation is needed.** Preserve consistent snapshots without reserving the writer lock. Keep atomic write transactions for ownership and delivery changes.

Further concerns visible in the code are unpaginated full lineage history, whole-graph cycle checks, unbounded event payload bytes before hook summarisation, and scratch database candidate scans outside the filesystem cleanup budget. They were reviewed but not separately load-tested. Event and request-receipt retention grows by design; this is not evidence of a memory leak.

Optimisation must retain the existing correctness boundaries: claim readiness and its event commit remain atomic; a prerequisite can refer to a superseded original outside the displayed subtree; completion can unblock dependants of any ancestor; and cancellation still takes precedence. Event query changes must preserve global event-ID order, exact ACK ranges and replay after expired offers. Streaming archives must preserve consistent snapshots, deterministic ordering, strict validation, atomic publication and whole-operation restore rollback.

## Method, evidence and reproduction

- Linux x86_64; Go 1.27.4; AMD EPYC 9V74, nine visible logical CPUs; shared host.
- Released taskctl 0.1.1 binary; its SHA-256 is recorded in `results/environment.json`.
- Runtime `GOGC=100`, no `GOMEMLIMIT` or race instrumentation. Compilation used separate memory-conscious settings.
- Go benchmarks exclude bulk fixture construction and report allocations. CLI measurements include fresh-process startup and output. Peak RSS is from the Go supervisor's child `ProcessState.SysUsage`.
- Three iterations for flat store/CLI measurements; single samples for deep lineage, archive operations and concurrent bursts. Filesystem caches were not flushed. Small millisecond differences are noise, not improvements.
- This check establishes scaling hazards. It does not establish p95/p99 latency, leak freedom, or a universal production capacity.

The benchmark kit contains the new `internal/store/performance_test.go`, `tests/perf` tools, raw logs, structured measurements and exact query-plan inputs. Overlay its `source/` tree onto the 0.1.1 source release, then use:

```sh
go test -run '^$' -bench '^BenchmarkStoreScale$' \
  -benchtime=3x -benchmem -timeout=5m ./internal/store
go test -run '^$' -bench '^BenchmarkLineageScale$' \
  -benchtime=1x -benchmem -timeout=2m ./internal/store

go build -buildvcs=false -o /tmp/taskctl-perf-measure ./tests/perf
python3 tests/perf/cli_scale.py --root /absolute/fresh-perf-directory \
  --binary /absolute/taskctl/bin/taskctl --measure /tmp/taskctl-perf-measure
python3 tests/perf/check_edges.py /absolute/fresh-perf-directory \
  /absolute/taskctl/bin/taskctl /tmp/taskctl-perf-measure
```

The CLI fixtures need roughly 1 GiB of disposable storage. Keep them separate from real projects. `check_edges.py` extends those synthetic fixtures and intentionally verifies the current oversized-JSON failure.

Method references: [Go benchmark documentation](https://pkg.go.dev/testing#hdr-Benchmarks) and [SQLite EXPLAIN QUERY PLAN](https://www.sqlite.org/eqp.html). Query plans are diagnostic evidence, not a stable API parsed by taskctl.
