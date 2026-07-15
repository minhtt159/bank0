-- +goose Up
-- ─────────────────────────────────────────────────────────────────────────────
-- Random NL IBAN allocation. The bare sequence (00007) minted SE…0001/0002/0003 —
-- visibly sequential, which reads as fake in demos and leaks account-open order.
-- Now: NL format (NLkk BNKO nnnnnnnnnn, 18 chars) — fictional bank code 'BNKO'
-- (deliberately not a real NL bank's BIC prefix) + 10 random digits through
-- iban_generate (unchanged MOD-97 checksum), re-rolling on the unlikely collision
-- (n²/2·10⁻¹⁰ over the 10-digit space — fine for a demo bank).
-- UNIQUE(accounts.iban) remains the backstop for the check/insert race.
-- random() (not crypto-strong) is deliberate: an IBAN is an identifier, not a
-- secret, and these are internal-only / non-routable (00007 header).
-- Seed accounts keep their SE IBANs; the ledger is country-agnostic and the
-- clients' IBAN helpers are registry-free (normalize/format/highlight only).
-- iban_seq is kept for Down; nothing else reads it.
-- ─────────────────────────────────────────────────────────────────────────────
-- +goose StatementBegin
CREATE OR REPLACE FUNCTION allocate_iban() RETURNS VARCHAR AS $$
DECLARE
    v_iban VARCHAR;
BEGIN
    LOOP
        v_iban := iban_generate('NL', 'BNKO' || (
            SELECT string_agg((floor(random() * 10))::int::text, '')
            FROM generate_series(1, 10)
        ));
        EXIT WHEN NOT EXISTS (SELECT 1 FROM accounts WHERE iban = v_iban);
    END LOOP;
    RETURN v_iban;
END;
$$ LANGUAGE plpgsql;
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
CREATE OR REPLACE FUNCTION allocate_iban() RETURNS VARCHAR AS $$
    SELECT iban_generate('SE', lpad(nextval('iban_seq')::text, 20, '0'));
$$ LANGUAGE sql;
-- +goose StatementEnd
