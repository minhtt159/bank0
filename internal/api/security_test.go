package api

import (
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/google/uuid"

	sqlc "github.com/minhtt159/bank0/internal/db/sqlc"
)

func sessPostH(t *testing.T, c *http.Client, url, jsonBody string, headers map[string]string) int {
	t.Helper()
	req, _ := http.NewRequest(http.MethodPost, url, strings.NewReader(jsonBody))
	req.Header.Set("Content-Type", "application/json")
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := c.Do(req)
	if err != nil {
		t.Fatalf("POST %s: %v", url, err)
	}
	resp.Body.Close()
	return resp.StatusCode
}

// Broken-access-control regression: the JSON admin API must enforce RBAC, not just a
// valid session. An auditor (read-only) must be refused every money/user/dispute
// mutation (403) while still able to READ the triage queue; a privileged role works.
func TestSecurityAdminMutationsRequireRole(t *testing.T) {
	ts, pg := newTestServer(t)
	resetDisputes(t, pg)
	aliceID, aliceName := mkUser(t, pg, sqlc.UserRoleCustomer)
	aliceAcct := mkAcct(t, pg, aliceID, 100_000)
	bobID, _ := mkUser(t, pg, sqlc.UserRoleCustomer)
	bobAcct := mkAcct(t, pg, bobID, 0)
	_, auditorName := mkUser(t, pg, sqlc.UserRoleAuditor)
	_, adminName := mkUser(t, pg, sqlc.UserRoleAdmin)
	aliceTok := clientToken(t, ts, aliceName, "pw")

	tid := postTransfer(t, ts, aliceTok, aliceAcct, bobAcct, 1000)
	_, b := doDispute(t, ts, aliceTok, tid, `{"category":"fraud"}`)
	did := disputeID(t, b)

	auditor := login(t, ts, auditorName, "pw")
	admin := login(t, ts, adminName, "pw")
	idem := func() map[string]string { return map[string]string{"Idempotency-Key": uuid.NewString()} }
	depositURL := ts.URL + "/accounts/" + aliceAcct.String() + "/deposit"

	// auditor (read-only) is refused every mutation -> 403
	if sc := sessPostH(t, auditor, depositURL, `{"amount_minor":100}`, idem()); sc != 403 {
		t.Errorf("auditor deposit = %d, want 403", sc)
	}
	if sc := sessPost(t, auditor, ts.URL+"/users", `{"username":"x`+uhex(6)+`","password":"pw","full_name":"X"}`); sc != 403 {
		t.Errorf("auditor createUser = %d, want 403", sc)
	}
	if sc := sessPost(t, auditor, ts.URL+"/admin/disputes/"+did+"/resolve", `{"status":"resolved"}`); sc != 403 {
		t.Errorf("auditor resolveDispute = %d, want 403", sc)
	}
	// ...but an auditor CAN still read the queue (reads are not over-gated)
	if r := get(t, auditor, ts.URL+"/admin/disputes", nil); r.StatusCode != 200 {
		t.Errorf("auditor read queue = %d, want 200", r.StatusCode)
	}
	// a privileged role (admin) is allowed
	if sc := sessPostH(t, admin, depositURL, `{"amount_minor":100}`, idem()); sc != 200 {
		t.Errorf("admin deposit = %d, want 200", sc)
	}
}

// JWT must be unforgeable: a tampered signature, a token signed with the wrong
// secret, and an alg=none token (algorithm-confusion) all -> 401.
func TestSecurityJWTForgery(t *testing.T) {
	ts, pg := newTestServer(t)
	_, name := mkUser(t, pg, sqlc.UserRoleCustomer)
	good := clientToken(t, ts, name, "pw")

	hit := func(tok string) int {
		return get(t, newClient(), ts.URL+"/me", map[string]string{"Authorization": "Bearer " + tok}).StatusCode
	}
	if c := hit(good); c != 200 {
		t.Fatalf("good token /me = %d, want 200", c)
	}

	claims := clientClaims{
		RegisteredClaims: jwt.RegisteredClaims{
			Subject:   uuid.NewString(),
			Issuer:    "bank0",
			Audience:  jwt.ClaimStrings{"bank0-client"},
			ExpiresAt: jwt.NewNumericDate(time.Now().Add(time.Hour)),
		},
		Role:     "admin", // attacker tries to mint an admin token
		Username: "attacker",
	}
	wrongSecret, _ := jwt.NewWithClaims(jwt.SigningMethodHS256, claims).SignedString([]byte("not-the-server-secret"))
	algNone, _ := jwt.NewWithClaims(jwt.SigningMethodNone, claims).SignedString(jwt.UnsafeAllowNoneSignatureType)

	for name, tok := range map[string]string{
		"tampered":     good[:len(good)-3] + "AAA",
		"wrong_secret": wrongSecret,
		"alg_none":     algNone,
		"garbage":      "not.a.jwt",
	} {
		if c := hit(tok); c != 401 {
			t.Errorf("%s token = %d, want 401", name, c)
		}
	}
}

// A client bearer (no portal session cookie) cannot reach the JSON admin surface:
// those routes live behind requireSession, so a JWT-only request is 401.
func TestSecurityClientCannotReachAdminJSON(t *testing.T) {
	ts, pg := newTestServer(t)
	aliceID, aliceName := mkUser(t, pg, sqlc.UserRoleCustomer)
	aliceAcct := mkAcct(t, pg, aliceID, 1000)
	tok := clientToken(t, ts, aliceName, "pw")
	bearer := map[string]string{"Authorization": "Bearer " + tok, "Idempotency-Key": uuid.NewString()}

	// deposit (admin-only) with a client bearer -> 401 (no session)
	req, _ := http.NewRequest(http.MethodPost, ts.URL+"/accounts/"+aliceAcct.String()+"/deposit", strings.NewReader(`{"amount_minor":100}`))
	for k, v := range bearer {
		req.Header.Set(k, v)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("deposit: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != 401 {
		t.Errorf("client bearer deposit = %d, want 401", resp.StatusCode)
	}
	// admin triage queue with a client bearer -> 401
	if r := get(t, newClient(), ts.URL+"/admin/disputes", map[string]string{"Authorization": "Bearer " + tok}); r.StatusCode != 401 {
		t.Errorf("client bearer admin queue = %d, want 401", r.StatusCode)
	}
}

// The CSRF same-origin guard is actually wired on the portal (cookie) surface: a
// cross-origin POST with a valid session is rejected; the same call with no Origin
// (non-browser) passes through.
func TestSecurityCSRFOnPortal(t *testing.T) {
	ts, pg := newTestServer(t)
	_, adminName := mkUser(t, pg, sqlc.UserRoleAdmin)
	admin := login(t, ts, adminName, "pw")

	post := func(origin string) int {
		req, _ := http.NewRequest(http.MethodPost, ts.URL+"/admin/expire-holds", nil)
		if origin != "" {
			req.Header.Set("Origin", origin)
		}
		resp, err := admin.Do(req)
		if err != nil {
			t.Fatalf("post: %v", err)
		}
		resp.Body.Close()
		return resp.StatusCode
	}
	if c := post("https://evil.example"); c != 403 {
		t.Errorf("cross-origin admin POST = %d, want 403 (CSRF)", c)
	}
	if c := post(""); c != 200 {
		t.Errorf("no-origin admin POST = %d, want 200", c)
	}
}
