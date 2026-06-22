package iban

import (
	"os"
	"regexp"
	"strconv"
	"testing"
)

// TestCountryLengthsNoDrift cross-checks the per-country IBAN length table across
// its three hand-maintained copies — Go (this package), the PWA (TypeScript), and
// the Postgres iban_country_length() function. The triplication is deliberate
// defense-in-depth, but editing one copy without the others would silently diverge;
// this fails loudly instead (the audit's IBAN-DRIFT-TEST-MISSING).
func TestCountryLengthsNoDrift(t *testing.T) {
	ts := parseCountryMap(t, "../../web/app/src/lib/iban.ts",
		regexp.MustCompile(`([A-Z]{2}):\s*(\d+)`))
	sql := parseCountryMap(t, "../../db/migrations/00002_iban.sql",
		regexp.MustCompile(`WHEN '([A-Z]{2})' THEN (\d+)`))

	compareToGo(t, "TypeScript (web/app/src/lib/iban.ts)", ts)
	compareToGo(t, "SQL (db/migrations/00002_iban.sql)", sql)
}

func parseCountryMap(t *testing.T, path string, re *regexp.Regexp) map[string]int {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	m := map[string]int{}
	for _, match := range re.FindAllStringSubmatch(string(b), -1) {
		n, _ := strconv.Atoi(match[2])
		m[match[1]] = n
	}
	if len(m) == 0 {
		t.Fatalf("parsed 0 country entries from %s — the extraction regex has drifted", path)
	}
	return m
}

func compareToGo(t *testing.T, name string, other map[string]int) {
	t.Helper()
	if len(other) != len(CountryLengths) {
		t.Errorf("%s has %d entries, Go table has %d", name, len(other), len(CountryLengths))
	}
	for cc, n := range CountryLengths {
		if got, ok := other[cc]; !ok {
			t.Errorf("%s is missing %s (Go: %s=%d)", name, cc, cc, n)
		} else if got != n {
			t.Errorf("%s has %s=%d but Go has %s=%d", name, cc, got, cc, n)
		}
	}
	for cc, n := range other {
		if _, ok := CountryLengths[cc]; !ok {
			t.Errorf("%s has %s=%d which is absent from the Go table", name, cc, n)
		}
	}
}
