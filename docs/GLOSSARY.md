# Local agent task tracking

Shared language for a local command-line tool that keeps a small number of coding agents' work current and traceable.

## Language

**Project**:
A named collection of tasks with a stable identity and one shared local store. Its associated checkouts and worktrees use the same tasks and claims.

**Task**:
A recorded definition of work for an agent or person to perform. When substantial progress or a change of scope makes the definition stale, evolve it into fresh tasks describing the remaining work.
_Avoid_: Ticket, issue when referring to the same record.

**Frontier**:
The current task definitions describing work still to do. These tasks are foregrounded in everyday views; earlier definitions are discoverable through history.

**Evolved task**:
A previous task definition replaced by one or more newer definitions of the remaining work. It is retained for reference and excluded from the frontier.

**Task lineage**:
The links connecting current tasks to their predecessor definitions, allowing a reader to trace the original intention. These are evolution links, not a parent task assigning concurrent subtasks.

**Task brief**:
A self-contained account of the remaining work, relevant work already completed, decisions that still matter, and useful references. Reading predecessor tasks is optional for understanding the current work.

**Completion watch**:
An observation of a task's remaining work that follows replacements, provides progress updates, and reports completion when all remaining branches have completed. Cancellation of a branch is reported distinctly and does not count as successful completion.

**Task subscription**:
A recorded interest in updates about particular work, following replacement tasks as that work changes. It identifies which updates a subscriber should receive.

**Prerequisite**:
Work that must complete before a task can be claimed. When the task evolves, the requirement follows the remaining replacement branches.

**Available task**:
A current, unfinished task with no owner and no unfinished prerequisites. Availability describes the current state and can change before a claim is attempted.

**Claim**:
The atomic operation that assigns one agent session exclusive ownership of an available task and marks it in progress.

**Task owner**:
The agent session holding a task's current claim. Release removes that ownership; reassignment invalidates the previous ownership.

**In-progress task**:
A current task with an owning agent session that has claimed it for work.

**Completed task**:
A task whose stated work has been fulfilled.

**Cancelled task**:
A task whose work is no longer being pursued. Cancellation does not assert that the stated work has been fulfilled.

**Activity**:
A coarse recorded indication of recent work on a task, used to keep inactivity reminders silent while activity is recent. It need not describe or quantify progress.

**Tag**:
A label attached to a task to help organise and find related work.

**Task metadata**:
Structured context attached to a task, including named fields for agent sessions and worktrees and additional key/value information. Status and ownership are explicit task properties.

**Scratch area**:
A temporary workspace outside the code repository, linked to a task. Important work belongs in the repository; scratch contents are disposable.

**Agent session**:
An identified execution of a coding agent that may work on tasks.

**Delivery claim**:
An expiring, token-fenced right to offer a specific subscription batch. It prevents concurrent consumers offering that batch. It does not own or release a task.

**Acknowledgement**:
An explicit receipt for the offered event range. Only a valid current delivery token can advance a subscription’s event cursor. Accepting a queued wake is not an acknowledgement.
