package db

import (
	"context"
	"testing"
)

// Server-side CoP/VOP verdict (resolve_account_by_iban + p_name_hint):
// the four-outcome model is decided in the DB, not per client.

func TestResolveVerdictFourOutcomes(t *testing.T) {
	pg := newTestPG(t)
	ctx := context.Background()

	owner := mkCustomer(t, pg)
	if _, err := pg.Pool.Exec(ctx,
		`UPDATE users SET full_name = 'Astrid Lindqvist' WHERE id = $1`, owner); err != nil {
		t.Fatalf("set name: %v", err)
	}
	acct := mkAccount(t, pg, owner)
	var ibanStr string
	if err := pg.Pool.QueryRow(ctx, `SELECT iban FROM accounts WHERE id = $1`, acct).Scan(&ibanStr); err != nil {
		t.Fatalf("iban: %v", err)
	}

	verdict := func(hint *string) (match, reason, gate string, suggested *string) {
		t.Helper()
		if err := pg.Pool.QueryRow(ctx,
			`SELECT match_result, reason_code, gate, suggested_name
			   FROM resolve_account_by_iban($1, $2)`, ibanStr, hint).
			Scan(&match, &reason, &gate, &suggested); err != nil {
			t.Fatalf("resolve: %v", err)
		}
		return
	}
	str := func(s string) *string { return &s }

	// match: exact, case/whitespace-insensitive; gate ok; nothing revealed.
	m, _, g, sug := verdict(str("  astrid   LINDQVIST "))
	if m != "match" || g != "ok" || sug != nil {
		t.Errorf("exact = %s/%s/%v, want match/ok/nil", m, g, sug)
	}

	// close_match: typo; the REGISTERED name is revealed; gate needs an ack.
	m, _, g, sug = verdict(str("Astrid Lindquist"))
	if m != "close_match" || g != "awaiting_acknowledgement" || sug == nil || *sug != "Astrid Lindqvist" {
		t.Errorf("typo = %s/%s/%v, want close_match/awaiting_acknowledgement/registered name", m, g, sug)
	}

	// no_match: different person; nothing revealed.
	m, _, g, sug = verdict(str("Markus Eklund"))
	if m != "no_match" || g != "awaiting_acknowledgement" || sug != nil {
		t.Errorf("stranger = %s/%s/%v, want no_match/awaiting_acknowledgement/nil", m, g, sug)
	}

	// unable: no name supplied.
	m, reason, g, _ := verdict(nil)
	if m != "unable" || reason != "NAME_NOT_SUPPLIED" || g != "awaiting_acknowledgement" {
		t.Errorf("no hint = %s/%s/%s, want unable/NAME_NOT_SUPPLIED/awaiting_acknowledgement", m, reason, g)
	}

	// Unknown IBAN still hides existence (P0001 -> 404), verdict or not.
	if _, err := pg.Pool.Exec(ctx,
		`SELECT * FROM resolve_account_by_iban($1, $2)`, "SE0000000000000000000000", "x"); sqlstate(err) != "P0001" {
		t.Errorf("unknown-iban SQLSTATE = %q, want P0001", sqlstate(err))
	}
}
