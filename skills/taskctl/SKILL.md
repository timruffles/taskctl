---
name: taskctl
description: Coordinate local coding agents with taskctl. Use when choosing or claiming work, recording progress, replacing stale task definitions, checking remaining work, receiving task notifications, or recovering interrupted work in a taskctl project.
---

# taskctl

Keep the current tasks an accurate account of the work left. Use the CLI for state changes; use repository files for durable code, decisions and evidence.

## Establish the project

Run `taskctl projects show --json`. Project selection is `--project ID`, then `TASKCTL_PROJECT`, then the nearest ancestor `.taskctl.json`. Carry an explicit project ID into other worktrees and subagent instructions. If selection or storage fails, resolve that failure before creating anything; create a project only for a new backlog the user requested.

Read `taskctl --help` when you need command syntax. Use `--json` for results you will parse. IDs below are examples; use the IDs, revisions and tokens actually returned.

## Inspect or coordinate

- Read `tasks list --limit 10 --json` for all current work, `tasks available` for claimable work, `tasks in-progress` for owners and activity, or `tasks blocked` for unmet prerequisites. Add a relevant `--tag` filter. Read `tasks show ID --json` for the selected task only.
- Follow `omitted` and `next_offset` before making claims about an entire task list. History, subscription and scratch pages instead use `more` and `next_offset`. Consult history only when current definitions leave a specific question unanswered.
- Create tasks with a concrete remaining outcome, acceptance evidence and relevant repository paths in the brief. Use prerequisites for ordering, tags for discovery, and metadata for worktree or agent-session references. Give retries of the same creation a stable `--request-id` and identical input.
- Give each worker the project ID, task ID, worktree, this skill and its session identity. Have the worker claim its task. If the coordinator claims on its behalf, use the worker's identity and pass the returned token explicitly. A task claim coordinates ownership; each writer still needs an isolated worktree or an agreed file boundary.

## Work on a task

1. Read the assigned task with `taskctl tasks show ID --json`. If choosing work, first list `tasks available --limit 10 --json`.
2. Run `taskctl tasks claim ID --session SESSION --json`. Start only after success and retain its `claim_id`. For an existing assignment, verify its current owner and token. With Codex hooks, `SESSION` must be the hook's `agent_id`, falling back to `session_id`; a nickname will not match.
3. Make the change and verify the brief's acceptance conditions. Record a meaningful checkpoint with `tasks progress ID --claim TOKEN --text TEXT`: completed work, evidence or paths, and what remains. Heartbeats only record activity; use the configured hook or `tasks heartbeat ID --claim TOKEN` for that.
4. When a substantial part is finished and the brief has become stale, read [supersession](references/supersession.md) and replace it with the actual remaining tasks. Keep small updates in progress. Use `tasks edit` for a correction to the same scope.
5. Once the task's acceptance conditions hold, run `tasks complete ID --claim TOKEN --summary TEXT`, including verification and durable artifact paths. For unfinished work being handed back, record the checkpoint and run `tasks release ID --claim TOKEN`. A command failure is not a successful transition.

End the handoff with the current task IDs, their actual states, verification performed and remaining work. An implementation task is complete only after verification and a successful completion transition; a partial handoff must identify its released or superseding work.

## Conditional operations

- **Substantial partial completion or changed scope:** read [supersession](references/supersession.md) before replacing tasks. It defines preserved context, revision checks, sibling prerequisites and continued ownership.
- **Waiting for another agent, processing a delivered update, or installing hooks:** read [notifications](references/notifications.md). Use a subscription and the harness's asynchronous delivery when the agent should continue other work.
- **Conflict, uncertain command result, inactivity, or restart:** read [recovery](references/recovery.md) before retrying or taking ownership. A stale claim stops further writes for that task until ownership is resolved.
- **Temporary files:** `taskctl scratch create ID --claim TOKEN --json` returns a disposable directory. Move important findings into the repository before completing or superseding. Replacement tasks inherit scratch references; worktree metadata is a reference, not managed storage.

Keep detailed briefs, history and event payloads out of routine status polling. Hook summaries may omit text; follow their detail commands only when the omitted content affects the next action.
