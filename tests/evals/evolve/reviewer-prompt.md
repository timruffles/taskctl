# Reviewer dispatch

Each review used a new `collaboration.spawn_agent` with `fork_turns: "none"`, `model: "gpt-6-luna"` and `reasoning_effort: "medium"`. Paths below were expanded to absolute paths. The only changing input between A and B was `README_VARIANT`; the final check used `readme-final.md` after the documentation fixes.

The persona was fetched from Google's [`agents/comprehension.md`](https://github.com/google/elephant-goldfish-model/blob/main/agents/comprehension.md) on 2026-11-22. Its SHA-256 was `974f4972f717dc7b102a9b1407b3a625749149f2ae905727241e919f43cc3fa5`.

```text
You are a fresh-reader comprehension evaluator of a local task CLI. Read ONLY these files: PERSONA (review persona), README_VARIANT (whole target README), and scenarios.json (questions). Mode REVIEW. Do not browse, inspect implementation, follow README links, read other workspace files, retrieve memory or ask other agents. Read-only: do not edit files. First read the persona and all of the target and questions. Return a single JSON object with: paraphrase (2-4 sentences), answers (one object per scenario with id, command, plan when requested, explanation), findings (array of severity/scope/description), prior_preference (null unless supplied in a visible earlier conversation), isolation_marker (any parent-only marker visible in earlier conversation, else null). Answer from the README; state 'not specified' when it doesn't establish an answer rather than invent behaviour. Use the terminology of this README. You are not being asked to choose a preferred name or compare alternatives.
```

The rubric was not supplied to readers. A marker was present only in the parent's conversation, not in the dispatch or any reader input file. Responses are recorded as JSON without changing their text. This checks declared comprehension; it does not prove the absence of every possible host-context source.
