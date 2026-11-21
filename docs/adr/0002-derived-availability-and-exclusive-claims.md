# 0002: Derive availability and claim tasks exclusively

Status: Accepted, 2026-11-21

A task is available when it describes current unfinished work, has no owner, and has no unfinished prerequisites. Prerequisites are optional and follow replacement work when their task definitions are superseded. Availability is derived from these facts rather than maintained as a separate readiness flag.

Claiming checks availability, assigns one owning agent session, and marks the task in progress in one atomic operation. Two different sessions racing for the same available task receive one success and one clear conflict. An available-task listing is a snapshot; only a successful claim grants ownership. Parallel work uses separate tasks. Explicit release removes ownership, and reassignment invalidates the previous ownership.

This keeps the meaning of available consistent with what the claim operation permits and prevents two agents from independently treating the same task as their exclusive work. Combining reservation and the transition to in-progress avoids a separate state that could be forgotten or left stale.
