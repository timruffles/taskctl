# Inspect work

Use the assigned project ID explicitly when the current directory may select another project. `taskctl projects show --json` identifies the selected project; use [coordination](coordination.md) if selection needs setup or repair.

| Question | Command |
| --- | --- |
| What remains? | `taskctl tasks list --limit 10 --json` |
| What can start? | `taskctl tasks available --limit 10 --json` |
| Who is working, and when were they active? | `taskctl tasks in-progress --json` |
| What is waiting on prerequisites? | `taskctl tasks blocked --json` |
| What is this task's current brief, progress and result? | `taskctl tasks show ID --json` |
| Which recent progress notes might answer my question? | `taskctl notes list ID --limit 10 --json` |
| What did one note record in detail? | `taskctl notes read NOTE_ID` |
| What did an earlier definition say? | `taskctl tasks history ID --json` |
| Has an original task's whole scope finished? | `taskctl tasks watch ID --once --json` |

Repeat `--tag` to require all listed tags. Repeat `--exclude-tag` to omit tasks carrying any of those tags. Default lists contain pending and in-progress definitions. Report evolved originals as history, not additional remaining work. `origin_id` identifies the top-level original; `parent_id` identifies the immediate predecessor.

Use `--where 'search(title, "query")'` for case-insensitive task search. Search `brief` for task text, or use `--where '"beads_id" in metadata && metadata["beads_id"] == "asc-42"'` for an external ID when that metadata has been set. Use `tasks list --status all` to search completed work as well.

Follow `omitted` and `next_offset` before making claims about an entire task list. History, subscription and scratch pages use `more` and `next_offset`; notes use `more` and `next_before`, passed as `--before`. Consult history for a specific unanswered question. Task views include the latest progress title; note lists contain titles and metadata only. `notes read` streams one Markdown body and does not accept `--json`.

List first, then load individual details that affect the answer. Activity is not evidence of accomplishment: inspect progress or verification when assessing a possible stall. For ownership recovery, read [recovery](recovery.md).
