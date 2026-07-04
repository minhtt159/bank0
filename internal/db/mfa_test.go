package db

import (
	"context"
	"testing"
)

// TOTP MFA state machine (00003): enroll -> confirm; recovery burn; lockout.

func TestMFAEnrollConfirmStateMachine(t *testing.T) {
	pg := newTestPG(t)
	ctx := context.Background()
	u := mkCustomer(t, pg)
	secret := []byte("ciphertext-blob-1")

	if enabled, err := pg.MFAEnabled(ctx, u); err != nil || enabled {
		t.Fatalf("fresh user MFAEnabled = %v/%v, want false", enabled, err)
	}
	if _, err := pg.MFABeginEnroll(ctx, u, secret); err != nil {
		t.Fatalf("begin enroll: %v", err)
	}
	// Unconfirmed => still disabled; pending secret readable; confirmed secret raises.
	if enabled, _ := pg.MFAEnabled(ctx, u); enabled {
		t.Error("unconfirmed credential must not enable MFA")
	}
	if got, err := pg.MFAPendingSecret(ctx, u); err != nil || string(got) != string(secret) {
		t.Errorf("pending secret = %q/%v", got, err)
	}
	if _, err := pg.MFAConfirmedSecret(ctx, u); sqlstate(err) != "P0001" {
		t.Errorf("confirmed-secret before confirm SQLSTATE = %q, want P0001", sqlstate(err))
	}
	// Re-enroll before confirm replaces the pending credential.
	if _, err := pg.MFABeginEnroll(ctx, u, []byte("ciphertext-blob-2")); err != nil {
		t.Fatalf("re-enroll: %v", err)
	}

	if err := pg.MFAConfirm(ctx, u, []string{sha256hex("rc1"), sha256hex("rc2")}); err != nil {
		t.Fatalf("confirm: %v", err)
	}
	if enabled, _ := pg.MFAEnabled(ctx, u); !enabled {
		t.Error("confirm must enable MFA")
	}
	if got, err := pg.MFAConfirmedSecret(ctx, u); err != nil || string(got) != "ciphertext-blob-2" {
		t.Errorf("confirmed secret = %q/%v", got, err)
	}
	// Enrolling again while enabled -> 23505.
	if _, err := pg.MFABeginEnroll(ctx, u, secret); sqlstate(err) != "23505" {
		t.Errorf("enroll-while-enabled SQLSTATE = %q, want 23505", sqlstate(err))
	}

	// Recovery codes burn exactly once.
	if ok, err := pg.MFABurnRecoveryCode(ctx, u, sha256hex("rc1")); err != nil || !ok {
		t.Errorf("first burn = %v/%v, want true", ok, err)
	}
	if ok, _ := pg.MFABurnRecoveryCode(ctx, u, sha256hex("rc1")); ok {
		t.Error("second burn of the same code must be false")
	}
	if ok, _ := pg.MFABurnRecoveryCode(ctx, u, sha256hex("nope")); ok {
		t.Error("unknown code must be false")
	}
}

func TestMFALockoutWindow(t *testing.T) {
	pg := newTestPG(t)
	ctx := context.Background()
	u := mkCustomer(t, pg)
	const maxFail, window = 3, 900

	for i := 0; i < maxFail-1; i++ {
		if locked, err := pg.MFARecordAttempt(ctx, u, false, "1.2.3.4", maxFail, window); err != nil || locked {
			t.Fatalf("fail #%d locked = %v/%v, want false", i+1, locked, err)
		}
	}
	if locked, _ := pg.MFAIsLocked(ctx, u, maxFail, window); locked {
		t.Error("below threshold must not be locked")
	}
	if locked, err := pg.MFARecordAttempt(ctx, u, false, "1.2.3.4", maxFail, window); err != nil || !locked {
		t.Errorf("threshold fail locked = %v/%v, want true", locked, err)
	}
	if locked, _ := pg.MFAIsLocked(ctx, u, maxFail, window); !locked {
		t.Error("MFAIsLocked must agree after the threshold")
	}
	// Sweep drops day-old rows only — a fresh lockout survives cleanup.
	if _, err := pg.Pool.Exec(ctx, `SELECT cleanup_mfa_attempts()`); err != nil {
		t.Fatalf("cleanup: %v", err)
	}
	if locked, _ := pg.MFAIsLocked(ctx, u, maxFail, window); !locked {
		t.Error("cleanup must not clear a fresh lockout")
	}
}

func TestIsKnownPayee(t *testing.T) {
	pg := newTestPG(t)
	ctx := context.Background()
	owner := mkCustomer(t, pg)
	other := mkAccount(t, pg, mkCustomer(t, pg))

	var known bool
	if err := pg.Pool.QueryRow(ctx,
		`SELECT EXISTS (SELECT 1 FROM beneficiaries WHERE owner_user_id=$1 AND credit_account_id=$2)`,
		owner, other).Scan(&known); err != nil || known {
		t.Fatalf("pre: known=%v err=%v, want false", known, err)
	}
	var ibanStr string
	_ = pg.Pool.QueryRow(ctx, `SELECT iban FROM accounts WHERE id=$1`, other).Scan(&ibanStr)
	if _, err := pg.Pool.Exec(ctx, `SELECT add_beneficiary($1,'pal',$2)`, owner, ibanStr); err != nil {
		t.Fatalf("add beneficiary: %v", err)
	}
	if err := pg.Pool.QueryRow(ctx,
		`SELECT EXISTS (SELECT 1 FROM beneficiaries WHERE owner_user_id=$1 AND credit_account_id=$2)`,
		owner, other).Scan(&known); err != nil || !known {
		t.Errorf("post: known=%v err=%v, want true", known, err)
	}
}
