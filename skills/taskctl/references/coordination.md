# Coordinate agents

## Select or create the project

Run `taskctl projects show --json`. Project selection is `--project ID`, then `TASKCTL_PROJECT`, then the nearest ancestor `.taskctl.json`. Carry the project ID into other worktrees and subagent instructions. Resolve selection or storage failures before creating anything; create a project only for a new backlog the user requested.

For new work, use `projects create --name NAME --path DIR`. Attach existing worktrees to that same project with `projects attach --project ID --path DIR`.

## Define and assign work

Inspect the current tasks before adding overlapping work; use the [inspection reference](inspection.md) for filters and pagination. A brief states a concrete remaining outcome, acceptance evidence and relevant repository paths. Use prerequisites for ordering, tags for discovery, and metadata for worktree or agent-session references. Give retries of the same creation a stable `--request-id` and identical input. Use `taskctl --help` for exact flags.

Give each worker the project ID, task ID, worktree, skill path and its session identity. Have the worker claim its task. If claiming on its behalf, use the worker's identity and pass the returned token explicitly. Each writer needs an isolated worktree or agreed file boundary: task claims protect database ownership, not filesystem writes.

For Codex hooks, match the hook's `agent_id`, falling back to `session_id`. A nickname will not match. Read the [hooks reference](hooks.md) when installing automatic activity and notification delivery.
