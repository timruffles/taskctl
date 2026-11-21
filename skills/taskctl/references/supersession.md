# Replace stale work

Supersede a pending or in-progress task when substantial work is done or its remaining scope needs a new definition. The old definition becomes immutable; new tasks become the current work. Completed and cancelled tasks are already terminal.

## Preserve the useful context

Read the current task, including its revision, before preparing the replacement:

```sh
taskctl tasks show T1 --json
```

Put completed work, verification and durable repository paths in the supersession summary. Give every replacement a brief that a fresh agent can act on without reading its ancestors: what remains, relevant interfaces or decisions, and the acceptance condition. Keep only the context needed for that work. Tasks inherit tags, metadata, prerequisites and scratch references.

For example, after the parser is implemented, `remaining.json` might contain:

```json
{
  "summary": "Parser implemented in parser/parse.go. Existing parser tests pass. Malformed-input handling and command integration remain.",
  "replacements": [
    {
      "key": "errors",
      "title": "Reject malformed parser input",
      "brief": "The parser is implemented in parser/parse.go. Return typed errors for unterminated strings and invalid escapes, preserving the Parse signature. Add tests covering both cases; the parser suite must pass."
    },
    {
      "key": "integration",
      "title": "Connect the parser to the command",
      "brief": "Replace command parsing in cmd/example with parser.Parse. Preserve typed errors and the documented exit codes. Add CLI cases for valid input and malformed input; both the parser and CLI suites must pass.",
      "prerequisites": ["@errors"]
    }
  ],
  "continue_with": "errors"
}
```

Use the revision from the read and the current claim for in-progress work:

```sh
taskctl tasks supersede T1 --claim CLAIM --revision REVISION --file remaining.json --json
```

For a pending task, omit `--claim`. `continue_with` is for an in-progress task whose owner will keep working; omit it to leave all replacements pending. It must name a ready replacement. The response is an array of the created tasks. Use the returned task IDs and, when continuing, the **new** claim token. The predecessor's token cannot mutate a replacement.

If the revision or claim conflicts, reread the task and revise the plan. Supersession, replacements and events either all commit or all roll back.

## Follow the original intention

`parent_id` identifies the immediate predecessor and `origin_id` the top-level original. `tasks history ID --json` returns a page of that original's lineage, including siblings; follow `more` and `next_offset` when needed. `tasks show ID` includes its latest progress and result. Human history output shows definitions; use JSON history or individual `show` commands for completion summaries.

A prerequisite or subscription on an original follows its replacement branches. It succeeds after all branches complete. Cancellation is a distinct outcome; the other unfinished branches remain visible. Keep subscriptions on the original when the whole intention matters.
