package db

import (
	"context"
	"strings"
	"sync"
	"testing"

	"github.com/google/uuid"

	sqlc "github.com/minhtt159/bank0/internal/db/sqlc"
)

// Invitation-gated registration (00003): create_invitation mints a single-use code
// against a verified customer's lifetime quota; register_user consumes it. These
// exercise the PL/pgSQL directly — quota accounting, the verified gate, single-use
// under a race, and the idempotency-namespace squat fix.

// setInvites pins a user's remaining quota (test knob to reach exhaustion cheaply).
func setInvites(t *testing.T, pg *Postgres, id uuid.UUID, n int32) {
	t.Helper()
	if err := pg.Queries.SetInvitesRemaining(context.Background(),
		sqlc.SetInvitesRemainingParams{ID: id, InvitesRemaining: n}); err != nil {
		t.Fatalf("set invites: %v", err)
	}
}

func invitesRemaining(t *testing.T, pg *Postgres, id uuid.UUID) int {
	t.Helper()
	var n int
	if err := pg.Pool.QueryRow(context.Background(),
		`SELECT invites_remaining FROM users WHERE id = $1`, id).Scan(&n); err != nil {
		t.Fatalf("read invites_remaining: %v", err)
	}
	return n
}

// TestInvitationQuotaDecrementsAndExhausts: minting spends one lifetime unit
// (default 10 -> 9), and a mint at 0 is refused with 23514 'invitation limit'.
func TestInvitationQuotaDecrementsAndExhausts(t *testing.T) {
	pg := newTestPG(t)
	ctx := context.Background()
	inviter := mkCustomer(t, pg)

	// Default budget is 10; mkCustomer users are active/active so they may invite.
	if got := invitesRemaining(t, pg, inviter); got != 10 {
		t.Fatalf("fresh customer invites_remaining = %d, want 10", got)
	}
	res, err := pg.CreateInvitation(ctx, inviter)
	if err != nil {
		t.Fatalf("create_invitation: %v", err)
	}
	if res.InvitesRemaining != 9 {
		t.Errorf("returned invites_remaining = %d, want 9", res.InvitesRemaining)
	}
	if got := invitesRemaining(t, pg, inviter); got != 9 {
		t.Errorf("stored invites_remaining = %d, want 9 (decremented)", got)
	}
	if res.Code == "" {
		t.Error("mint returned an empty code")
	}

	// Drain to zero, then the next mint hits the quota check (not the CHECK constraint).
	setInvites(t, pg, inviter, 0)
	_, err = pg.CreateInvitation(ctx, inviter)
	if sqlstate(err) != "23514" || !strings.Contains(err.Error(), "invitation limit") {
		t.Errorf("exhausted mint err = %v (sqlstate %q), want 23514 'invitation limit'", err, sqlstate(err))
	}
	if got := invitesRemaining(t, pg, inviter); got != 0 {
		t.Errorf("after refused mint invites_remaining = %d, want 0 (unchanged)", got)
	}
}

// TestInvitationVerifiedGate: a user still in pending_verification cannot mint
// (42501); once verify_contact promotes them to verified/active, minting works.
func TestInvitationVerifiedGate(t *testing.T) {
	pg := newTestPG(t)
	ctx := context.Background()
	email := "vg-" + uuid.NewString()[:8] + "@example.com"
	uname := "vg" + uuid.NewString()[:8]

	// register_user leaves the account locked + pending_verification.
	res, token, code := register(t, pg, uuid.NewString(), uname, &email, nil, freshInvite(t, pg))

	if _, err := pg.CreateInvitation(ctx, res.UserID); sqlstate(err) != "42501" {
		t.Errorf("unverified mint sqlstate = %q, want 42501", sqlstate(err))
	}

	// Verify the contact -> onboarding 'verified', status 'active'.
	if _, err := pg.VerifyContact(ctx, sha256hex(token), sha256hex(code)); err != nil {
		t.Fatalf("verify_contact: %v", err)
	}
	inv, err := pg.CreateInvitation(ctx, res.UserID)
	if err != nil {
		t.Fatalf("mint after verify: %v", err)
	}
	if inv.Code == "" || inv.InvitesRemaining != 9 {
		t.Errorf("post-verify mint = %+v, want a code + remaining 9", inv)
	}
}

// TestInvitationExpiryDoesNotRefund: an expired, unused code cannot register
// (23514 'expired') AND the inviter's decremented quota is NOT restored (lifetime
// budget semantics).
func TestInvitationExpiryDoesNotRefund(t *testing.T) {
	pg := newTestPG(t)
	ctx := context.Background()
	inviter := mkCustomer(t, pg)
	code := mintInvite(t, pg, inviter) // 10 -> 9
	if got := invitesRemaining(t, pg, inviter); got != 9 {
		t.Fatalf("after mint invites_remaining = %d, want 9", got)
	}

	// Backdate the code past its expiry.
	if _, err := pg.Pool.Exec(ctx,
		`UPDATE invitations SET expires_at = now() - interval '1 day' WHERE code = $1`, code); err != nil {
		t.Fatalf("backdate: %v", err)
	}

	email := "exp-" + uuid.NewString()[:8] + "@example.com"
	_, err := pg.RegisterUser(ctx, RegisterParams{
		IdempotencyKey: uuid.NewString(), Username: "exp" + uuid.NewString()[:8],
		Password: "correct-horse-battery", FullName: "X", Email: &email,
		Channel: "email", Destination: email, TokenHash: sha256hex("t" + uuid.NewString()),
		CodeHash: sha256hex("c"), VerifyToken: "t", InvitationCode: code,
	})
	if sqlstate(err) != "23514" || !strings.Contains(err.Error(), "expired") {
		t.Errorf("expired-code register err = %v (sqlstate %q), want 23514 'expired'", err, sqlstate(err))
	}
	// Lifetime budget: an expired code never refunds the mint decrement.
	if got := invitesRemaining(t, pg, inviter); got != 9 {
		t.Errorf("after expired code invites_remaining = %d, want 9 (no refund)", got)
	}
}

// TestInvitationSingleUseRace: two concurrent fresh registrations presenting the
// SAME code race on the FOR UPDATE lock — exactly one consumes it; the other sees
// it consumed and is refused with 23514 'already used'.
func TestInvitationSingleUseRace(t *testing.T) {
	pg := newTestPG(t)
	code := mintInvite(t, pg, mkCustomer(t, pg))

	const n = 2
	var wg sync.WaitGroup
	var mu sync.Mutex
	var ok, alreadyUsed, other int

	for i := 0; i < n; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			email := "race-" + uuid.NewString()[:8] + "@example.com"
			token := "tok-" + uuid.NewString()
			_, err := pg.RegisterUser(context.Background(), RegisterParams{
				IdempotencyKey: uuid.NewString(), // distinct keys: both take the fresh path
				Username:       "race" + uuid.NewString()[:8],
				Password:       "correct-horse-battery", FullName: "Race", Email: &email,
				Channel: "email", Destination: email, TokenHash: sha256hex(token),
				CodeHash: sha256hex("123456"), VerifyToken: token, InvitationCode: code,
			})
			mu.Lock()
			defer mu.Unlock()
			switch {
			case err == nil:
				ok++
			case sqlstate(err) == "23514" && strings.Contains(err.Error(), "already used"):
				alreadyUsed++
			default:
				other++
				t.Errorf("unexpected race error: %v (sqlstate %q)", err, sqlstate(err))
			}
		}(i)
	}
	wg.Wait()

	if ok != 1 || alreadyUsed != 1 || other != 0 {
		t.Errorf("single-use race split = {ok:%d already_used:%d other:%d}, want {1,1,0}", ok, alreadyUsed, other)
	}
}

// TestRegisterConsumesInvitation: a successful registration burns the code
// (consumed_at + invitee_id set) and the NEW user starts with the default budget.
func TestRegisterConsumesInvitation(t *testing.T) {
	pg := newTestPG(t)
	ctx := context.Background()
	inviter := mkCustomer(t, pg)
	code := mintInvite(t, pg, inviter)

	email := "cons-" + uuid.NewString()[:8] + "@example.com"
	res, _, _ := register(t, pg, uuid.NewString(), "cons"+uuid.NewString()[:8], &email, nil, code)

	var consumedAt *string
	var invitee *uuid.UUID
	if err := pg.Pool.QueryRow(ctx,
		`SELECT consumed_at::text, invitee_id FROM invitations WHERE code = $1`, code).
		Scan(&consumedAt, &invitee); err != nil {
		t.Fatalf("read invitation: %v", err)
	}
	if consumedAt == nil {
		t.Error("consumed_at not stamped on the used code")
	}
	if invitee == nil || *invitee != res.UserID {
		t.Errorf("invitee_id = %v, want %s", invitee, res.UserID)
	}
	// The invitee gets their own fresh lifetime budget.
	if got := invitesRemaining(t, pg, res.UserID); got != 10 {
		t.Errorf("new user invites_remaining = %d, want 10", got)
	}
}

// TestRegisterReplayConsumesOnce: replaying the same key (same params, incl. the
// same code) returns the stored response without re-consuming the code or
// re-spending the inviter's quota.
func TestRegisterReplayConsumesOnce(t *testing.T) {
	pg := newTestPG(t)
	ctx := context.Background()
	inviter := mkCustomer(t, pg)
	code := mintInvite(t, pg, inviter) // 10 -> 9
	key := uuid.NewString()
	email := "rep-" + uuid.NewString()[:8] + "@example.com"
	uname := "rep" + uuid.NewString()[:8]

	first, _, _ := register(t, pg, key, uname, &email, nil, code)
	replay, _, _ := register(t, pg, key, uname, &email, nil, code)
	if !replay.WasReplay || replay.UserID != first.UserID {
		t.Fatalf("replay = %+v, want was_replay + user %s", replay, first.UserID)
	}

	// The code is consumed exactly once (one consumed row for this inviter).
	var consumed int
	if err := pg.Pool.QueryRow(ctx,
		`SELECT count(*) FROM invitations WHERE inviter_id = $1 AND consumed_at IS NOT NULL`,
		inviter).Scan(&consumed); err != nil {
		t.Fatalf("count consumed: %v", err)
	}
	if consumed != 1 {
		t.Errorf("consumed invitations = %d, want 1 (replay must not re-consume)", consumed)
	}
	// The inviter's quota was spent once (at mint), never touched by the replay.
	if got := invitesRemaining(t, pg, inviter); got != 9 {
		t.Errorf("inviter invites_remaining = %d, want 9", got)
	}
}

// TestRegisterFingerprintRejectsDifferentCode: the invite code is folded into the
// request fingerprint, so replaying a key with a DIFFERENT code is a parameter
// mismatch (23514), never a silent success.
func TestRegisterFingerprintRejectsDifferentCode(t *testing.T) {
	pg := newTestPG(t)
	ctx := context.Background()
	c1 := freshInvite(t, pg)
	c2 := freshInvite(t, pg)
	key := uuid.NewString()
	email := "fp-" + uuid.NewString()[:8] + "@example.com"
	uname := "fp" + uuid.NewString()[:8]

	register(t, pg, key, uname, &email, nil, c1) // claims key under fingerprint(...,c1)

	// Same key + everything else, but a different code -> fingerprint mismatch.
	_, err := pg.RegisterUser(ctx, RegisterParams{
		IdempotencyKey: key, Username: uname,
		Password: "correct-horse-battery", FullName: "Reg Tester", Email: &email,
		Channel: "email", Destination: email, TokenHash: sha256hex("t" + uuid.NewString()),
		CodeHash: sha256hex("123456"), VerifyToken: "t", InvitationCode: c2,
	})
	if sqlstate(err) != "23514" {
		t.Errorf("different-code replay sqlstate = %q, want 23514", sqlstate(err))
	}
	// c2 must NOT have been consumed (the mismatch is caught before the invite gate).
	var consumed *string
	if err := pg.Pool.QueryRow(ctx, `SELECT consumed_at::text FROM invitations WHERE code = $1`, c2).
		Scan(&consumed); err != nil {
		t.Fatalf("read c2: %v", err)
	}
	if consumed != nil {
		t.Error("the mismatched second code must stay unconsumed")
	}
}

// TestRegisterKeyNamespaceIsolation is the squat-fix regression: registration
// claims live under the dedicated sentinel owner (…0001), disjoint from the
// all-zero money/system namespace. A client-chosen register key that COLLIDES with
// a deterministic system transfer key (e.g. 'dispute-reimburse-<id>') must not
// block that transfer — both succeed independently.
func TestRegisterKeyNamespaceIsolation(t *testing.T) {
	pg := newTestPG(t)
	ctx := context.Background()

	// A key shaped exactly like a system reimbursement key minted under the all-zero owner.
	key := "dispute-reimburse-" + uuid.NewString()

	// Register under the sentinel namespace with that key (helper fails on error).
	email := "ns-" + uuid.NewString()[:8] + "@example.com"
	register(t, pg, key, "ns"+uuid.NewString()[:8], &email, nil, freshInvite(t, pg))

	// The SAME key drives a real money transfer (all-zero system namespace) with no
	// collision: the PK is (owner_id, key), and the two owners differ.
	a := mkAccount(t, pg, mkCustomer(t, pg))
	b := mkAccount(t, pg, mkCustomer(t, pg))
	fund(t, pg, a, 10_000)
	res, err := testTransfer(ctx, pg, key, a, b, 2_000, "reimburse", sqlc.TransferKindTransfer)
	if err != nil {
		t.Fatalf("transfer with a register-shaped key must not collide: %v", err)
	}
	if res.WasReplay || res.Status != sqlc.TransferStatusPosted {
		t.Errorf("transfer = %+v, want a fresh posted transfer (no cross-namespace replay)", res)
	}
	if lb, _ := balance(t, pg, a); lb != 8_000 {
		t.Errorf("debit balance = %d, want 8000 (transfer actually posted)", lb)
	}
	reconcileClean(t, pg)
}
