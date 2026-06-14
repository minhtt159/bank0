//go:build e2e

package e2e

import (
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"sync/atomic"

	"github.com/google/uuid"
)

// uniq returns a short, process-unique token for building collision-free usernames
// (the suite shares one DB without truncation, as docs/specs/spec-e2e-harness.md asks).
func uniq() string {
	return strings.ToLower(strings.ReplaceAll(uuid.NewString(), "-", ""))[:12]
}

// ibanSalt perturbs the 10-digit account number per process so two runs against the
// same shared DB don't collide on accounts_iban_key (the suite never truncates).
var ibanSalt = int64(uuid.New().ID())

// ibanCtr makes each generated account number unique within a run.
var ibanCtr int64

// genNLIBAN builds a structurally-valid, checksum-correct NL IBAN (18 chars):
//
//	NL kk BBBB nnnnnnnnnn   (kk = check digits, BBBB = 4-letter bank code, n = 10 digits)
//
// Computed with ISO 7064 MOD-97-10 — the same algorithm the DB CHECK
// (accounts_iban_checksum / iban_is_valid) and the iban package enforce. Generating
// it in the test keeps the suite black-box (no production iban import needed). The
// account number is salted from a per-process UUID so reruns against the same shared
// DB get fresh IBANs without truncation.
func genNLIBAN() string {
	n := (ibanSalt + atomic.AddInt64(&ibanCtr, 1)) % 10_000_000_000
	if n < 0 {
		n += 10_000_000_000
	}
	bank := "ABNA"
	account := fmt.Sprintf("%010d", n)
	bban := bank + account
	check := ibanCheckDigits("NL", bban)
	return fmt.Sprintf("NL%s%s", check, bban)
}

// ibanCheckDigits returns the two check digits for country+bban: move CC+"00" to the
// end, convert letters A..Z -> 10..35, then 98 - (number mod 97), zero-padded.
func ibanCheckDigits(cc, bban string) string {
	rearranged := bban + cc + "00"
	rem := 0
	for _, c := range rearranged {
		switch {
		case c >= '0' && c <= '9':
			rem = (rem*10 + int(c-'0')) % 97
		case c >= 'A' && c <= 'Z':
			rem = (rem*100 + int(c-'A') + 10) % 97
		default:
			panic("invalid char in BBAN: " + string(c))
		}
	}
	check := 98 - rem
	return fmt.Sprintf("%02d", check)
}

// urlParse is a tiny wrapper so the test files don't each import net/url just for
// the cookie-jar lookup.
func urlParse(s string) (*url.URL, error) { return url.Parse(s) }

// cookieHeader renders a slice of cookies as a single Cookie request header value.
func cookieHeader(cookies []*http.Cookie) string {
	parts := make([]string, 0, len(cookies))
	for _, c := range cookies {
		parts = append(parts, c.Name+"="+c.Value)
	}
	return strings.Join(parts, "; ")
}
