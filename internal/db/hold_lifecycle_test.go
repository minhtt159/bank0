package db

import (
	"context"
	"testing"

	"github.com/google/uuid"

	sqlc "github.com/minhtt159/bank0/internal/db/sqlc"
)

// Held/under_review transfer lifecycle (Recs 22/25/23). The fraud/AML gates run
// only on the CLIENT path (client_transfer, non-sentinel caller); warning_rules and
// watchlist_entries ship EMPTY, so each gate test seeds its own rule/entry and
// cleans it up (both tables are global to the shared test DB).

// clientXfer drives client_transfer (subject-scoped, gate-bearing) and returns the
// transfer id, its resulting status, the replay flag and any error.
func clientXfer(ctx context.Context, pg *Postgres, subject uuid.UUID, key string, debit, credit uuid.UUID, amount int64) (uuid.UUID, sqlc.TransferStatus, bool, error) {
	var tid uuid.UUID
	var st sqlc.TransferStatus
	var replay bool
	err := pg.Pool.QueryRow(ctx,
		`SELECT transfer_id, status, was_replay FROM client_transfer($1,$2,$3,$4,$5,$6)`,
		subject, key, debit, credit, amount, "gate test").Scan(&tid, &st, &replay)
	return tid, st, replay, err
}

// mkNamedCustomer is mkCustomer with a chosen full_name (mkCustomer hardcodes
// "Test User", which the watchlist tests need to avoid matching).
func mkNamedCustomer(t *testing.T, pg *Postgres, fullName string) uuid.UUID {
	t.Helper()
	id, err := pg.Queries.CreateUser(context.Background(), sqlc.CreateUserParams{
		Username: "u" + uniqHex(16), Password: "pw", FullName: fullName, Role: sqlc.UserRoleCustomer,
	})
	if err != nil {
		t.Fatalf("create named user: %v", err)
	}
	return id
}

// addWarningRule inserts one warning_rule and schedules its deletion. Empty
// reasonCode/minBand map to NULL match keys. category is 'risk_warning'.
func addWarningRule(t *testing.T, pg *Postgres, reasonCode, minBand, decision string, requiredAck bool, coolingOff, priority int) uuid.UUID {
	t.Helper()
	var id uuid.UUID
	err := pg.Pool.QueryRow(context.Background(),
		`INSERT INTO warning_rules
		   (match_reason_code, match_min_band, category, headline, body, severity,
		    decision, required_ack, cooling_off_seconds, priority, active)
		 VALUES (NULLIF($1,'')::text, NULLIF($2,'')::text, 'risk_warning', 'test headline',
		         'test body', 'warning', $3, $4, $5, $6, TRUE)
		 RETURNING id`,
		reasonCode, minBand, decision, requiredAck, coolingOff, priority).Scan(&id)
	if err != nil {
		t.Fatalf("insert warning rule: %v", err)
	}
	t.Cleanup(func() {
		_, _ = pg.Pool.Exec(context.Background(), `DELETE FROM warning_rules WHERE id = $1`, id)
	})
	return id
}

// addWatchlist inserts one watchlist entry and schedules its deletion.
func addWatchlist(t *testing.T, pg *Postgres, pattern string) uuid.UUID {
	t.Helper()
	var id uuid.UUID
	if err := pg.Pool.QueryRow(context.Background(),
		`INSERT INTO watchlist_entries (pattern, reason, active) VALUES ($1, 'test', TRUE) RETURNING id`,
		pattern).Scan(&id); err != nil {
		t.Fatalf("insert watchlist: %v", err)
	}
	t.Cleanup(func() {
		_, _ = pg.Pool.Exec(context.Background(), `DELETE FROM watchlist_entries WHERE id = $1`, id)
	})
	return id
}

func transferStatus(t *testing.T, pg *Postgres, tid uuid.UUID) sqlc.TransferStatus {
	t.Helper()
	got, err := pg.Queries.GetTransfer(context.Background(), tid)
	if err != nil {
		t.Fatalf("get transfer: %v", err)
	}
	return got.Status
}

// TestHeldConfirmLifecycle: a 'review' rule parks a first payment as 'held' (no
// ledger movement, funds reserved); the customer confirms and it posts.
func TestHeldConfirmLifecycle(t *testing.T) {
	pg := newTestPG(t)
	ctx := context.Background()
	alice := mkCustomer(t, pg)
	aAcct := mkAccount(t, pg, alice)
	bAcct := mkAccount(t, pg, mkCustomer(t, pg))
	fund(t, pg, aAcct, 100_000)

	addWarningRule(t, pg, "first_payment_to_payee", "", "review", false, 0, 100)

	tid, st, replay, err := clientXfer(ctx, pg, alice, uuid.NewString(), aAcct, bAcct, 5_000)
	if err != nil {
		t.Fatalf("client_transfer: %v", err)
	}
	if st != sqlc.TransferStatusHeld || replay {
		t.Fatalf("want held/non-replay, got %s replay=%v", st, replay)
	}

	// No ledger movement yet; funds reserved (available down by the amount).
	if led, avail := balance(t, pg, aAcct); led != 100_000 || avail != 95_000 {
		t.Errorf("held: ledger=%d available=%d, want 100000/95000", led, avail)
	}
	if led, _ := balance(t, pg, bAcct); led != 0 {
		t.Errorf("held credit balance = %d, want 0 (not yet posted)", led)
	}
	got, _ := pg.Queries.GetTransfer(ctx, tid)
	if got.HoldReason == nil || *got.HoldReason == "" {
		t.Error("held transfer must carry a hold_reason")
	}
	if got.HoldExpiresAt == nil {
		t.Error("held transfer must carry a hold_expires_at")
	}

	// Customer confirms -> posts, money moves.
	st2, err := pg.Queries.ClientConfirmTransfer(ctx, sqlc.ClientConfirmTransferParams{CallerSubject: alice, ID: tid})
	if err != nil {
		t.Fatalf("client_confirm_transfer: %v", err)
	}
	if st2 != sqlc.TransferStatusPosted {
		t.Fatalf("confirm returned %s, want posted", st2)
	}
	if led, _ := balance(t, pg, aAcct); led != 95_000 {
		t.Errorf("after confirm debit = %d, want 95000", led)
	}
	if led, _ := balance(t, pg, bAcct); led != 5_000 {
		t.Errorf("after confirm credit = %d, want 5000", led)
	}
	// Confirm again is an idempotent no-op.
	if st3, err := pg.Queries.ClientConfirmTransfer(ctx, sqlc.ClientConfirmTransferParams{CallerSubject: alice, ID: tid}); err != nil || st3 != sqlc.TransferStatusPosted {
		t.Errorf("idempotent re-confirm: st=%s err=%v", st3, err)
	}
	reconcileClean(t, pg)
}

// TestConfirmGuards: confirm is owner-scoped and state-guarded.
func TestConfirmGuards(t *testing.T) {
	pg := newTestPG(t)
	ctx := context.Background()
	alice := mkCustomer(t, pg)
	aAcct := mkAccount(t, pg, alice)
	bob := mkCustomer(t, pg)
	bAcct := mkAccount(t, pg, mkCustomer(t, pg))
	fund(t, pg, aAcct, 100_000)

	addWarningRule(t, pg, "first_payment_to_payee", "", "review", false, 0, 100)

	// A held transfer owned by alice.
	held, st, _, err := clientXfer(ctx, pg, alice, uuid.NewString(), aAcct, bAcct, 5_000)
	if err != nil || st != sqlc.TransferStatusHeld {
		t.Fatalf("stage held: st=%s err=%v", st, err)
	}

	// Foreign confirm -> 'not found' (P0001), existence hidden.
	if _, err := pg.Queries.ClientConfirmTransfer(ctx, sqlc.ClientConfirmTransferParams{CallerSubject: bob, ID: held}); sqlstate(err) != "P0001" {
		t.Errorf("foreign confirm sqlstate = %q, want P0001", sqlstate(err))
	}

	// Confirming a plain pending (non-held) transfer -> P0001 'cannot confirm'.
	pend, err := testRequestTransfer(ctx, pg, uuid.NewString(), aAcct, bAcct, 1_000, "pending", sqlc.TransferKindTransfer)
	if err != nil {
		t.Fatalf("request pending: %v", err)
	}
	if _, err := pg.Queries.ClientConfirmTransfer(ctx, sqlc.ClientConfirmTransferParams{CallerSubject: alice, ID: pend.TransferID}); sqlstate(err) != "P0001" {
		t.Errorf("confirm-pending sqlstate = %q, want P0001", sqlstate(err))
	}

	// Expired confirmation window -> P0001.
	if _, err := pg.Pool.Exec(ctx, `UPDATE transfers SET hold_expires_at = now() - interval '1 hour' WHERE id = $1`, held); err != nil {
		t.Fatalf("backdate window: %v", err)
	}
	if _, err := pg.Queries.ClientConfirmTransfer(ctx, sqlc.ClientConfirmTransferParams{CallerSubject: alice, ID: held}); sqlstate(err) != "P0001" {
		t.Errorf("expired-window confirm sqlstate = %q, want P0001", sqlstate(err))
	}
	// Clean up the leftover held row so it never trips a later global reconcile.
	if _, err := pg.Queries.ClientCancelTransfer(ctx, sqlc.ClientCancelTransferParams{CallerSubject: alice, ID: held, Reason: "cleanup"}); err != nil {
		t.Fatalf("cleanup cancel: %v", err)
	}
}

// TestSweepCancelsParkedWithDistinctReasons: expire_holds lapses parked transfers
// to 'canceled' with state-specific failure_reasons, while a plain pending one
// still fails with 'hold expired'. Auto-cancel, never auto-release.
func TestSweepCancelsParkedWithDistinctReasons(t *testing.T) {
	pg := newTestPG(t)
	ctx := context.Background()
	alice := mkCustomer(t, pg)
	aAcct := mkAccount(t, pg, alice)
	plainDst := mkAccount(t, pg, mkCustomer(t, pg))
	heldDst := mkAccount(t, pg, mkCustomer(t, pg))
	watchedUser := mkNamedCustomer(t, pg, "Sweep Watched "+uniqHex(6))
	muleAcct := mkAccount(t, pg, watchedUser)
	fund(t, pg, aAcct, 100_000)

	var watchName string
	_ = pg.Pool.QueryRow(ctx, `SELECT full_name FROM users WHERE id = $1`, watchedUser).Scan(&watchName)
	addWatchlist(t, pg, "%"+watchName+"%")
	addWarningRule(t, pg, "first_payment_to_payee", "", "review", false, 0, 100)

	// pending (sentinel, no gate), held (review rule), under_review (watchlist).
	pend, _ := testRequestTransfer(ctx, pg, uuid.NewString(), aAcct, plainDst, 1_000, "p", sqlc.TransferKindTransfer)
	heldTid, hs, _, err := clientXfer(ctx, pg, alice, uuid.NewString(), aAcct, heldDst, 1_000)
	if err != nil || hs != sqlc.TransferStatusHeld {
		t.Fatalf("stage held: st=%s err=%v", hs, err)
	}
	urTid, us, _, err := clientXfer(ctx, pg, alice, uuid.NewString(), aAcct, muleAcct, 1_000)
	if err != nil || us != sqlc.TransferStatusUnderReview {
		t.Fatalf("stage under_review: st=%s err=%v", us, err)
	}

	// Backdate all three holds so the sweep expires them together.
	if _, err := pg.Pool.Exec(ctx,
		`UPDATE holds SET expires_at = now() - interval '1 hour'
		 WHERE transfer_id = ANY($1) AND status = 'active'`,
		[]uuid.UUID{pend.TransferID, heldTid, urTid}); err != nil {
		t.Fatalf("backdate holds: %v", err)
	}
	if _, err := pg.Pool.Exec(ctx, `SELECT expire_holds()`); err != nil {
		t.Fatalf("expire_holds: %v", err)
	}

	want := map[uuid.UUID]struct {
		status sqlc.TransferStatus
		reason string
	}{
		pend.TransferID: {sqlc.TransferStatusFailed, "hold expired"},
		heldTid:         {sqlc.TransferStatusCanceled, "confirmation window expired"},
		urTid:           {sqlc.TransferStatusCanceled, "review window expired"},
	}
	for id, w := range want {
		got, err := pg.Queries.GetTransfer(ctx, id)
		if err != nil {
			t.Fatalf("get %s: %v", id, err)
		}
		if got.Status != w.status {
			t.Errorf("%s status = %s, want %s", id, got.Status, w.status)
		}
		if got.FailureReason == nil || *got.FailureReason != w.reason {
			t.Errorf("%s failure_reason = %v, want %q", id, got.FailureReason, w.reason)
		}
	}
	// Funds released, no money moved.
	if led, avail := balance(t, pg, aAcct); led != 100_000 || avail != 100_000 {
		t.Errorf("after sweep ledger=%d available=%d, want 100000/100000", led, avail)
	}
	reconcileClean(t, pg)
}

// TestReplayNeverReRunsGates: a replayed idempotency key returns the stored result
// BEFORE any gate runs — even a match-everything 'block' rule added after the first
// call can't retroactively block the replay.
func TestReplayNeverReRunsGates(t *testing.T) {
	pg := newTestPG(t)
	ctx := context.Background()
	alice := mkCustomer(t, pg)
	aAcct := mkAccount(t, pg, alice)
	bAcct := mkAccount(t, pg, mkCustomer(t, pg))
	cAcct := mkAccount(t, pg, mkCustomer(t, pg))
	fund(t, pg, aAcct, 100_000)

	// (1) First call, no rules -> posts and stores the result.
	key := uuid.NewString()
	tid1, st1, replay1, err := clientXfer(ctx, pg, alice, key, aAcct, bAcct, 2_000)
	if err != nil || st1 != sqlc.TransferStatusPosted || replay1 {
		t.Fatalf("first call: st=%s replay=%v err=%v", st1, replay1, err)
	}

	// (2) A match-EVERYTHING block rule (band >= low always holds).
	addWarningRule(t, pg, "", "low", "block", false, 0, 100)

	// The rule is live: a FRESH transfer is blocked (23514).
	if _, _, _, err := clientXfer(ctx, pg, alice, uuid.NewString(), aAcct, cAcct, 2_000); sqlstate(err) != "23514" {
		t.Fatalf("fresh transfer under block rule sqlstate = %q, want 23514", sqlstate(err))
	}

	// (3) Replay the ORIGINAL key/params: short-circuits before the gate -> no error,
	// same transfer, still posted.
	tid2, st2, replay2, err := clientXfer(ctx, pg, alice, key, aAcct, bAcct, 2_000)
	if err != nil {
		t.Fatalf("replay under block rule must not error: %v", err)
	}
	if !replay2 || tid2 != tid1 || st2 != sqlc.TransferStatusPosted {
		t.Errorf("replay: tid=%s(want %s) st=%s replay=%v", tid2, tid1, st2, replay2)
	}
	reconcileClean(t, pg)
}
