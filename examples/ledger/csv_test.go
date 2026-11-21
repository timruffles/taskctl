package ledger

import (
	"bytes"
	"strings"
	"testing"
)

func TestReadCSV(t *testing.T) {
	xs, e := ReadCSV(strings.NewReader("date,category,amount\r\n2024-02-29,\"food, cafe\",1.20\r\n"))
	if e != nil || len(xs) != 1 || xs[0].Cents != 120 || xs[0].Category != "food, cafe" {
		t.Fatalf("%+v %v", xs, e)
	}
}
func TestReadCSVRejectsBadRows(t *testing.T) {
	for _, s := range []string{"date,category,amount\n2023-02-29,x,1\n", "date,category,amount\n2024-01-01,,1\n", "date,category,amount\n2024-01-01,x,1,extra\n", "date,category,amount\n2024-01-01,x,1.234\n"} {
		if _, e := ReadCSV(strings.NewReader(s)); e == nil {
			t.Fatalf("accepted %q", s)
		}
	}
}
func TestSummarizeBoundaries(t *testing.T) {
	s, e := Summarize([]Transaction{{Category: "a", Cents: 9223372036854775807}, {Category: "a", Cents: -9223372036854775807}, {Category: "b", Cents: 1}})
	if e != nil || s.TotalCents != 1 {
		t.Fatalf("%+v %v", s, e)
	}
	_, e = Summarize([]Transaction{{Category: "same", Cents: 9223372036854775807}, {Category: "same", Cents: 1}, {Category: "other", Cents: -1}})
	if e == nil {
		t.Fatal("missed category overflow")
	}
	_, e = Summarize([]Transaction{{Category: "x", Cents: 9223372036854775807}, {Category: "y", Cents: 1}})
	if e == nil {
		t.Fatal("missed total overflow")
	}
}

func TestReadCSVHeaderOnlyAndStrictHeader(t *testing.T) {
	xs, err := ReadCSV(strings.NewReader("date,category,amount\n"))
	if err != nil || len(xs) != 0 {
		t.Fatalf("header-only: %#v %v", xs, err)
	}
	for _, header := range []string{"Date,category,amount\n", " date,category,amount\n", "date,category,amount,extra\n", "date,category\n"} {
		if got, err := ReadCSV(strings.NewReader(header)); err == nil || got != nil {
			t.Fatalf("header %q returned %#v, %v", header, got, err)
		}
	}
}

func TestReadCSVRejectsInvalidUTF8WithoutPartialResult(t *testing.T) {
	data := append([]byte("date,category,amount\n2024-01-01,ok,1\n2024-01-02,"), 0xff)
	data = append(data, []byte(",2\n")...)
	got, err := ReadCSV(bytes.NewReader(data))
	if err == nil || got != nil {
		t.Fatalf("returned partial result %#v, %v", got, err)
	}
}

func TestReadCSVRejectsMalformedDateWithoutPartialResult(t *testing.T) {
	input := "date,category,amount\n2024-01-01,ok,1\n2024-02-30,nope,2\n"
	got, err := ReadCSV(strings.NewReader(input))
	if err == nil || got != nil {
		t.Fatalf("returned partial result %#v, %v", got, err)
	}
}
