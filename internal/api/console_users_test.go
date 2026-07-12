package api

import (
	"context"
	"net/http"
	"net/url"
	"testing"

	"github.com/google/uuid"

	"github.com/minhtt159/bank0/internal/db"
	sqlc "github.com/minhtt159/bank0/internal/db/sqlc"
)

// Operator console user management: creating users and editing per-user invitation
// quotas are gated to operators + admins (canCreateUsers); auditors are refused.
// This is the RBAC surface for invitation-gated registration on the portal side.

func userInvites(t *testing.T, pg *db.Postgres, id uuid.UUID) int {
	t.Helper()
	var n int
	if err := pg.Pool.QueryRow(context.Background(),
		`SELECT invites_remaining FROM users WHERE id = $1`, id).Scan(&n); err != nil {
		t.Fatalf("read invites_remaining: %v", err)
	}
	return n
}

// TestHTTPConsoleUsersRBAC drives the three gated actions (new-user form, create
// user, set invites) as operator (allowed), admin (allowed), and auditor (403).
func TestHTTPConsoleUsersRBAC(t *testing.T) {
	ts, pg := newTestServer(t)
	_, opName := mkUser(t, pg, sqlc.UserRoleOperator)
	_, adminName := mkUser(t, pg, sqlc.UserRoleAdmin)
	_, audName := mkUser(t, pg, sqlc.UserRoleAuditor)
	target, _ := mkUser(t, pg, sqlc.UserRoleCustomer) // a user whose quota we edit

	operator := login(t, ts, opName, "pw")
	admin := login(t, ts, adminName, "pw")
	auditor := login(t, ts, audName, "pw")

	// --- new-user form (GET) ---------------------------------------------
	if r := get(t, operator, ts.URL+"/console/users/new", nil); r.StatusCode != http.StatusOK {
		t.Errorf("operator new-user form = %d, want 200", r.StatusCode)
	}
	if r := get(t, admin, ts.URL+"/console/users/new", nil); r.StatusCode != http.StatusOK {
		t.Errorf("admin new-user form = %d, want 200", r.StatusCode)
	}
	if r := get(t, auditor, ts.URL+"/console/users/new", nil); r.StatusCode != http.StatusForbidden {
		t.Errorf("auditor new-user form = %d, want 403", r.StatusCode)
	}

	// --- create user (POST) ----------------------------------------------
	createForm := func() url.Values {
		return url.Values{
			"username":  {"co" + uhex(10)},
			"password":  {"correct-horse-battery"},
			"full_name": {"Console Made"},
			"role":      {string(sqlc.UserRoleCustomer)},
		}
	}
	if code, _ := sessForm(t, operator, ts.URL+"/console/users", createForm()); code != http.StatusOK {
		t.Errorf("operator create user = %d, want 200", code)
	}
	if code, _ := sessForm(t, admin, ts.URL+"/console/users", createForm()); code != http.StatusOK {
		t.Errorf("admin create user = %d, want 200", code)
	}
	if code, _ := sessForm(t, auditor, ts.URL+"/console/users", createForm()); code != http.StatusForbidden {
		t.Errorf("auditor create user = %d, want 403", code)
	}

	// --- set invites (POST) ----------------------------------------------
	invitesURL := ts.URL + "/console/users/" + target.String() + "/invites"

	// Auditor is refused and the value must not change.
	before := userInvites(t, pg, target)
	if code, _ := sessForm(t, auditor, invitesURL, url.Values{"invites_remaining": {"1"}}); code != http.StatusForbidden {
		t.Errorf("auditor set invites = %d, want 403", code)
	}
	if after := userInvites(t, pg, target); after != before {
		t.Errorf("auditor set invites changed value %d -> %d", before, after)
	}

	// Operator may set it (10 -> 3).
	if code, _ := sessForm(t, operator, invitesURL, url.Values{"invites_remaining": {"3"}}); code != http.StatusOK {
		t.Errorf("operator set invites = %d, want 200", code)
	}
	if got := userInvites(t, pg, target); got != 3 {
		t.Errorf("after operator set invites = %d, want 3", got)
	}

	// Admin may set it too (3 -> 7).
	if code, _ := sessForm(t, admin, invitesURL, url.Values{"invites_remaining": {"7"}}); code != http.StatusOK {
		t.Errorf("admin set invites = %d, want 200", code)
	}
	if got := userInvites(t, pg, target); got != 7 {
		t.Errorf("after admin set invites = %d, want 7", got)
	}
}
