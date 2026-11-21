# Ledger: live agent recovery exercise

Build a small standard-library-only Go package and CLI. Important code and tests belong in this directory. Disposable claim receipts and coordination markers belong in the external harness directory.

## Package contract

`ParseCents(string) (int64, error)` trims surrounding whitespace, accepts an optional sign, then one or more ASCII digits and an optional decimal point followed by one or two ASCII digits. Leading zeroes are allowed. Reject exponents, missing integer or fractional digits, other characters, more than two decimal places, and values outside the signed 64-bit cents range. `92233720368547758.07` and `-92233720368547758.08` are valid boundaries. Do not use floating point.

```go
type Transaction struct {
    Date string
    Category string
    Cents int64
}

type Summary struct {
    Count int `json:"count"`
    TotalCents int64 `json:"total_cents"`
    ByCategory map[string]int64 `json:"by_category"`
}

func ReadCSV(io.Reader) ([]Transaction, error)
func Summarize([]Transaction) (Summary, error)
```

CSV has exactly the header `date,category,amount`, exactly three fields per record, and trimmed data fields. Dates must be real dates written exactly as YYYY-MM-DD. Categories must be nonempty and valid UTF-8. Amounts use ParseCents. Invalid input fails with useful row context and no successful partial result. Quoted commas and CRLF must work. A header-only input is valid. Summarize checks overflow independently in the total and each category. An empty summary encodes `by_category` as `{}`.

## CLI contract

`ledger [FILE]` reads stdin with no filename or `-`. `ledger -- FILE` permits a filename beginning with `-`. `--help` prints usage. More than one file, unknown flags, or extra arguments with help return exit 2. Read, parse, and overflow errors return exit 1. Success returns exit 0 and exactly one newline-terminated JSON summary to stdout. Errors go to stderr without a successful JSON body.

## Ownership

- chaos-a: `amount.go` and initial implementation checkpoint. After recovery, `README.md` only.
- chaos-b: `csv.go`, `csv_test.go`, `summary.go`, `summary_test.go`, and the shared type declarations.
- chaos-c: `cmd/ledger/main.go`, `cmd/ledger/main_test.go`. After confirmed chaos-a termination and explicit task takeover, `amount.go` and `amount_test.go`.

Every command and file edit in the live exercise runs through the installed-hook harness. Read and acknowledge received hook context; stale delivery tokens should be replaced by fresh pending delivery. A heartbeat is not a progress report. On a confirmed crash, preserve completed work in a supersession summary and make the remaining verification task self-contained.
