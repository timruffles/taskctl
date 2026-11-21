package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"

	ledger "example.com/ledger"
)

const usage = "Usage: ledger [--] [FILE]\n       ledger --help\n"

type options struct {
	file string
	help bool
	err  error
}

func parseArgs(args []string) options {
	if len(args) == 0 {
		return options{}
	}
	if args[0] == "--help" {
		if len(args) != 1 {
			return options{err: errors.New("--help does not accept arguments")}
		}
		return options{help: true}
	}
	literal := args[0] == "--"
	if literal {
		args = args[1:]
	}
	if len(args) > 1 {
		return options{err: errors.New("expected at most one file")}
	}
	if len(args) == 0 {
		return options{}
	}
	file := args[0]
	if file == "" {
		return options{err: errors.New("file name must not be empty")}
	}
	if !literal && file != "-" && file[0] == '-' {
		return options{err: fmt.Errorf("unknown flag %s", file)}
	}
	return options{file: file}
}

func run(args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	o := parseArgs(args)
	if o.err != nil {
		fmt.Fprint(stderr, usage)
		fmt.Fprintln(stderr, "ledger:", o.err)
		return 2
	}
	if o.help {
		fmt.Fprint(stdout, usage)
		return 0
	}
	var input io.Reader = stdin
	var f *os.File
	if o.file != "" && o.file != "-" {
		var err error
		f, err = os.Open(o.file)
		if err != nil {
			fmt.Fprintln(stderr, "ledger:", err)
			return 1
		}
		defer f.Close()
		input = f
	}
	tx, err := ledger.ReadCSV(input)
	if err != nil {
		fmt.Fprintln(stderr, "ledger:", err)
		return 1
	}
	summary, err := ledger.Summarize(tx)
	if err != nil {
		fmt.Fprintln(stderr, "ledger:", err)
		return 1
	}
	enc := json.NewEncoder(stdout)
	if err = enc.Encode(summary); err != nil {
		fmt.Fprintln(stderr, "ledger:", err)
		return 1
	}
	return 0
}

func main() { os.Exit(run(os.Args[1:], os.Stdin, os.Stdout, os.Stderr)) }
