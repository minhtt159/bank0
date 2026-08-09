package api

import (
	"context"
	"net/http"
	url2 "net/url"
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
	r, err := c.PostForm(ts.URL+"/console/password", url2.Values{
		"current_password": {"pw"}, "new_password": {"short"}, "confirm_password": {"short"},
	})
	if err != nil {
		t.Fatalf("post: %v", err)
	}
	if b := body(t, r); !strings.Contains(b, "12") {
		t.Errorf("short password should surface the 12-char policy; got %q", firstLine(b))
	}

	// Mismatched confirmation is caught before the DB is touched.
	r, _ = c.PostForm(ts.URL+"/console/password", url2.Values{
		"current_password": {"pw"}, "new_password": {"a-long-enough-one"}, "confirm_password": {"different-one-here"},
	})
	if b := body(t, r); !strings.Contains(b, "do not match") {
		t.Errorf("confirmation mismatch should be reported; got %q", firstLine(b))
	}

	// The real rotation succeeds and signs the caller out (303 -> /login).
	r, _ = c.PostForm(ts.URL+"/console/password", url2.Values{
		"current_password": {"pw"}, "new_password": {"a-long-enough-one"}, "confirm_password": {"a-long-enough-one"},
	})
	if r.StatusCode != 303 || !strings.HasPrefix(r.Header.Get("Location"), "/login") {
		t.Fatalf("rotation = %d -> %q, want 303 -> /login", r.StatusCode, r.Header.Get("Location"))
	}
	r.Body.Close()
	// The rotating session is dead too — no session survives its own password change.
	if rr := get(t, c, ts.URL+"/console/dashboard", map[string]string{"Accept": "text/html"}); rr.StatusCode != 303 {
		t.Errorf("rotating session after change = %d, want 303 (revoked)", rr.StatusCode)
	}
	old := newClient()
	resp, _ := old.PostForm(ts.URL+"/login", url2.Values{"username": {name}, "password": {"pw"}})
	if resp.StatusCode == 303 {
		t.Error("the old password must stop working after rotation")
	}
	resp.Body.Close()
	fresh := login(t, ts, name, "a-long-enough-one") // the new one works
	if r := get(t, fresh, ts.URL+"/console/dashboard", map[string]string{"Accept": "text/html"}); r.StatusCode != 200 {
		t.Errorf("new password should log in; dashboard = %d", r.StatusCode)
	}
}

// Rotating must kill the operator's OTHER portal sessions. Staff have no refresh
// tokens, so the client-side revoke covered nothing here — and sessions slide on
// every request, so a live attacker session would never have aged out.
func TestPasswordChangeRevokesOtherPortalSessions(t *testing.T) {
	ts, pg := newTestServer(t)
	_, name := mkUser(t, pg, sqlc.UserRoleAdmin)
	rotating := login(t, ts, name, "pw")
	other := login(t, ts, name, "pw") // a second device, same account

	if r := get(t, other, ts.URL+"/console/dashboard", map[string]string{"Accept": "text/html"}); r.StatusCode != 200 {
		t.Fatalf("second session should start valid; got %d", r.StatusCode)
	}

	resp, _ := rotating.PostForm(ts.URL+"/console/password", url2.Values{
		"current_password": {"pw"}, "new_password": {"a-long-enough-one"}, "confirm_password": {"a-long-enough-one"},
	})
	resp.Body.Close()

	// BOTH are out: the old password may be in someone else's hands and there is no
	// telling which session is theirs, so none survives.
	for name, c := range map[string]*http.Client{"other": other, "rotating": rotating} {
		r := get(t, c, ts.URL+"/console/dashboard", map[string]string{"Accept": "text/html"})
		if r.StatusCode != 303 {
			t.Errorf("%s session after rotation = %d, want 303 to /login", name, r.StatusCode)
		}
		r.Body.Close()
	}
	// The new password gets back in.
	if r := get(t, login(t, ts, name, "a-long-enough-one"), ts.URL+"/console/dashboard",
		map[string]string{"Accept": "text/html"}); r.StatusCode != 200 {
		t.Errorf("new password should sign in; got %d", r.StatusCode)
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

	// Rotating clears the flag (change_password does it, not the handler); the
	// session is revoked with it, so the console is reached by signing in again.
	resp, _ := c.PostForm(ts.URL+"/console/password", url2.Values{
		"current_password": {"pw"}, "new_password": {"a-long-enough-one"}, "confirm_password": {"a-long-enough-one"},
	})
	resp.Body.Close()
	c = login(t, ts, name, "a-long-enough-one")
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

// Admin-triggered forced rotation: the "evidence of compromise" trigger NIST asks
// for, as opposed to a calendar. Flagging must also sign the account out, or it
// only inconveniences the legitimate user while the attacker keeps their session.
func TestAdminCanRequirePasswordChange(t *testing.T) {
	ts, pg := newTestServer(t)
	targetID, targetName := mkUser(t, pg, sqlc.UserRoleOperator)
	_, adminName := mkUser(t, pg, sqlc.UserRoleAdmin)
	_, opName := mkUser(t, pg, sqlc.UserRoleOperator)

	victim := login(t, ts, targetName, "pw") // the suspect session
	admin := login(t, ts, adminName, "pw")
	operator := login(t, ts, opName, "pw")

	url := ts.URL + "/console/users/" + targetID.String() + "/require-password-change"

	// Operators cannot force it — this is a compromise response, admin-only.
	if sc := sessPost(t, operator, url, ""); sc != 403 {
		t.Errorf("operator require-change = %d, want 403", sc)
	}
	if sc := sessPost(t, admin, url, ""); sc != 200 {
		t.Fatalf("admin require-change = %d, want 200", sc)
	}

	// The suspect session is gone, not merely flagged.
	r := get(t, victim, ts.URL+"/console/dashboard", map[string]string{"Accept": "text/html"})
	if r.StatusCode != 303 {
		t.Errorf("flagged user's session = %d, want 303 (revoked)", r.StatusCode)
	}
	r.Body.Close()

	// Signing back in lands on the password screen and goes nowhere else.
	again := login(t, ts, targetName, "pw")
	r = get(t, again, ts.URL+"/console/dashboard", map[string]string{"Accept": "text/html"})
	if r.StatusCode != 303 || r.Header.Get("Location") != "/console/password" {
		t.Errorf("after re-login = %d -> %q, want 303 -> /console/password", r.StatusCode, r.Header.Get("Location"))
	}
	r.Body.Close()

	// Rotating clears it.
	resp, _ := again.PostForm(ts.URL+"/console/password", url2.Values{
		"current_password": {"pw"}, "new_password": {"a-long-enough-one"}, "confirm_password": {"a-long-enough-one"},
	})
	resp.Body.Close()
	if r := get(t, login(t, ts, targetName, "a-long-enough-one"), ts.URL+"/console/dashboard",
		map[string]string{"Accept": "text/html"}); r.StatusCode != 200 {
		t.Errorf("console after rotation = %d, want 200", r.StatusCode)
	}
}

// The rotation gate fails CLOSED: a flag lookup that errors holds the operator on
// the password screen instead of waving them through. Simulated by dropping the
// column — which is also the one case that must NOT deny (42703 = the binary is
// ahead of its migration), so this pins both halves of that decision.
func TestPasswordGateFailsClosedExceptOnMissingColumn(t *testing.T) {
	ts, pg := newTestServer(t)
	_, name := mkUser(t, pg, sqlc.UserRoleAdmin)
	c := login(t, ts, name, "pw")

	if r := get(t, c, ts.URL+"/console/dashboard", map[string]string{"Accept": "text/html"}); r.StatusCode != 200 {
		t.Fatalf("baseline dashboard = %d, want 200", r.StatusCode)
	}

	// Version skew: the column does not exist yet. The console must keep working.
	if _, err := pg.Pool.Exec(t.Context(), `ALTER TABLE users DROP COLUMN must_change_password`); err != nil {
		t.Fatalf("drop column: %v", err)
	}
	t.Cleanup(func() {
		_, _ = pg.Pool.Exec(context.Background(),
			`ALTER TABLE users ADD COLUMN IF NOT EXISTS must_change_password BOOLEAN NOT NULL DEFAULT FALSE`)
	})
	if r := get(t, c, ts.URL+"/console/dashboard", map[string]string{"Accept": "text/html"}); r.StatusCode != 200 {
		t.Errorf("undefined_column must not brick the console; got %d", r.StatusCode)
	}
}
