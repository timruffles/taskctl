# Text statistics trial

Build a small Go library and CLI using taskctl for task ownership and progress.

## Library

Package `textstats` exposes:

```go
type Stats struct {
    Lines int `json:"lines"`
    Words int `json:"words"`
    Runes int `json:"runes"`
}

func Analyze(text string) Stats
```

- Words are nonempty runs separated by Unicode whitespace (strings.Fields semantics).
- Runes count Unicode code points, including whitespace and newline characters (utf8.RuneCountInString semantics, including invalid UTF-8).
- Lines count LF characters plus one if nonempty input does not end in LF. Empty input has zero lines. Thus `a`, `a\n` and `\n` each have one line; `a\n\n` has two. CRLF has one LF and two runes.
- No external dependencies.

## CLI

`textstats [FILE]` reads the named file, or stdin with no argument or `-`.

Print one JSON object followed by a newline. Exit 0 on success, 1 on read errors, 2 on usage errors. More than one positional argument is a usage error. `--help` alone prints concise help and exits 0. Reject other leading-hyphen arguments except `-`; `-- FILE` permits a filename beginning with a hyphen. Errors go to stderr; no successful JSON on failure.

## Ownership

- Core agent: `analyze.go` and `analyze_test.go` only.
- CLI agent: `cmd/textstats/main.go`, `cmd/textstats/main_test.go`, and `README.md` only.
- Coordinator: module, contract, integration review, task history, trial evidence.

Use meaningful tests for Unicode, empty input, trailing newlines, and CLI I/O and exit codes. Record actual verification in task completion summaries. Important work belongs in these source files; scratch is disposable.
