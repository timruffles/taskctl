# taskctl 0.1.1: chaos and live-agent validation

Date: 21 November 2026. Environment: Linux amd64, Go 1.27.4, modernc SQLite. macOS arm64 was cross-compiled; it was not executed on macOS.

## Result

The original happy-path exercise was insufficient. This campaign reproduced failures in database opening, notification delivery and restore validation, and added explicit protection for creation retries after an unknown command outcome. The fixes and regression tests are in 0.1.1.

The deterministic campaign passed its invariants. A three-Luna implementation exercise recovered through two agent interruptions, a real SIGKILL of an owned command, two hook-harness SIGKILLs, lost command output and dropped/delayed/duplicated hook context. The final project passes fresh race tests and 22 actual CLI fixtures. The task frontier is empty, history remains navigable, and the surviving worker's original subscription is caught up and silent.

This was supervised recovery. The agents made code and command mistakes, and the controller corrected several of them. Claim fencing worked; shared filesystem access remained an independent boundary. Native Codex-client hook execution could not be verified in this environment.

## Defects and changes

| Failure reproduced or gap exposed | Correction | Main regression evidence |
| --- | --- | --- |
| Deleting a closed project database made a normal list command recreate it and report an empty backlog | `OpenExisting` uses SQLite `mode=rw` and validates identity, version and durable tables before persistent initialization | Missing, empty, foreign, incomplete, wrong-project and future-schema databases fail without being initialized; black-box CLI coverage |
| A successful creation whose response disappeared could create duplicate work when retried | Optional `--request-id` commits the normalized input receipt, task and events together | Eight-process same-key race; real lost response; changed input conflicts; explicitly empty/malformed keys fail |
| A queue watcher's initial snapshot could skip an inactivity event created after subscription | Initial queued delivery pages from its persisted cursor | Reproduction plus seeded delivery model |
| Valid large snapshots or progress text could exceed the hook context budget forever | Adaptively compact detail and reduce/reoffer a page while preserving event identities and ACK range | 512-byte and 3,000-byte budget tests, including large progress/snapshots |
| A future event cursor could silently skip later completion events | Reject cursors beyond the database's global event high-water mark | Future-cursor and filtered-subtree regressions |
| Malformed archives could restore successfully and break later reads or event cursors | Validate stored scalar types, positive IDs, timestamps and event JSON before inserting | Eight formerly accepted malformed archives now rejected atomically |
| Operational storage failures lacked useful classification | Preserve `storage_full`, `storage_io`, and `storage_read_only`, including during restore | Bounded full-disk/read-only failure, rollback and recovery tests |

Full-disk rollback already worked in the reproduced case; its error classification was improved. Creation without a request ID remains intentionally non-idempotent. Reuse the same key and equivalent input after an unknown creation outcome; inspect state before retrying other mutations.

Request receipts are project-local operational state. They survive process restarts and task supersession, but restore into a new project resets them. Schema version remains 1 with additive tables.

## Deterministic campaign

### State and concurrency

The seeded state machine compares store results against a separate task/lineage/dependency model after each operation. It checks ownership, immutable originals, derived outcomes, availability and associated events, with periodic reopen and integrity checks.

| Measure | Observed |
| --- | ---: |
| Seeds, inclusive | 20261121–20261184 |
| Seeds completed | 64 |
| State-machine operations | 10,240 |
| Accepted operations | 5,253 |
| Expected rejected operations | 4,987 |
| Task-state comparisons | 216,060 |
| Derived-outcome comparisons | 216,060 |
| Immutable-record checks | 104,692 |
| Availability checks | 10,240 |
| Availability-event checks | 10,240 |
| Store reopen cycles | 668 |
| Integrity checks | 384 |

Additional tests exercised:

- 32 contention waves, each with eight competing claims, eight old-owner/takeover attempts and eight supersessions: **768 concurrent API attempts**. Exactly one owner and one competing supersession win; stale tokens cannot write after reassignment.
- Eight separate processes creating with one request ID: one task and its two initial events.
- 18 event-insertion failure points: the complete mutation and event set roll back together, and retry succeeds after removing the fault.
- 15 real writer SIGKILLs: five before supersession commit, five after supersession commit, and five after keyed creation commit. Each precommit case forces **424,392 bytes of dirty, uncommitted WAL** before the kill. Recovery preserves either the old committed state or the complete committed new state.
- Busy-writer timeout and subsequent recovery, without partial mutation.

The first extended invocation hit Go's default ten-minute test timeout after 45 complete seeds. No invariant assertion had failed. The unfinished seed and remaining 18 seeds were run with a longer explicit timeout, followed by the extended kill/contention cases. The combined evidence contains every seed exactly once. The initial timeout remains in the raw log rather than being hidden.

### Notifications and hooks

Twelve deterministic seeds each executed 256 delivery decisions: **3,072 decisions** total. These included 389 connection restart cycles, 348 delayed-ACK attempts, 164 duplicate-ACK retries, 385 invalid token/range pairs, and 54 accepted-queue/lost-receipt faults. All **566 in-scope events** were eventually consumed in order. The campaign also included 24 supersessions and 12 partial cancellations.

Two consumer processes were SIGKILLed after committed offers. External acceptance was deliberately recorded before a kill in one path: recovery can repeat the external acceptance, as expected under at-least-once delivery. Queue acceptance is not a consumption ACK.

Thirty-two simultaneous hook calls over eight database connections exercised offer contention. Quiet hooks, output failures, stale leases and bounded-context recovery were also checked. No exactly-once guarantee is claimed for arbitrary external callback effects.

### Filesystem and archives

- Four real scratch cleanup SIGKILL barriers: after reservation, during partial deletion, after marker unlink, and after directory unlink. Reopening and lease expiry recover safely.
- Forty scratch attachment/cleanup races. Live references, foreign files, worktree metadata and substituted outside symlinks remain protected. Permission faults, expired leases and paths over 4 KiB are covered.
- Two real backup publication SIGKILL barriers. Existing destinations remain intact; a public backup is absent or complete.
- 160 concurrent progress commits observed through 24 consistent exports/backups.
- 32 bounded output failures and 32 truncated archives, followed by retries/integrity checks.
- Eight malformed restore cases, including invalid event JSON, blank kinds, zero/negative IDs, invalid timestamps and coercible-but-invalid scalar types.
- SQLite full-disk simulation uses a bounded page limit. The test does not fill the host disk.

Across the selected campaign there are **23 scoped subprocess kills**: 15 writers, two consumers, four scratch phases and two backup phases. The ordinary full suite repeats several of those cases; this number does not inflate the campaign by counting repeat runs.

### Overall verification

`go test -race -p 1 -json ./...` passed all six packages: **100 top-level tests**, **237 passes including subtests**. After the final explicit-empty-request-ID CLI guard, its focused race regression was rerun and passed. `go vet -p 1 ./...` is clean. The separate ledger module passed a fresh, uncached race test after the controller's final CLI correction.

## Real Luna exercise

Three actual `gpt-6-luna` agents built the standard-library-only CSV ledger in `examples/ledger`:

- **chaos-a:** initial integer-cents parser, then README after being resumed.
- **chaos-b:** CSV input and checked totals; later the second recovery owner.
- **chaos-c:** command-line interface and the first recovery owner.

Every agent command and edit went through `tests/agent_harness/harness.py`. Each harness identity had real generated SessionStart/PostToolUse hook configuration installed. The harness executed the configured taskctl process automatically and returned hook context at tool-result boundaries. Agents read context and issued their own explicit ACK commands. There were no manually synthesized hook calls masquerading as agent integration.

The test shortened inactivity to 20 seconds and delivery leases to ten seconds; a 25-second delayed notification deliberately outlived its lease. Leases were then restored to five minutes for final convergence. Product defaults remain 30-minute inactivity and five-minute delivery leases.

### Faults and observed recovery

1. **Lost successful creation response.** chaos-a received an unknown-outcome result after its docs task committed. Retrying the same request ID and input returned T6. Final SQL checks confirm one such task.
2. **Dropped, delayed and duplicate hook context.** Each armed fault was consumed. Old ACKs failed; fresh offers and exact-token ACKs allowed the subscription to advance. Progress was not treated as completion.
3. **First worker interruption.** The controller interrupted an active model turn and the harness SIGKILLed its owned gate command. Important code was already in the repository and a progress checkpoint in T2. chaos-c received inactivity, inspected the harness crash record, took over explicitly, and superseded T2 with T7 for verification.
4. **Stale worker resumed.** chaos-a's T2 progress attempt failed with exit 3 and left the old progress unchanged. It later mistyped another claim token; that also failed rather than mutating another owner’s state.
5. **Recovery worker interruption.** The controller interrupted chaos-c after takeover. chaos-b took over its two tasks and replaced the remaining verification work with T9 and T10. chaos-c's queued continuation later attempted to complete T4/T7 with old claims; both were rejected.
6. **Hook harness restarts.** The server was SIGKILLed twice and restarted from its durable job/command records. The task database and notification leases remained authoritative.
7. **Final consumption.** chaos-b read and ACKed the original T1 subscription through event 51. Its next pending call returned zero bytes. Later global events were outside that subscription's scope. The other paused consumers retain their own unacknowledged state; they were not silently acknowledged by the controller.

The harness recorded **149 commands**, including coordinator commands and a deliberately injected server-revocation probe; **152 hook executions**, of which **96 emitted nothing**; and no hook-process failures. These are functional observations under artificial timing pressure, not a performance benchmark.

### What did not work automatically

The agents made shell-quoting, import, test-expectation and command-flag mistakes. One claimed a parser checkpoint was formatted when it still had byte/string comparison errors. Another blamed CSV parsing when its malformed shell command had removed newlines. The README itself needed correction, tracked as T8. Controller review also found four empty-filename argument cases accepted by the demo; these now fail correctly, with a regression test. All 22 real CLI fixtures then passed.

A resumed worker edited a shared source file despite its task claim being obsolete and despite explicit ownership instructions. A queued continuation also ran commands after a model interruption. **Taskctl fenced their task mutations correctly, but it cannot revoke filesystem access.** The harness now revokes new execution for a crashed identity, both before queuing and before process launch. Client- and server-side probes confirm rejected commands cannot create a marker file. This is cooperative test infrastructure, not a security sandbox against identity spoofing or database modification.

For isolated file ownership, use separate worktrees or restricted writable paths in the execution harness and attach their locations as metadata. Claims alone cannot make arbitrary shared-file edits concurrency-safe.

### Final state

| Work | Preserved lineage | Final current leaf |
| --- | --- | --- |
| Parser | T1 → T2 → T7 → T9 | T9 completed |
| CSV/totals | T1 → T3 | T3 completed |
| CLI | T1 → T4 → T10 | T10 completed |
| Documentation | T6; correction T8 depends on T6 | Both completed |
| Controller integration | T5 depends on T1 | Completed |

T1's definition remains **superseded**. Its derived work **outcome is completed**, verified through a current watch snapshot. Both `tasks in-progress` and `tasks available` return total 0.

Live storage, the final SQLite backup and a new project restored from JSON all pass `integrity_check` and `foreign_key_check`, contain the same ten historical tasks, and have no pending/in-progress work. Restored operational request receipts and subscriptions are empty as designed. The inherited scratch area was automatically removed after its final current reference completed; repository code remained intact.

## Reproduction

```sh
go test -race -p 1 -timeout 20m ./...
go vet -p 1 ./...

TASKCTL_CHAOS_SEEDS=64 TASKCTL_CHAOS_STEPS=160 \
TASKCTL_CHAOS_ROUNDS=32 TASKCTL_CHAOS_KILLS=5 \
go test -race -p 1 -timeout 20m ./internal/store \
  -run '^TestChaos(SeededTaskStateMachine|CreateReplayAndLostResponse|EventFailuresRollbackWholeMutation|KilledWriterRecovery|CreateRequestProcessRace|BusyWriterRecoversWithoutPartialMutation|ContentionAcrossOwnersAndDefinitions)$' \
  -count=1
```

For the live workflow, see `tests/agent_harness/README.md`, the demo contract and the evidence archive. The harness does not spawn language models; connect three actual agents to its command wrapper. The evidence includes installed hook configurations, all recorded commands and hook outputs, fault receipts, task history, seed statistics, backup/restore checks and the failed/passed demo fixtures.

## Limits and remaining risks

- **Native Codex integration is unverified here.** Installed `codex 0.172.0-alpha.2` failed during in-process app-server initialization with `Read-only file system (os error 30)`, before a model request or hook execution. A second bounded probe with documented writable SQLite/log locations failed at the same stage. The successful live trial used the explicitly described command harness; it does not prove native Codex idle resumption or queue delivery.
- **Application crash testing is not power-loss simulation.** These tests kill processes and inject bounded failures. They do not reorder unsynchronised disk writes, simulate torn sectors or verify every filesystem/device. SQLite's own [crash-testing documentation](https://www.sqlite.org/testing.html) describes the additional VFS machinery needed for those tests.
- **Backup staging files can survive a kill.** A SIGKILL before atomic publication may leave a private mode-0600 `.taskctl-backup-*` file. The public destination stays absent and retry succeeds. Automatic stale backup-staging cleanup was not added.
- **Local filesystem scope.** Network filesystems, multiple hosts, arbitrary physical corruption and malicious agents are outside this campaign.
- **Model reliability is separate.** This trial demonstrates recoverable task state and notification delivery with supervision. It does not establish that agents will follow file ownership instructions or accurately report tests without independent verification.

The evidence supports using this as a materially better-tested local prototype. It does not establish an error rate or a guarantee that every possible failure has been covered.
