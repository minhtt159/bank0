package db

import (
	"context"
	"testing"

	"github.com/google/uuid"
)

// These exercise the DB IBAN authority added in migrations 00022 + 00023:
// iban_is_valid / iban_generate / iban_country_length, plus the two CHECK
// constraints (accounts_iban_checksum, beneficiaries_iban_checksum). After
// 00023 the validator is length- and country-aware, and iban_generate RAISES
// for an unregistered country or a wrong-length BBAN.

// TestIbanIsValid is table-driven over iban_is_valid, covering the normalized
// forms (lowercase + spaces), bad checksums, wrong per-country length, and an
// unregistered country code.
func TestIbanIsValid(t *testing.T) {
	pg := newTestPG(t)
	ctx := context.Background()

	// A generated NL IBAN is valid by construction; use it as one of the TRUE cases.
	var nlGen string
	if err := pg.Pool.QueryRow(ctx,
		`SELECT iban_generate('NL', lpad('1',14,'0'))`).Scan(&nlGen); err != nil {
		t.Fatalf("iban_generate NL for fixture: %v", err)
	}

	cases := []struct {
		name string
		in   string
		want bool
	}{
		// Known-valid registered IBANs (correct checksum + correct length).
		{"GB valid", "GB82WEST12345698765432", true},
		{"DE valid", "DE89370400440532013000", true},
		{"NL generated valid", nlGen, true},
		// The function normalizes: lowercase + space-grouped forms must still pass.
		{"GB valid lowercase", "gb82west12345698765432", true},
		{"GB valid spaced", "GB82 WEST 1234 5698 7654 32", true},
		// Bad checksum (correct length+country, wrong check digits).
		{"GB bad checksum", "GB00WEST12345698765432", false},
		// Wrong per-country length: 24 chars, GB wants 22 (checksum-aside, length rejects).
		{"GB wrong length", "GB1800000000000000000000", false},
		// Unregistered country code (ZZ is not in the length table).
		{"ZZ unregistered country", "ZZ6600000000000000000", false},
	}
	for _, c := range cases {
		c := c
		t.Run(c.name, func(t *testing.T) {
			var got bool
			if err := pg.Pool.QueryRow(ctx, `SELECT iban_is_valid($1)`, c.in).Scan(&got); err != nil {
				t.Fatalf("iban_is_valid(%q): %v", c.in, err)
			}
			if got != c.want {
				t.Errorf("iban_is_valid(%q) = %v, want %v", c.in, got, c.want)
			}
		})
	}
}

// TestIbanIsValidNull asserts the NULL-input contract: iban_is_valid(NULL)
// RETURNS NULL (not TRUE/FALSE). Scan into *bool and assert it is nil.
func TestIbanIsValidNull(t *testing.T) {
	pg := newTestPG(t)
	ctx := context.Background()

	var got *bool
	if err := pg.Pool.QueryRow(ctx, `SELECT iban_is_valid(NULL::text)`).Scan(&got); err != nil {
		t.Fatalf("iban_is_valid(NULL): %v", err)
	}
	if got != nil {
		t.Errorf("iban_is_valid(NULL) = %v, want NULL (nil)", *got)
	}
}

// TestIbanGenerateRoundTrip generates IBANs for several countries with a
// correctly-sized BBAN, and asserts each output passes iban_is_valid.
func TestIbanGenerateRoundTrip(t *testing.T) {
	pg := newTestPG(t)
	ctx := context.Background()

	// total length per country (from iban_country_length); BBAN length = total - 4.
	cases := []struct {
		cc    string
		total int
	}{
		{"NL", 18},
		{"DE", 22},
		{"GB", 22},
		{"SE", 24},
		{"NO", 15},
	}
	for _, c := range cases {
		c := c
		t.Run(c.cc, func(t *testing.T) {
			bbanLen := c.total - 4
			var gen string
			// BBAN = bbanLen '1' digits (alnum, correct width for the country).
			if err := pg.Pool.QueryRow(ctx,
				`SELECT iban_generate($1, lpad('1', $2, '0'))`, c.cc, bbanLen).Scan(&gen); err != nil {
				t.Fatalf("iban_generate(%s): %v", c.cc, err)
			}
			if len(gen) != c.total {
				t.Errorf("iban_generate(%s) len = %d, want %d (%q)", c.cc, len(gen), c.total, gen)
			}
			var ok bool
			if err := pg.Pool.QueryRow(ctx, `SELECT iban_is_valid($1)`, gen).Scan(&ok); err != nil {
				t.Fatalf("iban_is_valid(%q): %v", gen, err)
			}
			if !ok {
				t.Errorf("generated IBAN %q failed iban_is_valid (round-trip broken)", gen)
			}
		})
	}
}

// TestIbanGenerateRejectsBadInput asserts 00023's hardening: iban_generate now
// RAISES for an unregistered country code and for a wrong-length BBAN. Both are
// bare RAISE EXCEPTION (no ERRCODE) -> P0001, so assert err != nil and a
// non-empty SQLSTATE.
func TestIbanGenerateRejectsBadInput(t *testing.T) {
	pg := newTestPG(t)
	ctx := context.Background()

	// Unregistered country: ZZ is not in the length table.
	var dummy string
	err := pg.Pool.QueryRow(ctx,
		`SELECT iban_generate('ZZ', lpad('1', 14, '0'))`).Scan(&dummy)
	if err == nil {
		t.Error("iban_generate('ZZ', ...) must raise for an unregistered country")
	} else if code := sqlstate(err); code != "P0001" {
		// bare RAISE EXCEPTION => P0001; tolerate any non-empty code defensively.
		if code == "" {
			t.Errorf("unregistered-country raise: sqlstate empty, err=%v", err)
		}
	}

	// Wrong-length BBAN: NL wants a 14-char BBAN (total 18); give it 10.
	err = pg.Pool.QueryRow(ctx,
		`SELECT iban_generate('NL', lpad('1', 10, '0'))`).Scan(&dummy)
	if err == nil {
		t.Error("iban_generate('NL', <wrong-length BBAN>) must raise")
	} else if code := sqlstate(err); code != "P0001" {
		if code == "" {
			t.Errorf("wrong-length-BBAN raise: sqlstate empty, err=%v", err)
		}
	}
}

// TestAccountsIbanChecksumCheck asserts the non-bypassable backstop: a direct
// INSERT of a customer account with a checksum-invalid (or wrong-length) IBAN is
// rejected by the accounts_iban_checksum CHECK (23514). A valid account inserts
// fine (built via mkAccount).
func TestAccountsIbanChecksumCheck(t *testing.T) {
	pg := newTestPG(t)
	ctx := context.Background()
	owner := mkCustomer(t, pg)

	// Sanity: the happy path works — a valid customer account is created.
	_ = mkAccount(t, pg, owner)

	// Bad: checksum-invalid IBAN. Format-valid (alnum, 22 chars, GB length) so it
	// passes the 00003 format CHECK and reaches the checksum CHECK; both raise 23514.
	owner2 := mkCustomer(t, pg)
	_, err := pg.Pool.Exec(ctx,
		`INSERT INTO accounts (user_id, kind, iban) VALUES ($1, 'customer', $2)`,
		owner2, "GB00WEST12345698765432")
	if err == nil {
		t.Fatal("INSERT of a checksum-invalid customer IBAN must be rejected")
	}
	if code := sqlstate(err); code != "23514" {
		t.Errorf("checksum-invalid account insert: sqlstate = %q, want 23514 (check_violation); err=%v", code, err)
	}
}

// TestBeneficiariesIbanChecksumCheck asserts the 00023 beneficiaries CHECK: a
// direct INSERT with a checksum-invalid IBAN fails with 23514. We borrow a real
// account's id + owner so only the IBAN CHECK is exercised.
func TestBeneficiariesIbanChecksumCheck(t *testing.T) {
	pg := newTestPG(t)
	ctx := context.Background()

	// A real, valid account (its IBAN passes the checksum) and its owner.
	credit := mkAccount(t, pg, mkCustomer(t, pg))
	var creditOwner uuid.UUID
	if err := pg.Pool.QueryRow(ctx,
		`SELECT user_id FROM accounts WHERE id = $1`, credit).Scan(&creditOwner); err != nil {
		t.Fatalf("lookup account owner: %v", err)
	}

	// Bad: checksum-invalid IBAN on the beneficiary row.
	_, err := pg.Pool.Exec(ctx,
		`INSERT INTO beneficiaries (owner_user_id, label, credit_account_id, iban, owner_name_masked)
		 VALUES ($1, $2, $3, $4, $5)`,
		creditOwner, "bad-payee", credit, "GB00WEST12345698765432", "B****")
	if err == nil {
		t.Fatal("INSERT of a beneficiary with a checksum-invalid IBAN must be rejected")
	}
	if code := sqlstate(err); code != "23514" {
		t.Errorf("checksum-invalid beneficiary insert: sqlstate = %q, want 23514 (check_violation); err=%v", code, err)
	}
}
