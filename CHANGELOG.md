# Changelog

## 0.4.0 — 2026-11-22

- Rename `tasks supersede` to `tasks evolve`; task statuses and event kinds are reported as `evolved`. Existing command and status-filter spellings remain accepted as input aliases. Plan fields and operation semantics are unchanged.
- Keep schema 2 and its durable spelling so existing databases, backups and already-running older watchers remain compatible. JSON consumers matching the old output spelling must use `evolved`.
- Clarify new task IDs, immutable predecessor definitions, fresh continuation claims, small edits and recovery after losing ownership. Keep agent guidance in the relevant progressive-disclosure reference.
- Record a controlled six-reader Luna comparison of README terminology, its limitations, and a reproducible Beads performance comparison.

## 0.3.0 — 2026-11-22

Progress notes with bounded context and a separate body store.

- Require a single-line title of at most 120 characters for new `progress --title` updates; stream optional Markdown from `--body-file`.
- Keep note metadata and latest titles in `tasks.sqlite`; store long bodies in indexed 64 KiB chunks in a separate `notes.sqlite`.
- Add paged note lists, metadata inspection, explicit streamed body reads and individual deletion. Deleting the current note does not revive older progress.
- Preserve legacy `--text`/`--text-file` inputs and migrate existing long progress into note bodies.
- Keep routine progress silent in Codex hooks; state-change deliveries and resumed snapshots expose only the latest relevant title per task.
- Add complete directory backups of both databases. Single-file backups reject stores with live bodies rather than silently losing them.

Schema-version-1 projects and backups are migrated to version 2. Older binaries cannot open the upgraded store. Compaction is not included.

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
