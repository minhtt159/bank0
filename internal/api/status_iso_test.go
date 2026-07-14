package api

import (
	"net/http"
	"strings"
	"testing"

	sqlc "github.com/minhtt159/bank0/internal/db/sqlc"
)

// The rail-ready additive fields (Rec 19/20): status_iso on transfer responses and
// currency on dispute responses. Contract-only — no UI/business change.

// TestHTTPStatusIsoOnTransfers: an auto-posted transfer carries status_iso=ACSC on
// the create response, its GET, and its list row; a held payment carries PDNG.
func TestHTTPStatusIsoOnTransfers(t *testing.T) {
	ts, pg := newTestServer(t)
	clearGates(t, pg)
	aliceID, aliceName := mkUser(t, pg, sqlc.UserRoleCustomer)
	aliceAcct := mkAcct(t, pg, aliceID, 1_000_000)
	bobID, _ := mkUser(t, pg, sqlc.UserRoleCustomer)
	bobAcct := mkAcct(t, pg, bobID, 0)
	tok := clientToken(t, ts, aliceName, "pw")
	auth := map[string]string{"Authorization": "Bearer " + tok}

	// Auto-post -> ACSC on the create response.
	r := createXfer(t, ts.URL, tok, aliceAcct, bobAcct, 5_000)
	if r.StatusCode != http.StatusOK {
		t.Fatalf("create = %d, want 200: %s", r.StatusCode, body(t, r))
	}
	var res struct {
		TransferID string `json:"transfer_id"`
		Status     string `json:"status"`
		StatusIso  string `json:"status_iso"`
	}
	decodeBody(t, r, &res)
	if res.Status != "posted" || res.StatusIso != "ACSC" {
		t.Fatalf("create status=%q status_iso=%q, want posted/ACSC", res.Status, res.StatusIso)
	}

	// GET /transfers/{id} carries it.
	if gb := body(t, get(t, http.DefaultClient, ts.URL+"/transfers/"+res.TransferID, auth)); !strings.Contains(gb, `"status_iso":"ACSC"`) {
		t.Errorf("GET transfer missing status_iso ACSC: %s", gb)
	}
	// List items carry it.
	if lb := body(t, get(t, http.DefaultClient, ts.URL+"/transfers", auth)); !strings.Contains(lb, `"status_iso":"ACSC"`) {
		t.Errorf("list item missing status_iso ACSC: %s", lb)
	}

	// A held outcome -> PDNG (reuse the warning-rule fixture pattern; 'review' parks
	// a first payment to a new payee as held).
	addWarningRule(t, pg, "first_payment_to_payee", "", "risk_warning", "review", false, 0, 100)
	carolID, _ := mkUser(t, pg, sqlc.UserRoleCustomer)
	carolAcct := mkAcct(t, pg, carolID, 0)
	hr := createXfer(t, ts.URL, tok, aliceAcct, carolAcct, 7_000)
	if hr.StatusCode != http.StatusOK {
		t.Fatalf("held create = %d, want 200: %s", hr.StatusCode, body(t, hr))
	}
	var held struct {
		Status    string `json:"status"`
		StatusIso string `json:"status_iso"`
	}
	decodeBody(t, hr, &held)
	if held.Status != "held" || held.StatusIso != "PDNG" {
		t.Fatalf("held create status=%q status_iso=%q, want held/PDNG", held.Status, held.StatusIso)
	}
}

// TestHTTPDisputeCarriesCurrency: the dispute create + GET responses carry the
// disputed transfer's ISO-4217 currency (EUR).
func TestHTTPDisputeCarriesCurrency(t *testing.T) {
	ts, pg := newTestServer(t)
	resetDisputes(t, pg)
	aliceID, aliceName := mkUser(t, pg, sqlc.UserRoleCustomer)
	aliceAcct := mkAcct(t, pg, aliceID, 100_000)
	bobID, _ := mkUser(t, pg, sqlc.UserRoleCustomer)
	bobAcct := mkAcct(t, pg, bobID, 0)
	aliceTok := clientToken(t, ts, aliceName, "pw")

	tid := postTransfer(t, ts, aliceTok, aliceAcct, bobAcct, 1000)
	code, b := doDispute(t, ts, aliceTok, tid, `{"category":"fraud","reason":"x"}`)
	if code != 201 {
		t.Fatalf("raise = %d, want 201: %s", code, b)
	}
	if cur := gjson(t, b, "currency"); cur != "EUR" {
		t.Errorf("raise dispute currency = %q, want EUR", cur)
	}
	did := disputeID(t, b)
	if gc, gb := clientGet(t, ts, aliceTok, "/disputes/"+did); gc != 200 || gjson(t, gb, "currency") != "EUR" {
		t.Errorf("get dispute currency: code=%d body=%s", gc, gb)
	}
}
