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

func decodeSuggestion(t *testing.T, b string) suggestion {
	t.Helper()
	var sg suggestion
	if err := json.Unmarshal([]byte(b), &sg); err != nil {
		t.Fatalf("decode suggestion: %v; body=%s", err, b)
	}
	return sg
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

// GET /transfers/suggestion: own-account safe default, scenario-driven mule, foreign
// from_account 403, and the suggested account still flows through POST /transfers.
// See spec-guided-transfer-suggestion.md.
func TestHTTPGuidedSuggestion(t *testing.T) {
	ts, pg := newTestServer(t)
	resetScenarios(t, pg)
	aliceID, aliceName := mkUser(t, pg, sqlc.UserRoleCustomer)
	a1 := mkAcct(t, pg, aliceID, 100_000)
	a2 := mkAcct(t, pg, aliceID, 0)
	bobID, _ := mkUser(t, pg, sqlc.UserRoleCustomer)
	bobAcct := mkAcct(t, pg, bobID, 0)
	tok := clientToken(t, ts, aliceName, "pw")

	// no bearer -> 401
	if code, _ := getSuggestion(t, ts, "", ""); code != 401 {
		t.Errorf("no-bearer = %d, want 401", code)
	}

	// own-account safe default (no scenario): from=a1 excludes a1 -> suggests a2
	code, b := getSuggestion(t, ts, tok, "?from_account="+a1.String())
	if code != 200 {
		t.Fatalf("own-account = %d, want 200; body=%s", code, b)
	}
	sg := decodeSuggestion(t, b)
	if sg.Source != "own_account" || sg.AccountID != a2.String() {
		t.Errorf("own-account: source=%s account=%s, want own_account/%s", sg.Source, sg.AccountID, a2)
	}
	if sg.Scenario != nil {
		t.Errorf("own-account scenario should be null, got %q", *sg.Scenario)
	}
	if strings.Contains(b, "balance_minor") || strings.Contains(b, "full_name") {
		t.Errorf("suggestion leaked balance/full_name: %s", b)
	}

	// foreign from_account (bob's) -> 403, before any resolve
	if code, _ := getSuggestion(t, ts, tok, "?from_account="+bobAcct.String()); code != 403 {
		t.Errorf("foreign from_account = %d, want 403", code)
	}

	// scenario: a global active scenario targeting bob (the mule) wins over own-account
	name := "app_scam_" + uhex(6)
	seedScenario(t, pg, name, bobAcct, "Verified payee", 0)
	code, b = getSuggestion(t, ts, tok, "?from_account="+a1.String())
	if code != 200 {
		t.Fatalf("scenario = %d, want 200; body=%s", code, b)
	}
	sg = decodeSuggestion(t, b)
	if sg.Source != "scenario" || sg.AccountID != bobAcct.String() {
		t.Errorf("scenario: source=%s account=%s, want scenario/%s", sg.Source, sg.AccountID, bobAcct)
	}
	if sg.Scenario == nil || *sg.Scenario != name {
		t.Errorf("scenario name = %v, want %q", sg.Scenario, name)
	}
	if strings.Contains(b, "balance_minor") || strings.Contains(b, "full_name") {
		t.Errorf("scenario suggestion leaked fields: %s", b)
	}

	// the suggested account flows through POST /transfers unchanged (money path intact)
	tbody := `{"debit_account":"` + a1.String() + `","credit_account":"` + sg.AccountID + `","amount_minor":100}`
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
		t.Errorf("transfer to suggested account = %d, want 200", resp.StatusCode)
	}
}

// amount_minor gates a scenario; below the threshold a single-account caller gets 204.
func TestHTTPGuidedSuggestionAmountGate(t *testing.T) {
	ts, pg := newTestServer(t)
	resetScenarios(t, pg)
	aliceID, aliceName := mkUser(t, pg, sqlc.UserRoleCustomer)
	a1 := mkAcct(t, pg, aliceID, 0)
	bobID, _ := mkUser(t, pg, sqlc.UserRoleCustomer)
	bobAcct := mkAcct(t, pg, bobID, 0)
	tok := clientToken(t, ts, aliceName, "pw")
	seedScenario(t, pg, "gate_"+uhex(6), bobAcct, "Verified payee", 10_000) // fires only >= €100

	// below threshold: scenario silent; alice has only a1 (excluded) -> 204
	if code, _ := getSuggestion(t, ts, tok, "?from_account="+a1.String()+"&amount_minor=5000"); code != 204 {
		t.Errorf("below-gate = %d, want 204", code)
	}
	// at/above threshold: scenario fires -> bob
	code, b := getSuggestion(t, ts, tok, "?from_account="+a1.String()+"&amount_minor=20000")
	if code != 200 {
		t.Fatalf("above-gate = %d, want 200; body=%s", code, b)
	}
	if sg := decodeSuggestion(t, b); sg.Source != "scenario" || sg.AccountID != bobAcct.String() {
		t.Errorf("above-gate = %+v, want scenario/%s", sg, bobAcct)
	}
}
