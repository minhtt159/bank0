-- +goose Up
-- +goose StatementBegin

-- Saved payees for the customer web app (docs/08). A beneficiary is a directory
-- entry the customer can fuzzy-search and transfer to; it carries the resolved
-- destination account id so createTransfer is unchanged. No money state lives
-- here — ownership is always scoped to owner_user_id (the JWT subject).
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

-- mask_name keeps the first letter of each word and stars the rest:
-- 'Alice Anderson' -> 'A**** A*******'. In Postgres' regex, \Y is the
-- non-word-boundary assertion, so \Y\w matches a word char that is NOT at a word
-- boundary, i.e. every letter except each word's first.
CREATE OR REPLACE FUNCTION mask_name(p_name TEXT) RETURNS TEXT AS $$
    SELECT regexp_replace(COALESCE(p_name, ''), '\Y\w', '*', 'g');
$$ LANGUAGE sql IMMUTABLE;

-- resolve_account_by_iban: confirmation-of-payee. Returns the destination
-- account id + a MASKED owner name for an active customer account. Never
-- exposes the balance or the full name. Raises (-> 404) if not found / inactive.
CREATE OR REPLACE FUNCTION resolve_account_by_iban(p_iban VARCHAR)
RETURNS TABLE (account_id UUID, iban VARCHAR, owner_name_masked TEXT) AS $$
BEGIN
    RETURN QUERY
    SELECT a.id, a.iban, mask_name(u.full_name)
    FROM accounts a
    JOIN users u ON u.id = a.user_id
    WHERE a.iban = p_iban AND a.kind = 'customer' AND a.status = 'active';
    IF NOT FOUND THEN
        RAISE EXCEPTION 'account with iban % not found', p_iban;
    END IF;
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
DROP FUNCTION IF EXISTS resolve_account_by_iban(VARCHAR);
DROP FUNCTION IF EXISTS mask_name(TEXT);
DROP TABLE IF EXISTS beneficiaries;
-- +goose StatementEnd
