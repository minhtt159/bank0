-- +goose Up
-- ─────────────────────────────────────────────────────────────────────────────
-- AUXILIARY — IBAN PRIMITIVES
-- Pure, table-independent helper functions: the IBAN authority (ISO 7064
-- MOD-97-10 checksum + per-country length table) and the name-masking helper used
-- by confirmation-of-payee. These are IMMUTABLE and depend on nothing, so they
-- load before core-banking — where accounts.iban's checksum CHECK calls
-- iban_is_valid(). Algorithms verified in docs/11-iban-verification.md; they mirror
-- internal/iban (Go) and web/app/src/lib/iban.ts (TS) — keep all three in sync.
-- ─────────────────────────────────────────────────────────────────────────────

-- +goose StatementBegin

-- Single source of truth for the per-country total IBAN length (SWIFT IBAN Registry,
-- Release 99, Dec 2024; NO=15 .. RU=33). NULL = country not registered for IBAN.
-- Mirrors iban.CountryLengths (Go) and COUNTRY_LENGTHS (TS) — keep all three in sync.
CREATE OR REPLACE FUNCTION iban_country_length(p_cc TEXT) RETURNS INT
LANGUAGE sql IMMUTABLE PARALLEL SAFE AS $$
    SELECT CASE upper(p_cc)
        WHEN 'AL' THEN 28 WHEN 'AD' THEN 24 WHEN 'AT' THEN 20 WHEN 'AZ' THEN 28
        WHEN 'BH' THEN 22 WHEN 'BY' THEN 28 WHEN 'BE' THEN 16 WHEN 'BA' THEN 20
        WHEN 'BR' THEN 29 WHEN 'BG' THEN 22 WHEN 'BI' THEN 27 WHEN 'CR' THEN 22
        WHEN 'HR' THEN 21 WHEN 'CY' THEN 28 WHEN 'CZ' THEN 24 WHEN 'DK' THEN 18
        WHEN 'DJ' THEN 27 WHEN 'DO' THEN 28 WHEN 'TL' THEN 23 WHEN 'EG' THEN 29
        WHEN 'SV' THEN 28 WHEN 'EE' THEN 20 WHEN 'FK' THEN 18 WHEN 'FO' THEN 18
        WHEN 'FI' THEN 18 WHEN 'FR' THEN 27 WHEN 'GE' THEN 22 WHEN 'DE' THEN 22
        WHEN 'GI' THEN 23 WHEN 'GR' THEN 27 WHEN 'GL' THEN 18 WHEN 'GT' THEN 28
        WHEN 'HN' THEN 28 WHEN 'HU' THEN 28 WHEN 'IS' THEN 26 WHEN 'IQ' THEN 23
        WHEN 'IE' THEN 22 WHEN 'IL' THEN 23 WHEN 'IT' THEN 27 WHEN 'JO' THEN 30
        WHEN 'KZ' THEN 20 WHEN 'XK' THEN 20 WHEN 'KW' THEN 30 WHEN 'LV' THEN 21
        WHEN 'LB' THEN 28 WHEN 'LY' THEN 25 WHEN 'LI' THEN 21 WHEN 'LT' THEN 20
        WHEN 'LU' THEN 20 WHEN 'MT' THEN 31 WHEN 'MR' THEN 27 WHEN 'MU' THEN 30
        WHEN 'MC' THEN 27 WHEN 'MD' THEN 24 WHEN 'MN' THEN 20 WHEN 'ME' THEN 22
        WHEN 'NL' THEN 18 WHEN 'NI' THEN 28 WHEN 'MK' THEN 19 WHEN 'NO' THEN 15
        WHEN 'OM' THEN 23 WHEN 'PK' THEN 24 WHEN 'PS' THEN 29 WHEN 'PL' THEN 28
        WHEN 'PT' THEN 25 WHEN 'QA' THEN 29 WHEN 'RO' THEN 24 WHEN 'RU' THEN 33
        WHEN 'LC' THEN 32 WHEN 'SM' THEN 27 WHEN 'ST' THEN 25 WHEN 'SA' THEN 24
        WHEN 'RS' THEN 22 WHEN 'SC' THEN 31 WHEN 'SK' THEN 24 WHEN 'SI' THEN 19
        WHEN 'SO' THEN 23 WHEN 'ES' THEN 24 WHEN 'SD' THEN 18 WHEN 'SE' THEN 24
        WHEN 'CH' THEN 21 WHEN 'TN' THEN 24 WHEN 'TR' THEN 26 WHEN 'UA' THEN 29
        WHEN 'AE' THEN 23 WHEN 'GB' THEN 22 WHEN 'VA' THEN 22 WHEN 'VG' THEN 24
        WHEN 'YE' THEN 30
        ELSE NULL
    END;
$$;

-- iban_is_valid: structure (2 alpha, 2 digit, rest alnum) + registered country +
-- exact per-country length + MOD-97 == 1. The non-bypassable DB authority behind
-- the accounts/beneficiaries IBAN checksum CHECKs (core-banking / features below).
-- IMMUTABLE + a bounded per-character fold (accumulator stays < 9700, no bignum) →
-- cheap enough for a per-insert CHECK.
CREATE OR REPLACE FUNCTION iban_is_valid(p_iban TEXT) RETURNS BOOLEAN
LANGUAGE plpgsql IMMUTABLE PARALLEL SAFE AS $$
DECLARE
    s    TEXT;
    want INT;
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
    want := iban_country_length(substr(s, 1, 2));
    IF want IS NULL OR length(s) <> want THEN     -- unregistered country or wrong length
        RETURN FALSE;
    END IF;
    rot := substr(s, 5) || substr(s, 1, 4);       -- move first 4 chars to the end
    FOR i IN 1 .. length(rot) LOOP
        code := ascii(substr(rot, i, 1));
        IF code BETWEEN 48 AND 57 THEN            -- '0'..'9'
            rem := (rem * 10 + (code - 48)) % 97;
        ELSE                                      -- 'A'..'Z' -> 10..35
            rem := (rem * 100 + (code - 55)) % 97;
        END IF;
    END LOOP;
    RETURN rem = 1;
END;
$$;

-- iban_generate: build a checksum-valid IBAN from a country code + BBAN; reject an
-- unknown country / wrong BBAN length so the generator cannot mint an IBAN its own
-- validator (and Go's) would reject. check digits = 98 - mod97(BBAN || CC || '00').
-- Used by the seeds.
CREATE OR REPLACE FUNCTION iban_generate(p_country TEXT, p_bban TEXT) RETURNS TEXT
LANGUAGE plpgsql IMMUTABLE AS $$
DECLARE cc TEXT; bban TEXT; want INT; rot TEXT; rem INT := 0; i INT; code INT; chk INT;
BEGIN
    cc := upper(p_country);
    bban := upper(p_bban);
    want := iban_country_length(cc);
    IF want IS NULL THEN
        RAISE EXCEPTION 'iban_generate: unregistered country code %', cc;
    END IF;
    IF length(bban) <> want - 4 THEN
        RAISE EXCEPTION 'iban_generate: % BBAN length % invalid (want %)', cc, length(bban), want - 4;
    END IF;
    rot := bban || cc || '00';
    FOR i IN 1 .. length(rot) LOOP
        code := ascii(substr(rot, i, 1));
        IF code BETWEEN 48 AND 57 THEN
            rem := (rem * 10 + (code - 48)) % 97;
        ELSE
            rem := (rem * 100 + (code - 55)) % 97;
        END IF;
    END LOOP;
    chk := 98 - rem;
    RETURN cc || lpad(chk::text, 2, '0') || bban;
END;
$$;

-- mask_name keeps the first letter of each word and stars the rest:
-- 'Alice Anderson' -> 'A**** A*******'. In Postgres' regex, \Y is the
-- non-word-boundary assertion, so \Y\w matches a word char that is NOT at a word
-- boundary, i.e. every letter except each word's first. Used by confirmation-of-payee
-- (resolve_account_by_iban) and the guided-transfer resolver (features below).
CREATE OR REPLACE FUNCTION mask_name(p_name TEXT) RETURNS TEXT AS $$
    SELECT regexp_replace(COALESCE(p_name, ''), '\Y\w', '*', 'g');
$$ LANGUAGE sql IMMUTABLE;

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DROP FUNCTION IF EXISTS mask_name(TEXT);
DROP FUNCTION IF EXISTS iban_generate(TEXT, TEXT);
DROP FUNCTION IF EXISTS iban_is_valid(TEXT);
DROP FUNCTION IF EXISTS iban_country_length(TEXT);
-- +goose StatementEnd
