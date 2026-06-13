package api

import (
	"net/http"
	"net/url"
	"strings"
	"testing"

	sqlc "github.com/minhtt159/bank0/internal/db/sqlc"
)

func sessForm(t *testing.T, c *http.Client, urlStr string, form url.Values) (int, string) {
	t.Helper()
	resp, err := c.PostForm(urlStr, form)
	if err != nil {
		t.Fatalf("POST %s: %v", urlStr, err)
	}
	return resp.StatusCode, body(t, resp)
}

// The operator console renders the dispute queue and drives resolve_dispute. The
// resolve action is gated to operators/admins (canActOnMoney) — an auditor sees the
// queue read-only. Mirrors the maker-checker console flow.
func TestHTTPConsoleDisputes(t *testing.T) {
	ts, pg := newTestServer(t)
	resetDisputes(t, pg)
	aliceID, aliceName := mkUser(t, pg, sqlc.UserRoleCustomer)
	aliceAcct := mkAcct(t, pg, aliceID, 100_000)
	bobID, _ := mkUser(t, pg, sqlc.UserRoleCustomer)
	bobAcct := mkAcct(t, pg, bobID, 0)
	_, adminName := mkUser(t, pg, sqlc.UserRoleAdmin)
	_, auditorName := mkUser(t, pg, sqlc.UserRoleAuditor)
	aliceTok := clientToken(t, ts, aliceName, "pw")

	tid := postTransfer(t, ts, aliceTok, aliceAcct, bobAcct, 1000)
	_, b := doDispute(t, ts, aliceTok, tid, `{"category":"fraud","reason":"nope"}`)
	did := disputeID(t, b)

	admin := login(t, ts, adminName, "pw")

	// panel renders; results list the dispute with a Resolve action for an operator
	if r := get(t, admin, ts.URL+"/console/disputes", nil); r.StatusCode != 200 {
		t.Fatalf("console disputes panel = %d, want 200", r.StatusCode)
	}
	if r := get(t, admin, ts.URL+"/console/disputes/results", nil); r.StatusCode != 200 {
		t.Fatalf("disputes results = %d, want 200", r.StatusCode)
	} else if rb := body(t, r); !strings.Contains(rb, did) || !strings.Contains(rb, aliceName) || !strings.Contains(rb, "Resolve") {
		t.Errorf("results missing dispute/raiser/action; body=%.300s", rb)
	}

	// auditor sees the queue read-only (no Resolve action) and cannot resolve
	auditor := login(t, ts, auditorName, "pw")
	if r := get(t, auditor, ts.URL+"/console/disputes/results", nil); r.StatusCode != 200 {
		t.Errorf("auditor results = %d, want 200", r.StatusCode)
	} else if strings.Contains(body(t, r), "/resolve?status=") {
		t.Error("auditor must not see resolve actions")
	}
	if code, _ := sessForm(t, auditor, ts.URL+"/console/disputes/"+did+"/resolve?status=resolved", url.Values{}); code != 403 {
		t.Errorf("auditor console resolve = %d, want 403", code)
	}

	// admin resolves with a note -> re-render shows resolved; the client sees it
	if code, rb := sessForm(t, admin, ts.URL+"/console/disputes/"+did+"/resolve?status=resolved",
		url.Values{"resolution_note": {"refunded via console"}}); code != 200 || !strings.Contains(rb, "resolved") {
		t.Fatalf("admin resolve = %d; body=%.200s", code, rb)
	}
	if gc, gb := clientGet(t, ts, aliceTok, "/disputes/"+did); gc != 200 ||
		gjson(t, gb, "status") != "resolved" || !strings.Contains(gb, "refunded via console") {
		t.Errorf("client view after console resolve = %d; body=%s", gc, gb)
	}
}
