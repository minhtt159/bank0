package db

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"strings"
	"testing"

	"github.com/google/uuid"
)

// Self-registration & contact verification (00011): register_user is the one
// atomic signup call (idempotency gate + locked user + first challenge);
// verify_contact consumes the code and unlocks login.

func sha256hex(s string) string {
	h := sha256.Sum256([]byte(s))
	return hex.EncodeToString(h[:])
}

// mintInvite mints one single-use invitation code for an inviter (create_invitation),
// spending one unit of their lifetime quota. The inviter must be active + verified/
// active; mkCustomer users are active/active and qualify.
func mintInvite(t *testing.T, pg *Postgres, inviter uuid.UUID) string {
	t.Helper()
	res, err := pg.CreateInvitation(context.Background(), inviter)
	if err != nil {
		t.Fatalf("create_invitation(%s): %v", inviter, err)
	}
	return res.Code
}

// freshInvite spins up a throwaway inviter and mints one code — the common case for
// tests that just need a valid code to get a registration past the invite gate.
func freshInvite(t *testing.T, pg *Postgres) string {
	t.Helper()
	return mintInvite(t, pg, mkCustomer(t, pg))
}

// register is a test shorthand around Postgres.RegisterUser with fresh token/code.
// The invite code is threaded through so callers control it: a replay MUST reuse the
// same code (it is folded into the request fingerprint) or the DB raises 23514.
func register(t *testing.T, pg *Postgres, key, username string, email *string, phone *string, code string) (RegisterResult, string, string) {
	t.Helper()
	token := "tok-" + uuid.NewString()
	vcode := "123456"
	dest := "x"
	channel := "phone"
	if email != nil {
		dest, channel = *email, "email"
	} else if phone != nil {
		dest = *phone
	}
	res, err := pg.RegisterUser(context.Background(), RegisterParams{
		IdempotencyKey: key,
		Username:       username,
		Password:       "correct-horse-battery",
		FullName:       "Reg Tester",
		Email:          email,
		PhoneNumber:    phone,
		Channel:        channel,
		Destination:    dest,
		TokenHash:      sha256hex(token),
		CodeHash:       sha256hex(vcode),
		VerifyToken:    token,
		InvitationCode: code,
	})
	if err != nil {
		t.Fatalf("register_user: %v", err)
	}
	return res, token, vcode
}

func TestRegisterCreatesLockedPendingUser(t *testing.T) {
	pg := newTestPG(t)
	ctx := context.Background()
	email := "reg-" + uuid.NewString()[:8] + "@example.com"
	uname := "reg" + uuid.NewString()[:8]
	res, _, _ := register(t, pg, uuid.NewString(), uname, &email, nil, freshInvite(t, pg))
	if res.WasReplay {
		t.Fatal("fresh key must not be a replay")
	}

	var status, onboarding string
	if err := pg.Pool.QueryRow(ctx,
		`SELECT status, onboarding_status FROM users WHERE id = $1`, res.UserID).
		Scan(&status, &onboarding); err != nil {
		t.Fatalf("read user: %v", err)
	}
	if status != "locked" || onboarding != "pending_verification" {
		t.Errorf("user = %s/%s, want locked/pending_verification", status, onboarding)
	}

	// A pending challenge exists for the email channel.
	var n int
	if err := pg.Pool.QueryRow(ctx,
		`SELECT count(*) FROM verification_challenges
		  WHERE user_id = $1 AND channel = 'email' AND status = 'pending'`, res.UserID).Scan(&n); err != nil {
		t.Fatalf("count challenges: %v", err)
	}
	if n != 1 {
		t.Errorf("pending challenges = %d, want 1", n)
	}

	// The locked user cannot log in yet (right creds, wrong lifecycle state).
	if _, _, _, ok, err := pg.Login(ctx, uname, "correct-horse-battery"); err != nil || ok {
		t.Errorf("login before verify: ok=%v err=%v, want denied", ok, err)
	}
}

func TestRegisterValidationAndDuplicates(t *testing.T) {
	pg := newTestPG(t)
	ctx := context.Background()

	// Neither email nor phone -> check_violation.
	_, err := pg.RegisterUser(ctx, RegisterParams{
		IdempotencyKey: uuid.NewString(), Username: "nochan" + uuid.NewString()[:8],
		Password: "correct-horse-battery", FullName: "X",
		Channel: "email", Destination: "x", TokenHash: sha256hex("t1" + uuid.NewString()), CodeHash: sha256hex("c"), VerifyToken: "t",
	})
	if got := sqlstate(err); got != "23514" {
		t.Errorf("no-channel SQLSTATE = %q, want 23514", got)
	}

	// Short password -> check_violation (policy matches change_password: >= 12).
	email := "pw-" + uuid.NewString()[:8] + "@example.com"
	_, err = pg.RegisterUser(ctx, RegisterParams{
		IdempotencyKey: uuid.NewString(), Username: "pwshort" + uuid.NewString()[:8],
		Password: "short", FullName: "X", Email: &email,
		Channel: "email", Destination: email, TokenHash: sha256hex("t2" + uuid.NewString()), CodeHash: sha256hex("c"), VerifyToken: "t",
	})
	if got := sqlstate(err); got != "23514" {
		t.Errorf("weak-password SQLSTATE = %q, want 23514", got)
	}

	// Duplicate username -> unique_violation. A distinct, unconsumed code is minted
	// for the second attempt so it clears the invite gate and reaches the INSERT.
	uname := "dup" + uuid.NewString()[:8]
	e1 := "d1-" + uuid.NewString()[:8] + "@example.com"
	register(t, pg, uuid.NewString(), uname, &e1, nil, freshInvite(t, pg))
	e2 := "d2-" + uuid.NewString()[:8] + "@example.com"
	_, err = pg.RegisterUser(ctx, RegisterParams{
		IdempotencyKey: uuid.NewString(), Username: uname,
		Password: "correct-horse-battery", FullName: "X", Email: &e2,
		Channel: "email", Destination: e2, TokenHash: sha256hex("t3" + uuid.NewString()), CodeHash: sha256hex("c"), VerifyToken: "t",
		InvitationCode: freshInvite(t, pg),
	})
	if got := sqlstate(err); got != "23505" {
		t.Errorf("duplicate-username SQLSTATE = %q, want 23505", got)
	}
}

func TestRegisterIdempotentReplay(t *testing.T) {
	pg := newTestPG(t)
	ctx := context.Background()
	key := uuid.NewString()
	email := "rp-" + uuid.NewString()[:8] + "@example.com"
	uname := "rp" + uuid.NewString()[:8]
	code := freshInvite(t, pg) // one code; the replay MUST reuse it (it's in the fingerprint)

	first, token, _ := register(t, pg, key, uname, &email, nil, code)

	// Same key, same params (incl. same code) -> replay: same user, stored response echoed.
	replay, _, _ := register(t, pg, key, uname, &email, nil, code)
	if !replay.WasReplay {
		t.Fatal("second call with the same key must be a replay")
	}
	if replay.UserID != first.UserID {
		t.Errorf("replay user = %s, want %s", replay.UserID, first.UserID)
	}
	if string(replay.Response) == "" || string(replay.Response) != string(first.Response) {
		t.Errorf("replay must return the stored response verbatim")
	}
	// The stored response still carries the original verify_token.
	if !strings.Contains(string(replay.Response), token) {
		t.Error("replay response lacks the original verify_token")
	}

	var n int
	if err := pg.Pool.QueryRow(ctx, `SELECT count(*) FROM users WHERE username = $1::citext`, uname).Scan(&n); err != nil {
		t.Fatalf("count users: %v", err)
	}
	if n != 1 {
		t.Errorf("users with name = %d, want 1 (no duplicate)", n)
	}

	// Same key, DIFFERENT params (username) -> fingerprint mismatch (23514).
	other := "other" + uuid.NewString()[:8]
	_, err := pg.RegisterUser(ctx, RegisterParams{
		IdempotencyKey: key, Username: other,
		Password: "correct-horse-battery", FullName: "X", Email: &email,
		Channel: "email", Destination: email, TokenHash: sha256hex("t4" + uuid.NewString()), CodeHash: sha256hex("c"), VerifyToken: "t",
		InvitationCode: code,
	})
	if got := sqlstate(err); got != "23514" {
		t.Errorf("fingerprint-mismatch SQLSTATE = %q, want 23514", got)
	}
}

func TestVerifyContactHappyPathUnlocksLogin(t *testing.T) {
	pg := newTestPG(t)
	ctx := context.Background()
	email := "vc-" + uuid.NewString()[:8] + "@example.com"
	uname := "vc" + uuid.NewString()[:8]
	res, token, code := register(t, pg, uuid.NewString(), uname, &email, nil, freshInvite(t, pg))

	v, err := pg.VerifyContact(ctx, sha256hex(token), sha256hex(code))
	if err != nil {
		t.Fatalf("verify_contact: %v", err)
	}
	if v.UserID != res.UserID || !v.LoginReady || v.OnboardingStatus != "verified" || v.Channel != "email" {
		t.Errorf("verify = %+v, want user %s verified/email/login_ready", v, res.UserID)
	}

	var status string
	var emailVerified *string
	if err := pg.Pool.QueryRow(ctx,
		`SELECT status, email_verified_at::text FROM users WHERE id = $1`, res.UserID).
		Scan(&status, &emailVerified); err != nil {
		t.Fatalf("read user: %v", err)
	}
	if status != "active" || emailVerified == nil {
		t.Errorf("after verify: status=%s email_verified_at=%v, want active + stamped", status, emailVerified)
	}

	// Login now succeeds.
	if _, _, _, ok, err := pg.Login(ctx, uname, "correct-horse-battery"); err != nil || !ok {
		t.Errorf("login after verify: ok=%v err=%v, want ok", ok, err)
	}

	// The consumed challenge cannot be replayed.
	if _, err := pg.VerifyContact(ctx, sha256hex(token), sha256hex(code)); sqlstate(err) != "28000" {
		t.Errorf("re-verify SQLSTATE = %q, want 28000", sqlstate(err))
	}
}

func TestVerifyContactWrongCodeLockoutAndUnknownToken(t *testing.T) {
	pg := newTestPG(t)
	ctx := context.Background()
	email := "wc-" + uuid.NewString()[:8] + "@example.com"
	_, token, code := register(t, pg, uuid.NewString(), "wc"+uuid.NewString()[:8], &email, nil, freshInvite(t, pg))

	// Unknown token -> P0001 (handler maps 'not found' -> 404).
	if _, err := pg.VerifyContact(ctx, sha256hex("no-such-token"), sha256hex(code)); sqlstate(err) != "P0001" {
		t.Errorf("unknown-token SQLSTATE = %q, want P0001", sqlstate(err))
	}

	// 5 wrong codes -> 28000 each; the 6th attempt (even with the right code) -> 23514.
	for i := 0; i < 5; i++ {
		if _, err := pg.VerifyContact(ctx, sha256hex(token), sha256hex("000000")); sqlstate(err) != "28000" {
			t.Fatalf("wrong code #%d SQLSTATE = %q, want 28000", i+1, sqlstate(err))
		}
	}
	if _, err := pg.VerifyContact(ctx, sha256hex(token), sha256hex(code)); sqlstate(err) != "23514" {
		t.Errorf("post-lockout SQLSTATE = %q, want 23514", sqlstate(err))
	}
}

func TestVerificationCooldownAndExpiry(t *testing.T) {
	pg := newTestPG(t)
	ctx := context.Background()
	email := "cd-" + uuid.NewString()[:8] + "@example.com"
	res, token, _ := register(t, pg, uuid.NewString(), "cd"+uuid.NewString()[:8], &email, nil, freshInvite(t, pg))

	// Resend within the 60s cooldown -> 53400.
	_, err := pg.Pool.Exec(ctx,
		`SELECT create_verification_challenge($1, 'email', $2, $3, $4)`,
		res.UserID, email, sha256hex(token), sha256hex("111111"))
	if got := sqlstate(err); got != "53400" {
		t.Errorf("cooldown SQLSTATE = %q, want 53400", got)
	}

	// With cooldown waived (0s): the pending row is refreshed in place — same
	// token still works, attempts reset, new code takes effect.
	newCode := "654321"
	var chID uuid.UUID
	if err := pg.Pool.QueryRow(ctx,
		`SELECT create_verification_challenge($1, 'email', $2, $3, $4, interval '0 seconds')`,
		res.UserID, email, sha256hex(token), sha256hex(newCode)).Scan(&chID); err != nil {
		t.Fatalf("refresh challenge: %v", err)
	}
	v, err := pg.VerifyContact(ctx, sha256hex(token), sha256hex(newCode))
	if err != nil || !v.LoginReady {
		t.Fatalf("verify with refreshed code: %+v err=%v", v, err)
	}

	// Expiry sweep: a backdated pending challenge flips to 'expired'.
	email2 := "ex-" + uuid.NewString()[:8] + "@example.com"
	res2, token2, code2 := register(t, pg, uuid.NewString(), "ex"+uuid.NewString()[:8], &email2, nil, freshInvite(t, pg))
	if _, err := pg.Pool.Exec(ctx,
		`UPDATE verification_challenges SET expires_at = now() - interval '1 minute'
		  WHERE user_id = $1 AND status = 'pending'`, res2.UserID); err != nil {
		t.Fatalf("backdate: %v", err)
	}
	var swept int
	if err := pg.Pool.QueryRow(ctx, `SELECT expire_verification_challenges()`).Scan(&swept); err != nil {
		t.Fatalf("sweep: %v", err)
	}
	if swept < 1 {
		t.Errorf("sweep = %d, want >= 1", swept)
	}
	if _, err := pg.VerifyContact(ctx, sha256hex(token2), sha256hex(code2)); sqlstate(err) != "28000" {
		t.Errorf("expired-challenge SQLSTATE = %q, want 28000", sqlstate(err))
	}
}
