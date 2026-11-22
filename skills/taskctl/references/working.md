# Work on a task

Use the assigned project ID explicitly when crossing directories. Confirm selection with `taskctl projects show --json`; read [coordination](coordination.md) if the project needs setup. Use `--json` for parsed results and `taskctl --help` for exact syntax. Use returned IDs, revisions and tokens in the examples below.

1. Read the assigned task with `taskctl tasks show ID --json`. If choosing work, first list `tasks available --limit 10 --json`.
2. Run `taskctl tasks claim ID --session SESSION --json`. Start after success and retain its `claim_id`. For an existing assignment, verify its current owner and token. With Codex hooks, match `agent_id`, falling back to `session_id`.
3. Implement and verify the brief's acceptance conditions. Record checkpoints with `tasks progress ID --claim TOKEN --title TITLE`: one line, at most 120 characters, stating current progress and what remains. Put optional evidence or investigation detail in `--body-file FILE` (`-` streams stdin). Keep durable decisions in the brief or repository. Heartbeats only record activity; use the configured hook or `tasks heartbeat ID --claim TOKEN` for that.
4. When substantial work is finished and the brief has become stale, read [supersession](supersession.md) and replace it with the actual remaining tasks. Keep small updates in progress; use `tasks edit` for a correction to the same scope.
5. Once acceptance conditions hold, run `tasks complete ID --claim TOKEN --summary TEXT`, including verification and durable artifact paths. For unfinished work being handed back, record the checkpoint and run `tasks release ID --claim TOKEN`. Confirm the transition succeeded.

Add worktree or session references with `tasks metadata ID --claim TOKEN --key KEY --value VALUE`. `scratch create ID --claim TOKEN --json` returns disposable storage; put important code and findings in the repository. Replacements inherit scratch references.

For conflicts, lost responses or stale tokens, read [recovery](recovery.md) before further mutations. Process delivered updates using [notifications](notifications.md).

End the handoff with current task IDs, actual states, verification and remaining work. Completion requires both verification and a successful completion transition. A partial handoff identifies the released or superseding tasks.
