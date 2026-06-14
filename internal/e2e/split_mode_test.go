//go:build e2e

package e2e

import (
	"net/http"
	"strings"
	"testing"
)

// Scenario 1 — split-mode integrity.
//
// The whole point of Tier A: the two REAL binaries (portal, api) only serve their
// own surface. mode=all (the only thing the in-process internal/api suite exercises)
// CANNOT catch a route that leaks across the split, because in mode=all every route
// is mounted on one router. Here each surface is a separate process, so a route that
// is absent shows up as a 404 and an auth mechanism that doesn't belong is rejected.

// portalOnlyRoutes are mounted only when portalOn (Router(): registerConsole +
// genadmin.HandlerFromMux + the static /transfers/pending). On the api binary they
// must be entirely absent (404) — not 401/403, which would mean the route exists but
// the auth failed.
// Note on /transfers/pending: it is NOT a clean 404 on the api binary. The client
// surface registers GET /transfers/{id} (the shared getTransfer op), and "pending"
// matches {id}. So on api the request is intercepted by the JWT-guarded client route
// (401 without a bearer) — it never reaches an admin pending-list handler, which is
// the property that matters. That subtlety is asserted separately in
// TestSplitMode_PendingTransfersIsAdminOnly.
var portalOnlyRoutes = []struct {
	method, path string
}{
	{http.MethodGet, "/"},                  // console shell (consoleHome)
	{http.MethodGet, "/console/dashboard"}, // console screen
	{http.MethodGet, "/console/users"},     // console screen
	{http.MethodGet, "/admin/reconcile"},   // admin JSON (genadmin)
	{http.MethodGet, "/admin/disputes"},    // admin JSON (genadmin)
	{http.MethodPost, "/users"},            // admin JSON: createUser
	{http.MethodPost, "/accounts"},         // admin JSON: createAccount
}

// clientOnlyRoutes are mounted only when apiOn. On the portal binary they must be
// absent (404). /auth/login is public on api; /me and /transfers are JWT-guarded.
var clientOnlyRoutes = []struct {
	method, path string
}{
	{http.MethodPost, "/auth/login"},   // public client auth
	{http.MethodPost, "/auth/refresh"}, // public client auth
	{http.MethodGet, "/me"},            // JWT-guarded client profile
	{http.MethodGet, "/me/sessions"},   // JWT-guarded client sessions
	{http.MethodGet, "/transfers"},     // JWT-guarded client transfer history
	{http.MethodGet, "/beneficiaries"}, // JWT-guarded client beneficiaries
}

func TestSplitMode_PortalRoutesAbsentOnAPI(t *testing.T) {
	e := requireHarness(t)

	// /health is served on BOTH surfaces — sanity-check the precondition.
	if code, _ := rawGet(t, e.api.baseURL, "/health", nil); code != http.StatusOK {
		t.Fatalf("api /health = %d, want 200 (api process not serving health?)", code)
	}
	if code, _ := rawGet(t, e.portal.baseURL, "/health", nil); code != http.StatusOK {
		t.Fatalf("portal /health = %d, want 200", code)
	}

	for _, rt := range portalOnlyRoutes {
		var code int
		switch rt.method {
		case http.MethodGet:
			code, _ = rawGet(t, e.api.baseURL, rt.path, nil)
		case http.MethodPost:
			code, _ = rawPost(t, e.api.baseURL, rt.path, map[string]string{"Content-Type": "application/json"}, "{}")
		}
		if code != http.StatusNotFound {
			t.Errorf("portal-only %s %s on API surface = %d, want 404 (route leaked across the split)",
				rt.method, rt.path, code)
		}
	}
}

func TestSplitMode_ClientRoutesAbsentOnPortal(t *testing.T) {
	e := requireHarness(t)

	for _, rt := range clientOnlyRoutes {
		var code int
		switch rt.method {
		case http.MethodGet:
			code, _ = rawGet(t, e.portal.baseURL, rt.path, nil)
		case http.MethodPost:
			code, _ = rawPost(t, e.portal.baseURL, rt.path, map[string]string{"Content-Type": "application/json"}, "{}")
		}
		if code != http.StatusNotFound {
			t.Errorf("client-only %s %s on PORTAL surface = %d, want 404 (route leaked across the split)",
				rt.method, rt.path, code)
		}
	}
}

// TestSplitMode_PortalRoutesPresentOnPortal proves the portal-only routes ARE wired
// on the portal binary (so the 404s above are real absence, not a typo'd path). An
// unauthenticated programmatic caller gets 401 for the session-guarded JSON routes;
// the console shell redirects an HTML browser to /login (303).
func TestSplitMode_PortalRoutesPresentOnPortal(t *testing.T) {
	e := requireHarness(t)

	// Session-guarded admin JSON -> 401 for a programmatic (non-HTML) caller.
	for _, path := range []string{"/transfers/pending", "/admin/reconcile", "/admin/disputes"} {
		if code, _ := rawGet(t, e.portal.baseURL, path, nil); code != http.StatusUnauthorized {
			t.Errorf("unauth portal %s = %d, want 401 (route present, session required)", path, code)
		}
	}
	// Console shell: an HTML browser is redirected to /login (303), proving the route
	// exists but needs a session.
	hdr := map[string]string{"Accept": "text/html"}
	if code, _ := rawGet(t, e.portal.baseURL, "/console/dashboard", hdr); code != http.StatusSeeOther {
		t.Errorf("unauth portal GET /console/dashboard (html) = %d, want 303 -> /login", code)
	}
	// The public portal login page renders (200).
	if code, _ := rawGet(t, e.portal.baseURL, "/login", hdr); code != http.StatusOK {
		t.Errorf("portal GET /login = %d, want 200", code)
	}
}

// TestSplitMode_ClientRoutesPresentOnAPI proves the client routes ARE wired on the
// api binary. Public /auth/login responds (401 invalid creds, not 404); the
// JWT-guarded routes 401 without a bearer.
func TestSplitMode_ClientRoutesPresentOnAPI(t *testing.T) {
	e := requireHarness(t)

	// Public /auth/login exists: bad creds -> 401 (a 404 would mean the route is absent).
	code, _ := rawPost(t, e.api.baseURL, "/auth/login",
		map[string]string{"Content-Type": "application/json"},
		`{"username":"nope-`+uniq()+`","password":"nope"}`)
	if code != http.StatusUnauthorized {
		t.Errorf("api POST /auth/login (bad creds) = %d, want 401 (route present)", code)
	}
	// JWT-guarded routes -> 401 without a bearer.
	for _, path := range []string{"/me", "/me/sessions", "/transfers", "/beneficiaries"} {
		if code, _ := rawGet(t, e.api.baseURL, path, nil); code != http.StatusUnauthorized {
			t.Errorf("unauth api GET %s = %d, want 401 (route present, JWT required)", path, code)
		}
	}
}

// TestSplitMode_CrossAuthRejected is the authorization half of the scenario:
//   - A client JWT (minted by the api binary) cannot reach the admin JSON surface.
//     The api binary doesn't serve /admin/* at all (404), AND the portal binary
//     rejects the bearer because it authenticates by cookie session, not JWT (401).
//   - A portal cookie cannot reach client routes: the portal binary doesn't serve
//     /me (404), AND the api binary ignores the cookie and 401s (needs a bearer).
func TestSplitMode_CrossAuthRejected(t *testing.T) {
	e := requireHarness(t)

	// A real, valid customer JWT from the api binary (admin/admin is staff, but a
	// JWT is a JWT — what matters is the portal won't accept ANY bearer).
	cust := loginAPI(t, e.api.baseURL, "admin", "admin")
	bearer := map[string]string{"Authorization": "Bearer " + cust.token}

	// Client JWT against the admin JSON surface on the PORTAL binary: the portal
	// guards with requireSession (cookie), so a bearer is unauthenticated -> 401.
	if code, _ := rawGet(t, e.portal.baseURL, "/admin/reconcile", bearer); code != http.StatusUnauthorized {
		t.Errorf("client JWT -> portal /admin/reconcile = %d, want 401 (portal is cookie-auth, not JWT)", code)
	}
	if code, _ := rawGet(t, e.portal.baseURL, "/transfers/pending", bearer); code != http.StatusUnauthorized {
		t.Errorf("client JWT -> portal /transfers/pending = %d, want 401", code)
	}
	// And the admin JSON surface simply doesn't exist on the api binary.
	if code, _ := rawGet(t, e.api.baseURL, "/admin/reconcile", bearer); code != http.StatusNotFound {
		t.Errorf("client JWT -> api /admin/reconcile = %d, want 404 (admin surface absent on api)", code)
	}

	// A portal cookie session.
	op := loginPortal(t, e.portal.baseURL, "admin", "admin")
	u, _ := urlParse(e.portal.baseURL)
	cookies := op.client.Jar.Cookies(u)
	if len(cookies) == 0 {
		t.Fatal("expected a portal session cookie")
	}
	cookieHdr := cookieHeader(cookies)

	// Portal cookie against a client route on the API binary: api guards with
	// requireJWT (bearer); the cookie is ignored -> 401.
	if code, _ := rawGet(t, e.api.baseURL, "/me", map[string]string{"Cookie": cookieHdr}); code != http.StatusUnauthorized {
		t.Errorf("portal cookie -> api /me = %d, want 401 (api is JWT-auth, not cookie)", code)
	}
	if code, _ := rawGet(t, e.api.baseURL, "/transfers", map[string]string{"Cookie": cookieHdr}); code != http.StatusUnauthorized {
		t.Errorf("portal cookie -> api /transfers = %d, want 401", code)
	}
	// And /me simply doesn't exist on the portal binary.
	if code, _ := rawGet(t, e.portal.baseURL, "/me", map[string]string{"Cookie": cookieHdr}); code != http.StatusNotFound {
		t.Errorf("portal cookie -> portal /me = %d, want 404 (client surface absent on portal)", code)
	}
}

// TestSplitMode_PendingTransfersIsAdminOnly captures the one genuinely subtle route
// in the split. On the PORTAL binary, GET /transfers/pending is a static admin JSON
// route (listPendingJSON, behind requireSession) registered ahead of any client
// subrouter, so an authenticated operator gets the pending-transfers JSON array. On
// the API binary there is no such route — but the client surface's greedy
// GET /transfers/{id} matches {id}="pending", so the request is intercepted by the
// JWT guard (401) and NEVER reaches a pending-list handler. mode=all can't surface
// this distinction because both routes live on one router there.
func TestSplitMode_PendingTransfersIsAdminOnly(t *testing.T) {
	e := requireHarness(t)

	// Portal: authenticated operator -> the admin pending-transfers JSON array.
	op := loginPortal(t, e.portal.baseURL, "admin", "admin")
	resp := op.get("/transfers/pending")
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("operator GET portal /transfers/pending = %d, want 200", resp.StatusCode)
	}
	if ct := resp.Header.Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
		t.Errorf("portal /transfers/pending Content-Type = %q, want application/json", ct)
	}
	var pending []map[string]any
	decodeJSONBody(t, resp, &pending) // a JSON array (possibly empty) — the admin list shape

	// API: same path, no bearer -> 401 from the client getTransfer route (NOT the
	// admin list, and NOT 200).
	if code, _ := rawGet(t, e.api.baseURL, "/transfers/pending", nil); code != http.StatusUnauthorized {
		t.Errorf("unauth api GET /transfers/pending = %d, want 401 (shadowed by client /transfers/{id})", code)
	}
	// API: with a valid bearer, "pending" is not a UUID, so the client route rejects
	// it (400 from the generated UUID param binding) — still never the admin list.
	cust := loginAPI(t, e.api.baseURL, "admin", "admin")
	code, _ := rawGet(t, e.api.baseURL, "/transfers/pending",
		map[string]string{"Authorization": "Bearer " + cust.token})
	if code == http.StatusOK {
		t.Errorf("api GET /transfers/pending with bearer = 200; the admin pending list must not be reachable on api")
	}
}
