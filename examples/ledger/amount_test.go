package ledger

import "testing"

func TestParseCentsValid(t *testing.T) {
	cases := []struct {
		input string
		want  int64
	}{
		{"0", 0}, {"00", 0}, {"00012", 1200}, {"12.3", 1230}, {"12.34", 1234}, {"+1", 100}, {"-1.02", -102}, {"  7.05\t", 705},
		{"92233720368547758.07", 9223372036854775807}, {"-92233720368547758.08", -9223372036854775808},
	}
	for _, tc := range cases {
		t.Run(tc.input, func(t *testing.T) {
			got, err := ParseCents(tc.input)
			if err != nil || got != tc.want {
				t.Fatalf("ParseCents(%q) = %d, %v; want %d, nil", tc.input, got, err, tc.want)
			}
		})
	}
}

func TestParseCentsInvalid(t *testing.T) {
	cases := []string{"", " ", "+", "-", ".5", "1.", "1.234", "1.2.3", "1e2", "--1", "1,00", "１２", "92233720368547758.08", "-92233720368547758.09", "184467440737095516.16"}
	for _, input := range cases {
		t.Run(input, func(t *testing.T) {
			if got, err := ParseCents(input); err == nil {
				t.Fatalf("ParseCents(%q) = %d, want error", input, got)
			}
		})
	}
}
