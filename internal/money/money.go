// Package money formats integer minor units (euro cents) for display. Storage
// and transport stay in minor units; only presentation converts.
package money

import (
	"errors"
	"fmt"
	"strconv"
	"strings"
)

// FormatMinor renders minor units as a euro string, e.g. 1050 -> "€10.50".
func FormatMinor(minor int64) string {
	neg := minor < 0
	if neg {
		minor = -minor
	}
	s := fmt.Sprintf("€%d.%02d", minor/100, minor%100)
	if neg {
		return "-" + s
	}
	return s
}

// PlainMinor renders minor units without the symbol, e.g. 1050 -> "10.50".
// Used for pre-filling number inputs.
func PlainMinor(minor int64) string {
	neg := minor < 0
	if neg {
		minor = -minor
	}
	s := fmt.Sprintf("%d.%02d", minor/100, minor%100)
	if neg {
		return "-" + s
	}
	return s
}

// ParseEuros parses an operator-entered amount ("250", "250.5", "1,250.00", "€12.34")
// into minor units. Rejects empty/garbage; tolerates 0-2 decimal places.
func ParseEuros(s string) (int64, error) {
	s = strings.TrimSpace(s)
	s = strings.NewReplacer("€", "", ",", "", " ", "").Replace(s)
	if s == "" {
		return 0, errors.New("empty amount")
	}
	neg := strings.HasPrefix(s, "-")
	s = strings.TrimPrefix(s, "-")
	whole, frac, hasFrac := strings.Cut(s, ".")
	if whole == "" {
		whole = "0"
	}
	w, err := strconv.ParseInt(whole, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("invalid amount %q", s)
	}
	var cents int64
	if hasFrac {
		switch {
		case len(frac) == 1:
			frac += "0"
		case len(frac) > 2:
			frac = frac[:2]
		}
		cents, err = strconv.ParseInt(frac, 10, 64)
		if err != nil {
			return 0, fmt.Errorf("invalid amount %q", s)
		}
	}
	minor := w*100 + cents
	if neg {
		minor = -minor
	}
	return minor, nil
}
