package ledger

import (
	"fmt"
	"math"
)

func Summarize(ts []Transaction) (Summary, error) {
	s := Summary{Count: len(ts), ByCategory: map[string]int64{}}
	for _, t := range ts {
		if (t.Cents > 0 && s.TotalCents > math.MaxInt64-t.Cents) || (t.Cents < 0 && s.TotalCents < math.MinInt64-t.Cents) {
			return Summary{}, fmt.Errorf("total overflow")
		}
		s.TotalCents += t.Cents
		v := s.ByCategory[t.Category]
		if (t.Cents > 0 && v > math.MaxInt64-t.Cents) || (t.Cents < 0 && v < math.MinInt64-t.Cents) {
			return Summary{}, fmt.Errorf("category %q overflow", t.Category)
		}
		s.ByCategory[t.Category] = v + t.Cents
	}
	return s, nil
}
