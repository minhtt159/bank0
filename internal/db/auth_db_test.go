package db

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// These exercise the auth PL/pgSQL surface directly via raw SQL:
//   sessions          (00012_sessions.sql)
//   refresh tokens    (00017_refresh_tokens.sql, 00021_session_device_label.sql)
//   change_password   (00018_change_password.sql)
// All DB functions are invoked with SELECT through pg.Pool; SQLSTATEs raised by
// the functions are asserted with sqlstate() so the test pins the exact contract
// the Go layer maps to HTTP (28P01/28000/42501/check_violation=23514).

// mkStaff creates an active staff user with the given role and a known password,
// returning the username (CITEXT) used to log in and the user id. create_user's
// role param is the 6th positional arg, so all six are passed explicitly.
func mkStaff(t *testing.T, pg *Postgres, role, password string) (username string, id uuid.UUID) {
	t.Helper()
	username = "s" + uniqHex(16)
	if err := pg.Pool.QueryRow(context.Background(),
		`SELECT create_user($1,$2,$3,$4,$5,$6)`,
		username, password, "Staff User", nil, nil, role,
	).Scan(&id); err != nil {
		t.Fatalf("create staff user (role=%s): %v", role, err)
	}
	return username, id
}

// --- create_staff_session role/status/credential gate -------------------------

func TestCreateStaffSessionRejectsCustomer(t *testing.T) {
	pg := newTestPG(t)
	ctx := context.Background()

	// mkCustomer makes an active 'customer' with password "pw"; credentials and
	// status pass, so the role check is what trips: 42501 (not staff).
	cust := mkCustomer(t, pg)
	username := usernameOf(t, pg, cust)

	var uid uuid.UUID
	var uname string
	var role string
	err := pg.Pool.QueryRow(ctx,
		`SELECT user_id, username, role FROM create_staff_session($1,$2,$3,$4,$5,$6)`,
		username, "pw", uniqHex(32), 900, "ua", "1.2.3.4",
	).Scan(&uid, &uname, &role)
	if err == nil {
		t.Fatal("customer must not be allowed a staff session")
	}
	if got := sqlstate(err); got != "42501" {
		t.Fatalf("customer staff session SQLSTATE = %q, want 42501", got)
	}
}

func TestCreateStaffSessionSuccess(t *testing.T) {
	pg := newTestPG(t)
	ctx := context.Background()

	username, id := mkStaff(t, pg, "operator", "operatorpassword")

	var uid uuid.UUID
	var uname string
	var role string
	if err := pg.Pool.QueryRow(ctx,
		`SELECT user_id, username, role FROM create_staff_session($1,$2,$3,$4,$5,$6)`,
		username, "operatorpassword", uniqHex(32), 900, "ua", "1.2.3.4",
	).Scan(&uid, &uname, &role); err != nil {
		t.Fatalf("create_staff_session for operator: %v", err)
	}
	if uid != id {
		t.Errorf("session user_id = %s, want %s", uid, id)
	}
	if role != "operator" {
		t.Errorf("session role = %q, want operator", role)
	}
}

func TestCreateStaffSessionWrongPassword(t *testing.T) {
	pg := newTestPG(t)
	ctx := context.Background()

	username, _ := mkStaff(t, pg, "operator", "correcthorsebattery")

	var uid uuid.UUID
	var uname, role string
	err := pg.Pool.QueryRow(ctx,
		`SELECT user_id, username, role FROM create_staff_session($1,$2,$3,$4,$5,$6)`,
		username, "wrongpassword", uniqHex(32), 900, "ua", "1.2.3.4",
	).Scan(&uid, &uname, &role)
	if err == nil {
		t.Fatal("wrong password must be rejected")
	}
	if got := sqlstate(err); got != "28P01" {
		t.Fatalf("wrong-password SQLSTATE = %q, want 28P01", got)
	}
}

func TestCreateStaffSessionInactiveRejected(t *testing.T) {
	pg := newTestPG(t)
	ctx := context.Background()

	username, id := mkStaff(t, pg, "operator", "operatorpassword")
	// Flip to an inactive status from the user_status enum (active|locked|closed).
	if _, err := pg.Pool.Exec(ctx, `UPDATE users SET status = 'locked' WHERE id = $1`, id); err != nil {
		t.Fatalf("lock user: %v", err)
	}

	var uid uuid.UUID
	var uname, role string
	err := pg.Pool.QueryRow(ctx,
		`SELECT user_id, username, role FROM create_staff_session($1,$2,$3,$4,$5,$6)`,
		username, "operatorpassword", uniqHex(32), 900, "ua", "1.2.3.4",
	).Scan(&uid, &uname, &role)
	if err == nil {
		t.Fatal("inactive staff user must be rejected")
	}
	if got := sqlstate(err); got != "28000" {
		t.Fatalf("inactive staff SQLSTATE = %q, want 28000", got)
	}
}

// --- refresh-token rotation + reuse ------------------------------------------

func TestRotateRefreshTokenAndReuseDetection(t *testing.T) {
	pg := newTestPG(t)
	ctx := context.Background()

	user := mkCustomer(t, pg)
	t0 := uniqHex(32) // family-opening token hash
	var fam uuid.UUID
	if err := pg.Pool.QueryRow(ctx,
		`SELECT issue_refresh_token($1,$2,$3,$4,$5,$6)`,
		user, t0, 3600, "ua", "1.2.3.4", "iPhone",
	).Scan(&fam); err != nil {
		t.Fatalf("issue_refresh_token: %v", err)
	}

	// Rotate the live token -> a child in the same family.
	t1 := uniqHex(32)
	var ruser, rfam uuid.UUID
	if err := pg.Pool.QueryRow(ctx,
		`SELECT user_id, family_id FROM rotate_refresh_token($1,$2,$3,$4,$5,$6)`,
		t0, t1, 3600, 86400*30, "ua", "1.2.3.4",
	).Scan(&ruser, &rfam); err != nil {
		t.Fatalf("rotate live token: %v", err)
	}
	if ruser != user || rfam != fam {
		t.Errorf("rotate returned user=%s fam=%s, want user=%s fam=%s", ruser, rfam, user, fam)
	}

	// Rotating the SAME (now-rotated) token again is reuse -> 28000.
	t2 := uniqHex(32)
	var u2, f2 uuid.UUID
	err := pg.Pool.QueryRow(ctx,
		`SELECT user_id, family_id FROM rotate_refresh_token($1,$2,$3,$4,$5,$6)`,
		t0, t2, 3600, 86400*30, "ua", "1.2.3.4",
	).Scan(&u2, &f2)
	if err == nil {
		t.Fatal("reusing a rotated token must be rejected")
	}
	if got := sqlstate(err); got != "28000" {
		t.Fatalf("reuse SQLSTATE = %q, want 28000", got)
	}

	// The RAISE rolled back its own work: the live child (t1) is still un-revoked
	// (the function does NOT auto-revoke the family; the app does that separately).
	var revoked *string // revoked_at as text, NULL while live
	if err := pg.Pool.QueryRow(ctx,
		`SELECT revoked_at::text FROM refresh_tokens WHERE id = $1`, t1,
	).Scan(&revoked); err != nil {
		t.Fatalf("inspect child token: %v", err)
	}
	if revoked != nil {
		t.Errorf("child token revoked_at = %v after reuse RAISE, want NULL (not auto-revoked)", *revoked)
	}

	// The app's separate revoke step then succeeds and revokes the live family.
	if _, err := pg.Pool.Exec(ctx, `SELECT revoke_refresh_family($1)`, t1); err != nil {
		t.Fatalf("revoke_refresh_family: %v", err)
	}
	if err := pg.Pool.QueryRow(ctx,
		`SELECT revoked_at::text FROM refresh_tokens WHERE id = $1`, t1,
	).Scan(&revoked); err != nil {
		t.Fatalf("re-inspect child token: %v", err)
	}
	if revoked == nil {
		t.Error("child token should be revoked after revoke_refresh_family")
	}
}

func TestRotateRefreshTokenAbsoluteCap(t *testing.T) {
	pg := newTestPG(t)
	ctx := context.Background()

	user := mkCustomer(t, pg)
	t0 := uniqHex(32)
	var fam uuid.UUID
	if err := pg.Pool.QueryRow(ctx,
		`SELECT issue_refresh_token($1,$2,$3,$4,$5,$6)`,
		user, t0, 3600, "ua", "1.2.3.4", "iPhone",
	).Scan(&fam); err != nil {
		t.Fatalf("issue_refresh_token: %v", err)
	}

	// Idle window is wide (3600s) and the token is live & un-rotated, so the only
	// trip is the absolute family cap: with a negative absolute budget,
	// now() > family_start + interval(neg) is always true -> 28P01.
	t1 := uniqHex(32)
	var u, f uuid.UUID
	err := pg.Pool.QueryRow(ctx,
		`SELECT user_id, family_id FROM rotate_refresh_token($1,$2,$3,$4,$5,$6)`,
		t0, t1, 3600, -1, "ua", "1.2.3.4",
	).Scan(&u, &f)
	if err == nil {
		t.Fatal("rotation past the absolute lifetime must be rejected")
	}
	if got := sqlstate(err); got != "28P01" {
		t.Fatalf("absolute-cap SQLSTATE = %q, want 28P01", got)
	}
}

// --- family ownership scoping -------------------------------------------------

func TestRevokeRefreshFamilyScopedOwnership(t *testing.T) {
	pg := newTestPG(t)
	ctx := context.Background()

	userA := mkCustomer(t, pg)
	userB := mkCustomer(t, pg)

	tokA := uniqHex(32)
	var famF uuid.UUID
	if err := pg.Pool.QueryRow(ctx,
		`SELECT issue_refresh_token($1,$2,$3,$4,$5,$6)`,
		userA, tokA, 3600, "ua", "1.2.3.4", "laptop",
	).Scan(&famF); err != nil {
		t.Fatalf("issue family F for A: %v", err)
	}

	// B does not own family F: revokes 0.
	var n int
	if err := pg.Pool.QueryRow(ctx,
		`SELECT revoke_refresh_family_scoped($1,$2)`, userB, famF,
	).Scan(&n); err != nil {
		t.Fatalf("scoped revoke (B): %v", err)
	}
	if n != 0 {
		t.Errorf("revoke by non-owner B revoked %d tokens, want 0", n)
	}

	// A owns family F: revokes > 0.
	if err := pg.Pool.QueryRow(ctx,
		`SELECT revoke_refresh_family_scoped($1,$2)`, userA, famF,
	).Scan(&n); err != nil {
		t.Fatalf("scoped revoke (A): %v", err)
	}
	if n <= 0 {
		t.Errorf("revoke by owner A revoked %d tokens, want > 0", n)
	}
}

// --- list_user_sessions lifecycle --------------------------------------------

func TestListUserSessionsLifecycle(t *testing.T) {
	pg := newTestPG(t)
	ctx := context.Background()

	user := mkCustomer(t, pg)
	t0 := uniqHex(32)
	var fam uuid.UUID
	if err := pg.Pool.QueryRow(ctx,
		`SELECT issue_refresh_token($1,$2,$3,$4,$5,$6)`,
		user, t0, 3600, "Mozilla/5.0", "1.2.3.4", "Pixel 8",
	).Scan(&fam); err != nil {
		t.Fatalf("issue_refresh_token: %v", err)
	}

	if got := countSessions(t, pg, user); got != 1 {
		t.Fatalf("after issue, active families = %d, want 1", got)
	}

	// Verify the label/family surfaced by the listing.
	var gotFam uuid.UUID
	var label *string
	if err := pg.Pool.QueryRow(ctx,
		`SELECT family_id, device_label FROM list_user_sessions($1)`, user,
	).Scan(&gotFam, &label); err != nil {
		t.Fatalf("list_user_sessions row: %v", err)
	}
	if gotFam != fam {
		t.Errorf("listed family = %s, want %s", gotFam, fam)
	}
	if label == nil || *label != "Pixel 8" {
		t.Errorf("listed device_label = %v, want \"Pixel 8\"", label)
	}

	// Rotating the tip keeps exactly one active family (chain advances, not splits).
	t1 := uniqHex(32)
	var u, f uuid.UUID
	if err := pg.Pool.QueryRow(ctx,
		`SELECT user_id, family_id FROM rotate_refresh_token($1,$2,$3,$4,$5,$6)`,
		t0, t1, 3600, 86400*30, "Mozilla/5.0", "1.2.3.4",
	).Scan(&u, &f); err != nil {
		t.Fatalf("rotate tip: %v", err)
	}
	if got := countSessions(t, pg, user); got != 1 {
		t.Fatalf("after rotate, active families = %d, want 1", got)
	}

	// Revoking the family removes it from the active listing.
	if _, err := pg.Pool.Exec(ctx, `SELECT revoke_refresh_family_scoped($1,$2)`, user, fam); err != nil {
		t.Fatalf("revoke family: %v", err)
	}
	if got := countSessions(t, pg, user); got != 0 {
		t.Fatalf("after revoke, active families = %d, want 0", got)
	}
}

// --- change_password policy ---------------------------------------------------

func TestChangePasswordPolicy(t *testing.T) {
	pg := newTestPG(t)
	ctx := context.Background()

	const current = "currentpassword123" // 18 chars, valid baseline
	username := "c" + uniqHex(16)
	var id uuid.UUID
	if err := pg.Pool.QueryRow(ctx,
		`SELECT create_user($1,$2,$3,$4,$5,$6)`,
		username, current, "Pw User", nil, nil, "customer",
	).Scan(&id); err != nil {
		t.Fatalf("create user: %v", err)
	}

	// New password too short (<12) -> check_violation (23514).
	err := changePassword(ctx, pg, id, current, "short")
	if err == nil {
		t.Fatal("too-short new password must be rejected")
	}
	if got := sqlstate(err); got != "23514" {
		t.Fatalf("too-short SQLSTATE = %q, want 23514 (check_violation)", got)
	}

	// New password identical to current -> check_violation (23514).
	err = changePassword(ctx, pg, id, current, current)
	if err == nil {
		t.Fatal("unchanged new password must be rejected")
	}
	if got := sqlstate(err); got != "23514" {
		t.Fatalf("same-password SQLSTATE = %q, want 23514 (check_violation)", got)
	}

	// Wrong current password -> 28P01.
	err = changePassword(ctx, pg, id, "notthecurrentone", "brandnewpassword12345")
	if err == nil {
		t.Fatal("wrong current password must be rejected")
	}
	if got := sqlstate(err); got != "28P01" {
		t.Fatalf("wrong-current SQLSTATE = %q, want 28P01", got)
	}

	// Valid 12+ char, different new password -> succeeds.
	const next = "brandnewpassword12345"
	if err := changePassword(ctx, pg, id, current, next); err != nil {
		t.Fatalf("valid password change: %v", err)
	}
	// And it actually took effect: the new password verifies, the old does not.
	if !credsValid(t, pg, username, next) {
		t.Error("new password should verify after change")
	}
	if credsValid(t, pg, username, current) {
		t.Error("old password should no longer verify after change")
	}
}

// --- small local helpers ------------------------------------------------------

func usernameOf(t *testing.T, pg *Postgres, id uuid.UUID) string {
	t.Helper()
	var u string
	if err := pg.Pool.QueryRow(context.Background(),
		`SELECT username::text FROM users WHERE id = $1`, id).Scan(&u); err != nil {
		t.Fatalf("lookup username: %v", err)
	}
	return u
}

func countSessions(t *testing.T, pg *Postgres, user uuid.UUID) int {
	t.Helper()
	var n int
	if err := pg.Pool.QueryRow(context.Background(),
		`SELECT count(*) FROM list_user_sessions($1)`, user).Scan(&n); err != nil {
		t.Fatalf("count list_user_sessions: %v", err)
	}
	return n
}

func changePassword(ctx context.Context, pg *Postgres, id uuid.UUID, current, next string) error {
	_, err := pg.Pool.Exec(ctx, `SELECT change_password($1,$2,$3)`, id, current, next)
	return err
}

func credsValid(t *testing.T, pg *Postgres, username, password string) bool {
	t.Helper()
	var id uuid.UUID
	err := pg.Pool.QueryRow(context.Background(),
		`SELECT user_id FROM check_user_credentials($1,$2)`, username, password).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		return false
	}
	if err != nil {
		t.Fatalf("check_user_credentials: %v", err)
	}
	return true
}
