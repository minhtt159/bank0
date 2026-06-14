package api

import (
	"context"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/minhtt159/bank0/internal/db"
	sqlc "github.com/minhtt159/bank0/internal/db/sqlc"
)

// mkNamedUser creates a customer with a specific full name so we can assert on
// the masked-owner-name (confirmation-of-payee) output.
func mkNamedUser(t *testing.T, pg *db.Postgres, fullName string) (uuid.UUID, string) {
	t.Helper()
	name := "u" + uhex(16)
	id, err := pg.Queries.CreateUser(context.Background(), sqlc.CreateUserParams{
		Username: name, Password: "pw", FullName: fullName, Role: sqlc.UserRoleCustomer,
	})
	if err != nil {
		t.Fatalf("create user: %v", err)
	}
	return id, name
}

func acctIban(t *testing.T, pg *db.Postgres, acct uuid.UUID) string {
	t.Helper()
	a, err := pg.Queries.GetAccount(context.Background(), acct)
	if err != nil || a.Iban == nil {
		t.Fatalf("get account iban: %v", err)
	}
	return *a.Iban
}

// authed JSON request helpers (Bearer token).
func doJSON(t *testing.T, c *http.Client, method, url, tok, jsonBody string) *http.Response {
	t.Helper()
	var rdr *strings.Reader
	if jsonBody != "" {
		rdr = strings.NewReader(jsonBody)
	} else {
		rdr = strings.NewReader("")
	}
	req, _ := http.NewRequest(method, url, rdr)
	req.Header.Set("Authorization", "Bearer "+tok)
	if jsonBody != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := c.Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, url, err)
	}
	return resp
}

// GetMe returns the caller's own profile (scoped to the JWT subject) and never
// leaks the password hash.
func TestHTTPGetMe(t *testing.T) {
	ts, pg := newTestServer(t)
	aliceID, aliceName := mkUser(t, pg, sqlc.UserRoleCustomer)
	_ = aliceID
	anon := newClient()

	if r := get(t, anon, ts.URL+"/me", nil); r.StatusCode != 401 {
		t.Errorf("no-token /me = %d, want 401", r.StatusCode)
	}

	tok := clientToken(t, ts, aliceName, "pw")
	resp := get(t, anon, ts.URL+"/me", map[string]string{"Authorization": "Bearer " + tok})
	if resp.StatusCode != 200 {
		t.Fatalf("/me = %d, want 200", resp.StatusCode)
	}
	b := body(t, resp)
	if !strings.Contains(b, aliceName) {
		t.Errorf("/me body should contain own username; got %.160s", b)
	}
	if strings.Contains(b, "password_hash") {
		t.Errorf("/me must not leak password_hash; got %.160s", b)
	}
}

// Beneficiaries: resolve (confirmation-of-payee), add/list/delete, ownership
// scoping, self-add rejection, duplicate rejection, and a transfer to a saved
// payee's account.
func TestHTTPBeneficiaries(t *testing.T) {
	ts, pg := newTestServer(t)
	aliceID, aliceName := mkUser(t, pg, sqlc.UserRoleCustomer)
	aliceAcct := mkAcct(t, pg, aliceID, 10_000)
	bobID, _ := mkNamedUser(t, pg, "Bob Builder")
	bobAcct := mkAcct(t, pg, bobID, 0)
	bobIban := acctIban(t, pg, bobAcct)
	aliceIban := acctIban(t, pg, aliceAcct)

	tok := clientToken(t, ts, aliceName, "pw")
	bearer := map[string]string{"Authorization": "Bearer " + tok}
	anon := newClient()

	// empty list first
	if r := get(t, anon, ts.URL+"/beneficiaries", bearer); r.StatusCode != 200 || strings.TrimSpace(body(t, r)) != "[]" {
		t.Errorf("initial beneficiaries should be []; status %d", r.StatusCode)
	}

	// resolve a malformed IBAN (bad checksum/length) -> 422
	if r := get(t, anon, ts.URL+"/beneficiaries/resolve?iban=DE00UNKNOWN0000000", bearer); r.StatusCode != 422 {
		t.Errorf("resolve malformed = %d, want 422", r.StatusCode)
	}
	// resolve a valid-but-unknown IBAN -> 404
	if r := get(t, anon, ts.URL+"/beneficiaries/resolve?iban=GB82WEST12345698765432", bearer); r.StatusCode != 404 {
		t.Errorf("resolve unknown = %d, want 404", r.StatusCode)
	}

	// resolve bob's IBAN -> 200 with masked name, no balance leaked
	resp := get(t, anon, ts.URL+"/beneficiaries/resolve?iban="+bobIban, bearer)
	if resp.StatusCode != 200 {
		t.Fatalf("resolve bob = %d, want 200", resp.StatusCode)
	}
	var ra struct {
		AccountID       string `json:"account_id"`
		Iban            string `json:"iban"`
		OwnerNameMasked string `json:"owner_name_masked"`
	}
	if err := json.Unmarshal([]byte(body(t, resp)), &ra); err != nil {
		t.Fatalf("decode resolve: %v", err)
	}
	if ra.OwnerNameMasked != "B** B******" {
		t.Errorf("masked name = %q, want %q", ra.OwnerNameMasked, "B** B******")
	}
	if ra.AccountID != bobAcct.String() {
		t.Errorf("resolved account = %q, want %q", ra.AccountID, bobAcct.String())
	}

	// add bob as a beneficiary -> 201
	resp = doJSON(t, anon, http.MethodPost, ts.URL+"/beneficiaries", tok,
		`{"label":"Bob","iban":"`+bobIban+`"}`)
	if resp.StatusCode != 201 {
		t.Fatalf("add beneficiary = %d, want 201; body=%.160s", resp.StatusCode, body(t, resp))
	}
	var ben struct {
		ID              string `json:"id"`
		Label           string `json:"label"`
		CreditAccountID string `json:"credit_account_id"`
		OwnerNameMasked string `json:"owner_name_masked"`
	}
	if err := json.Unmarshal([]byte(body(t, resp)), &ben); err != nil || ben.ID == "" {
		t.Fatalf("decode beneficiary: %v", err)
	}
	if ben.CreditAccountID != bobAcct.String() || ben.OwnerNameMasked != "B** B******" {
		t.Errorf("beneficiary fields wrong: %+v", ben)
	}

	// duplicate (same account) -> 409
	if r := doJSON(t, anon, http.MethodPost, ts.URL+"/beneficiaries", tok,
		`{"label":"Bob again","iban":"`+bobIban+`"}`); r.StatusCode != 409 {
		t.Errorf("duplicate beneficiary = %d, want 409", r.StatusCode)
	}

	// adding your own account -> 409 (rejected by the function)
	if r := doJSON(t, anon, http.MethodPost, ts.URL+"/beneficiaries", tok,
		`{"label":"Me","iban":"`+aliceIban+`"}`); r.StatusCode != 409 {
		t.Errorf("self beneficiary = %d, want 409", r.StatusCode)
	}

	// list now shows exactly one
	resp = get(t, anon, ts.URL+"/beneficiaries", bearer)
	var list []map[string]any
	if err := json.Unmarshal([]byte(body(t, resp)), &list); err != nil || len(list) != 1 {
		t.Fatalf("list should have 1 beneficiary, got %d", len(list))
	}

	// transfer to the saved payee's account succeeds
	tr := `{"debit_account":"` + aliceAcct.String() + `","credit_account":"` + ben.CreditAccountID + `","amount_minor":100}`
	req, _ := http.NewRequest(http.MethodPost, ts.URL+"/transfers", strings.NewReader(tr))
	req.Header.Set("Authorization", "Bearer "+tok)
	req.Header.Set("Idempotency-Key", uuid.NewString())
	req.Header.Set("Content-Type", "application/json")
	if resp, err := anon.Do(req); err != nil || resp.StatusCode != 200 {
		t.Fatalf("transfer to beneficiary failed: err=%v status=%v", err, resp.StatusCode)
	}

	// ownership: a different user cannot delete alice's beneficiary -> 404
	_, bobName := mkUser(t, pg, sqlc.UserRoleCustomer)
	bobTok := clientToken(t, ts, bobName, "pw")
	if r := doJSON(t, anon, http.MethodDelete, ts.URL+"/beneficiaries/"+ben.ID, bobTok, ""); r.StatusCode != 404 {
		t.Errorf("cross-user delete = %d, want 404", r.StatusCode)
	}

	// owner deletes -> 204, then 404 on repeat
	if r := doJSON(t, anon, http.MethodDelete, ts.URL+"/beneficiaries/"+ben.ID, tok, ""); r.StatusCode != 204 {
		t.Errorf("owner delete = %d, want 204", r.StatusCode)
	}
	if r := doJSON(t, anon, http.MethodDelete, ts.URL+"/beneficiaries/"+ben.ID, tok, ""); r.StatusCode != 404 {
		t.Errorf("delete again = %d, want 404", r.StatusCode)
	}
}
