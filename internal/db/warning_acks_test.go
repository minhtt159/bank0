package db

import (
	"context"
	"testing"

	"github.com/google/uuid"
)

// Fraud-warning evidence (warning_acks, 00008): scoped, append-only.

func TestWarningAckScopedAndAppendOnly(t *testing.T) {
	pg := newTestPG(t)
	ctx := context.Background()
	alice := mkCustomer(t, pg)
	bob := mkCustomer(t, pg)
	aliceAcct := mkAccount(t, pg, alice)

	// Recorded against the caller's own account.
	var id uuid.UUID
	if err := pg.Pool.QueryRow(ctx,
		`SELECT record_warning_ack($1, 'cop_no_match', 'NO_MATCH', TRUE, $2, 'SE1200000000000000000001', 5000, 'web')`,
		alice, aliceAcct).Scan(&id); err != nil {
		t.Fatalf("record: %v", err)
	}

	// Evidence can't be planted on someone else's account.
	if _, err := pg.Pool.Exec(ctx,
		`SELECT record_warning_ack($1, 'cop_no_match', '', TRUE, $2, '', NULL, '')`,
		bob, aliceAcct); sqlstate(err) != "42501" {
		t.Errorf("foreign-account SQLSTATE = %q, want 42501", sqlstate(err))
	}

	// Unknown category -> table CHECK (23514).
	if _, err := pg.Pool.Exec(ctx,
		`SELECT record_warning_ack($1, 'made_up', '', TRUE, NULL, '', NULL, '')`,
		alice); sqlstate(err) != "23514" {
		t.Errorf("bad-category SQLSTATE = %q, want 23514", sqlstate(err))
	}

	// Append-only: no rewrite, no delete.
	if _, err := pg.Pool.Exec(ctx,
		`UPDATE warning_acks SET acknowledged = FALSE WHERE id = $1`, id); sqlstate(err) != "23001" {
		t.Errorf("UPDATE SQLSTATE = %q, want 23001", sqlstate(err))
	}
	if _, err := pg.Pool.Exec(ctx,
		`DELETE FROM warning_acks WHERE id = $1`, id); sqlstate(err) != "23001" {
		t.Errorf("DELETE SQLSTATE = %q, want 23001", sqlstate(err))
	}
}
