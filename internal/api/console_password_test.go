package api

import (
	"net/url"
	"strings"
	"testing"

	sqlc "github.com/minhtt159/bank0/internal/db/sqlc"
)

// BOOTSTRAP-ADMIN-ROTATION: the seeded admin/admin operator (00016) had no
// supported way to change its own password — POST /me/password is the client
// surface, the admin JSON surface refuses password writes, and the console had no
// screen. psql was the only path. These pin the operator screen AND the gate that
// makes "change it immediately" binding rather than advisory.

func TestConsoleOperatorCanRotateOwnPassword(t *testing.T) {
	ts, pg := newTestServer(t)
	_, name := mkUser(t, pg, sqlc.UserRoleAdmin)
	c := login(t, ts, name, "pw")

	// The screen exists and is reachable with a portal session alone.
	if r := get(t, c, ts.URL+"/console/password", map[string]string{"Accept": "text/html"}); r.StatusCode != 200 {
		t.Fatalf("GET /console/password = %d, want 200", r.StatusCode)
	}

	// Policy is the DB's: too short is refused and the password does not change.
	r, err := c.PostForm(ts.URL+"/console/password", url.Values{
		"current_password": {"pw"}, "new_password": {"short"}, "confirm_password": {"short"},
	})
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	if b := body(t, r); !strings.Contains(b, "12") {
		t.Errorf("short password should surface the 12-char policy; got %q", firstLine(b))
	}

	// Mismatched confirmation is caught before the DB is touched.
	r, _ = c.PostForm(ts.URL+"/console/password", url.Values{
		"current_password": {"pw"}, "new_password": {"a-long-enough-one"}, "confirm_password": {"different-one-here"},
	})
	if b := body(t, r); !strings.Contains(b, "do not match") {
		t.Errorf("confirmation mismatch should be reported; got %q", firstLine(b))
	}

	// The real rotation succeeds, and the OLD password stops working.
	r, _ = c.PostForm(ts.URL+"/console/password", url.Values{
		"current_password": {"pw"}, "new_password": {"a-long-enough-one"}, "confirm_password": {"a-long-enough-one"},
	})
	if b := body(t, r); !strings.Contains(b, "Password changed") {
		t.Fatalf("rotation should succeed; got %q", firstLine(b))
	}
	old := newClient()
	resp, _ := old.PostForm(ts.URL+"/login", url.Values{"username": {name}, "password": {"pw"}})
	if resp.StatusCode == 303 {
		t.Error("the old password must stop working after rotation")
	}
	resp.Body.Close()
	fresh := login(t, ts, name, "a-long-enough-one") // the new one works
	if r := get(t, fresh, ts.URL+"/console/dashboard", map[string]string{"Accept": "text/html"}); r.StatusCode != 200 {
		t.Errorf("new password should log in; dashboard = %d", r.StatusCode)
	}
}

// The gate: an operator flagged must_change_password reaches the password screen
// and nothing else, and clearing it restores the console.
func TestConsoleBlocksUntilSeededPasswordRotated(t *testing.T) {
	ts, pg := newTestServer(t)
	id, name := mkUser(t, pg, sqlc.UserRoleAdmin)
	if _, err := pg.Pool.Exec(t.Context(),
		`UPDATE users SET must_change_password = TRUE WHERE id = $1`, id); err != nil {
		t.Fatalf("flag user: %v", err)
	}
	c := login(t, ts, name, "pw")

	// Every other console screen bounces to the password screen.
	for _, path := range []string{"/console/dashboard", "/console/users", "/"} {
		r := get(t, c, ts.URL+path, map[string]string{"Accept": "text/html"})
		if r.StatusCode != 303 || r.Header.Get("Location") != "/console/password" {
			t.Errorf("%s = %d -> %q, want 303 -> /console/password", path, r.StatusCode, r.Header.Get("Location"))
		}
		r.Body.Close()
	}

	// The admin JSON surface is barred too — a flagged account must not drive the
	// API around the screen it is being held on.
	if r := get(t, c, ts.URL+"/admin/reconcile", nil); r.StatusCode != 403 {
		t.Errorf("admin JSON while flagged = %d, want 403", r.StatusCode)
	}

	// HTMX callers get the header redirect, not a body they would swap in.
	r := get(t, c, ts.URL+"/console/users/results", map[string]string{"HX-Request": "true"})
	if r.Header.Get("HX-Redirect") != "/console/password" {
		t.Errorf("HX-Redirect = %q, want /console/password", r.Header.Get("HX-Redirect"))
	}
	r.Body.Close()

	// ...but the password screen itself is reachable, or the operator is bricked.
	if r := get(t, c, ts.URL+"/console/password", map[string]string{"Accept": "text/html"}); r.StatusCode != 200 {
		t.Fatalf("password screen while flagged = %d, want 200", r.StatusCode)
	}

	// Rotating clears the flag (change_password does it, not the handler) and the
	// console opens up again.
	resp, _ := c.PostForm(ts.URL+"/console/password", url.Values{
		"current_password": {"pw"}, "new_password": {"a-long-enough-one"}, "confirm_password": {"a-long-enough-one"},
	})
	resp.Body.Close()
	var still bool
	if err := pg.Pool.QueryRow(t.Context(),
		`SELECT must_change_password FROM users WHERE id = $1`, id).Scan(&still); err != nil {
		t.Fatalf("read flag: %v", err)
	}
	if still {
		t.Error("change_password must clear must_change_password")
	}
	if r := get(t, c, ts.URL+"/console/dashboard", map[string]string{"Accept": "text/html"}); r.StatusCode != 200 {
		t.Errorf("dashboard after rotation = %d, want 200", r.StatusCode)
	}
}

func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 && i < 200 {
		return s[:i]
	}
	if len(s) > 200 {
		return s[:200]
	}
	return s
}
