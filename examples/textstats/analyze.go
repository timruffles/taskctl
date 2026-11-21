package textstats

import (
	"strings"
	"unicode/utf8"
)

type Stats struct {
	Lines int `json:"lines"`
	Words int `json:"words"`
	Runes int `json:"runes"`
}

func Analyze(text string) Stats {
	lines := strings.Count(text, "\n")
	if len(text) > 0 && !strings.HasSuffix(text, "\n") {
		lines++
	}
	return Stats{
		Lines: lines,
		Words: len(strings.Fields(text)),
		Runes: utf8.RuneCountInString(text),
	}
}
