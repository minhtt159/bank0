package api

import (
	"encoding/json"
	"testing"

	sqlc "github.com/minhtt159/bank0/internal/db/sqlc"
)

// Server-side ledger filters: direction, free-text, and absolute-amount range over
// GET /accounts/{id}/ledger. (Composite-cursor tie coverage is in the db package's
// TestLedgerKeysetCoversTies.)
func TestHTTPLedgerFilters(t *testing.T) {
	ts, pg := newTestServer(t)
	aliceID, aliceName := mkUser(t, pg, sqlc.UserRoleCustomer)
	aliceAcct := mkAcct(t, pg, aliceID, 100_000) // 1 credit entry, description "fund", amount 100000
	bobID, _ := mkUser(t, pg, sqlc.UserRoleCustomer)
	bobAcct := mkAcct(t, pg, bobID, 0)
	tok := clientToken(t, ts, aliceName, "pw")

	postTransfer(t, ts, tok, aliceAcct, bobAcct, 250) // 1 debit entry on alice, amount 250

	count := func(query string) int {
		t.Helper()
		code, b := clientGet(t, ts, tok, "/accounts/"+aliceAcct.String()+"/ledger"+query)
		if code != 200 {
			t.Fatalf("ledger%s = %d; body=%s", query, code, b)
		}
		var rows []map[string]any
		if err := json.Unmarshal([]byte(b), &rows); err != nil {
			t.Fatalf("decode ledger: %v; body=%s", err, b)
		}
		return len(rows)
	}

	if n := count(""); n != 2 {
		t.Errorf("unfiltered ledger = %d entries, want 2 (fund credit + transfer debit)", n)
	}
	if n := count("?direction=debit"); n != 1 {
		t.Errorf("direction=debit = %d, want 1", n)
	}
	if n := count("?direction=credit"); n != 1 {
		t.Errorf("direction=credit = %d, want 1", n)
	}
	if n := count("?q=fund"); n != 1 {
		t.Errorf("q=fund = %d, want 1 (matches the deposit description)", n)
	}
	if n := count("?min_minor=1000"); n != 1 {
		t.Errorf("min_minor=1000 = %d, want 1 (the 100000 deposit; 250 excluded)", n)
	}
	if n := count("?max_minor=1000"); n != 1 {
		t.Errorf("max_minor=1000 = %d, want 1 (the 250 transfer; 100000 excluded)", n)
	}
	if n := count("?limit=1"); n != 1 {
		t.Errorf("limit=1 first page = %d, want 1", n)
	}
}
