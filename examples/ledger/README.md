# Ledger CLI

`ledger` reads a CSV ledger from standard input by default, or from one named file. It prints one JSON summary on success.

## Examples

Read from standard input:

```sh
cat <<'CSV' | go run ./cmd/ledger
date,category,amount
2026-01-05,food,12.34
2026-01-06,travel,-2.50
CSV
```

Read from a file:

```sh
go run ./cmd/ledger transactions.csv
```

A file whose name begins with a dash can be passed after `--`:

```sh
go run ./cmd/ledger -- -transactions.csv
```

The CSV header must be exactly `date,category,amount`. Each row needs a real `YYYY-MM-DD` date, a nonempty category, and an amount. Quoted CSV fields are supported. A header-only file is a valid empty ledger.

## Amounts

Amounts are parsed as exact integer cents; no floating-point rounding is used. An optional `+` or `-` is allowed, as are leading zeroes. The whole-number part requires at least one digit. A decimal point may be followed by one or two digits: `7`, `7.5`, and `007.50` mean 700, 750, and 750 cents. Surrounding whitespace is ignored. Exponents, commas, currency symbols, a missing whole or fractional part, and more than two decimal places are rejected.

Values must fit signed 64-bit cents. The accepted endpoints are `92233720368547758.07` and `-92233720368547758.08`.

## Exit codes

- `0`: success; one newline-terminated JSON summary is written to standard output.
- `1`: file read, CSV or amount parse, or summary overflow error. Details go to standard error.
- `2`: invalid command-line arguments, including unknown flags, multiple files, or extra arguments with `--help`.

`--help` prints usage. Errors do not produce a successful JSON summary.
