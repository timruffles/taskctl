package main

import (
	"context"
	"encoding/json"
	"flag"
	"io"
	"os"
	"os/exec"
	"runtime"
	"syscall"
	"time"
)

func main() {
	input := flag.String("input", "", "stdin file")
	output := flag.String("output", "", "stdout file")
	timeout := flag.Duration("timeout", 15*time.Second, "command deadline")
	flag.Parse()
	if flag.NArg() == 0 || *output == "" {
		panic("output and command are required")
	}
	out, err := os.Create(*output)
	if err != nil {
		panic(err)
	}
	defer out.Close()
	errout, err := os.CreateTemp("", "taskctl-perf-stderr-*")
	if err != nil {
		panic(err)
	}
	defer os.Remove(errout.Name())
	defer errout.Close()
	ctx, cancel := context.WithTimeout(context.Background(), *timeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, flag.Arg(0), flag.Args()[1:]...)
	cmd.Stdout, cmd.Stderr = out, errout
	if *input != "" {
		in, err := os.Open(*input)
		if err != nil {
			panic(err)
		}
		defer in.Close()
		cmd.Stdin = in
	}
	started := time.Now()
	err = cmd.Run()
	elapsed := time.Since(started)
	if cmd.ProcessState == nil {
		panic(err)
	}
	usage := cmd.ProcessState.SysUsage().(*syscall.Rusage)
	rss := usage.Maxrss
	if runtime.GOOS != "darwin" {
		rss *= 1024
	}
	info, _ := out.Stat()
	errout.Seek(0, io.SeekStart)
	errorText, _ := io.ReadAll(io.LimitReader(errout, 4096))
	json.NewEncoder(os.Stdout).Encode(map[string]any{
		"elapsed_ms":     float64(elapsed) / float64(time.Millisecond),
		"peak_rss_bytes": rss, "exit": cmd.ProcessState.ExitCode(),
		"stdout_bytes": info.Size(), "stderr": string(errorText),
		"timed_out": ctx.Err() != nil,
	})
}
