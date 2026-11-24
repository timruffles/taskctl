package main

import (
	"os"

	"taskctl/internal/cli"
)

var version = "0.6.0"

func main() {
	os.Exit(cli.Execute(os.Args[1:], os.Stdin, os.Stdout, os.Stderr, version))
}
