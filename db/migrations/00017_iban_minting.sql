-- +goose Up
-- ─────────────────────────────────────────────────────────────────────────────
-- IBAN MINTING — bank0's own allocation POLICY, deliberately separate from the
-- generic accounts domain (00007): how THIS bank numbers the accounts it opens,
-- not generic account behavior. open_customer_account (00007) calls it at
-- runtime (plpgsql late binding), so it may load last.
--
-- A REAL ISO 13616 'NL' IBAN (iban_generate, 00002 — valid MOD-97 check digits,
-- so it passes the accounts checksum CHECK and every client-side validator):
-- the bank's own code from the bank_settings.iban_bank_code policy knob
-- (00009; default INGB, operator-tunable — one institution, one code) + a
-- RANDOM 10-digit account number — sequential BBANs read as fake in demos and
-- leak account-open order. The loop re-rolls the astronomically unlikely
-- collision; UNIQUE(accounts.iban) is the backstop for the check/insert race.
-- random() (not crypto-strong) is deliberate: an IBAN is an identifier, not a
-- secret. Internal-only: not routable at any real NL bank.
-- ─────────────────────────────────────────────────────────────────────────────
-- +goose StatementBegin
CREATE OR REPLACE FUNCTION allocate_iban() RETURNS VARCHAR AS $$
DECLARE
    v_iban VARCHAR;
BEGIN
    LOOP
        v_iban := iban_generate('NL',
            (SELECT iban_bank_code FROM bank_settings)
            || lpad((floor(random() * 1e10))::bigint::text, 10, '0'));
        EXIT WHEN NOT EXISTS (SELECT 1 FROM accounts WHERE iban = v_iban);
    END LOOP;
    RETURN v_iban;
END;
$$ LANGUAGE plpgsql;
-- +goose StatementEnd

-- +goose Down
DROP FUNCTION IF EXISTS allocate_iban();
