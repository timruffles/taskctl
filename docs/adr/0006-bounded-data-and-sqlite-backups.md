# 0006: Bound data access and use SQLite backups

Status: Accepted, 2026-11-21; implemented in 0.2.0

## Context

The first performance check found two avoidable sources of growth. JSON archives and SQLite restore accumulated entire tables as Go objects. Routine mutations calculated readiness across all tasks, and outcome calculation expanded every ancestor's descendants. A long replacement chain therefore made an unrelated progress update expensive. Unbounded history and subscription reads also worked against progressive disclosure.

The intended workload is a small number of local agents with an accumulating history. Current operations should depend on relevant current work. Large backups should use sequential row or page access and SQLite's normal storage machinery.

## Decision

### One backup format

Remove JSON archive export and restore. `backup --file FILE.sqlite` creates a consistent snapshot using `VACUUM INTO`, syncs the staging file and publishes it atomically without overwriting an existing file.

`restore --file FILE.sqlite --name NAME` creates a new project. Hold one read-only source transaction and stream rows through reusable scan buffers and prepared inserts into the application's own schema. Validate types, fields, references, lineage and dependency cycles before committing the destination transaction. Never import source triggers, views or other executable schema objects. Failure rolls back the destination.

Release restored active claims, clear subscriptions and creation request receipts, and retain scratch-reference history marked unavailable. Scratch files are disposable and are not backed up. Structured CLI output and supersession plans still use JSON.

### Scope readiness work

Derive readiness from a task's prerequisites and the replacement descendants needed to resolve them. Do not expand every historical root or maintain a persistent outcome cache.

Operations that cannot make other tasks ready skip readiness comparison. Creation, release, prerequisite removal and supersession consider their specific changed or new tasks. Completion considers pending dependents of the completed task and every relevant superseded ancestor. Insert availability events with `INSERT … SELECT` inside the task transaction; retain no full-project ready-ID slice or map.

Check normal graph changes by traversing the proposed edge's reachable tasks. Full restore validation uses indexed SQLite temporary tables and topological removal. Large temporary structures can spill to disk rather than becoming a graph-sized Go object structure.

### Bound discovery and notifications

History, subscription and scratch lists default to 20 records and cap at 200. JSON envelopes contain the named collection, `more` and `next_offset`. `tasks show` includes one scratch page with explicit continuation fields. Existing task-list totals and offsets remain available.

Full event pages use both their event-count limit and a 1 MiB stored-payload target. Permit one oversized event so the consumer can advance; preserve full event content for explicit reads and callbacks. Hooks and Codex queue watchers fetch bounded text summaries and omit unused event data before loading records into Go. Context formatting may compact these summaries further, with omitted detail disclosed.

Use task-indexed event retrieval with bounded candidate IDs and global event ordering. Use the session index to select bounded subscription candidates. Keep event order, replay, exact offered ranges, delivery-token fencing, consumption ACKs and the independent queue cursor unchanged.

`tasks watch --once` without `--after` returns one current snapshot. With `--after`, it performs one event-page check.

### Keep read transactions and maintenance narrow

Pure reads use deferred transactions when multiple statements need one snapshot. Opening a current database skips the schema write transaction; missing additive objects are installed when required. Ownership and delivery mutations retain immediate transactions and atomic checks. No transaction spans external callbacks or waiting.

Scratch mutations refresh only their affected areas in the mutation transaction. Cleanup reads indexed eligible candidates in bounded batches and retains its filesystem budget and fencing. It does not refresh the entire scratch table first.

## Consequences

The CLI changes deliberately require version 0.2.0: JSON archives disappear and previously unbounded collections become paged envelopes. Existing schema-version-1 SQLite projects and backups remain supported; the new indexes are additive.

A backup can be committed as a binary file, but there is no deterministic text archive for Git diffs. The simpler format removes the inconsistent JSON export/restore size policy and the duplicated archive representation.

Memory depends on page contents, the largest individual record and SQLite's working sets and temporary storage. This is not a constant-memory claim. Exact list counts, broad watched replacement trees and full graph validation still process the relevant data. Full callback serialization may retain its bounded page or an oversized individual event. These costs are explicit, and the scale benchmarks in `docs/PERFORMANCE.md` check their practical behaviour.
