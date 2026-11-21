# Inspect work

Use the assigned project ID explicitly when the current directory may select another project. `taskctl projects show --json` identifies the selected project; use [coordination](coordination.md) if selection needs setup or repair.

| Question | Command |
| --- | --- |
| What remains? | `taskctl tasks list --limit 10 --json` |
| What can start? | `taskctl tasks available --limit 10 --json` |
| Who is working, and when were they active? | `taskctl tasks in-progress --json` |
| What is waiting on prerequisites? | `taskctl tasks blocked --json` |
| What is this task's current brief, progress and result? | `taskctl tasks show ID --json` |
| What did an earlier definition say? | `taskctl tasks history ID --json` |
| Has an original task's whole scope finished? | `taskctl tasks watch ID --once --json` |

Filter with relevant `--tag` values; repeated tags mean AND. Default lists contain pending and in-progress definitions. Report superseded originals as history, not additional remaining work. `origin_id` identifies the top-level original; `parent_id` identifies the immediate predecessor.

Follow `omitted` and `next_offset` before making claims about an entire task list. History, subscription and scratch pages use `more` and `next_offset`. Consult history only for a specific unanswered question; JSON history includes progress and results, while human history output shows definitions.

List first, then load individual details that affect the answer. Activity is not evidence of accomplishment: inspect progress or verification when assessing a possible stall. For ownership recovery, read [recovery](recovery.md).
