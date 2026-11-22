# Recover interrupted work

## Read the failure before choosing an action

With `--json`, errors are on stderr as `error.code` and `error.message`. Exit 3 covers several conflicts, including a busy database; use the error code and current state to distinguish them.

| Situation | Next action |
| --- | --- |
| Another worker won the claim | Read the task; choose other available work or coordinate with its owner. |
| Claim token is stale | Stop mutations and file writes for that assignment; resolve ownership with the coordinator. |
| Edit or supersession revision changed | Reread the definition, preserve intervening changes, and rebuild the edit or replacement plan. |
| Command response was lost | Inspect task state before retrying. A failed transport does not prove the mutation failed. |
| Creation response was lost | Retry the same `--request-id` with the same input. A changed payload requires a new request ID. |
| Subscription delivery token expired | Request a fresh pending offer and consume its exact range. |
| Missing, invalid, full or read-only storage | Report the concrete storage error; restore access or use the backup procedure below. |

## Recover a claimed task

```sh
taskctl tasks in-progress --json
taskctl tasks show T1 --json
```

Inspect its owner, activity time, progress, metadata and worktree. Confirm that the old worker has stopped or have the harness revoke its ability to write. Claims protect taskctl mutations, not filesystem writes; a live old process can still change files after takeover.

If recovery is the requested next step, use the exact current `claim_id`:

```sh
taskctl tasks takeover T1 --expected-claim OLD_CLAIM --session NEW_SESSION --reason 'Previous worker terminated; recovering its worktree' --json
```

Retain the new token. Inspect the actual files and run relevant checks before recording a recovery checkpoint. Use durable artifact references and the latest progress title first. If detail is missing, list note titles with `notes list ID --limit 10 --json`, then read a relevant body with `notes read NOTE_ID`. Supersede a stale brief before continuing substantial remaining work.

Task claims never expire automatically. If the original owner is available, it can record progress and `tasks release` instead. Fresh activity prevents inactivity warnings but does not establish visible accomplishment; when reporting stalled work, compare the recorded progress with the files and tests.

## Back up or restore

```sh
taskctl backup --directory /absolute/backups/taskctl-snapshot
taskctl restore --directory /absolute/backups/taskctl-snapshot --name recovered-project
```

Choose a new backup directory: an existing destination is rejected. A full backup includes `tasks.sqlite`, `notes.sqlite` when bodies are present, and a completion marker. Copying the live databases separately is not a consistent backup. Single-file `backup --file` requires no live note bodies; `restore --file` accepts old SQLite backups. JSON archives and compaction are not supported.

Restore creates a new project, releases old active claims, clears subscriptions and creation request receipts, and marks restored scratch references unavailable. It preserves task definitions, notes, results and lineage. Attach the new project to the intended worktree with `projects attach --project NEW_PROJECT --path DIR`, then claim work and create subscriptions afresh. Note bodies are durable and separate from disposable scratch.
