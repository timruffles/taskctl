package ledger

import (
	"encoding/csv"
	"fmt"
	"io"
	"strings"
	"time"
	"unicode/utf8"
)

type Transaction struct {
	Date     string
	Category string
	Cents    int64
}
type Summary struct {
	Count      int              `json:"count"`
	TotalCents int64            `json:"total_cents"`
	ByCategory map[string]int64 `json:"by_category"`
}

func ReadCSV(r io.Reader) ([]Transaction, error) {
	c := csv.NewReader(r)
	c.FieldsPerRecord = 3
	c.TrimLeadingSpace = false
	head, e := c.Read()
	if e != nil {
		return nil, fmt.Errorf("read header: %w", e)
	}
	if len(head) != 3 || head[0] != "date" || head[1] != "category" || head[2] != "amount" {
		return nil, fmt.Errorf("invalid header: expected date,category,amount")
	}
	var out []Transaction
	for row := 2; ; row++ {
		rec, e := c.Read()
		if e == io.EOF {
			break
		}
		if e != nil {
			return nil, fmt.Errorf("row %d: %w", row, e)
		}
		for i := range rec {
			rec[i] = strings.TrimSpace(rec[i])
		}
		d := rec[0]
		parsed, err := time.Parse("2006-01-02", d)
		if err != nil || parsed.Format("2006-01-02") != d {
			return nil, fmt.Errorf("row %d: invalid date %q", row, d)
		}
		if rec[1] == "" || !utf8.ValidString(rec[1]) {
			return nil, fmt.Errorf("row %d: category must be nonempty valid UTF-8", row)
		}
		n, err := ParseCents(rec[2])
		if err != nil {
			return nil, fmt.Errorf("row %d amount: %w", row, err)
		}
		out = append(out, Transaction{d, rec[1], n})
	}
	return out, nil
}
