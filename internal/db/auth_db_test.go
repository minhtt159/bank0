package db

import (
	"context"
	"errors"
	"testing"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// These exercise the auth PL/pgSQL surface directly via raw SQL (sessions and
// refresh tokens in 00004_auth_tokens.sql; change_password in 00003_users.sql).
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

// --- revoke_refresh_token single-session logout -------------------------------

// TestRevokeRefreshTokenSingleSession pins single-session logout: revoking ONE
// token (family) kills exactly that session — a later rotate of the revoked token
// is refused as reuse (28000) — while a sibling family issued to the same user keeps
// rotating normally. This is what makes "log out this device" not "log out
// everywhere".
func TestRevokeRefreshTokenSingleSession(t *testing.T) {
	pg := newTestPG(t)
	ctx := context.Background()
	user := mkCustomer(t, pg)

	hashA, hashB := uniqHex(32), uniqHex(32)
	famA, err := pg.IssueRefreshToken(ctx, user, hashA, 3600, "ua", "1.2.3.4", "laptop")
	if err != nil {
		t.Fatalf("issue family A: %v", err)
	}
	famB, err := pg.IssueRefreshToken(ctx, user, hashB, 3600, "ua", "1.2.3.4", "phone")
	if err != nil {
		t.Fatalf("issue family B: %v", err)
	}
	if famA == famB {
		t.Fatal("two logins must open two distinct families")
	}

	// Single-session logout of family A.
	if err := pg.RevokeRefreshToken(ctx, hashA); err != nil {
		t.Fatalf("revoke_refresh_token: %v", err)
	}

	// A: rotating the revoked token is refused as reuse (28000).
	var u, f uuid.UUID
	err = pg.Pool.QueryRow(ctx,
		`SELECT user_id, family_id FROM rotate_refresh_token($1,$2,$3,$4,$5,$6)`,
		hashA, uniqHex(32), 3600, 86400*30, "ua", "1.2.3.4").Scan(&u, &f)
	if got := sqlstate(err); got != "28000" {
		t.Errorf("rotate of revoked token SQLSTATE = %q, want 28000; err=%v", got, err)
	}

	// B: the sibling family still rotates cleanly.
	if err := pg.Pool.QueryRow(ctx,
		`SELECT user_id, family_id FROM rotate_refresh_token($1,$2,$3,$4,$5,$6)`,
		hashB, uniqHex(32), 3600, 86400*30, "ua", "1.2.3.4").Scan(&u, &f); err != nil {
		t.Fatalf("rotate sibling family B: %v", err)
	}
	if u != user || f != famB {
		t.Errorf("sibling rotate returned user=%s fam=%s, want %s/%s", u, f, user, famB)
	}
}

// --- validate_session lifecycle ----------------------------------------------

// TestValidateSession: a live session validates (returning its user); an expired
// one and a revoked (deleted) one both fail closed with no rows.
func TestValidateSession(t *testing.T) {
	pg := newTestPG(t)
	ctx := context.Background()
	username, id := mkStaff(t, pg, "operator", "operatorpassword")

	// Valid: a fresh session validates and returns its user.
	live := uniqHex(32)
	if _, err := pg.CreateStaffSession(ctx, username, "operatorpassword", live, 900, "ua", "1.2.3.4"); err != nil {
		t.Fatalf("create live session: %v", err)
	}
	if su, ok, err := pg.ValidateSession(ctx, live, 900); err != nil || !ok || su.UserID != id {
		t.Fatalf("validate live session: ok=%v user=%s err=%v, want ok=true user=%s", ok, su.UserID, err, id)
	}

	// Expired: backdate expires_at into the past -> validate returns no rows.
	expired := uniqHex(32)
	if _, err := pg.CreateStaffSession(ctx, username, "operatorpassword", expired, 900, "ua", "1.2.3.4"); err != nil {
		t.Fatalf("create session to expire: %v", err)
	}
	if _, err := pg.Pool.Exec(ctx,
		`UPDATE sessions SET expires_at = now() - interval '1 hour' WHERE id = $1`, expired); err != nil {
		t.Fatalf("backdate session: %v", err)
	}
	if _, ok, err := pg.ValidateSession(ctx, expired, 900); err != nil || ok {
		t.Errorf("expired session ok=%v err=%v, want ok=false", ok, err)
	}

	// Revoked: revoke_session deletes the row -> validate fails closed.
	revoked := uniqHex(32)
	if _, err := pg.CreateStaffSession(ctx, username, "operatorpassword", revoked, 900, "ua", "1.2.3.4"); err != nil {
		t.Fatalf("create session to revoke: %v", err)
	}
	if err := pg.RevokeSession(ctx, revoked); err != nil {
		t.Fatalf("revoke_session: %v", err)
	}
	if _, ok, err := pg.ValidateSession(ctx, revoked, 900); err != nil || ok {
		t.Errorf("revoked session ok=%v err=%v, want ok=false", ok, err)
	}
}

// --- maintenance sweeps -------------------------------------------------------

// TestCleanupSessionsAndRefreshTokens: the maintenance sweeps reap stale rows while
// leaving live ones intact. cleanup_sessions drops expired sessions; cleanup_refresh_tokens
// drops tokens long past their idle expiry (grace: 1 day). A fresh row of each kind
// survives both sweeps.
func TestCleanupSessionsAndRefreshTokens(t *testing.T) {
	pg := newTestPG(t)
	ctx := context.Background()
	user := mkCustomer(t, pg)

	// --- sessions: fresh survives, expired reaped ---
	freshSess := uniqHex(32)
	if _, err := pg.Pool.Exec(ctx,
		`INSERT INTO sessions (id, user_id, expires_at) VALUES ($1, $2, now() + interval '1 hour')`,
		freshSess, user); err != nil {
		t.Fatalf("insert fresh session: %v", err)
	}
	staleSess := uniqHex(32)
	if _, err := pg.Pool.Exec(ctx,
		`INSERT INTO sessions (id, user_id, expires_at) VALUES ($1, $2, now() - interval '1 hour')`,
		staleSess, user); err != nil {
		t.Fatalf("insert stale session: %v", err)
	}
	if _, err := pg.Pool.Exec(ctx, `SELECT cleanup_sessions()`); err != nil {
		t.Fatalf("cleanup_sessions: %v", err)
	}
	if !rowExists(t, pg, `SELECT 1 FROM sessions WHERE id = $1`, freshSess) {
		t.Error("cleanup_sessions reaped a live session")
	}
	if rowExists(t, pg, `SELECT 1 FROM sessions WHERE id = $1`, staleSess) {
		t.Error("cleanup_sessions left an expired session behind")
	}

	// --- refresh tokens: fresh survives, long-expired reaped ---
	freshTok := uniqHex(32)
	if _, err := pg.IssueRefreshToken(ctx, user, freshTok, 3600, "ua", "1.2.3.4", "live"); err != nil {
		t.Fatalf("issue fresh token: %v", err)
	}
	staleTok := uniqHex(32)
	if _, err := pg.IssueRefreshToken(ctx, user, staleTok, 3600, "ua", "1.2.3.4", "stale"); err != nil {
		t.Fatalf("issue stale token: %v", err)
	}
	// Push it past the 1-day idle-expiry grace cleanup_refresh_tokens enforces.
	if _, err := pg.Pool.Exec(ctx,
		`UPDATE refresh_tokens SET expires_at = now() - interval '2 days' WHERE id = $1`, staleTok); err != nil {
		t.Fatalf("backdate token: %v", err)
	}
	if _, err := pg.Pool.Exec(ctx, `SELECT cleanup_refresh_tokens()`); err != nil {
		t.Fatalf("cleanup_refresh_tokens: %v", err)
	}
	if !rowExists(t, pg, `SELECT 1 FROM refresh_tokens WHERE id = $1`, freshTok) {
		t.Error("cleanup_refresh_tokens reaped a live token")
	}
	if rowExists(t, pg, `SELECT 1 FROM refresh_tokens WHERE id = $1`, staleTok) {
		t.Error("cleanup_refresh_tokens left a stale token behind")
	}
}

// --- small local helpers ------------------------------------------------------

// rowExists reports whether the 1-column existence probe returns a row.
func rowExists(t *testing.T, pg *Postgres, query string, args ...any) bool {
	t.Helper()
	var one int
	err := pg.Pool.QueryRow(context.Background(), query, args...).Scan(&one)
	if errors.Is(err, pgx.ErrNoRows) {
		return false
	}
	if err != nil {
		t.Fatalf("rowExists probe: %v", err)
	}
	return true
}

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
