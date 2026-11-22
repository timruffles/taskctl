# V1 implementation contract

This is the original implementation brief. For the current CLI, storage and terminology, use [README](README.md), [design](docs/DESIGN.md) and [decisions](docs/adr/).

Working name: taskctl. Standard library CLI, modernc.org/sqlite v1.62.0, Go 1.26+. Toolchain installed for this workspace at /tmp/taskctl-go-1.27.4/go/bin/go (the system `go` is not the Go compiler).

One project database outside repos in XDG_DATA_HOME/taskctl/projects/ID/tasks.sqlite. XDG_STATE_HOME/taskctl/scratch/ID holds managed scratch. Nearest `.taskctl.json` selects a stable project ID; `--project ID` is explicit. No runtime Git dependency.

## Package ownership

- Root: internal/store/common.go, cmd/taskctl, internal/project, internal/scratch, documentation, live demo and packaging.
- Core agent: internal/store/tasks.go, task-specific types and tests. Define taskSchema constant.
- Notifications agent: internal/store/events.go, subscriptions.go, corresponding types/tests, internal/codex adapter. Define notificationSchema constant. Root implements CLI loops using store methods.
- QA agent: black-box process tests in tests, independent defect review. Do not modify other agents' files.

## Shared store foundation

Store exposes DB *sql.DB, ProjectID string, Now func() time.Time. Open(path, projectID) creates all schema; Close(). `write(fn func(*sql.Tx) error)` wraps BEGIN IMMEDIATE semantics via driver `_txlock=immediate`. `timestamp(time.Time)` fixed UTC nanoseconds. `newID(prefix)` crypto random. `taskNumber(string)` validates full T<number>. `failure(code,message)` *Error. `emit(tx,taskID,kind,message,data)` inserts event in same transaction and returns int64 ID. `refreshScratch(tx)` updates unreferenced times after task changes; supplied by root. `Queryer` interface permits querying through DB or transaction. Agents coordinate names if necessary; prefer exported Go structs matching JSON snake_case.

## Core task contract

Task: ID, Title, Brief, Status (pending/in_progress/completed/cancelled/superseded), OriginID, ParentID, Revision, Owner, ClaimID, Tags []string, Metadata map[string]string, Prerequisites []string, Progress, Result, CreatedAt, UpdatedAt, LastActivityAt. List rows must OMIT briefs/metadata/result/full progress; CLI explicitly projects ListItem, never marshals full Task on cheap lists.

CreateSpec: Key(optional internal reference for replacements), Title, Brief, Tags, Metadata, Prerequisites. SupersedeSpec: Summary, Replacements []CreateSpec, ContinueWith(optional key). New sibling prerequisites can use @key. All replacements self-contained with nonempty title/brief. Inherit original tags/metadata/prerequisites then apply child additions/overrides. Reject cycles across prerequisites and supersession edges. One predecessor, many successors; no merges in V1.

Public methods desired: Create(CreateSpec)(Task,error), Get(id)(Task,error), List(filter string,tags []string,limit,offset int)([]Task,int,error), Claim(id,session)(Task,error), Release(id,claim)(Task,error), Takeover(id,expectedClaim,session,reason)(Task,error), Progress(id,claim,text)(Task,error), Heartbeat(id,claim)error, Complete(id,claim,summary)(Task,error), Cancel(id,claim,reason)(Task,error), Supersede(id,claim string,revision int64,spec SupersedeSpec)([]Task,error), Edit(id,claim string,revision int64,title,brief *string)(Task,error), Tag(id,claim,tag string,remove bool)error, MetadataSet(id,claim,key,value string)error, AddPrerequisite(id,claim,prerequisite string,remove bool)error, History(id)([]Task,error), Outcome(id)(string,error). Outcome follows descendants: pending until all completed; any cancelled means cancelled immediately; superseded itself never becomes completed. Read-only Outcome should use one snapshot.

Task writes validate claim fencing when task in_progress; progress/heartbeat/complete/release require current claim. Planner edits to pending tasks allowed, with revision checks for whole brief/replacement operations. No reopening terminal history. Completion requires nonempty summary; cancellation/release/takeover preserve context. Claims never expire on heartbeat silence. All changes emit events except heartbeat, which only updates activity. A duplicate same-owner claim does NOT grant a new token; return existing claim safely or an explicit conflict; document.

DB tasks numeric id INTEGER PRIMARY KEY AUTOINCREMENT; JSON IDs T1. columns: id,title,brief,status,origin_id,parent_id,revision,owner,claim_id,progress,result,created_at,updated_at,last_activity_at. tags(task_id,tag), metadata(task_id,key,value), prerequisites(task_id,prerequisite_id). Core agent controls schema and may extend; inform notifications/root if names change.

## Notifications contract

events(id INTEGER PK AUTOINCREMENT,task_id INTEGER,kind,message,data TEXT JSON,created_at). Kinds: created,claimed,released,reassigned,progress,completed,cancelled,superseded,edited,tags_changed,metadata_changed,prerequisite_changed,inactive. A subscription follows descendants of its target task (not every task with same origin).

All subscription state, cursors, delivery claim tokens/leases and acknowledgements are in SQLite. No transaction remains open during waiting, callbacks or Codex calls. Stable event IDs, explicit ACK, at-least-once semantics. Delivery lock prevents simultaneous consumers for same subscription; lease expiry permits retry after hook crash; stale acknowledgement must not advance a newer claim. Pending events survive attempted hook output/queue acceptance. Subscription creation includes current state so already completed tasks can notify. Progress coalesced; do not advance past omitted undelivered events. Only explicit opt-in can queue an idle Codex turn; progress alone never queues. Preserve user interruption.

Desired methods (coordinate actual signatures): Events(taskID string,after int64,limit int)(EventPage,error), Subscribe(taskID,session string,resumeIdle bool)(Subscription,error), Subscriptions(session string)([]Subscription,error), Unsubscribe(id,session string)error, Offer(subscriptionID,session string,limit int,lease time.Duration)(Delivery,error), Acknowledge(subscriptionID,session,token string,through int64)error, ReleaseDelivery(subscriptionID,token string)error, DetectInactivity(age time.Duration)(int,error). EventPage fields Events,Cursor,More; Delivery fields Subscription,Token,Through,Events,Snapshot,More. Empty offer produces no output. Current terminal snapshots must be acknowledged without endless repetition.

Codex adapter: decode stdin hook JSON strictly enough to validate required event identity, allow extra upstream fields. session_id root; agent_id when present concrete child. Short asynchronous PostToolUse, SessionStart restore current state. Record only correct owner's activity (requires current owned tasks lookup; session-only bulk heartbeat MUST fence concurrent reassignment within tx). Output valid hookSpecificOutput.additionalContext or nothing. Include concise IDs and ACK command with token; cap context (5 events by default), don't expose all history. No untrusted text as shell arguments; exec.Command separate args for optional queue. Generate hooks config; do NOT install or alter user's real Codex config automatically. Actual queue is only invoked by explicit `watch --codex`/subscription resume configuration, not during adapter tests. Pass executable path through config as vetted user choice. Queue acceptance is not acknowledgement.

## CLI semantics

Commands: projects create/attach/show; tasks create/available/in-progress/blocked/list/show/history/claim/release/takeover/progress/heartbeat/complete/cancel/supersede/edit/tag/untag/metadata/depend/undepend/watch/wait/subscribe/subscriptions/unsubscribe; subscriptions pending/acknowledge; scratch create/list/attach/cleanup; export/backup/restore; integrations codex config/hook. Literal verbs, no abbreviations or implicit mutation. Unknown flags/JSON fields fail. Flags supported after positional IDs. --json machine output, stderr errors, distinct usage/conflict/cancelled/timeout exits. Read output bounded with omitted count; default 20. Watch prints initial current outcome then progress, follows replacements, --after cursor supports replay; wait returns 0 completed, 4 cancelled, 5 timeout; blocking wait needs explicit timeout or runs until terminal. --exec program callback receives event JSON stdin, executes without shell, cursor advances only after success. Watch supports --once for tests.

V1 defaults: 30m inactivity; one event per silence period, no ownership change; 7d scratch grace; cleanup opportunistic on normal commands with a time budget plus explicit cleanup, no deletion of attached worktrees. Tag repeated filters mean AND. Pending/claimed-only task edits. No priorities beyond user tags in V1.

## Validation and live use

Meaningful tests: independent process claim races, stale owner writes after takeover, simultaneous subscriptions offers and stale ACK, atomic supersession failure rollback, effective dependency cycles, cancellation vs completion, event replay after crash, scratch active refs/symlink safety, backup/export restore, short hook output and no output when no changes. Demonstrate with real Luna agents using taskctl on a tiny Go project: one agent core function, another tests, then integration/review task dependent on original; use supersession, progress, subscriptions, completion and breadcrumbs. Record real outcomes and distinguish hook protocol simulation from native Codex automatic delivery.
