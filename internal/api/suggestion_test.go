package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/minhtt159/bank0/internal/db"
	sqlc "github.com/minhtt159/bank0/internal/db/sqlc"
)

type suggestion struct {
	AccountID       string  `json:"account_id"`
	Iban            string  `json:"iban"`
	OwnerNameMasked string  `json:"owner_name_masked"`
	Reason          string  `json:"reason"`
	Scenario        *string `json:"scenario"`
	Source          string  `json:"source"`
}

func getSuggestion(t *testing.T, ts *httptest.Server, token, query string) (int, string) {
	t.Helper()
	hdr := map[string]string{}
	if token != "" {
		hdr["Authorization"] = "Bearer " + token
	}
	resp := get(t, newClient(), ts.URL+"/transfers/suggestion"+query, hdr)
	return resp.StatusCode, body(t, resp)
}

// decodeOptions parses the {"options": [...]} wrapper and returns the options. A
// bare array (or any non-object) fails the unmarshal — pinning the wrapper shape.
func decodeOptions(t *testing.T, b string) []suggestion {
	t.Helper()
	var wrap struct {
		Options []suggestion `json:"options"`
	}
	if err := json.Unmarshal([]byte(b), &wrap); err != nil {
		t.Fatalf("decode {options:[...]}: %v; body=%s", err, b)
	}
	return wrap.Options
}

// resetScenarios clears the shared guided_scenarios table so each test is isolated
// (a global scenario from one test would otherwise match any later caller).
func resetScenarios(t *testing.T, pg *db.Postgres) {
	t.Helper()
	if _, err := pg.Pool.Exec(context.Background(), `TRUNCATE guided_scenarios`); err != nil {
		t.Fatalf("truncate guided_scenarios: %v", err)
	}
}

// seedScenario inserts a guided_scenarios row directly (it's seed/console-controlled;
// there's no client write path — the client only toggles whether it ASKS).
func seedScenario(t *testing.T, pg *db.Postgres, name string, target uuid.UUID, reason string, minAmount int64) {
	t.Helper()
	if _, err := pg.Pool.Exec(context.Background(),
		`INSERT INTO guided_scenarios (name, target_account_id, reason, min_amount_minor) VALUES ($1,$2,$3,$4)`,
		name, target, reason, minAmount); err != nil {
		t.Fatalf("seed scenario: %v", err)
	}
}

// GET /transfers/suggestion (mule menu, v2): {"options":[...]} of up to 3 third-party
// mule candidates drawn from the active guided_scenarios short-list — never the
// caller's own; empty when no scenario applies; foreign from_account 403; a chosen
// candidate still flows through POST /transfers. See spec-banking-grade-hardening.md §5.
func TestHTTPGuidedMenu(t *testing.T) {
	ts, pg := newTestServer(t)
	resetScenarios(t, pg)
	aliceID, aliceName := mkUser(t, pg, sqlc.UserRoleCustomer)
	a1 := mkAcct(t, pg, aliceID, 100_000)
	a2 := mkAcct(t, pg, aliceID, 0)
	bobID, _ := mkUser(t, pg, sqlc.UserRoleCustomer)
	bobAcct := mkAcct(t, pg, bobID, 0)
	carolID, _ := mkUser(t, pg, sqlc.UserRoleCustomer)
	carolAcct := mkAcct(t, pg, carolID, 0)
	tok := clientToken(t, ts, aliceName, "pw")

	// no bearer -> 401
	if code, _ := getSuggestion(t, ts, "", ""); code != 401 {
		t.Errorf("no-bearer = %d, want 401", code)
	}

	// no scenario -> empty menu (the client falls back to its own account)
	code, b := getSuggestion(t, ts, tok, "?from_account="+a1.String())
	if code != 200 {
		t.Fatalf("empty menu = %d, want 200; body=%s", code, b)
	}
	if opts := decodeOptions(t, b); len(opts) != 0 {
		t.Errorf("no scenario should give an empty menu, got %d options; body=%s", len(opts), b)
	}

	// two active global scenarios (mules) -> the menu draws from them, source=scenario
	seedScenario(t, pg, "mule_"+uhex(6), bobAcct, "Verified payee", 0)
	seedScenario(t, pg, "mule_"+uhex(6), carolAcct, "Trusted merchant", 0)
	code, b = getSuggestion(t, ts, tok, "?from_account="+a1.String())
	if code != 200 {
		t.Fatalf("menu = %d, want 200; body=%s", code, b)
	}
	opts := decodeOptions(t, b)
	if len(opts) == 0 || len(opts) > 3 {
		t.Fatalf("menu size = %d, want 1..3; body=%s", len(opts), b)
	}
	mules := map[string]bool{bobAcct.String(): true, carolAcct.String(): true}
	for _, o := range opts {
		if !mules[o.AccountID] {
			t.Errorf("option %s is not a seeded mule (never the caller's own, never an arbitrary peer)", o.AccountID)
		}
		if o.AccountID == a1.String() || o.AccountID == a2.String() {
			t.Errorf("menu must never include the caller's own account; got %s", o.AccountID)
		}
		if o.Source != "scenario" {
			t.Errorf("backend option source = %q, want scenario", o.Source)
		}
	}
	if strings.Contains(b, "balance_minor") || strings.Contains(b, "full_name") {
		t.Errorf("menu leaked balance/full_name: %s", b)
	}

	// foreign from_account (bob's) -> 403, before any resolve
	if code, _ := getSuggestion(t, ts, tok, "?from_account="+bobAcct.String()); code != 403 {
		t.Errorf("foreign from_account = %d, want 403", code)
	}

	// a chosen candidate flows through POST /transfers unchanged (money path intact)
	tbody := `{"debit_account":"` + a1.String() + `","credit_account":"` + opts[0].AccountID + `","amount_minor":100}`
	req, _ := http.NewRequest(http.MethodPost, ts.URL+"/transfers", strings.NewReader(tbody))
	req.Header.Set("Authorization", "Bearer "+tok)
	req.Header.Set("Idempotency-Key", uuid.NewString())
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("transfer: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != 200 {
		t.Errorf("transfer to a menu candidate = %d, want 200", resp.StatusCode)
	}
}

// amount_minor gates pool membership: below a scenario's threshold its mule is not
// eligible (empty menu); at/above it the mule appears.
func TestHTTPGuidedMenuAmountGate(t *testing.T) {
	ts, pg := newTestServer(t)
	resetScenarios(t, pg)
	aliceID, aliceName := mkUser(t, pg, sqlc.UserRoleCustomer)
	a1 := mkAcct(t, pg, aliceID, 0)
	bobID, _ := mkUser(t, pg, sqlc.UserRoleCustomer)
	bobAcct := mkAcct(t, pg, bobID, 0)
	tok := clientToken(t, ts, aliceName, "pw")
	seedScenario(t, pg, "gate_"+uhex(6), bobAcct, "Verified payee", 10_000) // eligible only >= €100

	// below threshold: the only scenario is gated out -> empty menu
	code, b := getSuggestion(t, ts, tok, "?from_account="+a1.String()+"&amount_minor=5000")
	if code != 200 {
		t.Fatalf("below-gate = %d, want 200; body=%s", code, b)
	}
	if opts := decodeOptions(t, b); len(opts) != 0 {
		t.Errorf("below-gate menu should be empty, got %d", len(opts))
	}

	// at/above threshold: the mule becomes eligible
	code, b = getSuggestion(t, ts, tok, "?from_account="+a1.String()+"&amount_minor=20000")
	if code != 200 {
		t.Fatalf("above-gate = %d, want 200; body=%s", code, b)
	}
	opts := decodeOptions(t, b)
	if len(opts) != 1 || opts[0].AccountID != bobAcct.String() || opts[0].Source != "scenario" {
		t.Errorf("above-gate = %+v, want exactly [bob with source=scenario]", opts)
	}
}
