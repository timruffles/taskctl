package ledger

import (
	"errors"
	"math"
	"strings"
)

func ParseCents(s string) (int64, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0, errors.New("invalid amount")
	}
	neg := false
	if s[0] == '+' || s[0] == '-' {
		neg = s[0] == '-'
		s = s[1:]
	}
	if s == "" {
		return 0, errors.New("invalid amount")
	}
	whole, frac, dot := "", "", false
	for _, r := range s {
		if r == '.' {
			if dot {
				return 0, errors.New("invalid amount")
			}
			dot = true
			continue
		}
		if r < '0' || r > '9' {
			return 0, errors.New("invalid amount")
		}
		if dot {
			frac += string(r)
		} else {
			whole += string(r)
		}
	}
	if whole == "" || (dot && frac == "") || len(frac) > 2 {
		return 0, errors.New("invalid amount")
	}
	var n uint64
	for _, r := range whole + frac + strings.Repeat("0", 2-len(frac)) {
		d := uint64(r - '0')
		if n > (math.MaxUint64-d)/10 {
			return 0, errors.New("amount out of range")
		}
		n = n*10 + d
	}
	limit := uint64(math.MaxInt64)
	if neg {
		limit++
	}
	if n > limit {
		return 0, errors.New("amount out of range")
	}
	if neg {
		if n == limit {
			return math.MinInt64, nil
		}
		return -int64(n), nil
	}
	return int64(n), nil
}
