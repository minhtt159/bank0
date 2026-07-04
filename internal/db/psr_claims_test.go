package db

import (
	"context"
	"testing"

	"github.com/google/uuid"
)

// Rec 12 — the PSR claim machine: SLA clock, decide (reimbursement is REAL
// money), recall state machine. Rec 11/15 — recipient risk + the TRA seam.

func TestDisputeClaimReimbursesForReal(t *testing.T) {
	pg := newTestPG(t)
	ctx := context.Background()
	victim := mkCustomer(t, pg)
	a := mkAccount(t, pg, victim)
	b := mkAccount(t, pg, mkCustomer(t, pg))
	admin := mkCustomer(t, pg)
	fund(t, pg, a, 200_000)
	tid := postedTransferAmount(t, pg, a, b, 50_000)

	var did uuid.UUID
	if err := pg.Pool.QueryRow(ctx,
		`SELECT raise_dispute($1,$2,'fraud','scammed','impersonation')`, tid, victim).Scan(&did); err != nil {
		t.Fatalf("raise: %v", err)
	}
	// SLA clock set ~15 business days out (>= 15 calendar days).
	var slaOK bool
	if err := pg.Pool.QueryRow(ctx,
		`SELECT sla_due_at >= now() + interval '14 days' AND scam_type = 'impersonation'
		   FROM disputes WHERE id = $1`, did).Scan(&slaOK); err != nil || !slaOK {
		t.Errorf("sla/scam_type not set: ok=%v err=%v", slaOK, err)
	}

	ledgerBefore, _ := balance(t, pg, a)

	// Full reimbursement of 3000: excess (€100 default) deducted -> payout 2900,
	// moved from EXTERNAL_CLEARING into the victim's account.
	var payout int64
	if err := pg.Pool.QueryRow(ctx,
		`SELECT decide_dispute($1,$2,'reimbursed',50000,NULL,'confirmed APP scam')`, did, admin).Scan(&payout); err != nil {
		t.Fatalf("decide: %v", err)
	}
	if payout != 40_000 {
		t.Errorf("payout = %d, want 40000 (50000 - 10000 excess)", payout)
	}
	ledgerAfter, _ := balance(t, pg, a)
	if ledgerAfter-ledgerBefore != 40_000 {
		t.Errorf("victim ledger moved %d, want 40000", ledgerAfter-ledgerBefore)
	}

	var status, decision string
	var stored int64
	if err := pg.Pool.QueryRow(ctx,
		`SELECT status, decision, reimbursed_amount_minor FROM disputes WHERE id = $1`, did).
		Scan(&status, &decision, &stored); err != nil {
		t.Fatalf("read dispute: %v", err)
	}
	if status != "resolved" || decision != "reimbursed" || stored != 40_000 {
		t.Errorf("dispute = %s/%s/%d, want resolved/reimbursed/40000", status, decision, stored)
	}

	// Terminal: decide again -> 409-shaped P0001; books still balanced.
	if _, err := pg.Pool.Exec(ctx,
		`SELECT decide_dispute($1,$2,'declined',NULL,NULL,'')`, did, admin); sqlstate(err) != "P0001" {
		t.Errorf("double-decide SQLSTATE = %q, want P0001", sqlstate(err))
	}
	var issues int
	_ = pg.Pool.QueryRow(ctx, `SELECT count(*) FROM reconcile()`).Scan(&issues)
	if issues != 0 {
		t.Errorf("reconcile issues after reimbursement = %d, want 0", issues)
	}
}

func TestDisputeVulnerableWaivesExcessAndDeclinePath(t *testing.T) {
	pg := newTestPG(t)
	ctx := context.Background()
	victim := mkCustomer(t, pg)
	a := mkAccount(t, pg, victim)
	b := mkAccount(t, pg, mkCustomer(t, pg))
	admin := mkCustomer(t, pg)
	fund(t, pg, a, 500_000)

	// Vulnerable: excess waived -> the full amount back.
	t1 := postedTransferAmount(t, pg, a, b, 50_000)
	var d1 uuid.UUID
	_ = pg.Pool.QueryRow(ctx, `SELECT raise_dispute($1,$2,'fraud','', 'romance')`, t1, victim).Scan(&d1)
	var payout int64
	if err := pg.Pool.QueryRow(ctx,
		`SELECT decide_dispute($1,$2,'reimbursed',50000,TRUE,'vulnerable customer')`, d1, admin).Scan(&payout); err != nil {
		t.Fatalf("decide vulnerable: %v", err)
	}
	if payout != 50_000 {
		t.Errorf("vulnerable payout = %d, want 50000 (excess waived)", payout)
	}

	// Decline: no money moves, status rejected.
	t2 := postedTransferAmount(t, pg, a, b, 50_000)
	var d2 uuid.UUID
	_ = pg.Pool.QueryRow(ctx, `SELECT raise_dispute($1,$2,'fraud','','purchase')`, t2, victim).Scan(&d2)
	before, _ := balance(t, pg, a)
	if err := pg.Pool.QueryRow(ctx,
		`SELECT decide_dispute($1,$2,'declined',NULL,NULL,'not a scam')`, d2, admin).Scan(&payout); err != nil {
		t.Fatalf("decline: %v", err)
	}
	after, _ := balance(t, pg, a)
	if payout != 0 || after != before {
		t.Errorf("decline moved money: payout=%d delta=%d", payout, after-before)
	}
	var status string
	_ = pg.Pool.QueryRow(ctx, `SELECT status FROM disputes WHERE id = $1`, d2).Scan(&status)
	if status != "rejected" {
		t.Errorf("declined dispute status = %s, want rejected", status)
	}

	// Over-ask: reimbursement above the disputed amount -> 23514.
	t3 := postedTransferAmount(t, pg, a, b, 50_000)
	var d3 uuid.UUID
	_ = pg.Pool.QueryRow(ctx, `SELECT raise_dispute($1,$2,'fraud','','other')`, t3, victim).Scan(&d3)
	if _, err := pg.Pool.Exec(ctx,
		`SELECT decide_dispute($1,$2,'reimbursed',999999,NULL,'')`, d3, admin); sqlstate(err) != "23514" {
		t.Errorf("over-ask SQLSTATE = %q, want 23514", sqlstate(err))
	}
}

func TestDisputeRecallStateMachine(t *testing.T) {
	pg := newTestPG(t)
	ctx := context.Background()
	victim := mkCustomer(t, pg)
	a := mkAccount(t, pg, victim)
	b := mkAccount(t, pg, mkCustomer(t, pg))
	op := mkCustomer(t, pg)
	fund(t, pg, a, 10_000)
	tid := postedTransfer(t, pg, a, b)
	var did uuid.UUID
	_ = pg.Pool.QueryRow(ctx, `SELECT raise_dispute($1,$2,'fraud','','invoice')`, tid, victim).Scan(&did)

	// Answering before requesting is illegal.
	if _, err := pg.Pool.Exec(ctx,
		`SELECT set_dispute_recall($1,$2,'funds_returned','')`, did, op); sqlstate(err) != "P0001" {
		t.Errorf("answer-before-request SQLSTATE = %q, want P0001", sqlstate(err))
	}
	var st string
	if err := pg.Pool.QueryRow(ctx,
		`SELECT set_dispute_recall($1,$2,'requested','FRAD')`, did, op).Scan(&st); err != nil || st != "requested" {
		t.Fatalf("request recall = %s/%v", st, err)
	}
	// Re-requesting is illegal; answering once is fine; re-answering is illegal.
	if _, err := pg.Pool.Exec(ctx,
		`SELECT set_dispute_recall($1,$2,'requested','')`, did, op); sqlstate(err) != "P0001" {
		t.Errorf("re-request SQLSTATE = %q, want P0001", sqlstate(err))
	}
	if err := pg.Pool.QueryRow(ctx,
		`SELECT set_dispute_recall($1,$2,'refused','beneficiary bank said no')`, did, op).Scan(&st); err != nil || st != "refused" {
		t.Fatalf("answer recall = %s/%v", st, err)
	}
	if _, err := pg.Pool.Exec(ctx,
		`SELECT set_dispute_recall($1,$2,'funds_returned','')`, did, op); sqlstate(err) != "P0001" {
		t.Errorf("re-answer SQLSTATE = %q, want P0001", sqlstate(err))
	}
}

func TestRecipientRiskSignalsAndTRA(t *testing.T) {
	pg := newTestPG(t)
	ctx := context.Background()
	caller := mkCustomer(t, pg)
	callerAcct := mkAccount(t, pg, caller)
	mule := mkAccount(t, pg, mkCustomer(t, pg))
	fund(t, pg, callerAcct, 100_000)

	// Flag the destination as a mule (the operator rule seam). Target it at THIS
	// caller and clean up after: guided_scenarios is shared with the suggestion
	// suite, which owns global scenario state.
	scenario := "risk-test-" + uuid.NewString()[:8]
	if _, err := pg.Pool.Exec(ctx,
		`INSERT INTO guided_scenarios (name, target_account_id, target_user_id) VALUES ($1, $2, $3)`,
		scenario, mule, caller); err != nil {
		t.Fatalf("flag mule: %v", err)
	}
	t.Cleanup(func() {
		_, _ = pg.Pool.Exec(context.Background(), `DELETE FROM guided_scenarios WHERE name = $1`, scenario)
	})

	var ibanStr string
	_ = pg.Pool.QueryRow(ctx, `SELECT iban FROM accounts WHERE id = $1`, mule).Scan(&ibanStr)

	// Rec 11: resolve with caller context -> high risk, mule_suspected, signals.
	var risk string
	var muleSusp, firstPay bool
	var signals []string
	if err := pg.Pool.QueryRow(ctx,
		`SELECT recipient_risk, mule_suspected, signals, is_first_payment_to_payee
		   FROM resolve_account_by_iban($1, NULL, $2)`, ibanStr, caller).
		Scan(&risk, &muleSusp, &signals, &firstPay); err != nil {
		t.Fatalf("resolve: %v", err)
	}
	if risk != "high" || !muleSusp || !firstPay {
		t.Errorf("mule resolve = %s/%v/%v, want high/true/true (signals %v)", risk, muleSusp, firstPay, signals)
	}

	// Rec 15: the TRA seam scores the mule destination 'high'
	// (destination_flagged +3, first_payment +1, new debit account +1).
	var band string
	var score int
	var reasons []string
	if err := pg.Pool.QueryRow(ctx,
		`SELECT risk_band, score, reasons FROM assess_transfer_risk($1,$2,$3,1000)`,
		caller, callerAcct, mule).Scan(&band, &score, &reasons); err != nil {
		t.Fatalf("assess: %v", err)
	}
	if band != "high" {
		t.Errorf("mule TRA band = %s (score %d, %v), want high", band, score, reasons)
	}

	// A clean repeat payee scores low: post once, then re-assess.
	clean := mkAccount(t, pg, mkCustomer(t, pg))
	_ = postedTransferAmount(t, pg, callerAcct, clean, 1_000)
	if err := pg.Pool.QueryRow(ctx,
		`SELECT risk_band FROM assess_transfer_risk($1,$2,$3,1000)`,
		caller, callerAcct, clean).Scan(&band); err != nil {
		t.Fatalf("assess clean: %v", err)
	}
	if band == "high" {
		t.Errorf("clean repeat payee band = %s, want low/medium", band)
	}
}

// postedTransferAmount posts a transfer of a given amount (helper variant).
func postedTransferAmount(t *testing.T, pg *Postgres, a, b uuid.UUID, amount int64) uuid.UUID {
	t.Helper()
	var tid uuid.UUID
	var status string
	var wasReplay bool
	if err := pg.Pool.QueryRow(context.Background(),
		`SELECT transfer_id, status, was_replay FROM transfer($1,$2,$3,$4,$5,'transfer')`,
		uuid.NewString(), a, b, amount, "risk helper").Scan(&tid, &status, &wasReplay); err != nil {
		t.Fatalf("transfer: %v", err)
	}
	return tid
}
