package api

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	sqlc "github.com/minhtt159/bank0/internal/db/sqlc"
)

func postPassword(t *testing.T, ts *httptest.Server, token, jsonBody string) int {
	t.Helper()
	req, _ := http.NewRequest(http.MethodPost, ts.URL+"/me/password", strings.NewReader(jsonBody))
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("POST /me/password: %v", err)
	}
	resp.Body.Close()
	return resp.StatusCode
}

func postLogin(t *testing.T, ts *httptest.Server, username, password string) int {
	t.Helper()
	resp, err := http.Post(ts.URL+"/auth/login", "application/json",
		strings.NewReader(`{"username":"`+username+`","password":"`+password+`"}`))
	if err != nil {
		t.Fatalf("login: %v", err)
	}
	resp.Body.Close()
	return resp.StatusCode
}

// POST /me/password verifies the current password, stores the new one, and revokes
// every OTHER refresh family (sparing the session that supplied its refresh token).
// See spec-change-password.md.
func TestHTTPChangePassword(t *testing.T) {
	ts, pg := newTestServer(t)
	_, name := mkUser(t, pg, sqlc.UserRoleCustomer)

	sessA := clientLogin(t, ts, name, "pw") // family A
	sessB := clientLogin(t, ts, name, "pw") // family B

	const newPw = "new-password-123"

	// no bearer -> 401
	if code := postPassword(t, ts, "", `{"current_password":"pw","new_password":"`+newPw+`"}`); code != 401 {
		t.Errorf("no-bearer = %d, want 401", code)
	}
	// weak new password -> 422 (Go pre-check)
	if code := postPassword(t, ts, sessA.Token, `{"current_password":"pw","new_password":"short"}`); code != 422 {
		t.Errorf("weak new = %d, want 422", code)
	}
	// wrong current -> 401 (no revocation: access token used, no refresh rotation)
	if code := postPassword(t, ts, sessA.Token, `{"current_password":"nope","new_password":"`+newPw+`"}`); code != 401 {
		t.Errorf("wrong current = %d, want 401", code)
	}

	// happy path: change pw, spare A's family via its refresh token
	if code := postPassword(t, ts, sessA.Token,
		`{"current_password":"pw","new_password":"`+newPw+`","refresh_token":"`+sessA.RefreshToken+`"}`); code != 204 {
		t.Fatalf("change pw = %d, want 204", code)
	}
	// B (other family) revoked; A (spared) still rotates
	if _, code := doRefresh(t, ts, sessB.RefreshToken); code != 401 {
		t.Errorf("other-session refresh = %d, want 401 (revoked)", code)
	}
	if _, code := doRefresh(t, ts, sessA.RefreshToken); code != 200 {
		t.Errorf("current-session refresh = %d, want 200 (spared)", code)
	}
	// old password no longer logs in; the new one does
	if code := postLogin(t, ts, name, "pw"); code != 401 {
		t.Errorf("old pw login = %d, want 401", code)
	}
	if code := postLogin(t, ts, name, newPw); code != 200 {
		t.Errorf("new pw login = %d, want 200", code)
	}
}

// Omitting refresh_token revokes ALL families, including the caller's own.
func TestHTTPChangePasswordNoRefreshTokenRevokesAll(t *testing.T) {
	ts, pg := newTestServer(t)
	_, name := mkUser(t, pg, sqlc.UserRoleCustomer)
	sess := clientLogin(t, ts, name, "pw")
	if code := postPassword(t, ts, sess.Token, `{"current_password":"pw","new_password":"another-strong-pw-9"}`); code != 204 {
		t.Fatalf("change = %d, want 204", code)
	}
	if _, code := doRefresh(t, ts, sess.RefreshToken); code != 401 {
		t.Errorf("own refresh after no-token change = %d, want 401 (all revoked)", code)
	}
}
