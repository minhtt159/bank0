//go:build e2e

package e2e

import (
	"net/http"
	"testing"

	"github.com/google/uuid"
)

// Scenario 2 — cross-surface money flow.
//
// REAL cross-surface provisioning (not a DB-seed shortcut): an OPERATOR drives the
// PORTAL binary (cookie session) entirely through the admin JSON API to create two
// customers, give each an account, and fund the payer. Then the CUSTOMER drives the
// API binary (JWT) to log in, see the balance, transfer to the payee, and observe
// both the debit row in /transfers and the new balance — all asserted against the
// api JSON, never HTML, so a UI change can't mask a ledger bug (the spec's rule).
//
// The two binaries share one Postgres, so a row the operator writes on the portal is
// immediately visible to the customer on the api: that cross-process visibility is
// the property this scenario proves.

const (
	custPassword = "password" // shared test password for provisioned customers
	openingMinor = 50_000      // €500 opening balance (well under the maker-checker threshold)
	transferMinor = 12_345     // €123.45 customer-initiated transfer
)

// provisionCustomer creates a user + funded(optional) account via the PORTAL admin
// JSON API and returns the customer's username, user id, and account id.
func provisionCustomer(t *testing.T, op *portalSession, fund int64) (username string, userID, accountID uuid.UUID) {
	t.Helper()
	username = "cust_" + uniq()

	// POST /users (admin JSON) -> 201 {id}
	resp := op.postJSON("/users", map[string]any{
		"username":  username,
		"password":  custPassword,
		"full_name": "E2E Customer " + username,
		"role":      "customer",
	})
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("operator POST /users = %d, want 201 (body=%.200s)", resp.StatusCode, readBody(t, resp))
	}
	var created struct {
		ID uuid.UUID `json:"id"`
	}
	decodeJSONBody(t, resp, &created)
	userID = created.ID

	// POST /accounts (admin JSON) -> 201 {id}
	resp = op.postJSON("/accounts", map[string]any{
		"user_id":              userID.String(),
		"iban":                 genNLIBAN(),
		"pin":                  "1234",
		"transfer_limit_minor": 100_000_000,
	})
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("operator POST /accounts = %d, want 201 (body=%.200s)", resp.StatusCode, readBody(t, resp))
	}
	var acct struct {
		ID uuid.UUID `json:"id"`
	}
	decodeJSONBody(t, resp, &acct)
	accountID = acct.ID

	// POST /accounts/{id}/deposit (admin JSON) -> 200 {transfer_id, requires_approval}
	if fund > 0 {
		resp = op.postJSONIdem("/accounts/"+accountID.String()+"/deposit", uuid.NewString(), map[string]any{
			"amount_minor": fund,
			"description":  "E2E opening deposit",
		})
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("operator deposit = %d, want 200 (body=%.200s)", resp.StatusCode, readBody(t, resp))
		}
		resp.Body.Close()
	}
	return username, userID, accountID
}

// acctBalanceViaAPI reads an account's balance through the API binary's client JSON
// (the customer's own view) — the authoritative assertion target.
func acctBalanceViaAPI(t *testing.T, cust *apiSession, accountID uuid.UUID) (balance, available int64) {
	t.Helper()
	resp := cust.get("/accounts/" + accountID.String())
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("customer GET /accounts/%s = %d, want 200 (body=%.200s)", accountID, resp.StatusCode, readBody(t, resp))
	}
	var a struct {
		BalanceMinor   int64 `json:"balance_minor"`
		AvailableMinor int64 `json:"available_minor"`
	}
	decodeJSONBody(t, resp, &a)
	return a.BalanceMinor, a.AvailableMinor
}

func TestCrossSurface_MoneyFlow(t *testing.T) {
	e := requireHarness(t)

	// --- operator on the PORTAL provisions both sides --------------------------
	op := loginPortal(t, e.portal.baseURL, "admin", "admin")
	payerName, payerUserID, payerAcct := provisionCustomer(t, op, openingMinor)
	payeeName, _, payeeAcct := provisionCustomer(t, op, 0)

	// --- customer on the API logs in and verifies their starting state ---------
	cust := loginAPI(t, e.api.baseURL, payerName, custPassword)

	// GET /me resolves to the user the operator just created on the *other* binary.
	meResp := cust.get("/me")
	if meResp.StatusCode != http.StatusOK {
		t.Fatalf("customer GET /me = %d, want 200 (body=%.200s)", meResp.StatusCode, readBody(t, meResp))
	}
	var me struct {
		ID       uuid.UUID `json:"id"`
		Username string    `json:"username"`
	}
	decodeJSONBody(t, meResp, &me)
	if me.ID != payerUserID {
		t.Fatalf("GET /me id = %s, want %s (api can't see the user the portal created)", me.ID, payerUserID)
	}
	if me.Username != payerName {
		t.Fatalf("GET /me username = %q, want %q", me.Username, payerName)
	}

	// The opening balance the operator deposited on the portal is visible on the api.
	if bal, avail := acctBalanceViaAPI(t, cust, payerAcct); bal != openingMinor || avail != openingMinor {
		t.Fatalf("opening balance via api = (bal=%d, avail=%d), want %d (portal deposit not visible on api)",
			bal, avail, openingMinor)
	}

	// --- customer initiates a transfer to the payee ----------------------------
	idem := uuid.NewString()
	trResp := cust.postJSONIdem("/transfers", idem, map[string]any{
		"debit_account":  payerAcct.String(),
		"credit_account": payeeAcct.String(),
		"amount_minor":   transferMinor,
		"description":    "E2E cross-surface transfer",
	})
	if trResp.StatusCode != http.StatusOK {
		t.Fatalf("customer POST /transfers = %d, want 200 (body=%.200s)", trResp.StatusCode, readBody(t, trResp))
	}
	var transfer struct {
		TransferID uuid.UUID `json:"transfer_id"`
		Status     string    `json:"status"`
		WasReplay  bool      `json:"was_replay"`
	}
	decodeJSONBody(t, trResp, &transfer)
	if transfer.Status != "posted" {
		t.Fatalf("transfer status = %q, want posted", transfer.Status)
	}
	if transfer.TransferID == uuid.Nil {
		t.Fatal("transfer returned a nil transfer_id")
	}

	// --- the debit is visible in the customer's /transfers history -------------
	listResp := cust.get("/transfers")
	if listResp.StatusCode != http.StatusOK {
		t.Fatalf("customer GET /transfers = %d, want 200", listResp.StatusCode)
	}
	// /transfers (listMyTransfers) returns a bare array of TRANSFER rows the caller
	// is a party to, newest first, with a per-caller "direction" (out/in). The payer
	// just sent money, so it appears as an outbound debit from their account.
	var rows []struct {
		ID              uuid.UUID `json:"id"`
		DebitAccountID  uuid.UUID `json:"debit_account_id"`
		CreditAccountID uuid.UUID `json:"credit_account_id"`
		AmountMinor     int64     `json:"amount_minor"`
		Status          string    `json:"status"`
		Kind            string    `json:"kind"`
		Direction       string    `json:"direction"`
	}
	decodeJSONBody(t, listResp, &rows)
	var found bool
	for _, row := range rows {
		if row.ID != transfer.TransferID {
			continue
		}
		found = true
		if row.Direction != "out" {
			t.Errorf("payer's transfer direction = %q, want out (debit side)", row.Direction)
		}
		if row.DebitAccountID != payerAcct {
			t.Errorf("debit_account_id = %s, want payer %s", row.DebitAccountID, payerAcct)
		}
		if row.CreditAccountID != payeeAcct {
			t.Errorf("credit_account_id = %s, want payee %s", row.CreditAccountID, payeeAcct)
		}
		if row.AmountMinor != transferMinor {
			t.Errorf("transfer amount_minor = %d, want %d", row.AmountMinor, transferMinor)
		}
		if row.Status != "posted" {
			t.Errorf("transfer status = %q, want posted", row.Status)
		}
		if row.Kind != "transfer" {
			t.Errorf("transfer kind = %q, want transfer", row.Kind)
		}
	}
	if !found {
		t.Fatalf("transfer %s not found in payer's /transfers history", transfer.TransferID)
	}

	// --- balances reflect the debit (asserted against api JSON, not HTML) -------
	wantPayer := int64(openingMinor - transferMinor)
	if bal, avail := acctBalanceViaAPI(t, cust, payerAcct); bal != wantPayer || avail != wantPayer {
		t.Errorf("payer balance after transfer = (bal=%d, avail=%d), want %d", bal, avail, wantPayer)
	}

	// The credit side landed too. Verify it through the PAYEE's own api login — a
	// different JWT subject — so this is the real credited balance, not a debit-side
	// illusion. (Both ledger legs of a posted double-entry transfer must agree.)
	payee := loginAPI(t, e.api.baseURL, payeeName, custPassword)
	if bal, avail := acctBalanceViaAPI(t, payee, payeeAcct); bal != transferMinor || avail != transferMinor {
		t.Errorf("payee balance after transfer = (bal=%d, avail=%d), want %d", bal, avail, transferMinor)
	}
}
