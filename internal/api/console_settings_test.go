package api

import (
	"context"
	"net/url"
	"strings"
	"testing"

	sqlc "github.com/minhtt159/bank0/internal/db/sqlc"
)

// API-8: the Settings panel exposes bank_settings. All staff view it; only admins
// edit. bank_settings is a singleton, so restore the seeded threshold on cleanup.
func TestHTTPConsoleSettings(t *testing.T) {
	ts, pg := newTestServer(t)
	t.Cleanup(func() {
		_, _ = pg.Pool.Exec(context.Background(), `SELECT update_bank_settings(1000000, 50000, NULL)`)
	})
	_, adminName := mkUser(t, pg, sqlc.UserRoleAdmin)
	_, auditorName := mkUser(t, pg, sqlc.UserRoleAuditor)
	admin := login(t, ts, adminName, "pw")
	auditor := login(t, ts, auditorName, "pw")

	// admin sees the editable form
	if r := get(t, admin, ts.URL+"/console/settings", nil); r.StatusCode != 200 {
		t.Fatalf("admin settings = %d, want 200", r.StatusCode)
	} else if b := body(t, r); !strings.Contains(b, "maker_checker_threshold") || !strings.Contains(b, "Save settings") {
		t.Errorf("admin panel missing edit form; body=%.300s", b)
	}

	// auditor sees it read-only (no save button)
	if r := get(t, auditor, ts.URL+"/console/settings", nil); r.StatusCode != 200 {
		t.Fatalf("auditor settings = %d, want 200", r.StatusCode)
	} else if strings.Contains(body(t, r), "Save settings") {
		t.Error("auditor must not see the edit form")
	}

	// admin can save
	if code, _ := sessForm(t, admin, ts.URL+"/console/settings", url.Values{
		"maker_checker_threshold": {"250.00"}, "default_transfer_limit": {"500.00"},
	}); code != 200 {
		t.Errorf("admin save = %d, want 200", code)
	}
	// auditor cannot save -> 403 (canManageSettings = admin only)
	if code, _ := sessForm(t, auditor, ts.URL+"/console/settings", url.Values{
		"maker_checker_threshold": {"999.00"}, "default_transfer_limit": {"500.00"},
	}); code != 403 {
		t.Errorf("auditor save = %d, want 403", code)
	}
}
