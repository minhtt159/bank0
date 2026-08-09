package money

import "testing"

func TestParseEuros(t *testing.T) {
	ok := []struct {
		in   string
		want int64
	}{
		{"0", 0},
		{"0.01", 1},
		{"250", 25000},
		{"250.5", 25050},
		{"250.50", 25050},
		{"1,250.00", 125000},
		{"€12.34", 1234},
		{" 100 ", 10000},
		{"-5", -500},
		{"-12.34", -1234},
		{".5", 50}, // missing whole part
		// Decimal comma: a euro-area operator types "1,50" for EUR 1.50. Reading it
		// as grouping would post EUR 150.00 — a 100x wrong payment.
		{"1,50", 150},
		{"12,3", 1230},
		{"1,250", 125000},     // 3 digits after the comma -> grouping, not decimals
		{"1.234.567,89", 123456789}, // full euro-style formatting
	}
	for _, c := range ok {
		got, err := ParseEuros(c.in)
		if err != nil {
			t.Errorf("ParseEuros(%q) unexpected error: %v", c.in, err)
			continue
		}
		if got != c.want {
			t.Errorf("ParseEuros(%q) = %d, want %d", c.in, got, c.want)
		}
	}

	// >2 decimals are REJECTED, never truncated: "10.999" is a typo, and silently
	// booking EUR 10.99 hides it.
	bad := []string{"", "   ", "abc", "12.x", "€", "10.999", "0.001"}
	for _, in := range bad {
		if _, err := ParseEuros(in); err == nil {
			t.Errorf("ParseEuros(%q) expected error, got nil", in)
		}
	}
}

func TestFormatMinor(t *testing.T) {
	cases := map[int64]string{
		0:     "€0.00",
		5:     "€0.05",
		1050:  "€10.50",
		-1050: "-€10.50",
		100000: "€1000.00",
	}
	for in, want := range cases {
		if got := FormatMinor(in); got != want {
			t.Errorf("FormatMinor(%d) = %q, want %q", in, got, want)
		}
	}
}

func TestPlainMinor(t *testing.T) {
	cases := map[int64]string{0: "0.00", 5: "0.05", 1050: "10.50", -1050: "-10.50"}
	for in, want := range cases {
		if got := PlainMinor(in); got != want {
			t.Errorf("PlainMinor(%d) = %q, want %q", in, got, want)
		}
	}
}

// Round-trip: a formatted plain amount parses back to the same minor units.
func TestPlainMinorParseRoundTrip(t *testing.T) {
	for _, m := range []int64{0, 1, 99, 100, 12345, 99999999} {
		got, err := ParseEuros(PlainMinor(m))
		if err != nil || got != m {
			t.Errorf("round-trip %d -> %q -> %d (err %v)", m, PlainMinor(m), got, err)
		}
	}
}
