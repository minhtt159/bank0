package api

import (
	"context"
	"net/url"
	"strings"
	"testing"

	sqlc "github.com/minhtt159/bank0/internal/db/sqlc"
	"github.com/minhtt159/bank0/internal/iban"
)

// Regression for the IBAN review findings (operator-console create-account, M1):
// the console path previously had NO iban.IsValid gate and stored the raw form
// value. It must now (1) reject a checksum-invalid IBAN without creating an
// account, and (2) accept a valid IBAN in printed form (lowercase + spaces) and
// store it in canonical electronic form so it stays resolvable by the exact-match
// IBAN lookup (resolve_account_by_iban).
func TestHTTPConsoleCreateAccountValidatesAndNormalizesIban(t *testing.T) {
	ts, pg := newTestServer(t)
	ctx := context.Background()
	_, adminName := mkUser(t, pg, sqlc.UserRoleAdmin)
	admin := login(t, ts, adminName, "pw")

	// --- valid IBAN in printed form (lowercase + a space) is accepted + normalized ---
	good, err := iban.Generate("NL")
	if err != nil {
		t.Fatalf("gen iban: %v", err)
	}
	printed := strings.ToLower(good[:8] + " " + good[8:]) // e.g. "nl91 0000..."
	okCust, _ := mkUser(t, pg, sqlc.UserRoleCustomer)

	r := admin
	resp, err := r.PostForm(ts.URL+"/console/users/"+okCust.String()+"/accounts", url.Values{
		"iban": {printed}, "pin": {"1234"}, "limit": {"100.00"},
	})
	if err != nil || resp.StatusCode != 200 {
		t.Fatalf("console create (printed IBAN) err=%v status=%v, want 200", err, resp.StatusCode)
	}
	var stored string
	if err := pg.Pool.QueryRow(ctx,
		`SELECT iban FROM accounts WHERE user_id=$1 AND kind='customer'`, okCust).Scan(&stored); err != nil {
		t.Fatalf("printed IBAN should have created exactly one account: %v", err)
	}
	if stored != good {
		t.Errorf("stored iban = %q, want normalized %q", stored, good)
	}

	// --- checksum-invalid IBAN is rejected; no account is created ---
	badCust, _ := mkUser(t, pg, sqlc.UserRoleCustomer)
	resp, err = admin.PostForm(ts.URL+"/console/users/"+badCust.String()+"/accounts", url.Values{
		"iban": {"GB00WEST12345698765432"}, "pin": {"1234"}, "limit": {"100.00"}, // format-valid, bad checksum
	})
	if err != nil {
		t.Fatalf("console create (bad IBAN): %v", err)
	}
	if got := strings.ToLower(body(t, resp)); !strings.Contains(got, "invalid iban") {
		t.Errorf("bad IBAN response should flag 'Invalid IBAN'; body did not")
	}
	var n int
	if err := pg.Pool.QueryRow(ctx, `SELECT count(*) FROM accounts WHERE user_id=$1`, badCust).Scan(&n); err != nil {
		t.Fatalf("count accounts: %v", err)
	}
	if n != 0 {
		t.Errorf("checksum-invalid IBAN must not create an account; got %d", n)
	}
}
