package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	sqlc "github.com/minhtt159/bank0/internal/db/sqlc"
)

type sessionRow struct {
	FamilyID    string `json:"family_id"`
	Current     bool   `json:"current"`
	DeviceLabel string `json:"device_label"`
}

func listSessions(t *testing.T, ts *httptest.Server, token, refresh string) []sessionRow {
	t.Helper()
	resp := get(t, newClient(), ts.URL+"/me/sessions", map[string]string{
		"Authorization": "Bearer " + token, "X-Refresh-Token": refresh,
	})
	if resp.StatusCode != 200 {
		t.Fatalf("list sessions = %d, want 200", resp.StatusCode)
	}
	var rows []sessionRow
	if err := json.Unmarshal([]byte(body(t, resp)), &rows); err != nil {
		t.Fatalf("decode sessions: %v", err)
	}
	return rows
}

func doDelete(t *testing.T, ts *httptest.Server, token, path string) int {
	t.Helper()
	req, _ := http.NewRequest(http.MethodDelete, ts.URL+path, nil)
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("DELETE %s: %v", path, err)
	}
	resp.Body.Close()
	return resp.StatusCode
}

// GET /me/sessions lists the caller's devices (one row per refresh family) with a
// current flag; DELETE /me/sessions/{family} is selective, idempotent sign-out.
// A device_label sent at login is clamped/trimmed and echoed on the session row.
func TestHTTPLoginDeviceLabelShowsInSessions(t *testing.T) {
	ts, pg := newTestServer(t)
	_, name := mkUser(t, pg, sqlc.UserRoleCustomer)

	r := postJSON(t, ts.URL+"/auth/login", nil, map[string]string{
		"username": name, "password": "pw", "device_label": "  Pixel 8  ",
	})
	if r.StatusCode != http.StatusOK {
		t.Fatalf("login = %d, want 200", r.StatusCode)
	}
	var out struct {
		Token        string `json:"token"`
		RefreshToken string `json:"refresh_token"`
	}
	decodeBody(t, r, &out)

	rows := listSessions(t, ts, out.Token, out.RefreshToken)
	if len(rows) != 1 || rows[0].DeviceLabel != "Pixel 8" {
		t.Fatalf("sessions = %+v, want one row labeled %q", rows, "Pixel 8")
	}
}

func TestHTTPListAndRevokeSessions(t *testing.T) {
	ts, pg := newTestServer(t)
	_, name := mkUser(t, pg, sqlc.UserRoleCustomer)
	sessA := clientLogin(t, ts, name, "pw") // device A
	sessB := clientLogin(t, ts, name, "pw") // device B

	if resp := get(t, newClient(), ts.URL+"/me/sessions", nil); resp.StatusCode != 401 {
		t.Errorf("no-bearer list = %d, want 401", resp.StatusCode)
	}

	rows := listSessions(t, ts, sessA.Token, sessA.RefreshToken)
	if len(rows) != 2 {
		t.Fatalf("sessions = %d, want 2", len(rows))
	}
	currents, otherFam := 0, ""
	for _, r := range rows {
		if r.Current {
			currents++
		} else {
			otherFam = r.FamilyID
		}
	}
	if currents != 1 {
		t.Fatalf("current flag set on %d rows, want exactly 1", currents)
	}

	// revoke the OTHER device -> 204; its refresh dies, the current one survives
	if c := doDelete(t, ts, sessA.Token, "/me/sessions/"+otherFam); c != 204 {
		t.Fatalf("revoke other device = %d, want 204", c)
	}
	if _, code := doRefresh(t, ts, sessB.RefreshToken); code != 401 {
		t.Errorf("revoked device refresh = %d, want 401", code)
	}
	if _, code := doRefresh(t, ts, sessA.RefreshToken); code != 200 {
		t.Errorf("surviving device refresh = %d, want 200", code)
	}
	// idempotent: revoking the same owned family again -> 204
	if c := doDelete(t, ts, sessA.Token, "/me/sessions/"+otherFam); c != 204 {
		t.Errorf("idempotent re-revoke = %d, want 204", c)
	}
}

// A caller can neither see nor revoke another user's family (404, no effect); revoking
// one's own current family signs this device out at the next refresh.
func TestHTTPRevokeSessionCrossUserAndCurrent(t *testing.T) {
	ts, pg := newTestServer(t)
	_, aliceName := mkUser(t, pg, sqlc.UserRoleCustomer)
	_, bobName := mkUser(t, pg, sqlc.UserRoleCustomer)
	alice := clientLogin(t, ts, aliceName, "pw")
	bob := clientLogin(t, ts, bobName, "pw")

	bobRows := listSessions(t, ts, bob.Token, bob.RefreshToken)
	if len(bobRows) != 1 {
		t.Fatalf("bob sessions = %d, want 1", len(bobRows))
	}
	bobFam := bobRows[0].FamilyID

	// cross-user revoke -> 404, bob unaffected
	if c := doDelete(t, ts, alice.Token, "/me/sessions/"+bobFam); c != 404 {
		t.Errorf("cross-user revoke = %d, want 404", c)
	}
	if _, code := doRefresh(t, ts, bob.RefreshToken); code != 200 {
		t.Errorf("bob refresh after alice's failed revoke = %d, want 200", code)
	}

	// revoking your own (current) family -> 204, then your refresh 401s
	aliceFam := listSessions(t, ts, alice.Token, alice.RefreshToken)[0].FamilyID
	if c := doDelete(t, ts, alice.Token, "/me/sessions/"+aliceFam); c != 204 {
		t.Fatalf("revoke own current = %d, want 204", c)
	}
	if _, code := doRefresh(t, ts, alice.RefreshToken); code != 401 {
		t.Errorf("refresh after self-revoke = %d, want 401", code)
	}
}
