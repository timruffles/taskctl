# 8. Evolve names the transition to an accurate remaining frontier

Status: accepted, 2026-11-22.

The public command is `tasks evolve`; retired definitions and their transition events are reported as `evolved`. It creates new task IDs, preserves an immutable predecessor and passes forward relevant context. It changes no ownership, prerequisite, notification, note or scratch semantics. `edit` keeps the same task ID, `progress` records a checkpoint, and `complete` records fulfilled work. Evolution is not subtask containment or escalation.

Six fresh-context Luna readers compared terminology-only README variants using the same ten scenarios. Neither name caused confusion about the central operation. The review exposed missing explicit guidance about immutable predecessors and ownership recovery. The [evaluation report](../EVOLVE_EVALUATION.md) records the protocol, answers, scoring and limitations. The naming choice is a design judgement, not a demonstrated model-performance win.

The original `supersede` command and `superseded` status filter remain accepted as input aliases. Plan fields, including `replacements` and `continue_with`, remain unchanged. JSON consumers matching the historical public status or event kind must accept `evolved` with version 0.4.0.

SQLite remains schema 2 and persists `superseded`. Task and event read boundaries map that value to the public spelling; status filters map back. Backups preserve the durable representation. User-written titles, briefs, messages and note bodies are never rewritten.

This small compatibility boundary avoids a data rewrite and preserves interoperability with existing processes. In particular, a 0.3 watcher follows descendants only for stored `superseded`. Replacing that value in a schema migration would let an already-open old watcher incorrectly conclude that unfinished work had completed. Rejecting old binaries on a fresh open would not protect that watcher. Keeping the storage value preserves its interpretation throughout a rolling upgrade.
