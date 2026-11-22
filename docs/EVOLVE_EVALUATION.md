# `supersede` versus `evolve`: fresh-reader evaluation

## Finding

All six Luna readers selected the intended operations: replace a stale definition, edit a small correction, record a progress checkpoint, and complete finished work. None interpreted `evolve` as editing the original task in place, discarding completed implementation, or completing the original before its replacement branches finish.

This small evaluation does **not** establish that either name is better understood. `evolve` remains a reasonable design choice, with explicit wording about identity and ownership. The useful result was finding where the README leaves those rules implicit.

## Method and evidence

On 22 November 2026, six fresh `gpt-6-luna` agents at medium reasoning effort read one complete README each: three read `supersede`, three read `evolve`. Each received the same ten practical scenarios. They were asked to paraphrase the document, answer from it, and say when it did not establish an answer. They were not asked to vote for a name and did not receive the parent's preferred name or conversation history.

The method adapts Google's [Elephant–Goldfish fresh-reader approach](https://github.com/google/elephant-goldfish-model) and its [comprehension persona](https://github.com/google/elephant-goldfish-model/blob/main/agents/comprehension.md). This was a focused comprehension check, not a run of the complete EGM review-loop plugin. EGM's [Codex guidance](https://github.com/google/elephant-goldfish-model/blob/main/docs/codex.md) distinguishes fresh parent turns from full host isolation. All six reviewers returned null for prior preference and the parent-only isolation marker; host instruction and filesystem isolation remain unverified.

Variant A is the README at commit `51ceee073bf59424ad30aebb5a3f9ef1bcde1ea7`. Variant B changes only `supersede`/`superseded`/`supersession` to `evolve`/`evolved`/`evolution`. It changes the status and prose as well as the verb, so the experiment does not isolate the command name alone.

Frozen inputs and outputs:

- [Protocol and hashes](../tests/evals/evolve/protocol.json), [scenarios](../tests/evals/evolve/scenarios.json), and the [predeclared rubric](../tests/evals/evolve/rubric.json).
- [README A](../tests/evals/evolve/readme-a.md) and [README B](../tests/evals/evolve/readme-b.md).
- Raw responses: [A1](../tests/evals/evolve/responses/a1.json), [A2](../tests/evals/evolve/responses/a2.json), [A3](../tests/evals/evolve/responses/a3.json), [B1](../tests/evals/evolve/responses/b1.json), [B2](../tests/evals/evolve/responses/b2.json), [B3](../tests/evals/evolve/responses/b3.json).
- [Every score and its rationale](../tests/evals/evolve/scores.json).
- [Reviewer dispatch](../tests/evals/evolve/reviewer-prompt.md), including the persona source and hash.

The rubric was saved before dispatch. A separate scoring agent assessed two binary criteria per scenario. It had the parent context, so scoring was not blinded. A point means the answer unambiguously covered the whole criterion. An omitted, ambiguous, or explicitly unspecified rule scores zero; that does not necessarily mean the reader believed a false rule.

## Results

| README | Run 1 | Run 2 | Run 3 | Explicit rubric coverage |
| --- | ---: | ---: | ---: | ---: |
| `supersede` | 15/20 | 17/20 | 17/20 | 49/60 |
| `evolve` | 16/20 | 15/20 | 16/20 | 47/60 |

The two-point aggregate difference is too weak and sensitive to judgement calls to select a name. These are three responses per name, not sixty independent trials. No significance or reliability probability is inferred.

Both groups consistently understood:

- A stale definition becomes historical and the remaining work gets replacement tasks.
- A typo uses `edit`; a checkpoint uses `progress`; verified finished work uses `complete`.
- `continue_with` selects a replacement and returns its ID and a fresh claim token.
- Dependents and supervisors still wait while any replacement branch is unfinished. The original's stored historical state does not become `completed`.
- Newly discovered necessary work can belong in a successor without discarding implemented code.
- Omitting `continue_with` leaves replacements pending. Tags, metadata, prerequisites and scratch references carry forward; old notes stay with their original task.
- Cheap current-work views and lineage inspection avoid loading old note bodies.

## What the answers exposed

**State identity explicitly.** Four readers declined to affirm that `edit` preserves the task ID. Five did not unambiguously establish that the retired original could no longer be modified with its former claim. Those readers still chose the right operation and correctly used the new continuation ID and token. The frozen README implies these rules but does not state them directly.

**Describe the former owner's next step.** Some readers repeated takeover instructions intended for a crashed worker instead of explaining what to do when another live worker now owns the task. A worker that lost ownership should inspect state and coordinate with the current owner. Original authorship supplies no override.

**Do not suggest passing around the current owner's claim.** B1 proposed obtaining that token from the owner. It meets the rubric's literal request to coordinate, so the score records that coverage, but it exposes an additional protocol risk. The current owner should perform the change, or release the task so the next worker obtains its own claim. A token exchange alone does not describe that ownership transition. A2's more general “authorized claim” also leaves the transfer procedure unstated.

**Make handoff content concrete.** Some answers said each new brief should stand alone without mentioning acceptance evidence. The README should state that a successor names its remaining work, relevant decisions or paths, and the evidence needed for completion.

One common deduction reflects the evaluation itself: the discovered-work rubric asked for completed-work and repository-path recording in a summary, but its question did not request a summary or provide paths. All six answers lost that criterion. Keep the frozen scores for auditability, but do not interpret this as a naming defect. B2 also used a revision placeholder despite being given revision 4; the rubric required the flag, so that detail did not change its score.

## Documentation revision and executable checks

After the comparison, the README was revised to state that editing keeps the ID, evolution makes the predecessor definition immutable, and a former owner must arrange a release and fresh claim. One additional fresh Luna reader saw that [revised draft](../tests/evals/evolve/readme-final.md) with the same scenarios. Its [response](../tests/evals/evolve/responses/final.json) correctly stated all three rules and suggested no token exchange. This was a post-review check, not a fourth sample of either frozen variant; [its protocol](../tests/evals/evolve/follow-up.json) is separate.

The [CLI replay test](../tests/evolve_cli_test.go) executes all seven readers' unmodified `remaining` plans, binding the illustrative task ID, claim and revision to fresh isolated projects. All seven passed. Three used the legacy command alias and four used `evolve`. Each produced two successor tasks with a fresh continuation claim, preserved the predecessor and lineage, rejected the old claim on the successor, and kept a dependent blocked until both branches completed. Both status-filter spellings returned canonical `evolved` output.

`go test -p=1 -count=1 ./...` and `go vet -p=1 ./...` passed after the rename. The [store compatibility tests](../internal/store/evolve_compat_test.go) also check the unchanged SQLite representation, event and subscription outputs, and backup/restore with note bodies. These checks establish behaviour for the recorded plans; the agents themselves did not interactively execute the scenarios.

The [mixed-version check](../tests/evals/evolve/mixed_versions.py) ran a clean 0.3.0 binary from commit `51ceee0` as a live watcher before 0.4.0 evolved its task. Both versions still reported a pending outcome after the first successor completed. The old watcher exited successfully only after the second completed. Old output retained `superseded`, new output used `evolved`, and the database stayed at schema 2. [Recorded results](../tests/evals/evolve/mixed-version-result.json) include binary hashes and the old watcher's event stream.

## Limits and decision

The scenarios repeatedly describe the operation as replacing a stale definition. Combined with a full README and example plan, that makes this a test of documented workflow comprehension rather than spontaneous inference from a bare verb. The sample contains one small-model configuration and no independent human readers. The later CLI replay does not turn this into an autonomous agent task-completion benchmark. More model samples would not by themselves resolve implementation or upgrade risks.

The chosen name is `evolve`, with the literal explanation: **record completed work, retire the old definition, and create new tasks for what remains**. Ownership and identity rules are now explicit and the edited text has had a fresh review. This evaluation provides no evidence for marketing `evolve` as the empirically superior name.
