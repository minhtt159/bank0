-- +goose Up
-- +goose StatementBegin

-- API-8: operator-tweakable bank policy lives in the DB, not app config — so the
-- maker-checker threshold (and friends) are the source of truth and can be changed
-- from the admin console without a redeploy. Single-row table (id CHECK keeps it a
-- singleton); column DEFAULTs are the sensible out-of-the-box policy.
CREATE TABLE bank_settings (
    id                            BOOLEAN     PRIMARY KEY DEFAULT TRUE CHECK (id),
    -- 4-eyes: money moves strictly above this need a second approver. €10,000.00.
    maker_checker_threshold_minor BIGINT      NOT NULL DEFAULT 1000000 CHECK (maker_checker_threshold_minor >= 0),
    -- per-account default transfer limit applied at account creation. €500.00.
    default_transfer_limit_minor  BIGINT      NOT NULL DEFAULT 50000   CHECK (default_transfer_limit_minor >= 0),
    updated_at                    TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_by                    UUID        REFERENCES users(id) ON DELETE SET NULL
);
INSERT INTO bank_settings (id) VALUES (TRUE);  -- the one row, all defaults

-- requires_approval: the maker-checker DECISION lives in the DB (rule 1), not Go.
-- Returns the verdict + the active threshold (so callers can render it without a
-- second read).
CREATE OR REPLACE FUNCTION requires_approval(p_amount_minor BIGINT)
RETURNS TABLE(required BOOLEAN, threshold_minor BIGINT) AS $$
    SELECT p_amount_minor > s.maker_checker_threshold_minor, s.maker_checker_threshold_minor
    FROM bank_settings s WHERE s.id;
$$ LANGUAGE sql STABLE;

CREATE OR REPLACE FUNCTION default_transfer_limit() RETURNS BIGINT AS $$
    SELECT default_transfer_limit_minor FROM bank_settings WHERE id;
$$ LANGUAGE sql STABLE;

CREATE OR REPLACE FUNCTION update_bank_settings(
    p_threshold_minor     BIGINT,
    p_default_limit_minor BIGINT,
    p_actor               UUID
) RETURNS VOID AS $$
    UPDATE bank_settings
       SET maker_checker_threshold_minor = p_threshold_minor,
           default_transfer_limit_minor  = p_default_limit_minor,
           updated_at = now(), updated_by = p_actor
     WHERE id;
$$ LANGUAGE sql;

-- create_account now sources its default limit from bank_settings when the caller
-- passes <= 0 (was a hardcoded 50000 in two Go call sites). Same signature; the
-- DEFAULT becomes 0 = "use the configured default".
CREATE OR REPLACE FUNCTION create_account(
    p_user_id              UUID,
    p_iban                 VARCHAR(34),
    p_pin                  TEXT,
    p_transfer_limit_minor BIGINT DEFAULT 0
) RETURNS UUID AS $$
DECLARE
    v_account_id UUID;
    v_is_default BOOLEAN;
BEGIN
    PERFORM 1 FROM users WHERE id = p_user_id FOR UPDATE;
    IF NOT FOUND THEN
        RAISE EXCEPTION 'user % does not exist', p_user_id;
    END IF;

    IF p_transfer_limit_minor <= 0 THEN
        p_transfer_limit_minor := default_transfer_limit();
    END IF;

    v_is_default := NOT EXISTS (
        SELECT 1 FROM accounts WHERE user_id = p_user_id AND is_default
    );

    INSERT INTO accounts (user_id, kind, iban, pin_hash, transfer_limit_minor, is_default)
    VALUES (p_user_id, 'customer', p_iban,
            crypt(p_pin, gen_salt('bf', 10)), p_transfer_limit_minor, v_is_default)
    RETURNING id INTO v_account_id;

    RETURN v_account_id;
END;
$$ LANGUAGE plpgsql;

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
CREATE OR REPLACE FUNCTION create_account(
    p_user_id              UUID,
    p_iban                 VARCHAR(34),
    p_pin                  TEXT,
    p_transfer_limit_minor BIGINT DEFAULT 50000
) RETURNS UUID AS $$
DECLARE
    v_account_id UUID;
    v_is_default BOOLEAN;
BEGIN
    PERFORM 1 FROM users WHERE id = p_user_id FOR UPDATE;
    IF NOT FOUND THEN
        RAISE EXCEPTION 'user % does not exist', p_user_id;
    END IF;

    v_is_default := NOT EXISTS (
        SELECT 1 FROM accounts WHERE user_id = p_user_id AND is_default
    );

    INSERT INTO accounts (user_id, kind, iban, pin_hash, transfer_limit_minor, is_default)
    VALUES (p_user_id, 'customer', p_iban,
            crypt(p_pin, gen_salt('bf', 10)), p_transfer_limit_minor, v_is_default)
    RETURNING id INTO v_account_id;

    RETURN v_account_id;
END;
$$ LANGUAGE plpgsql;
-- +goose StatementEnd
DROP FUNCTION IF EXISTS update_bank_settings(BIGINT, BIGINT, UUID);
DROP FUNCTION IF EXISTS default_transfer_limit();
DROP FUNCTION IF EXISTS requires_approval(BIGINT);
DROP TABLE IF EXISTS bank_settings;
