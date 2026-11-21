# Changelog

## 0.2.0 — 2026-11-21

Bounded data access and performance release.

- Remove JSON archive export and restore. Backups and restores use SQLite only.
- Stream validated restore rows into a new project; keep atomic rollback and claim/subscription resets.
- Scope readiness queries and availability events to affected work; remove whole-project calculations from progress and ownership changes.
- Replace full-graph Go buffers with SQLite temporary tables for restore validation.
- Page history, subscription and scratch lists with explicit continuation fields.
- Seek scoped event indexes, budget full event pages by bytes, and fetch bounded hook/queue summaries before formatting.
- Use deferred read snapshots, skip unnecessary schema writer transactions, and refresh only affected scratch areas.
- Make `tasks watch --once` without `--after` return one current snapshot.

Existing SQLite schema-version-1 projects remain supported. JSON list envelope changes are intentional; see README for exact fields. See `docs/PERFORMANCE.md` for measured improvements, correctness checks and remaining limits.

## 0.1.1 — 2026-11-21

Recovery and failure testing release.

- Add optional `tasks create --request-id KEY` with an atomic project-local receipt, so a retry after a lost response returns the original task. Conflicting input and explicitly empty keys fail.
- Open normal commands with `OpenExisting`; missing, empty, foreign or incomplete databases cannot become a successful empty frontier.
- Reject malformed archive scalar values, event JSON, identifiers and timestamps before restore commits. Keep operational storage failures distinct from malformed input.
- Classify full-disk, I/O and read-only failures explicitly.
- Preserve events arriving between subscription creation and its initial queued snapshot.
- Adapt hook summaries to the context budget so a large snapshot or progress message cannot indefinitely prevent delivery.
- Reject event cursors beyond the current database high-water mark.
- Add deterministic seeded model tests, concurrent process races, real SIGKILL tests, filesystem/archive fault tests, and an installed-hook harness for a three-Luna recovery trial.

See `docs/CHAOS_VALIDATION.md` for evidence and limitations. Schema version remains 1; the new request-receipt table is additive. Restore into a new project resets operational request receipts.

## 0.1.0 — 2026-11-21

Initial local Go/SQLite CLI, frontier replacement model, claims, tags, metadata, disposable scratch, subscriptions, Codex adapters, deterministic export and SQLite backup/restore.
