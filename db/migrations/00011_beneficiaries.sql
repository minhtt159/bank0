-- +goose Up
-- ─────────────────────────────────────────────────────────────────────────────
-- BENEFICIARIES & CONFIRMATION-OF-PAYEE — saved payees + the CoP/VOP resolver
-- A customer-facing directory of saved payees (docs/07) with no money state, plus
-- the server-side confirmation-of-payee verdict (resolve_account_by_iban) that both
-- the Go gate (sqlc IsKnownPayee) and evaluate_transfer (00015) lean on. The
-- resolver's recipient-risk signals read the guided_scenarios mule pool (00012) and
-- live fraud disputes (00013) via late-bound plpgsql, so this file loads ahead of
-- them. IBANs pass the same checksum authority (iban_is_valid, 00002) as accounts;
-- masking uses mask_name (00002).
-- ─────────────────────────────────────────────────────────────────────────────

-- ─────────────────────────────────────────────────────────────────────────────
-- beneficiaries  (saved payees for the customer web app, docs/07)
-- A directory entry the customer can fuzzy-search and transfer to; carries the
-- resolved destination account id so createTransfer is unchanged. No money state —
-- ownership is always scoped to owner_user_id (the JWT subject).
-- ─────────────────────────────────────────────────────────────────────────────
CREATE TABLE beneficiaries (
    id                 UUID PRIMARY KEY DEFAULT uuidv7(),
    owner_user_id      UUID NOT NULL REFERENCES users(id)    ON DELETE CASCADE,
    label              TEXT NOT NULL,
    credit_account_id  UUID NOT NULL REFERENCES accounts(id) ON DELETE RESTRICT,
    iban               VARCHAR(34) NOT NULL,
    owner_name_masked  TEXT NOT NULL DEFAULT '',
    created_at         TIMESTAMPTZ NOT NULL DEFAULT now(),
    CHECK (label <> ''),
    UNIQUE (owner_user_id, credit_account_id)
);
CREATE INDEX idx_beneficiaries_owner ON beneficiaries (owner_user_id);

-- Every persisted IBAN passes the same checksum authority as accounts (00002/00007 accounts).
ALTER TABLE beneficiaries
    ADD CONSTRAINT beneficiaries_iban_checksum CHECK (iban_is_valid(iban));

-- The ONE definition of the step-up "known payee" predicate: the credit account
-- is one of the caller's saved beneficiaries. Used by both the Go gate (sqlc
-- IsKnownPayee) and evaluate_transfer, so preview and enforcement can't diverge.
-- (Prior-posted-transfer relaxation is a possible follow-up — change it HERE.)
-- +goose StatementBegin
CREATE FUNCTION is_known_payee(p_owner UUID, p_credit UUID)
RETURNS BOOLEAN LANGUAGE sql STABLE AS $$
    SELECT EXISTS (
        SELECT 1 FROM beneficiaries
         WHERE owner_user_id = p_owner
           AND credit_account_id = p_credit);
$$;
-- +goose StatementEnd

-- +goose StatementBegin

-- ─────────────────────────────────────────────────────────────────────────────
-- Beneficiary / confirmation-of-payee functions
-- ─────────────────────────────────────────────────────────────────────────────

-- resolve_account_by_iban: confirmation-of-payee with the SERVER-SIDE verdict
-- (CoP/VOP four-outcome model; spec-banking-grade-hardening Rec 9). The caller
-- may supply the name the customer typed (p_name_hint); the DB — not each
-- client — decides match / close_match / no_match / unable, so web/iOS/Android
-- gate identically by construction. On a close_match the ACTUAL registered name
-- is returned (suggested_name) so the customer can fix a typo — that disclosure
-- is what CoP/VOP mandate; on no_match nothing is revealed beyond the mask.
-- gate: ok = proceed; awaiting_acknowledgement = warn, customer may proceed
-- (the VOP liability pivot); 'blocked' is reserved for the future risk seam.
-- Matching: exact after whitespace/case normalization -> match; pg_trgm
-- similarity >= 0.55 -> close_match (threshold is a product knob, hedged — the
-- EPC wire tokens are not public). Raises (-> 404) if not found / inactive.
CREATE OR REPLACE FUNCTION resolve_account_by_iban(
    p_iban      VARCHAR,
    p_name_hint TEXT DEFAULT NULL,
    p_caller    UUID DEFAULT NULL
)
RETURNS TABLE (account_id UUID, iban VARCHAR, owner_name_masked TEXT,
               match_result TEXT, reason_code TEXT, suggested_name TEXT,
               account_type TEXT, gate TEXT, checked_at TIMESTAMPTZ,
               recipient_risk TEXT, mule_suspected BOOLEAN,
               signals TEXT[], is_first_payment_to_payee BOOLEAN) AS $$
DECLARE
    v_id      UUID;
    v_iban    VARCHAR;
    v_full    TEXT;
    v_created TIMESTAMPTZ;
    v_hint    TEXT;
    v_reg     TEXT;
    v_match   TEXT;
    v_reason  TEXT;
    v_suggest TEXT;
    v_gate    TEXT;
    -- recipient-risk signals (Rec 11)
    v_signals TEXT[] := '{}';
    v_mule    BOOLEAN := FALSE;
    v_first   BOOLEAN := FALSE;
    v_risk    TEXT;
BEGIN
    SELECT a.id, a.iban, u.full_name, a.created_at INTO v_id, v_iban, v_full, v_created
    FROM accounts a
    JOIN users u ON u.id = a.user_id
    WHERE a.iban = p_iban AND a.kind = 'customer' AND a.status = 'active';
    IF NOT FOUND THEN
        RAISE EXCEPTION 'account with iban % not found', p_iban;
    END IF;

    IF p_name_hint IS NULL OR btrim(p_name_hint) = '' THEN
        v_match := 'unable';  v_reason := 'NAME_NOT_SUPPLIED';  v_gate := 'awaiting_acknowledgement';
    ELSE
        v_hint := lower(regexp_replace(btrim(p_name_hint), '\s+', ' ', 'g'));
        v_reg  := lower(regexp_replace(btrim(v_full),      '\s+', ' ', 'g'));
        IF v_hint = v_reg THEN
            v_match := 'match';       v_reason := 'MATCH';       v_gate := 'ok';
        ELSIF similarity(v_hint, v_reg) >= 0.55 THEN
            v_match := 'close_match'; v_reason := 'CLOSE_MATCH'; v_gate := 'awaiting_acknowledgement';
            v_suggest := v_full;   -- the one deliberate disclosure (CoP close-match)
        ELSE
            v_match := 'no_match';    v_reason := 'NO_MATCH';    v_gate := 'awaiting_acknowledgement';
        END IF;
    END IF;

    -- Recipient-risk signals (Rec 11). The operator-flagged mule pool
    -- (guided_scenarios) IS the rule seam; a destination sitting in it — or on
    -- the credit side of a live fraud dispute — resolves high. The caller-scoped
    -- signals (new payee / first payment) power first-payment friction.
    IF EXISTS (SELECT 1 FROM guided_scenarios gs
                WHERE gs.target_account_id = v_id AND gs.active) THEN
        v_signals := v_signals || 'mule_flagged'::text;  v_mule := TRUE;
    END IF;
    IF EXISTS (SELECT 1 FROM disputes d
                JOIN transfers t ON t.id = d.transfer_id
                WHERE t.credit_account_id = v_id
                  AND d.status IN ('open', 'under_review')
                  AND d.category IN ('fraud', 'unrecognised')) THEN
        v_signals := v_signals || 'reported'::text;  v_mule := TRUE;
    END IF;
    IF v_created > now() - INTERVAL '30 days' THEN
        v_signals := v_signals || 'recently_opened'::text;
    END IF;
    IF p_caller IS NOT NULL THEN
        IF NOT EXISTS (SELECT 1 FROM beneficiaries b
                        WHERE b.owner_user_id = p_caller AND b.credit_account_id = v_id) THEN
            v_signals := v_signals || 'new_payee'::text;
        END IF;
        v_first := NOT EXISTS (
            SELECT 1 FROM transfers t
            JOIN accounts da ON da.id = t.debit_account_id
            WHERE da.user_id = p_caller AND t.credit_account_id = v_id
              AND t.status IN ('posted', 'reversed'));
        IF v_first THEN
            v_signals := v_signals || 'first_payment'::text;
        END IF;
    END IF;
    v_risk := CASE
        WHEN v_mule THEN 'high'
        WHEN v_signals @> ARRAY['new_payee','first_payment','recently_opened'] THEN 'medium'
        ELSE 'low' END;

    RETURN QUERY SELECT v_id, v_iban, mask_name(v_full), v_match, v_reason,
                        v_suggest, 'personal'::text, v_gate, now(),
                        v_risk, v_mule, v_signals, v_first;
END;
$$ LANGUAGE plpgsql STABLE;

-- add_beneficiary: resolve the IBAN, then store the entry for p_owner. Rejects
-- saving your own account. Duplicate (owner, account) hits the UNIQUE index
-- (23505 -> 409).
CREATE OR REPLACE FUNCTION add_beneficiary(
    p_owner UUID,
    p_label TEXT,
    p_iban  VARCHAR
) RETURNS UUID AS $$
DECLARE
    v_acct     UUID;
    v_mask     TEXT;
    v_owner_of UUID;
    v_id       UUID;
BEGIN
    SELECT r.account_id, r.owner_name_masked INTO v_acct, v_mask
    FROM resolve_account_by_iban(p_iban) r;

    SELECT user_id INTO v_owner_of FROM accounts WHERE id = v_acct;
    IF v_owner_of = p_owner THEN
        RAISE EXCEPTION 'cannot add your own account as a beneficiary';
    END IF;

    INSERT INTO beneficiaries (owner_user_id, label, credit_account_id, iban, owner_name_masked)
    VALUES (p_owner, p_label, v_acct, p_iban, v_mask)
    RETURNING id INTO v_id;
    RETURN v_id;
END;
$$ LANGUAGE plpgsql;

-- delete_beneficiary: scoped delete; raises (-> 404) if it isn't the caller's.
CREATE OR REPLACE FUNCTION delete_beneficiary(p_owner UUID, p_id UUID)
RETURNS VOID AS $$
BEGIN
    DELETE FROM beneficiaries WHERE id = p_id AND owner_user_id = p_owner;
    IF NOT FOUND THEN
        RAISE EXCEPTION 'beneficiary % not found', p_id;
    END IF;
END;
$$ LANGUAGE plpgsql;

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DROP FUNCTION IF EXISTS delete_beneficiary(UUID, UUID);
DROP FUNCTION IF EXISTS add_beneficiary(UUID, TEXT, VARCHAR);
DROP FUNCTION IF EXISTS is_known_payee(UUID, UUID);
DROP FUNCTION IF EXISTS resolve_account_by_iban(VARCHAR, TEXT, UUID);
-- +goose StatementEnd
DROP TABLE IF EXISTS beneficiaries;
