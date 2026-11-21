# 0004: Store subscriptions and delivery claims in SQLite

Status: Accepted and implemented, 2026-11-21

A subscription is durable database state. It records a task and its replacement descendants, a subscriber session, the acknowledgement cursor, pending initial snapshot, and a separately fenced delivery claim. Task changes and their event records commit together.

Offering a batch claims it in a short immediate transaction and returns a random token, lease expiry and exact through-event ID. Another hook or watcher cannot offer that subscription while its delivery lease is active. Acknowledgement validates the current token and offered range before advancing the cursor. A callback runs after the transaction is released and is acknowledged only after exit 0. Expiry permits redelivery but never permits a stale token to acknowledge a newer offer.

External effects are at least once. A crash after a callback succeeds but before its ACK can repeat the callback. Consumers use event IDs to deduplicate effects where needed. Task ownership uses separate non-expiring claim tokens; a notification timeout cannot reassign work.

Codex hook output itself is not proof the model received context. Keep the batch until explicit acknowledgement. New subscriptions carry a current snapshot even if their target already completed. Active hooks remain quiet when nothing is pending.

Idle resumption requires explicit subscription opt-in and an external queue watcher. Its scan cursor and accepted wake-up watermark are separate from the acknowledgement cursor. Otherwise a page of unacknowledged progress could permanently hide a later completion event. Queue-only delivery tokens cannot acknowledge agent context. Successful queue submission records transport acceptance, releases the offer, and leaves the actual context pending for an active hook or explicit read.

Completion, cancellation, newly available work and inactivity justify an opted-in wake. Progress alone does not. Availability events are derived and inserted in the same transaction that makes work claimable, including completion of a prerequisite outside the subscriber’s task lineage.

This needs no background process for ordinary task operations or active hook checks. Foreground observation and idle resumption require a running observer. No database lock is held during polling waits or external commands.
