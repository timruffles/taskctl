package main

import (
	"bytes"
	"strings"
	"testing"
)

func TestParseArgs(t *testing.T) {
	cases := []struct {
		name    string
		args    []string
		file    string
		help    bool
		wantErr bool
	}{
		{"stdin", nil, "", false, false}, {"dash stdin", []string{"-"}, "-", false, false}, {"file", []string{"data.csv"}, "data.csv", false, false}, {"dash file", []string{"--", "-data.csv"}, "-data.csv", false, false}, {"help", []string{"--help"}, "", true, false}, {"help args", []string{"--help", "x"}, "", false, true}, {"two", []string{"a", "b"}, "", false, true}, {"unknown", []string{"--wat"}, "", false, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := parseArgs(tc.args)
			if (got.err != nil) != tc.wantErr {
				t.Fatalf("parseArgs(%v) error=%v", tc.args, got.err)
			}
			if !tc.wantErr && (got.file != tc.file || got.help != tc.help) {
				t.Fatalf("parseArgs(%v) = %+v", tc.args, got)
			}
		})
	}
}

func TestRunHelpAndArgumentErrors(t *testing.T) {
	var out, errout bytes.Buffer
	if code := run([]string{"--help"}, strings.NewReader(""), &out, &errout); code != 0 || out.String() != usage || errout.Len() != 0 {
		t.Fatalf("help: code=%d stdout=%q stderr=%q", code, out.String(), errout.String())
	}
	out.Reset()
	errout.Reset()
	if code := run([]string{"--help", "file"}, strings.NewReader(""), &out, &errout); code != 2 || out.Len() != 0 || errout.Len() == 0 {
		t.Fatalf("bad help args: code=%d stdout=%q stderr=%q", code, out.String(), errout.String())
	}
}

func TestRunWritesSingleSummaryLine(t *testing.T) {
	var out, errout bytes.Buffer
	code := run(nil, strings.NewReader("date,category,amount\n2024-01-02,food,12.30\n"), &out, &errout)
	if code != 0 || errout.Len() != 0 || !strings.HasSuffix(out.String(), "\n") || strings.Count(out.String(), "\n") != 1 || !strings.Contains(out.String(), "total_cents") {
		t.Fatalf("code=%d stdout=%q stderr=%q", code, out.String(), errout.String())
	}
}

func TestRunErrorsHaveNoJSON(t *testing.T) {
	var out, errout bytes.Buffer
	code := run(nil, strings.NewReader("bad\n"), &out, &errout)
	if code != 1 || out.Len() != 0 || errout.Len() == 0 {
		t.Fatalf("code=%d stdout=%q stderr=%q", code, out.String(), errout.String())
	}
}

func TestRunRejectsEmptyFilenames(t *testing.T) {
	for _, args := range [][]string{{""}, {"", "data.csv"}, {"", ""}, {"--", ""}} {
		var out, errout bytes.Buffer
		code := run(args, strings.NewReader("date,category,amount\n"), &out, &errout)
		if code != 2 || out.Len() != 0 || errout.Len() == 0 {
			t.Fatalf("args=%q code=%d stdout=%q stderr=%q", args, code, out.String(), errout.String())
		}
	}
}
