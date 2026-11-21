package ledger

import (
	"math"
	"testing"
)

func TestSummarizeEmpty(t *testing.T) {
	s, err := Summarize(nil)
	if err != nil {
		t.Fatal(err)
	}
	if s.Count != 0 || s.TotalCents != 0 || s.ByCategory == nil || len(s.ByCategory) != 0 {
		t.Fatalf("unexpected empty summary: %#v", s)
	}
}
func TestSummarizeSignedBoundsAndNegativeTotals(t *testing.T) {
	s, err := Summarize([]Transaction{{Category: "max", Cents: math.MaxInt64}, {Category: "min", Cents: math.MinInt64}})
	if err != nil || s.TotalCents != -1 {
		t.Fatalf("bounds summary=%+v err=%v", s, err)
	}
	s, err = Summarize([]Transaction{{Category: "negative", Cents: -25}, {Category: "negative", Cents: -17}})
	if err != nil || s.TotalCents != -42 || s.ByCategory["negative"] != -42 {
		t.Fatalf("negative summary=%+v err=%v", s, err)
	}
}
func TestSummarizeCategoryOverflowWithInRangeTotal(t *testing.T) {
	tx := []Transaction{{Category: "same", Cents: math.MaxInt64}, {Category: "same", Cents: 1}, {Category: "offset", Cents: -1}}
	s, err := Summarize(tx)
	if err == nil {
		t.Fatalf("expected category overflow despite total %d", s.TotalCents)
	}
}
