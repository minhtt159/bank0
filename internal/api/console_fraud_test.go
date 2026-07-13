package api

import (
	"context"
	"net/http"
	"net/url"
	"strings"
	"testing"

	"github.com/minhtt159/bank0/internal/db"
	sqlc "github.com/minhtt159/bank0/internal/db/sqlc"
)

// countAdminActions returns how many admin_actions rows exist for an action+target —
// used to assert a console mutation wrote its audit trail.
func countAdminActions(t *testing.T, pg *db.Postgres, action, targetID string) int {
	t.Helper()
	var n int
	if err := pg.Pool.QueryRow(context.Background(),
		`SELECT count(*) FROM admin_actions WHERE action = $1 AND target_id = $2`,
		action, targetID).Scan(&n); err != nil {
		t.Fatalf("count admin_actions: %v", err)
	}
	return n
}

func latestWarningRuleID(t *testing.T, pg *db.Postgres) string {
	t.Helper()
	var id string
	if err := pg.Pool.QueryRow(context.Background(),
		`SELECT id FROM warning_rules ORDER BY created_at DESC LIMIT 1`).Scan(&id); err != nil {
		t.Fatalf("latest warning rule: %v", err)
	}
	return id
}

// Rec 22: the warning-rules panel. All staff view (read-only for non-admins); only
// admins (canManageSettings) create/edit/toggle, and each mutation is audited.
func TestHTTPConsoleWarningRules(t *testing.T) {
	ts, pg := newTestServer(t)
	t.Cleanup(func() { _, _ = pg.Pool.Exec(context.Background(), `TRUNCATE warning_rules`) })

	_, adminName := mkUser(t, pg, sqlc.UserRoleAdmin)
	_, opName := mkUser(t, pg, sqlc.UserRoleOperator)
	_, audName := mkUser(t, pg, sqlc.UserRoleAuditor)
	admin := login(t, ts, adminName, "pw")
	op := login(t, ts, opName, "pw")
	aud := login(t, ts, audName, "pw")

	// all staff can view the panel + results.
	for name, c := range map[string]*http.Client{"admin": admin, "operator": op, "auditor": aud} {
		if r := get(t, c, ts.URL+"/console/warning-rules", nil); r.StatusCode != 200 {
			t.Errorf("%s warning-rules panel = %d, want 200", name, r.StatusCode)
		}
	}
	// admin sees the create form; non-admins get it read-only (no form).
	if b := body(t, get(t, admin, ts.URL+"/console/warning-rules/results", nil)); !strings.Contains(b, "New warning rule") {
		t.Errorf("admin missing create form; body=%.200s", b)
	}
	if b := body(t, get(t, op, ts.URL+"/console/warning-rules/results", nil)); strings.Contains(b, "New warning rule") {
		t.Error("operator must not see the create form")
	}

	form := url.Values{
		"match_min_band": {"high"}, "category": {"risk_warning"}, "severity": {"warning"},
		"decision": {"warn"}, "cooling_off_seconds": {"0"}, "priority": {"10"},
		"headline": {"High-risk payment"}, "body": {"Please double-check."}, "active": {"true"},
	}

	// RBAC: operator + auditor cannot create -> 403.
	if code, _ := sessForm(t, op, ts.URL+"/console/warning-rules", form); code != 403 {
		t.Errorf("operator create = %d, want 403", code)
	}
	if code, _ := sessForm(t, aud, ts.URL+"/console/warning-rules", form); code != 403 {
		t.Errorf("auditor create = %d, want 403", code)
	}

	// validation: neither match key set -> flash, no row created.
	bad := url.Values{
		"category": {"risk_warning"}, "severity": {"warning"}, "decision": {"warn"},
		"cooling_off_seconds": {"0"}, "priority": {"0"},
	}
	if code, b := sessForm(t, admin, ts.URL+"/console/warning-rules", bad); code != 200 || !strings.Contains(b, "at least one match key") {
		t.Errorf("no-match-key create = %d; body=%.200s", code, b)
	}

	// admin creates -> round-trips into the list.
	if code, b := sessForm(t, admin, ts.URL+"/console/warning-rules", form); code != 200 || !strings.Contains(b, "Warning rule created") {
		t.Fatalf("admin create = %d; body=%.200s", code, b)
	}
	if b := body(t, get(t, admin, ts.URL+"/console/warning-rules/results", nil)); !strings.Contains(b, "risk_warning") || !strings.Contains(b, "high") {
		t.Errorf("created rule not listed; body=%.300s", b)
	}
	ruleID := latestWarningRuleID(t, pg)

	// toggle inactive -> re-render + DB reflect + audit.
	if code, b := sessForm(t, admin, ts.URL+"/console/warning-rules/"+ruleID+"/toggle", url.Values{"active": {"false"}}); code != 200 || !strings.Contains(b, "deactivated") {
		t.Fatalf("toggle off = %d; body=%.200s", code, b)
	}
	var active bool
	_ = pg.Pool.QueryRow(context.Background(), `SELECT active FROM warning_rules WHERE id = $1`, ruleID).Scan(&active)
	if active {
		t.Error("rule still active after deactivate")
	}

	// edit -> UpdateWarningRule round-trips (change decision to review).
	edit := url.Values{
		"match_min_band": {"high"}, "category": {"risk_warning"}, "severity": {"critical"},
		"decision": {"review"}, "cooling_off_seconds": {"3600"}, "priority": {"20"},
		"headline": {"Reviewed"}, "body": {"held"}, "active": {"true"},
	}
	if code, b := sessForm(t, admin, ts.URL+"/console/warning-rules/"+ruleID, edit); code != 200 || !strings.Contains(b, "Warning rule updated") {
		t.Fatalf("edit = %d; body=%.200s", code, b)
	}

	// audit trail: create + toggle + update all recorded against the rule id.
	if n := countAdminActions(t, pg, "create_warning_rule", ruleID); n != 1 {
		t.Errorf("create audit rows = %d, want 1", n)
	}
	if n := countAdminActions(t, pg, "toggle_warning_rule", ruleID); n != 1 {
		t.Errorf("toggle audit rows = %d, want 1", n)
	}
	if n := countAdminActions(t, pg, "update_warning_rule", ruleID); n != 1 {
		t.Errorf("update audit rows = %d, want 1", n)
	}
}

// Rec 25: the AML watchlist panel. Same RBAC as warning rules — all staff view, only
// admins add/toggle — and each mutation is audited.
func TestHTTPConsoleWatchlist(t *testing.T) {
	ts, pg := newTestServer(t)
	t.Cleanup(func() { _, _ = pg.Pool.Exec(context.Background(), `TRUNCATE watchlist_entries`) })

	_, adminName := mkUser(t, pg, sqlc.UserRoleAdmin)
	_, opName := mkUser(t, pg, sqlc.UserRoleOperator)
	admin := login(t, ts, adminName, "pw")
	op := login(t, ts, opName, "pw")

	// admin sees the add form; operator does not.
	if b := body(t, get(t, admin, ts.URL+"/console/watchlist/results", nil)); !strings.Contains(b, "New watchlist entry") {
		t.Errorf("admin missing add form; body=%.200s", b)
	}
	if b := body(t, get(t, op, ts.URL+"/console/watchlist/results", nil)); strings.Contains(b, "New watchlist entry") {
		t.Error("operator must not see the add form")
	}

	pattern := "%ACME LAUNDERING%"
	form := url.Values{"pattern": {pattern}, "reason": {"sanctions list"}}

	// RBAC: operator cannot add -> 403.
	if code, _ := sessForm(t, op, ts.URL+"/console/watchlist", form); code != 403 {
		t.Errorf("operator add = %d, want 403", code)
	}
	// empty pattern -> flash, no row.
	if code, b := sessForm(t, admin, ts.URL+"/console/watchlist", url.Values{"pattern": {"  "}}); code != 200 || !strings.Contains(b, "non-empty") {
		t.Errorf("empty-pattern add = %d; body=%.200s", code, b)
	}

	// admin adds -> round-trips into the list.
	if code, b := sessForm(t, admin, ts.URL+"/console/watchlist", form); code != 200 || !strings.Contains(b, "Watchlist entry added") {
		t.Fatalf("admin add = %d; body=%.200s", code, b)
	}
	if b := body(t, get(t, admin, ts.URL+"/console/watchlist/results", nil)); !strings.Contains(b, pattern) {
		t.Errorf("added entry not listed; body=%.300s", b)
	}

	var entryID string
	if err := pg.Pool.QueryRow(context.Background(),
		`SELECT id FROM watchlist_entries WHERE pattern = $1`, pattern).Scan(&entryID); err != nil {
		t.Fatalf("find entry: %v", err)
	}

	// deactivate then reactivate.
	if code, b := sessForm(t, admin, ts.URL+"/console/watchlist/"+entryID+"/toggle", url.Values{"active": {"false"}}); code != 200 || !strings.Contains(b, "deactivated") {
		t.Fatalf("deactivate = %d; body=%.200s", code, b)
	}
	var active bool
	_ = pg.Pool.QueryRow(context.Background(), `SELECT active FROM watchlist_entries WHERE id = $1`, entryID).Scan(&active)
	if active {
		t.Error("entry still active after deactivate")
	}
	if code, _ := sessForm(t, admin, ts.URL+"/console/watchlist/"+entryID+"/toggle", url.Values{"active": {"true"}}); code != 200 {
		t.Errorf("reactivate = %d, want 200", code)
	}

	// audit trail: add + two toggles.
	if n := countAdminActions(t, pg, "create_watchlist_entry", entryID); n != 1 {
		t.Errorf("create audit rows = %d, want 1", n)
	}
	if n := countAdminActions(t, pg, "toggle_watchlist_entry", entryID); n != 2 {
		t.Errorf("toggle audit rows = %d, want 2", n)
	}
}
