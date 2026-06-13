package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	sqlc "github.com/minhtt159/bank0/internal/db/sqlc"
)

func patchMe(t *testing.T, ts *httptest.Server, token, jsonBody string) (*http.Response, string) {
	t.Helper()
	req, _ := http.NewRequest(http.MethodPatch, ts.URL+"/me", strings.NewReader(jsonBody))
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("PATCH /me: %v", err)
	}
	return resp, body(t, resp)
}

// PATCH /me edits only name/email/phone, scoped to the JWT subject, and can never
// touch password/status/role (escalation guard). See spec-self-service-profile.md.
func TestHTTPUpdateMe(t *testing.T) {
	ts, pg := newTestServer(t)
	aliceID, aliceName := mkUser(t, pg, sqlc.UserRoleCustomer)
	tok := clientToken(t, ts, aliceName, "pw")

	// no bearer -> 401
	if r, _ := patchMe(t, ts, "", `{"email":"x@y.com"}`); r.StatusCode != 401 {
		t.Errorf("no-bearer PATCH /me = %d, want 401", r.StatusCode)
	}

	// happy path: set email + name
	newEmail := strings.ToLower(uhex(8)) + "@example.com"
	r, b := patchMe(t, ts, tok, `{"email":"`+newEmail+`","full_name":"Alice A"}`)
	if r.StatusCode != 200 {
		t.Fatalf("PATCH /me = %d, want 200; body=%s", r.StatusCode, b)
	}
	var u struct {
		Email    string `json:"email"`
		FullName string `json:"full_name"`
		Role     string `json:"role"`
		Status   string `json:"status"`
	}
	if err := json.Unmarshal([]byte(b), &u); err != nil {
		t.Fatalf("decode user: %v", err)
	}
	if !strings.EqualFold(u.Email, newEmail) || u.FullName != "Alice A" {
		t.Errorf("update not reflected: %+v", u)
	}
	// GET /me reflects it
	if rb := body(t, get(t, newClient(), ts.URL+"/me", map[string]string{"Authorization": "Bearer " + tok})); !strings.Contains(strings.ToLower(rb), newEmail) {
		t.Errorf("GET /me does not reflect new email; body=%s", rb)
	}
	// password untouched: original "pw" still logs in (fatals otherwise)
	_ = clientToken(t, ts, aliceName, "pw")

	// empty body -> 200 no-op
	if r, _ := patchMe(t, ts, tok, `{}`); r.StatusCode != 200 {
		t.Errorf("empty PATCH /me = %d, want 200", r.StatusCode)
	}
	// invalid email shape -> 422
	if r, _ := patchMe(t, ts, tok, `{"email":"not-an-email"}`); r.StatusCode != 422 {
		t.Errorf("invalid email = %d, want 422", r.StatusCode)
	}

	// escalation attempt: role/status/password are ignored (not in UpdateMeRequest)
	if r, _ := patchMe(t, ts, tok, `{"role":"admin","status":"frozen","password":"hijacked-pw-123"}`); r.StatusCode != 200 {
		t.Fatalf("escalation PATCH should 200 with extra fields ignored, got %d", r.StatusCode)
	}
	got, err := pg.Queries.GetUserByID(context.Background(), aliceID)
	if err != nil {
		t.Fatalf("get user: %v", err)
	}
	if string(got.Role) != "customer" || string(got.Status) != "active" {
		t.Errorf("escalation leaked: role=%s status=%s", got.Role, got.Status)
	}
	_ = clientToken(t, ts, aliceName, "pw") // password still "pw"

	// duplicate email -> 409
	_, bobName := mkUser(t, pg, sqlc.UserRoleCustomer)
	bobTok := clientToken(t, ts, bobName, "pw")
	dupEmail := strings.ToLower(uhex(8)) + "@dup.com"
	if r, _ := patchMe(t, ts, bobTok, `{"email":"`+dupEmail+`"}`); r.StatusCode != 200 {
		t.Fatal("bob set email should 200")
	}
	if r, _ := patchMe(t, ts, tok, `{"email":"`+dupEmail+`"}`); r.StatusCode != 409 {
		t.Errorf("duplicate email = %d, want 409", r.StatusCode)
	}
}
