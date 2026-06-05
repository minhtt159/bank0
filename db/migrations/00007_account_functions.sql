-- +goose Up
-- +goose StatementBegin

-- account_available = balance - SUM(active holds). The funds figure the
-- transfer functions and the operator console both rely on.
CREATE OR REPLACE FUNCTION account_available(p_account_id UUID) RETURNS BIGINT AS $$
    SELECT a.balance_minor
         - COALESCE((SELECT SUM(h.amount_minor) FROM holds h
                      WHERE h.account_id = a.id AND h.status = 'active'), 0)
    FROM accounts a
    WHERE a.id = p_account_id;
$$ LANGUAGE sql STABLE;

-- create_account: first account for a user becomes the default. The partial
-- unique index (uq_accounts_one_default) guarantees there is never more than one.
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
    -- Lock the owner row so two concurrent first-inserts can't both claim default.
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

-- set_default_account: clear-then-set in one statement-pair under a user lock.
CREATE OR REPLACE FUNCTION set_default_account(
    p_user_id    UUID,
    p_account_id UUID
) RETURNS VOID AS $$
BEGIN
    PERFORM 1 FROM users WHERE id = p_user_id FOR UPDATE;
    IF NOT EXISTS (SELECT 1 FROM accounts WHERE id = p_account_id AND user_id = p_user_id) THEN
        RAISE EXCEPTION 'account % does not belong to user %', p_account_id, p_user_id;
    END IF;

    UPDATE accounts SET is_default = FALSE WHERE user_id = p_user_id AND is_default;
    UPDATE accounts SET is_default = TRUE  WHERE id = p_account_id;
END;
$$ LANGUAGE plpgsql;

-- set_account_status: active | frozen | closed. A frozen/closed account is
-- rejected by request_transfer (status <> 'active').
CREATE OR REPLACE FUNCTION set_account_status(
    p_account_id UUID,
    p_status     account_status
) RETURNS VOID AS $$
BEGIN
    UPDATE accounts SET status = p_status WHERE id = p_account_id;
    IF NOT FOUND THEN
        RAISE EXCEPTION 'account % does not exist', p_account_id;
    END IF;
END;
$$ LANGUAGE plpgsql;

-- update_transfer_limit. NOTE: there is deliberately NO function to set balance
-- directly — balance changes only via the ledger (see deposit/withdraw/transfer).
CREATE OR REPLACE FUNCTION update_transfer_limit(
    p_account_id           UUID,
    p_transfer_limit_minor BIGINT
) RETURNS VOID AS $$
BEGIN
    IF p_transfer_limit_minor < 0 THEN
        RAISE EXCEPTION 'transfer limit must be >= 0';
    END IF;
    UPDATE accounts SET transfer_limit_minor = p_transfer_limit_minor WHERE id = p_account_id;
    IF NOT FOUND THEN
        RAISE EXCEPTION 'account % does not exist', p_account_id;
    END IF;
END;
$$ LANGUAGE plpgsql;

-- +goose StatementEnd

-- +goose Down
DROP FUNCTION IF EXISTS update_transfer_limit(UUID, BIGINT);
DROP FUNCTION IF EXISTS set_account_status(UUID, account_status);
DROP FUNCTION IF EXISTS set_default_account(UUID, UUID);
DROP FUNCTION IF EXISTS create_account(UUID, VARCHAR, TEXT, BIGINT);
DROP FUNCTION IF EXISTS account_available(UUID);
