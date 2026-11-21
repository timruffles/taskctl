# 0005: Recovery boundaries and uncertain command outcomes

Status: accepted after V1 chaos testing.

## Decisions

Normal commands must open existing validated storage. Only explicit project creation and restore can initialize a database. Absence or damage is an operational failure, never evidence that the task frontier is empty.

Task creation accepts an optional project-local request ID. The canonical input, created task and events commit in one transaction. Equivalent retries return the original task; reuse with different input conflicts. The receipt remains after task completion or supersession. It is operational state and does not transfer to a restored new project. Empty supplied CLI keys fail rather than silently disabling retry protection.

Other mutations continue to use their existing claim/revision guards. If their command response disappears, the caller reads current state before retrying. A transport failure alone cannot distinguish a rollback from a committed transaction whose reply was lost.

Subscriptions have durable cursors, exclusive delivery tokens and expiring delivery leases. Delivery is at least once. An external transport accepting a message does not acknowledge consumption. No exactly-once claim is made for arbitrary callback effects. Current snapshots and bounded summaries cannot skip pending event identities. An old delivery token cannot acknowledge a new offer.

Task claims do not expire. Inactivity starts an investigation; independently confirmed termination permits explicit takeover using the previous claim token. Supersession then records the saved checkpoint and creates a crisp, self-contained task for what remains. Both takeover and continued supersession return fresh tokens.

## Filesystem boundary

Claim fencing applies to taskctl mutations. The CLI cannot prevent an arbitrary process from writing files it can access. The live trial demonstrated this: a resumed worker's stale task mutation failed, but it subsequently edited a shared source file contrary to its ownership instructions. The controller stopped further edits and the recovery owner reviewed the file.

For file isolation, use separate agent worktrees or an execution harness that restricts writable paths; attach those paths as task metadata. Stopping the old process before handing shared files to a new worker remains the harness's responsibility. Merely issuing a new task token does not revoke operating-system filesystem access.

## Limits

Tests kill application processes and inject bounded storage failures. They do not simulate arbitrary unsynchronised sector writes, power loss or hardware faults. Backup publication is atomic, but a kill before publication may leave a private staging file. Native Codex-client delivery must be verified separately from the installed-hook command harness.
