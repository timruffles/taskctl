# textstats

`textstats` counts lines, whitespace separated words, and Unicode code points.

Run it with standard input:

```sh
printf 'one two\n' | go run ./cmd/textstats
```

Or pass a file name. Use `-` for standard input explicitly, and `--` before a
file name that begins with a hyphen:

```sh
go run ./cmd/textstats notes.txt
go run ./cmd/textstats -
go run ./cmd/textstats -- -notes.txt
```

The command prints one JSON object with `lines`, `words`, and `runes` fields.
It exits with status 1 on read errors and status 2 for invalid arguments. Use
`--help` for a short usage summary.
