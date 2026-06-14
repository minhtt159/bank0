// Package iban validates and generates IBANs (ISO 13616) using the ISO 7064
// MOD-97-10 check-digit scheme. The algorithm + the per-country length table were
// research-verified (see docs/11-iban-verification.md): an executable check of the
// validator/generator against known IBANs passed, and the length table was
// cross-checked across four sources (SWIFT registry, iban.com, Wikipedia, samples).
//
// Validation order: strip spaces + uppercase, structural check (CC alpha, check
// digits, alphanumeric), exact per-country length, then MOD-97 == 1 computed by a
// running per-character fold (no bignum).
package iban

import (
	"crypto/rand"
	"fmt"
	"math/big"
	"strings"
)

// CountryLengths is the total IBAN length per ISO 3166-1 alpha-2 country code,
// from the SWIFT IBAN Registry (Release 99, Dec 2024). Range NO=15 .. RU=33.
var CountryLengths = map[string]int{
	"AL": 28, "AD": 24, "AT": 20, "AZ": 28, "BH": 22, "BY": 28, "BE": 16, "BA": 20,
	"BR": 29, "BG": 22, "BI": 27, "CR": 22, "HR": 21, "CY": 28, "CZ": 24, "DK": 18,
	"DJ": 27, "DO": 28, "TL": 23, "EG": 29, "SV": 28, "EE": 20, "FK": 18, "FO": 18,
	"FI": 18, "FR": 27, "GE": 22, "DE": 22, "GI": 23, "GR": 27, "GL": 18, "GT": 28,
	"HN": 28, "HU": 28, "IS": 26, "IQ": 23, "IE": 22, "IL": 23, "IT": 27, "JO": 30,
	"KZ": 20, "XK": 20, "KW": 30, "LV": 21, "LB": 28, "LY": 25, "LI": 21, "LT": 20,
	"LU": 20, "MT": 31, "MR": 27, "MU": 30, "MC": 27, "MD": 24, "MN": 20, "ME": 22,
	"NL": 18, "NI": 28, "MK": 19, "NO": 15, "OM": 23, "PK": 24, "PS": 29, "PL": 28,
	"PT": 25, "QA": 29, "RO": 24, "RU": 33, "LC": 32, "SM": 27, "ST": 25, "SA": 24,
	"RS": 22, "SC": 31, "SK": 24, "SI": 19, "SO": 23, "ES": 24, "SD": 18, "SE": 24,
	"CH": 21, "TN": 24, "TR": 26, "UA": 29, "AE": 23, "GB": 22, "VA": 22, "VG": 24,
	"YE": 30,
}

// Normalize strips all whitespace and uppercases — turning the printed form
// (groups of four) into the electronic form used for validation.
func Normalize(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z':
			b.WriteRune(r - 32)
		case r == ' ' || r == '\t' || r == '\n' || r == '\r':
			// drop whitespace
		default:
			b.WriteRune(r)
		}
	}
	return b.String()
}

// Validate reports why an IBAN is invalid (nil = valid). It enforces structure,
// the exact per-country length, and the MOD-97 checksum.
func Validate(s string) error {
	s = Normalize(s)
	if len(s) < 15 || len(s) > 34 {
		return fmt.Errorf("iban: length %d out of range (15-34)", len(s))
	}
	if !(isAlpha(s[0]) && isAlpha(s[1]) && isDigit(s[2]) && isDigit(s[3])) {
		return fmt.Errorf("iban: must start with 2 letters + 2 check digits")
	}
	cc := s[:2]
	want, ok := CountryLengths[cc]
	if !ok {
		return fmt.Errorf("iban: unknown country code %q", cc)
	}
	if len(s) != want {
		return fmt.Errorf("iban: %s length is %d, want %d", cc, len(s), want)
	}
	for i := 0; i < len(s); i++ {
		if !isAlnum(s[i]) {
			return fmt.Errorf("iban: non-alphanumeric character")
		}
	}
	if mod97(s[4:]+s[:4]) != 1 {
		return fmt.Errorf("iban: checksum failed")
	}
	return nil
}

// IsValid is Validate == nil.
func IsValid(s string) bool { return Validate(s) == nil }

// Compute builds a checksum-valid IBAN for a country code and BBAN: check digits =
// 98 - mod97(BBAN + CC + "00"). The BBAN must be the country's BBAN length
// (total - 4) and alphanumeric. Used by deterministic callers (e.g. the seed).
func Compute(cc, bban string) (string, error) {
	cc = strings.ToUpper(cc)
	bban = strings.ToUpper(bban)
	total, ok := CountryLengths[cc]
	if !ok {
		return "", fmt.Errorf("iban: unknown country code %q", cc)
	}
	if len(bban) != total-4 {
		return "", fmt.Errorf("iban: %s BBAN length is %d, want %d", cc, len(bban), total-4)
	}
	for i := 0; i < len(bban); i++ {
		if !isAlnum(bban[i]) {
			return "", fmt.Errorf("iban: BBAN must be alphanumeric")
		}
	}
	check := 98 - mod97(bban+cc+"00")
	return fmt.Sprintf("%s%02d%s", cc, check, bban), nil
}

// Generate returns a fresh checksum-valid IBAN for cc with a random NUMERIC BBAN
// (numeric BBANs are valid for the checksum and pass structural validation for
// every country — national BBAN sub-structure is not part of ISO 13616 validation).
// Uses crypto/rand, so it is concurrency-safe and collision-resistant.
func Generate(cc string) (string, error) {
	cc = strings.ToUpper(cc)
	total, ok := CountryLengths[cc]
	if !ok {
		return "", fmt.Errorf("iban: unknown country code %q", cc)
	}
	n := total - 4
	digits := make([]byte, n)
	for i := 0; i < n; i++ {
		b, err := rand.Int(rand.Reader, big.NewInt(10))
		if err != nil {
			return "", err
		}
		digits[i] = byte('0' + b.Int64())
	}
	return Compute(cc, string(digits))
}

// mod97 computes the integer value of s (letters mapped A=10..Z=35) modulo 97,
// folding character-by-character so the accumulator never exceeds ~9700 — no
// bignum needed. Non-alphanumeric bytes are skipped (callers validate charset).
func mod97(s string) int {
	rem := 0
	for i := 0; i < len(s); i++ {
		c := s[i]
		switch {
		case c >= '0' && c <= '9':
			rem = (rem*10 + int(c-'0')) % 97
		case c >= 'A' && c <= 'Z':
			rem = (rem*100 + int(c-'A') + 10) % 97 // two-digit value 10..35
		}
	}
	return rem
}

func isDigit(b byte) bool { return b >= '0' && b <= '9' }
func isAlpha(b byte) bool { return b >= 'A' && b <= 'Z' }
func isAlnum(b byte) bool { return isDigit(b) || isAlpha(b) }
