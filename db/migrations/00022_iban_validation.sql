-- +goose Up
-- +goose StatementBegin

-- IBAN check-digit validation (ISO 7064 MOD-97-10) as the DB authority. The prior
-- accounts.iban CHECK (00003) is a FORMAT gate only (^[A-Z0-9]{15,34}$) — it accepts
-- checksum-invalid strings. These functions add the checksum. Algorithm verified in
-- docs/11-iban-verification.md; mirrors internal/iban (Go). IMMUTABLE + a bounded
-- per-character fold (accumulator stays < 9700, no bignum) → ~0.1 µs/call, cheap for
-- a per-insert CHECK.

-- iban_is_valid: structure (2 alpha, 2 digit, rest alnum, len 15..34) + MOD-97 == 1.
-- Per-country exact length is enforced by the app layers (Go/TS); this DB backstop
-- enforces the cross-country invariant (the checksum), which catches typos/transpositions.
CREATE OR REPLACE FUNCTION iban_is_valid(p_iban TEXT) RETURNS BOOLEAN
LANGUAGE plpgsql IMMUTABLE PARALLEL SAFE AS $$
DECLARE
    s    TEXT;
    rot  TEXT;
    rem  INT := 0;
    i    INT;
    code INT;
BEGIN
    IF p_iban IS NULL THEN
        RETURN NULL;
    END IF;
    s := upper(regexp_replace(p_iban, '\s', '', 'g'));
    IF s !~ '^[A-Z]{2}[0-9]{2}[A-Z0-9]+$' OR length(s) < 15 OR length(s) > 34 THEN
        RETURN FALSE;
    END IF;
    rot := substr(s, 5) || substr(s, 1, 4);        -- move first 4 chars to the end
    FOR i IN 1 .. length(rot) LOOP
        code := ascii(substr(rot, i, 1));
        IF code BETWEEN 48 AND 57 THEN              -- '0'..'9'
            rem := (rem * 10 + (code - 48)) % 97;
        ELSE                                        -- 'A'..'Z' -> 10..35
            rem := (rem * 100 + (code - 55)) % 97;
        END IF;
    END LOOP;
    RETURN rem = 1;
END;
$$;

-- iban_generate: build a checksum-valid IBAN from a country code + BBAN.
-- check digits = 98 - mod97(BBAN || CC || '00'). Used by the seeds.
CREATE OR REPLACE FUNCTION iban_generate(p_country TEXT, p_bban TEXT) RETURNS TEXT
LANGUAGE plpgsql IMMUTABLE AS $$
DECLARE rot TEXT; rem INT := 0; i INT; code INT; chk INT;
BEGIN
    rot := upper(p_bban) || upper(p_country) || '00';
    FOR i IN 1 .. length(rot) LOOP
        code := ascii(substr(rot, i, 1));
        IF code BETWEEN 48 AND 57 THEN
            rem := (rem * 10 + (code - 48)) % 97;
        ELSE
            rem := (rem * 100 + (code - 55)) % 97;
        END IF;
    END LOOP;
    chk := 98 - rem;
    RETURN upper(p_country) || lpad(chk::text, 2, '0') || upper(p_bban);
END;
$$;

-- The non-bypassable backstop: every customer account IBAN must pass the checksum.
-- (System/GL accounts have iban NULL, which is allowed.) Applies to the admin API,
-- the operator console, seeds, and any future writer — honoring rules #1/#2.
ALTER TABLE accounts
    ADD CONSTRAINT accounts_iban_checksum CHECK (iban IS NULL OR iban_is_valid(iban));

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
ALTER TABLE accounts DROP CONSTRAINT IF EXISTS accounts_iban_checksum;
DROP FUNCTION IF EXISTS iban_generate(TEXT, TEXT);
DROP FUNCTION IF EXISTS iban_is_valid(TEXT);
-- +goose StatementEnd
