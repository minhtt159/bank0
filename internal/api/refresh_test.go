package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	sqlc "github.com/minhtt159/bank0/internal/db/sqlc"
)

type loginResult struct {
	UserID       string `json:"user_id"`
	Token        string `json:"token"`
	RefreshToken string `json:"refresh_token"`
}

func clientLogin(t *testing.T, ts *httptest.Server, username, password string) loginResult {
	t.Helper()
	resp, err := http.Post(ts.URL+"/auth/login", "application/json",
		strings.NewReader(`{"username":"`+username+`","password":"`+password+`"}`))
	if err != nil {
		t.Fatalf("login: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Fatalf("login status = %d", resp.StatusCode)
	}
	var lr loginResult
	if err := json.NewDecoder(resp.Body).Decode(&lr); err != nil {
		t.Fatalf("decode login: %v", err)
	}
	return lr
}

// post /auth/refresh, returning the rotated pair and the HTTP status.
func doRefresh(t *testing.T, ts *httptest.Server, refresh string) (loginResult, int) {
	t.Helper()
	resp, err := http.Post(ts.URL+"/auth/refresh", "application/json",
		strings.NewReader(`{"refresh_token":"`+refresh+`"}`))
	if err != nil {
		t.Fatalf("refresh: %v", err)
	}
	defer resp.Body.Close()
	var lr loginResult
	if resp.StatusCode == 200 {
		_ = json.NewDecoder(resp.Body).Decode(&lr)
	}
	return lr, resp.StatusCode
}

func TestHTTPRefreshRotationAndReuse(t *testing.T) {
	ts, pg := newTestServer(t)
	_, name := mkUser(t, pg, sqlc.UserRoleCustomer)

	lr := clientLogin(t, ts, name, "pw")
	if lr.RefreshToken == "" || lr.Token == "" {
		t.Fatal("login must return both an access token and a refresh token")
	}

	// rotate -> a fresh, different pair
	r1, code := doRefresh(t, ts, lr.RefreshToken)
	if code != 200 {
		t.Fatalf("refresh = %d, want 200", code)
	}
	if r1.RefreshToken == "" || r1.RefreshToken == lr.RefreshToken || r1.Token == "" {
		t.Fatalf("rotation must mint a new refresh token; got %+v", r1)
	}

	// the new access token works on a protected route
	if r := get(t, newClient(), ts.URL+"/me", map[string]string{"Authorization": "Bearer " + r1.Token}); r.StatusCode != 200 {
		t.Errorf("rotated access token /me = %d, want 200", r.StatusCode)
	}

	// reuse the OLD (already-rotated) refresh token -> 401 (theft signal)
	if _, code := doRefresh(t, ts, lr.RefreshToken); code != 401 {
		t.Errorf("reuse of rotated token = %d, want 401", code)
	}

	// ...and reuse detection revoked the whole family, so the child token is dead too
	if _, code := doRefresh(t, ts, r1.RefreshToken); code != 401 {
		t.Errorf("post-reuse child token = %d, want 401 (family revoked)", code)
	}
}

func TestHTTPLogoutAndLogoutAll(t *testing.T) {
	ts, pg := newTestServer(t)
	_, name := mkUser(t, pg, sqlc.UserRoleCustomer)

	// single-session logout revokes that refresh token
	lr := clientLogin(t, ts, name, "pw")
	resp, _ := http.Post(ts.URL+"/auth/logout", "application/json",
		strings.NewReader(`{"refresh_token":"`+lr.RefreshToken+`"}`))
	if resp.StatusCode != 204 {
		t.Fatalf("logout = %d, want 204", resp.StatusCode)
	}
	resp.Body.Close()
	if _, code := doRefresh(t, ts, lr.RefreshToken); code != 401 {
		t.Errorf("refresh after logout = %d, want 401", code)
	}

	// logout-all revokes every family for the user
	a := clientLogin(t, ts, name, "pw")
	b := clientLogin(t, ts, name, "pw")
	req, _ := http.NewRequest(http.MethodPost, ts.URL+"/auth/logout-all", nil)
	req.Header.Set("Authorization", "Bearer "+a.Token)
	r, err := newClient().Do(req)
	if err != nil || r.StatusCode != 204 {
		t.Fatalf("logout-all err=%v status=%v, want 204", err, r.StatusCode)
	}
	if _, code := doRefresh(t, ts, a.RefreshToken); code != 401 {
		t.Errorf("refresh session A after logout-all = %d, want 401", code)
	}
	if _, code := doRefresh(t, ts, b.RefreshToken); code != 401 {
		t.Errorf("refresh session B after logout-all = %d, want 401", code)
	}

	// logout-all without a bearer -> 401
	resp2, _ := http.Post(ts.URL+"/auth/logout-all", "application/json", nil)
	if resp2.StatusCode != 401 {
		t.Errorf("logout-all without token = %d, want 401", resp2.StatusCode)
	}
}

// An admin operator force-revoking a customer's app sessions from the console
// kills that customer's refresh token (docs/06 operator force-revoke).
func TestHTTPConsoleRevokeSessions(t *testing.T) {
	ts, pg := newTestServer(t)
	_, adminName := mkUser(t, pg, sqlc.UserRoleAdmin)
	custID, custName := mkUser(t, pg, sqlc.UserRoleCustomer)

	lr := clientLogin(t, ts, custName, "pw")
	if _, code := doRefresh(t, ts, lr.RefreshToken); code != 200 {
		t.Fatalf("precondition refresh = %d, want 200", code)
	}

	// a non-admin (auditor) cannot revoke
	_, audName := mkUser(t, pg, sqlc.UserRoleAuditor)
	aud := login(t, ts, audName, "pw")
	if r, _ := aud.PostForm(ts.URL+"/console/users/"+custID.String()+"/revoke-sessions", url.Values{}); r.StatusCode != 403 {
		t.Errorf("auditor revoke = %d, want 403", r.StatusCode)
	}

	// admin revokes from the user-detail rail
	admin := login(t, ts, adminName, "pw")
	r, err := admin.PostForm(ts.URL+"/console/users/"+custID.String()+"/revoke-sessions", url.Values{})
	if err != nil || r.StatusCode != 200 {
		t.Fatalf("admin revoke err=%v status=%v, want 200", err, r.StatusCode)
	}

	// the customer's latest refresh token is now dead -> must re-login
	if _, code := doRefresh(t, ts, lr.RefreshToken); code != 401 {
		t.Errorf("refresh after console revoke = %d, want 401", code)
	}
}
