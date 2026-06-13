package api

import (
	"encoding/json"
	"strings"
	"testing"

	sqlc "github.com/minhtt159/bank0/internal/db/sqlc"
)

// GET /transfers: cross-account, ownership-scoped, caller-relative direction, bare
// array, with filters. See spec-list-my-transfers.md (bare-array variant).
func TestHTTPListMyTransfers(t *testing.T) {
	ts, pg := newTestServer(t)
	aliceID, aliceName := mkUser(t, pg, sqlc.UserRoleCustomer)
	aliceAcct := mkAcct(t, pg, aliceID, 100_000)
	bobID, bobName := mkUser(t, pg, sqlc.UserRoleCustomer)
	bobAcct := mkAcct(t, pg, bobID, 100_000)
	_, carolName := mkUser(t, pg, sqlc.UserRoleCustomer)
	aliceTok := clientToken(t, ts, aliceName, "pw")
	bobTok := clientToken(t, ts, bobName, "pw")
	carolTok := clientToken(t, ts, carolName, "pw")

	postTransfer(t, ts, aliceTok, aliceAcct, bobAcct, 250) // alice -> bob  (alice: out)
	postTransfer(t, ts, bobTok, bobAcct, aliceAcct, 300)   // bob   -> alice (alice: in)

	list := func(tok, query string) []map[string]any {
		t.Helper()
		code, b := clientGet(t, ts, tok, "/transfers"+query)
		if code != 200 {
			t.Fatalf("GET /transfers%s = %d; %s", query, code, b)
		}
		var rows []map[string]any
		if err := json.Unmarshal([]byte(b), &rows); err != nil {
			t.Fatalf("decode: %v; %s", err, b)
		}
		return rows
	}

	// 3 transfers touch alice: the opening deposit (in), alice->bob (out), bob->alice (in).
	if al := list(aliceTok, ""); len(al) != 3 {
		t.Errorf("alice list = %d rows, want 3 (deposit + 2 transfers)", len(al))
	}
	if n := len(list(aliceTok, "?direction=out")); n != 1 {
		t.Errorf("alice direction=out = %d, want 1", n)
	}
	if n := len(list(aliceTok, "?direction=in")); n != 2 {
		t.Errorf("alice direction=in = %d, want 2 (deposit + bob->alice)", n)
	}
	if n := len(list(aliceTok, "?kind=deposit")); n != 1 {
		t.Errorf("alice kind=deposit = %d, want 1 (the opening deposit)", n)
	}
	if n := len(list(aliceTok, "?kind=transfer")); n != 2 {
		t.Errorf("alice kind=transfer = %d, want 2", n)
	}
	if n := len(list(aliceTok, "?limit=1")); n != 1 {
		t.Errorf("limit=1 first page = %d, want 1", n)
	}
	// the same alice<->bob transfers are caller-relative for bob too
	if n := len(list(bobTok, "?direction=out")); n != 1 { // bob's out = bob->alice
		t.Errorf("bob direction=out = %d, want 1", n)
	}
	// a non-party sees an empty array (never null)
	if cc, cb := clientGet(t, ts, carolTok, "/transfers"); cc != 200 || strings.TrimSpace(cb) != "[]" {
		t.Errorf("carol list = %d %q, want 200 []", cc, cb)
	}
	// invalid filters -> 400
	if c, _ := clientGet(t, ts, aliceTok, "/transfers?status=bogus"); c != 400 {
		t.Errorf("status=bogus = %d, want 400", c)
	}
	if c, _ := clientGet(t, ts, aliceTok, "/transfers?direction=sideways"); c != 400 {
		t.Errorf("direction=sideways = %d, want 400", c)
	}
}
