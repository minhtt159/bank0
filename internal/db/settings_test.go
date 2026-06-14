package db

import (
	"context"
	"testing"

	"github.com/google/uuid"

	"github.com/minhtt159/bank0/internal/iban"
)

// API-8: bank policy is DB-authoritative. requires_approval reflects updates, and
// create_account sources its default limit from bank_settings. bank_settings is a
// singleton, so reset it on cleanup to keep other tests' seeded threshold intact.
func TestBankSettingsAuthority(t *testing.T) {
	pg := newTestPG(t)
	ctx := context.Background()
	t.Cleanup(func() {
		_, _ = pg.Pool.Exec(ctx, `SELECT update_bank_settings(1000000, 50000, NULL)`)
	})

	// Seeded default: €10,000 threshold.
	ra, err := pg.RequiresApproval(ctx, 2_000_000)
	if err != nil {
		t.Fatalf("requires_approval: %v", err)
	}
	if !ra.Required || ra.ThresholdMinor != 1_000_000 {
		t.Errorf("seeded: required=%v threshold=%d, want true/1000000", ra.Required, ra.ThresholdMinor)
	}

	// Raise the threshold; the same amount no longer needs approval — the decision
	// follows the DB, not a hardcoded value.
	if _, err := pg.Pool.Exec(ctx, `SELECT update_bank_settings($1,$2,NULL)`, int64(5_000_000), int64(70_000)); err != nil {
		t.Fatalf("update_bank_settings: %v", err)
	}
	ra, _ = pg.RequiresApproval(ctx, 2_000_000)
	if ra.Required || ra.ThresholdMinor != 5_000_000 {
		t.Errorf("after raise: required=%v threshold=%d, want false/5000000", ra.Required, ra.ThresholdMinor)
	}

	// create_account with limit <= 0 now sources the configured default (70,000).
	owner := mkCustomer(t, pg)
	ibanStr, _ := iban.Generate("SE")
	var acctID uuid.UUID
	if err := pg.Pool.QueryRow(ctx, `SELECT create_account($1,$2,$3,0)`, owner, ibanStr, "1234").Scan(&acctID); err != nil {
		t.Fatalf("create_account: %v", err)
	}
	acct, err := pg.Queries.GetAccount(ctx, acctID)
	if err != nil {
		t.Fatalf("get account: %v", err)
	}
	if acct.TransferLimitMinor != 70_000 {
		t.Errorf("create_account default limit = %d, want 70000 (from bank_settings)", acct.TransferLimitMinor)
	}
}
