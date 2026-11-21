# Agent workflow

Use `--json` for structured results. The project is selected by `--project`, `TASKCTL_PROJECT`, or the nearest `.taskctl.json`.

1. Read `tasks available --limit 10 --json`. Read `tasks show ID --json` only for the task you are considering.
2. Claim it with `tasks claim ID --session YOUR_SESSION --json`. Keep the returned `claim_id`. Only a successful claim grants ownership; listings are snapshots. For Codex hooks, use your `agent_id` when present, otherwise your `session_id`.
3. Use that claim token for progress, metadata changes, heartbeat, scratch creation, completion or release. A conflict means reread state; do not blindly take over.
4. Record meaningful checkpoints with `tasks progress ID --claim TOKEN --text TEXT`. Heartbeats describe activity, not accomplishment, and remain silent.
5. When substantial work is finished and a stale brief no longer describes what remains, supersede it. Read its revision, write a plan JSON with a completed-work summary and self-contained replacement briefs, then run `tasks supersede ID --claim TOKEN --revision N --file PLAN`. Optional `continue_with` transfers you to one ready successor and returns a new token. Preserve useful paths, decisions and verified results in the new brief. Ancestors are optional background.
6. Complete with `tasks complete ID --claim TOKEN --summary TEXT`, recording actual verification. Release unfinished work explicitly when another agent should take it. Never treat inactivity as permission to steal a claim.

Use tags to filter work and metadata for worktree/session references. Scratch is disposable and inherited by replacements. Put important code and findings in the repository; tasks should link to them.

Subscribe to original work when replacements should remain in scope. Read a delivered batch before acknowledging its exact token and through-event ID. An accepted queued wake is not an ACK or proof of completion. If a delivery lease expires, request a fresh batch. Prefer configured asynchronous hooks to blocking an active agent with a watch.

Use a stable `--request-id` when creating work that might be retried after a lost response. Retry the same key with the same input. For other mutations with an unknown result, inspect current state before deciding whether to retry. A heartbeat or inactivity event does not establish whether an agent is alive; confirm termination before explicit takeover. After takeover or supersession, use the newly returned claim token.

Use `tasks in-progress` to inspect the current work. Use `tasks show` for current detail and `tasks history ID --json` only when the original intent or completed work is needed.

History, subscription and scratch lists are paged. Read `more` and use `next_offset` to continue; task detail uses `scratch_more` and `scratch_next_offset`. Full event pages may end at the byte budget before the count limit: acknowledge only their exact offered range, then request the next page. Hook and queue summaries can omit detail; read the full task or events when needed. Use SQLite `backup --file FILE.sqlite` and `restore --file FILE.sqlite --name NAME`; JSON archives are not supported.
