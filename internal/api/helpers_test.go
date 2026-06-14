package api

import (
	"bytes"
	"context"
	"io/fs"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"
	webstatic "github.com/minhtt159/bank0/web/static"
	template "github.com/minhtt159/bank0/web/template"
)

// The operator console moves money, so it must load htmx from our own origin
// (vendored + embedded), never a third-party CDN with no integrity guarantee
// (WEB-4 / docs/10). Guard both the embed and the rendered markup.
func TestHTMXSelfHosted(t *testing.T) {
	if _, err := fs.Stat(webstatic.FS, "htmx.min.js"); err != nil {
		t.Fatalf("htmx.min.js must be embedded for same-origin serving: %v", err)
	}
	var buf bytes.Buffer
	if err := template.Shell("op", "admin", 0).Render(context.Background(), &buf); err != nil {
		t.Fatalf("render shell: %v", err)
	}
	html := buf.String()
	if !strings.Contains(html, `src="/static/htmx.min.js"`) {
		t.Error("console shell must load htmx from /static/htmx.min.js")
	}
	if strings.Contains(html, "unpkg") || strings.Contains(html, "://cdn") {
		t.Error("console shell must not reference a third-party CDN")
	}
}

func TestRoleGates(t *testing.T) {
	cases := []struct {
		role                          string
		money, users, approve         bool
	}{
		{"operator", true, false, false},
		{"admin", true, true, true},
		{"auditor", false, false, false},
		{"customer", false, false, false},
		{"", false, false, false},
	}
	for _, c := range cases {
		if got := canActOnMoney(c.role); got != c.money {
			t.Errorf("canActOnMoney(%q)=%v want %v", c.role, got, c.money)
		}
		if got := canManageUsers(c.role); got != c.users {
			t.Errorf("canManageUsers(%q)=%v want %v", c.role, got, c.users)
		}
		if got := canApprove(c.role); got != c.approve {
			t.Errorf("canApprove(%q)=%v want %v", c.role, got, c.approve)
		}
	}
}

func TestValidRole(t *testing.T) {
	for _, r := range []string{"customer", "operator", "admin", "auditor"} {
		if !validRole(r) {
			t.Errorf("validRole(%q) = false, want true", r)
		}
	}
	for _, r := range []string{"", "root", "Admin", "guest"} {
		if validRole(r) {
			t.Errorf("validRole(%q) = true, want false", r)
		}
	}
}

func TestStrOrNil(t *testing.T) {
	if strOrNil("  ") != nil || strOrNil("") != nil {
		t.Error("strOrNil(empty/blank) should be nil")
	}
	if p := strOrNil("  x "); p == nil || *p != "x" {
		t.Errorf("strOrNil trims and keeps; got %v", p)
	}
}

func TestOwnsAccount(t *testing.T) {
	me := uuid.New()
	other := uuid.New()
	if ownsAccount(me, nil) {
		t.Error("nil owner (system account) must not be owned")
	}
	if !ownsAccount(me, &me) {
		t.Error("own account should be owned")
	}
	if ownsAccount(me, &other) {
		t.Error("other's account must not be owned")
	}
}

func TestSearchQAndHasCursor(t *testing.T) {
	r1 := httptest.NewRequest("GET", "/x?q=alice", nil)
	if p := searchQ(r1); p == nil || *p != "alice" {
		t.Errorf("searchQ should read q; got %v", p)
	}
	r2 := httptest.NewRequest("GET", "/x?q=", nil)
	if searchQ(r2) != nil {
		t.Error("empty q should be nil")
	}
	r3 := httptest.NewRequest("GET", "/x", nil)
	if searchQ(r3) != nil {
		t.Error("absent q should be nil")
	}
	if isPagerNav(httptest.NewRequest("GET", "/x", nil)) {
		t.Error("no nav -> false")
	}
	if !isPagerNav(httptest.NewRequest("GET", "/x?nav=1", nil)) {
		t.Error("nav present -> true")
	}
}

// The keyset cursor must survive a buildPageURL -> pageCursor round-trip, incl. q.
func TestCursorRoundTrip(t *testing.T) {
	ts := time.Date(2026, 6, 8, 22, 13, 13, 355706000, time.UTC)
	id := uuid.New()
	q := "Bulk"
	cursor := ts.Format(time.RFC3339Nano) + "|" + id.String()
	u := buildPageURL("/console/transfers/results", cursor, nil, &q)

	req := httptest.NewRequest("GET", u, nil)
	gotTS, gotID := pageCursor(req)
	if gotTS == nil || !gotTS.Equal(ts) {
		t.Errorf("cursor ts round-trip: got %v want %v", gotTS, ts)
	}
	if gotID == nil || *gotID != id {
		t.Errorf("cursor id round-trip: got %v want %v", gotID, id)
	}
	if p := searchQ(req); p == nil || *p != q {
		t.Errorf("q round-trip: got %v want %q", p, q)
	}

	// First page: no cursor params -> both nil.
	if ts2, id2 := pageCursor(httptest.NewRequest("GET", "/x", nil)); ts2 != nil || id2 != nil {
		t.Error("absent cursor should yield (nil,nil)")
	}
}

// Prev should pop the history stack and Next should push the current cursor,
// so a Next then Prev round-trips back to the page we started on.
func TestPagerPrevNext(t *testing.T) {
	ts := time.Date(2026, 6, 8, 22, 13, 13, 0, time.UTC)
	id := uuid.New()

	// First page: a Next link exists, no Prev.
	prev, next := pagerLinks(httptest.NewRequest("GET", "/console/audit/results", nil),
		"/console/audit/results", nil, ts, id, true)
	if prev != "" {
		t.Errorf("first page should have no prev, got %q", prev)
	}
	if next == "" {
		t.Fatal("first page should have a next link")
	}

	// Follow Next -> second page. It must offer a Prev back to page one.
	req2 := httptest.NewRequest("GET", next, nil)
	prev2, _ := pagerLinks(req2, "/console/audit/results", nil, ts, id, false)
	if prev2 == "" {
		t.Fatal("second page should have a prev link")
	}

	// Following Prev lands on a request with no cursor (the first page).
	req1 := httptest.NewRequest("GET", prev2, nil)
	if c, _ := pageCursor(req1); c != nil {
		t.Error("prev from page two should return to the cursorless first page")
	}
}

func TestClientIP(t *testing.T) {
	r := httptest.NewRequest("GET", "/", nil)
	r.Header.Set("X-Forwarded-For", "1.2.3.4, 5.6.7.8")
	if ip := clientIP(r); ip != "1.2.3.4" {
		t.Errorf("XFF should use first hop; got %q", ip)
	}
	r2 := httptest.NewRequest("GET", "/", nil)
	r2.RemoteAddr = "9.9.9.9:1234"
	if ip := clientIP(r2); ip != "9.9.9.9" {
		t.Errorf("RemoteAddr host; got %q", ip)
	}
}

func TestHashTokenDeterministicOpaque(t *testing.T) {
	a := hashToken("secret-token")
	if a != hashToken("secret-token") {
		t.Error("hashToken must be deterministic")
	}
	if a == hashToken("other-token") {
		t.Error("different tokens must hash differently")
	}
	if len(a) != 64 { // sha256 hex
		t.Errorf("sha256 hex should be 64 chars; got %d", len(a))
	}
	if a == "secret-token" {
		t.Error("hash must not equal the raw token")
	}
}

func TestNewSessionTokenUnique(t *testing.T) {
	seen := map[string]bool{}
	for i := 0; i < 100; i++ {
		tok := newSessionToken()
		if tok == "" || seen[tok] {
			t.Fatalf("session tokens must be non-empty and unique; dup at %d", i)
		}
		seen[tok] = true
	}
}
