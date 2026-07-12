package api

import (
	"context"
	"net/http"
	"testing"

	"github.com/google/uuid"

	"github.com/minhtt159/bank0/internal/db"
	sqlc "github.com/minhtt159/bank0/internal/db/sqlc"
)

// Invitation-gated registration over HTTP: minting (POST /me/invitations), listing
// (GET /me/invitations), the register-side code errors (unknown/used/expired), and
// the verified-only mint gate. The DB owns the rules (create_invitation /
// register_user); these assert the handler + mapDBError wiring around them.

// registerBody posts /auth/register with a fresh key and returns the response.
func registerBody(t *testing.T, tsURL, uname, email, code string) *http.Response {
	t.Helper()
	return postJSON(t, tsURL+"/auth/register", map[string]string{"Idempotency-Key": uuid.NewString()},
		map[string]string{"username": uname, "password": "correct-horse-battery",
			"full_name": "Inv T", "email": email, "invitation_code": code})
}

// TestHTTPRegisterInvitationCodeErrors covers the register-side code failures the
// mapDBError layer translates: missing (422), unknown (404), already-used (409),
// expired (409).
func TestHTTPRegisterInvitationCodeErrors(t *testing.T) {
	ts, pg, _ := newRegTestServer(t)

	// Missing code -> 422 (handler fast-fail).
	r := postJSON(t, ts.URL+"/auth/register", map[string]string{"Idempotency-Key": uuid.NewString()},
		map[string]string{"username": "im" + uhex(8), "password": "correct-horse-battery", "full_name": "X", "email": "im" + uhex(6) + "@example.com"})
	if r.StatusCode != http.StatusUnprocessableEntity {
		t.Errorf("missing code = %d, want 422", r.StatusCode)
	}

	// Unknown code -> 404 (register_user raises P0001 'not found').
	r = registerBody(t, ts.URL, "iu"+uhex(8), "iu"+uhex(6)+"@example.com", "no-such-code-"+uhex(8))
	if r.StatusCode != http.StatusNotFound {
		t.Errorf("unknown code = %d, want 404: %s", r.StatusCode, body(t, r))
	}

	// Already-used code -> 409. First registration consumes it; a second use is refused.
	used := freshInviteCode(t, pg)
	if r = registerBody(t, ts.URL, "iua"+uhex(8), "iua"+uhex(6)+"@example.com", used); r.StatusCode != http.StatusCreated {
		t.Fatalf("first use of code = %d, want 201: %s", r.StatusCode, body(t, r))
	}
	r = registerBody(t, ts.URL, "iub"+uhex(8), "iub"+uhex(6)+"@example.com", used)
	if r.StatusCode != http.StatusConflict {
		t.Errorf("reused code = %d, want 409", r.StatusCode)
	}

	// Expired code -> 409 (backdate the row past its expiry).
	expd := freshInviteCode(t, pg)
	if _, err := pg.Pool.Exec(context.Background(),
		`UPDATE invitations SET expires_at = now() - interval '1 day' WHERE code = $1`, expd); err != nil {
		t.Fatalf("backdate: %v", err)
	}
	r = registerBody(t, ts.URL, "ie"+uhex(8), "ie"+uhex(6)+"@example.com", expd)
	if r.StatusCode != http.StatusConflict {
		t.Errorf("expired code = %d, want 409", r.StatusCode)
	}
}

// TestHTTPInvitationLifecycle is the full client-surface chain: verified inviter A
// mints a code, B registers with it and verifies, B (now verified) can itself mint,
// A's list reflects consumed + pending statuses, /me exposes invites_remaining, an
// unverified caller is refused (403), and an exhausted budget yields 409.
func TestHTTPInvitationLifecycle(t *testing.T) {
	ts, pg, cap := newRegTestServer(t)

	// A: an active/verified customer (mkUser => active/active) may mint.
	aID, aName := mkUser(t, pg, sqlc.UserRoleCustomer)
	aTok := bearerFor(t, ts.URL, aName, "pw")
	aAuth := map[string]string{"Authorization": "Bearer " + aTok}

	// A mints an invitation -> 201 with the code + decremented remaining.
	r := postJSON(t, ts.URL+"/me/invitations", aAuth, nil)
	if r.StatusCode != http.StatusCreated {
		t.Fatalf("A mint = %d, want 201: %s", r.StatusCode, body(t, r))
	}
	var minted struct {
		Code             string `json:"code"`
		InvitesRemaining int    `json:"invites_remaining"`
	}
	decodeBody(t, r, &minted)
	if minted.Code == "" || minted.InvitesRemaining != 9 {
		t.Fatalf("A mint body = %+v, want a code + remaining 9", minted)
	}

	// A mints a SECOND code, left unused, so the list later shows a pending row too.
	r = postJSON(t, ts.URL+"/me/invitations", aAuth, nil)
	if r.StatusCode != http.StatusCreated {
		t.Fatalf("A second mint = %d, want 201", r.StatusCode)
	}

	// B registers with A's first code, then verifies the emailed code.
	bName := "invb" + uhex(8)
	bEmail := bName + "@example.com"
	rb := registerBody(t, ts.URL, bName, bEmail, minted.Code)
	if rb.StatusCode != http.StatusCreated {
		t.Fatalf("B register = %d, want 201: %s", rb.StatusCode, body(t, rb))
	}
	var breg struct {
		VerifyToken string `json:"verify_token"`
	}
	decodeBody(t, rb, &breg)
	code := cap.last(t)
	rv := postJSON(t, ts.URL+"/auth/verify-contact", nil,
		map[string]string{"verify_token": breg.VerifyToken, "code": code})
	if rv.StatusCode != http.StatusOK {
		t.Fatalf("B verify = %d, want 200: %s", rv.StatusCode, body(t, rv))
	}

	// B logs in (now active/verified) and can itself mint an invitation.
	bTok := bearerFor(t, ts.URL, bName, "correct-horse-battery")
	bAuth := map[string]string{"Authorization": "Bearer " + bTok}
	if r = postJSON(t, ts.URL+"/me/invitations", bAuth, nil); r.StatusCode != http.StatusCreated {
		t.Errorf("B (verified) mint = %d, want 201: %s", r.StatusCode, body(t, r))
	}

	// A's list: newest first, exactly one consumed (used by B) and at least one pending.
	rl := get(t, http.DefaultClient, ts.URL+"/me/invitations", aAuth)
	if rl.StatusCode != http.StatusOK {
		t.Fatalf("A list = %d, want 200", rl.StatusCode)
	}
	var list []struct {
		Code       string  `json:"code"`
		Status     string  `json:"status"`
		ConsumedAt *string `json:"consumed_at"`
	}
	decodeBody(t, rl, &list)
	var consumed, pending int
	for _, iv := range list {
		switch iv.Status {
		case "consumed":
			consumed++
		case "pending":
			pending++
		}
	}
	if consumed != 1 || pending < 1 {
		t.Errorf("A list statuses = {consumed:%d pending:%d}, want 1 consumed + >=1 pending; list=%+v", consumed, pending, list)
	}

	// GET /me exposes the remaining budget (A minted 2 of 10).
	rm := get(t, http.DefaultClient, ts.URL+"/me", aAuth)
	if rm.StatusCode != http.StatusOK {
		t.Fatalf("A /me = %d, want 200", rm.StatusCode)
	}
	var me struct {
		InvitesRemaining int `json:"invites_remaining"`
	}
	decodeBody(t, rm, &me)
	if me.InvitesRemaining != 8 {
		t.Errorf("/me invites_remaining = %d, want 8", me.InvitesRemaining)
	}

	// Unverified caller C -> 403. Synthetic state: keep status active (so login yields
	// a bearer) but onboarding pending_verification (so create_invitation's gate trips).
	cID, cName := mkUser(t, pg, sqlc.UserRoleCustomer)
	if _, err := pg.Pool.Exec(context.Background(),
		`UPDATE users SET onboarding_status = 'pending_verification' WHERE id = $1`, cID); err != nil {
		t.Fatalf("set C pending: %v", err)
	}
	cTok := bearerFor(t, ts.URL, cName, "pw")
	if r = postJSON(t, ts.URL+"/me/invitations", map[string]string{"Authorization": "Bearer " + cTok}, nil); r.StatusCode != http.StatusForbidden {
		t.Errorf("unverified mint = %d, want 403", r.StatusCode)
	}

	// Exhausted budget -> 409 invitation_limit. Pin A's quota to 0 and mint again.
	setInvitesAPI(t, pg, aID, 0)
	r = postJSON(t, ts.URL+"/me/invitations", aAuth, nil)
	if r.StatusCode != http.StatusConflict {
		t.Fatalf("exhausted mint = %d, want 409: %s", r.StatusCode, body(t, r))
	}
	var e struct {
		Error string `json:"error"`
	}
	decodeBody(t, r, &e)
	if e.Error != "invitation_limit" {
		t.Errorf("exhausted mint error code = %q, want invitation_limit", e.Error)
	}
}

// setInvitesAPI pins a user's remaining invitation budget (test knob).
func setInvitesAPI(t *testing.T, pg *db.Postgres, id uuid.UUID, n int32) {
	t.Helper()
	if err := pg.Queries.SetInvitesRemaining(context.Background(),
		sqlc.SetInvitesRemainingParams{ID: id, InvitesRemaining: n}); err != nil {
		t.Fatalf("set invites: %v", err)
	}
}
