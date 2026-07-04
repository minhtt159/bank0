package api

import (
	"net/http"
	"strings"
	"testing"

	"github.com/google/uuid"
	sqlc "github.com/minhtt159/bank0/internal/db/sqlc"
)

// HTTP flows for customer self-service accounts + the limit-request queue.

// bearerFor logs a user in on the client surface and returns the access token.
func bearerFor(t *testing.T, tsURL, username, password string) string {
	t.Helper()
	r := postJSON(t, tsURL+"/auth/login", nil, map[string]string{"username": username, "password": password})
	if r.StatusCode != http.StatusOK {
		t.Fatalf("client login = %d, want 200", r.StatusCode)
	}
	var out struct {
		Token string `json:"token"`
	}
	decodeBody(t, r, &out)
	return out.Token
}

func TestHTTPOpenMyAccount(t *testing.T) {
	ts, pg := newTestServer(t)
	uid, uname := mkUser(t, pg, sqlc.UserRoleCustomer)
	_ = uid
	tok := bearerFor(t, ts.URL, uname, "pw")
	auth := map[string]string{"Authorization": "Bearer " + tok}

	// Missing Idempotency-Key -> 400 (generated wrapper).
	r := postJSON(t, ts.URL+"/me/accounts", auth, nil)
	if r.StatusCode != http.StatusBadRequest {
		t.Errorf("no key = %d, want 400", r.StatusCode)
	}

	// Open: 201 with a server-minted SE IBAN, owned by the caller.
	key := uuid.NewString()
	hdr := map[string]string{"Authorization": "Bearer " + tok, "Idempotency-Key": key}
	r = postJSON(t, ts.URL+"/me/accounts", hdr, nil)
	if r.StatusCode != http.StatusCreated {
		t.Fatalf("open = %d, want 201: %s", r.StatusCode, body(t, r))
	}
	var acct struct {
		ID     string `json:"id"`
		UserID string `json:"user_id"`
		Iban   string `json:"iban"`
	}
	decodeBody(t, r, &acct)
	if !strings.HasPrefix(acct.Iban, "SE") || len(acct.Iban) != 24 {
		t.Errorf("iban = %q, want SE ISO IBAN", acct.Iban)
	}
	if acct.UserID != uid.String() {
		t.Errorf("owner = %s, want caller %s", acct.UserID, uid)
	}

	// Replay: same key -> 201, same account, replay header.
	r = postJSON(t, ts.URL+"/me/accounts", hdr, nil)
	if r.StatusCode != http.StatusCreated || r.Header.Get("Idempotency-Replayed") != "true" {
		t.Fatalf("replay = %d (replayed=%q), want 201/true", r.StatusCode, r.Header.Get("Idempotency-Replayed"))
	}
	var acct2 struct {
		ID string `json:"id"`
	}
	decodeBody(t, r, &acct2)
	if acct2.ID != acct.ID {
		t.Errorf("replay opened a different account: %s vs %s", acct2.ID, acct.ID)
	}

	// Unsupported kind -> 422.
	hdr["Idempotency-Key"] = uuid.NewString()
	r = postJSON(t, ts.URL+"/me/accounts", hdr, map[string]string{"kind": "savings"})
	if r.StatusCode != http.StatusUnprocessableEntity {
		t.Errorf("savings = %d, want 422", r.StatusCode)
	}

	// Cap -> 409 account_limit (fill to the configured cap first).
	for i := 0; i < 10; i++ {
		hdr["Idempotency-Key"] = uuid.NewString()
		r = postJSON(t, ts.URL+"/me/accounts", hdr, nil)
		if r.StatusCode == http.StatusConflict {
			break
		}
		if r.StatusCode != http.StatusCreated {
			t.Fatalf("open #%d = %d, want 201 or 409", i+2, r.StatusCode)
		}
	}
	if r.StatusCode != http.StatusConflict {
		t.Errorf("cap never hit; last = %d, want 409", r.StatusCode)
	}
	if b := body(t, r); !strings.Contains(b, "account_limit") {
		t.Errorf("cap body = %s, want account_limit", b)
	}
}

func TestHTTPLimitRequestFlow(t *testing.T) {
	ts, pg := newTestServer(t)
	uid, uname := mkUser(t, pg, sqlc.UserRoleCustomer)
	acct := mkAcct(t, pg, uid, 0)
	otherID, _ := mkUser(t, pg, sqlc.UserRoleCustomer)
	otherAcct := mkAcct(t, pg, otherID, 0)
	_, adminName := mkUser(t, pg, sqlc.UserRoleAdmin)

	tok := bearerFor(t, ts.URL, uname, "pw")
	auth := map[string]string{"Authorization": "Bearer " + tok}

	// A limit request on someone else's account -> 403.
	r := postJSON(t, ts.URL+"/accounts/"+otherAcct.String()+"/limit-requests", auth,
		map[string]any{"transfer_limit_minor": 12_345_600, "reason": "nope"})
	if r.StatusCode != http.StatusForbidden {
		t.Errorf("foreign limit request = %d, want 403", r.StatusCode)
	}

	// Own account -> 201 pending.
	r = postJSON(t, ts.URL+"/accounts/"+acct.String()+"/limit-requests", auth,
		map[string]any{"transfer_limit_minor": 12_345_600, "reason": "travel"})
	if r.StatusCode != http.StatusCreated {
		t.Fatalf("limit request = %d, want 201: %s", r.StatusCode, body(t, r))
	}
	var lr struct {
		RequestID string `json:"request_id"`
		Status    string `json:"status"`
	}
	decodeBody(t, r, &lr)
	if lr.Status != "pending" || lr.RequestID == "" {
		t.Fatalf("limit request body = %+v", lr)
	}

	// Admin sees it in the queue and applies it.
	admin := login(t, ts, adminName, "pw")
	q := get(t, admin, ts.URL+"/admin/limit-requests", nil)
	if q.StatusCode != http.StatusOK {
		t.Fatalf("queue = %d, want 200", q.StatusCode)
	}
	if b := body(t, q); !strings.Contains(b, lr.RequestID) {
		t.Fatalf("queue does not list the request: %s", b)
	}
	ar, err := admin.Post(ts.URL+"/admin/limit-requests/"+lr.RequestID+"/approve", "application/json", nil)
	if err != nil || ar.StatusCode != http.StatusOK {
		t.Fatalf("approve = %v/%d, want 200", err, ar.StatusCode)
	}

	// The new limit is live.
	a, err := pg.Queries.GetAccount(t.Context(), acct)
	if err != nil {
		t.Fatalf("get account: %v", err)
	}
	if a.TransferLimitMinor != 12_345_600 {
		t.Errorf("limit = %d, want 12345600", a.TransferLimitMinor)
	}

	// Double-approve -> 409.
	ar2, err := admin.Post(ts.URL+"/admin/limit-requests/"+lr.RequestID+"/approve", "application/json", nil)
	if err != nil || ar2.StatusCode != http.StatusConflict {
		t.Errorf("double approve = %v/%d, want 409", err, ar2.StatusCode)
	}

	// Unauthenticated queue access -> 401.
	anon := newClient()
	if resp := get(t, anon, ts.URL+"/admin/limit-requests", nil); resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("anon queue = %d, want 401", resp.StatusCode)
	}
}

func TestHTTPTransferEchoesRailIDs(t *testing.T) {
	ts, pg := newTestServer(t)
	uid, uname := mkUser(t, pg, sqlc.UserRoleCustomer)
	from := mkAcct(t, pg, uid, 50_000)
	toID, _ := mkUser(t, pg, sqlc.UserRoleCustomer)
	to := mkAcct(t, pg, toID, 0)

	tok := bearerFor(t, ts.URL, uname, "pw")
	hdr := map[string]string{"Authorization": "Bearer " + tok, "Idempotency-Key": uuid.NewString()}

	r := postJSON(t, ts.URL+"/transfers", hdr, map[string]any{
		"debit_account": from.String(), "credit_account": to.String(),
		"amount_minor": 1_000, "description": "rail", "end_to_end_id": "E2E-77",
	})
	if r.StatusCode != http.StatusOK {
		t.Fatalf("transfer = %d: %s", r.StatusCode, body(t, r))
	}
	var res struct {
		TransferID string `json:"transfer_id"`
	}
	decodeBody(t, r, &res)

	// GET /transfers/{id} exposes uetr + the echoed end_to_end_id.
	g := get(t, http.DefaultClient, ts.URL+"/transfers/"+res.TransferID,
		map[string]string{"Authorization": "Bearer " + tok})
	if g.StatusCode != http.StatusOK {
		t.Fatalf("get transfer = %d", g.StatusCode)
	}
	b := body(t, g)
	if !strings.Contains(b, `"end_to_end_id":"E2E-77"`) || !strings.Contains(b, `"uetr"`) {
		t.Errorf("transfer body missing rail ids: %s", b)
	}

	// Replay sets the Idempotency-Replayed header.
	r2 := postJSON(t, ts.URL+"/transfers", hdr, map[string]any{
		"debit_account": from.String(), "credit_account": to.String(),
		"amount_minor": 1_000, "description": "rail", "end_to_end_id": "E2E-77",
	})
	if r2.StatusCode != http.StatusOK || r2.Header.Get("Idempotency-Replayed") != "true" {
		t.Errorf("replay = %d (replayed=%q), want 200/true", r2.StatusCode, r2.Header.Get("Idempotency-Replayed"))
	}
}
