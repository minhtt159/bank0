// Package money formats integer minor units (euro cents) for display. Storage
// and transport stay in minor units; only presentation converts.
package money

import "fmt"

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
