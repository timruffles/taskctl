package main

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func invoke(args []string, input string) (int, string, string) {
	var stdout, stderr bytes.Buffer
	code := run(args, strings.NewReader(input), &stdout, &stderr)
	return code, stdout.String(), stderr.String()
}

func TestRunStdinAndDash(t *testing.T) {
	for _, args := range [][]string{nil, {"-"}} {
		code, out, errOut := invoke(args, "café\n🙂")
		if code != 0 || out != "{\"lines\":2,\"words\":2,\"runes\":6}\n" || errOut != "" {
			t.Errorf("run(%v) = (%d, %q, %q)", args, code, out, errOut)
		}
	}
}

func TestRunNamedFileAndDashFilename(t *testing.T) {
	dir := t.TempDir()
	for _, name := range []string{"input.txt", "-input.txt"} {
		path := filepath.Join(dir, name)
		if err := os.WriteFile(path, []byte("hello world\n"), 0600); err != nil {
			t.Fatal(err)
		}
		args := []string{path}
		if strings.HasPrefix(name, "-") {
			args = []string{"--", path}
		}
		code, out, errOut := invoke(args, "ignored")
		if code != 0 || out != "{\"lines\":1,\"words\":2,\"runes\":12}\n" || errOut != "" {
			t.Errorf("run(%v) = (%d, %q, %q)", args, code, out, errOut)
		}
	}
}

func TestHelpAndUsageErrors(t *testing.T) {
	code, out, errOut := invoke([]string{"--help"}, "")
	if code != 0 || !strings.HasPrefix(out, "Usage: textstats [FILE]\n") || errOut != "" {
		t.Errorf("help = (%d, %q, %q)", code, out, errOut)
	}
	for _, args := range [][]string{{"one", "two"}, {"--unknown"}, {"--"}, {"--help", "file"}} {
		code, out, errOut = invoke(args, "")
		if code != 2 || out != "" || errOut != "usage: textstats [FILE]\n" {
			t.Errorf("run(%v) = (%d, %q, %q)", args, code, out, errOut)
		}
	}
}

func TestReadError(t *testing.T) {
	code, out, errOut := invoke([]string{"missing-file"}, "")
	if code != 1 || out != "" || !strings.HasPrefix(errOut, "textstats: open missing-file:") || !strings.HasSuffix(errOut, "no such file or directory\n") {
		t.Errorf("read error = (%d, %q, %q)", code, out, errOut)
	}
}
