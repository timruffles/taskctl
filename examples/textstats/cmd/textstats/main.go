package main

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"strings"

	"example.com/textstats"
)

const helpText = "Usage: textstats [FILE]\nCount lines, words and Unicode code points in FILE or standard input.\nUse - for standard input; use -- before a filename beginning with '-'.\n"

func run(args []string, stdin io.Reader, stdout, stderr io.Writer) int {
	if len(args) == 1 && args[0] == "--help" {
		fmt.Fprint(stdout, helpText)
		return 0
	}

	useStdin := len(args) == 0
	filename := ""
	if len(args) > 0 {
		if args[0] == "--" {
			if len(args) != 2 {
				fmt.Fprintln(stderr, "usage: textstats [FILE]")
				return 2
			}
			filename = args[1]
		} else if len(args) != 1 {
			fmt.Fprintln(stderr, "usage: textstats [FILE]")
			return 2
		} else if args[0] == "-" {
			useStdin = true
		} else if strings.HasPrefix(args[0], "-") {
			fmt.Fprintln(stderr, "usage: textstats [FILE]")
			return 2
		} else {
			filename = args[0]
		}
	}

	var data []byte
	var err error
	if useStdin {
		data, err = io.ReadAll(stdin)
	} else {
		data, err = os.ReadFile(filename)
	}
	if err != nil {
		fmt.Fprintf(stderr, "textstats: %v\n", err)
		return 1
	}

	encoded, err := json.Marshal(textstats.Analyze(string(data)))
	if err != nil {
		fmt.Fprintf(stderr, "textstats: %v\n", err)
		return 1
	}
	_, _ = fmt.Fprintf(stdout, "%s\n", encoded)
	return 0
}

func main() {
	os.Exit(run(os.Args[1:], os.Stdin, os.Stdout, os.Stderr))
}
