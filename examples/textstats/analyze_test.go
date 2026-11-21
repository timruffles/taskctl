package textstats

import "testing"

func TestAnalyze(t *testing.T) {
	tests := []struct {
		name string
		text string
		want Stats
	}{
		{name: "empty", text: "", want: Stats{}},
		{name: "ascii", text: "one two\nthree", want: Stats{Lines: 2, Words: 3, Runes: 13}},
		{name: "unicode whitespace", text: "  café\u2003東京\t🙂  ", want: Stats{Lines: 1, Words: 3, Runes: 13}},
		{name: "single line", text: "a", want: Stats{Lines: 1, Words: 1, Runes: 1}},
		{name: "trailing newline", text: "a\n", want: Stats{Lines: 1, Words: 1, Runes: 2}},
		{name: "blank line", text: "\n", want: Stats{Lines: 1, Words: 0, Runes: 1}},
		{name: "multiple trailing newlines", text: "a\n\n", want: Stats{Lines: 2, Words: 1, Runes: 3}},
		{name: "crlf", text: "a\r\nb", want: Stats{Lines: 2, Words: 2, Runes: 4}},
		{name: "invalid utf8", text: string([]byte{'a', 0xff, ' ', 0xc3}), want: Stats{Lines: 1, Words: 2, Runes: 4}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := Analyze(tt.text); got != tt.want {
				t.Errorf("Analyze(%q) = %+v, want %+v", tt.text, got, tt.want)
			}
		})
	}
}
