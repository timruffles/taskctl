# 0001: Foreground current tasks and retain superseded definitions

Status: Accepted, 2026-11-21

When substantial work has been completed or scope has changed, fresh tasks describe the remaining work and supersede the previous definition. Everyday task lists foreground this current frontier. Previous definitions remain available through explicit history links, so humans and agents can trace the original intention when needed.

Long-lived definitions tend to become stale and force readers to reconstruct what is actually left. Supersession makes the current state explicit while retaining the earlier context. The tradeoff is that task identity changes as work is reformulated, so the links between definitions must be preserved and discoverable.

Every replacement has a self-contained brief: remaining work, relevant work already completed, decisions that still matter, and references to useful code, results or scratch files. Agents carry forward the context needed to proceed; full historical detail remains available separately.

Completion watches follow replacements automatically and provide progress updates. Supersession produces an event; completion is reported once all remaining branches have completed, including any subsequent replacements. Superseded records retain that state and do not require separate ongoing maintenance.

Scratch remains while a current task references it. When no current task references an area, it becomes eligible for cleanup after a configurable grace period. Supersession preserves any references needed by successors. Activity silence alone does not cause deletion, and important work belongs in the repository.

Prerequisites also follow the remaining replacement branches. Supersession does not satisfy a requirement for completed work.

Cancellation stops pursuing a task and remains distinct from success throughout its replacement history. If one replacement branch is cancelled, watchers receive that cancellation rather than a successful-completion notification for the original work. Other branches continue normally.

The exact progress-update interface and the termination behaviour of a blocking wait command remain to be decided.
