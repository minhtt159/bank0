package api

import (
	"net/http"
	"net/url"
	"strings"
	"testing"

	sqlc "github.com/minhtt159/bank0/internal/db/sqlc"
)

// HTTP: server-side CoP verdict + warning-ack evidence.

func TestHTTPResolveVerdictAndWarningAck(t *testing.T) {
	ts, pg := newTestServer(t)
	payerID, payerName := mkUser(t, pg, sqlc.UserRoleCustomer)
	payerAcct := mkAcct(t, pg, payerID, 10_000)
	payeeID, _ := mkUser(t, pg, sqlc.UserRoleCustomer)
	payeeAcct := mkAcct(t, pg, payeeID, 0)
	if _, err := pg.Pool.Exec(t.Context(),
		`UPDATE users SET full_name = 'Greta Svensson' WHERE id = $1`, payeeID); err != nil {
		t.Fatalf("set name: %v", err)
	}
	var payeeIban string
	if err := pg.Pool.QueryRow(t.Context(),
		`SELECT iban FROM accounts WHERE id = $1`, payeeAcct).Scan(&payeeIban); err != nil {
		t.Fatalf("iban: %v", err)
	}

	tok := bearerFor(t, ts.URL, payerName, "pw")
	auth := map[string]string{"Authorization": "Bearer " + tok}

	// Typo'd name -> close_match + the registered name + an ack-gate.
	u := ts.URL + "/beneficiaries/resolve?iban=" + url.QueryEscape(payeeIban) + "&name=" + url.QueryEscape("Greta Svenson")
	r := get(t, http.DefaultClient, u, auth)
	if r.StatusCode != http.StatusOK {
		t.Fatalf("resolve = %d", r.StatusCode)
	}
	b := body(t, r)
	for _, want := range []string{`"match_result":"close_match"`, `"suggested_name":"Greta Svensson"`, `"gate":"awaiting_acknowledgement"`} {
		if !strings.Contains(b, want) {
			t.Errorf("resolve body missing %s: %s", want, b)
		}
	}
	// No name -> unable; masked name still present, full name absent.
	r = get(t, http.DefaultClient, ts.URL+"/beneficiaries/resolve?iban="+url.QueryEscape(payeeIban), auth)
	if b := body(t, r); !strings.Contains(b, `"match_result":"unable"`) || strings.Contains(b, `"suggested_name"`) {
		t.Errorf("no-name resolve leaked or mis-verdicted: %s", b)
	}

	// The customer proceeds anyway: evidence row.
	ar := postJSON(t, ts.URL+"/me/warning-acks", auth, map[string]any{
		"category": "cop_close_match", "reason_code": "CLOSE_MATCH",
		"debit_account_id": payerAcct.String(), "counterparty_iban": payeeIban,
		"amount_minor": 5_000, "device": "web",
	})
	if ar.StatusCode != http.StatusCreated {
		t.Fatalf("warning ack = %d: %s", ar.StatusCode, body(t, ar))
	}

	// Bad category -> 422; anonymous -> 401.
	if r := postJSON(t, ts.URL+"/me/warning-acks", auth, map[string]any{"category": "nah"}); r.StatusCode != http.StatusUnprocessableEntity {
		t.Errorf("bad category = %d, want 422", r.StatusCode)
	}
	if r := postJSON(t, ts.URL+"/me/warning-acks", nil, map[string]any{"category": "other"}); r.StatusCode != http.StatusUnauthorized {
		t.Errorf("anon ack = %d, want 401", r.StatusCode)
	}
}
