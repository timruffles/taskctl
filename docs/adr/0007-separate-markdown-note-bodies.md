# ADR 0007: Separate Markdown note bodies from task metadata

Date: 2026-11-22

Status: accepted

## Context

Running notes can grow much longer than the state needed to coordinate work. Keeping full progress text in task rows and event messages makes ordinary reads expensive and encourages historical notes to enter agent context. The current task view needs the latest concise update; recovery may need older details.

## Decision

Store a required single-line title, at most 120 Unicode characters, in the main task database with the note identity, task, event, author and timestamp. Keep optional Markdown bodies in a separate `notes.sqlite`, stored as indexed 64 KiB chunks with their byte length and SHA-256. Input and explicit body reads stream; task views, lists and notifications do not load body chunks.

Use explicit commands for paged title discovery, metadata inspection, reading one body and deleting one note. Deletion retains a text-free record so history references remain meaningful. Deleting the current note clears its displayed progress rather than reviving an earlier update. Freed SQLite pages can be reused; this release does not implement physical compaction or automatic summarisation.

This design keeps Markdown content without creating a file for every note or maintaining offsets into a shared journal. Body publication must succeed before the main database publishes a reference. Publication, deletion and backups coordinate to preserve referential consistency; failure recovery must tolerate unreferenced body data.

[SQLite WAL transactions are atomic per database, not across attached databases](https://www.sqlite.org/lang_attach.html). A project-local advisory lock coordinates operations spanning both stores. Bodies commit first; the task transaction rechecks the claim before publishing the note. Deletion commits its tombstone before removing body chunks, so interrupted cleanup can be retried. A crash may leave an unreferenced body or unpublished initialisation file; automatic collection of those leftovers is not included.

A complete backup directory includes the main SQLite snapshot and the body snapshot when referenced, plus a completion marker. A single-file backup remains available only when it cannot omit live bodies. Version-1 progress text is migrated into notes; legacy progress CLI input remains accepted.

## Consequences

Ordinary context and reads remain bounded as note history grows. Full note text is still available when explicitly requested. The body store can grow independently and deletion does not promise immediate file shrinkage. Backups must cover both stores consistently. Notes are durable project data, distinct from automatically cleaned scratch.

Migration reads old TEXT values through SQLite's [incremental BLOB interface](https://www.sqlite.org/c3ref/blob_open.html), which also supports TEXT. SQL substring reads can materialise the entire original value internally. The existing SQLite dependency supplies this interface; no additional driver is introduced. Restore stages a consistent source snapshot and batches body writes before committing their references.
