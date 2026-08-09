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

// ParseEuros parses an operator-entered amount ("250", "250.5", "1,250.00",
// "1,50", "€12.34") into minor units. Rejects empty/garbage and more than 2
// decimal places — a silent truncation of a mistyped amount is a wrong payment.
//
// Separator rules, because this console is operated in the euro area: with both
// separators present, '.' is the decimal point and ',' groups thousands
// ("1,250.00"). With only a comma, it is the decimal comma when 1-2 digits follow
// ("1,50" is €1.50, NOT €150.00) and grouping otherwise ("1,250" is €1250).
func ParseEuros(s string) (int64, error) {
	s = strings.TrimSpace(s)
	s = strings.NewReplacer("€", "", " ", "").Replace(s)
	if s == "" {
		return 0, errors.New("empty amount")
	}
	dot, comma := strings.LastIndex(s, "."), strings.LastIndex(s, ",")
	switch {
	case dot >= 0 && comma >= 0:
		// Both present: the LAST one is the decimal separator, the other groups
		// thousands. Handles "1,250.00" and "1.234.567,89" alike.
		if comma > dot {
			s = strings.ReplaceAll(s, ".", "")
			s = strings.Replace(s, ",", ".", 1)
		} else {
			s = strings.ReplaceAll(s, ",", "")
		}
	case comma >= 0:
		if d := len(s) - comma - 1; strings.Count(s, ",") == 1 && d >= 1 && d <= 2 {
			s = s[:comma] + "." + s[comma+1:]
		} else {
			s = strings.ReplaceAll(s, ",", "")
		}
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
			return 0, fmt.Errorf("invalid amount %q: at most 2 decimal places", s)
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
