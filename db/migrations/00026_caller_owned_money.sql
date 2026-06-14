-- +goose Up
-- +goose StatementBegin

-- TRANSFER-1: client-surface money entrypoints that enforce caller ownership IN THE
-- DB, so the Go handlers no longer make a separate ownership-probe round trip before
-- the real call. The admin/console surface keeps calling the unguarded base functions
-- (transfer/post_transfer/cancel_transfer) — operators act on the bank's behalf.
-- Thin wrappers (not new params on the base functions) keep the core money path,
-- its lock ordering, and the deposit/withdraw/maker-checker callers untouched.

-- assert_caller_owns: raise 42501 (-> 403) unless p_subject owns p_account. A
-- nonexistent or system-owned (NULL) account is treated as not-owned, so existence
-- is never leaked on the create path (the caller supplied the id, so 403 not 404).
CREATE OR REPLACE FUNCTION assert_caller_owns(p_subject UUID, p_account UUID) RETURNS VOID AS $$
DECLARE v_owner UUID;
BEGIN
    SELECT user_id INTO v_owner FROM accounts WHERE id = p_account;
    IF v_owner IS NULL OR v_owner <> p_subject THEN
        RAISE EXCEPTION 'debit account not owned by caller' USING ERRCODE = '42501';
    END IF;
END;
$$ LANGUAGE plpgsql;

-- client_transfer: the caller may only debit an account they own; then auto-post via
-- transfer(). Same RETURNS shape as transfer() so the handler is unchanged downstream.
CREATE OR REPLACE FUNCTION client_transfer(
    p_caller_subject  UUID,
    p_idempotency_key TEXT,
    p_debit_account   UUID,
    p_credit_account  UUID,
    p_amount_minor    BIGINT,
    p_description     TEXT DEFAULT ''
) RETURNS TABLE (transfer_id UUID, status transfer_status, was_replay BOOLEAN) AS $$
BEGIN
    PERFORM assert_caller_owns(p_caller_subject, p_debit_account);
    RETURN QUERY
        SELECT t.transfer_id, t.status, t.was_replay
        FROM transfer(p_idempotency_key, p_debit_account, p_credit_account,
                      p_amount_minor, p_description, 'transfer') t;
END;
$$ LANGUAGE plpgsql;

-- client_post_transfer / client_cancel_transfer: act on a pending transfer only if
-- the caller owns its DEBIT account. A transfer the caller doesn't own (or that does
-- not exist) raises 'not found' -> 404, hiding existence — the id is a secret, unlike
-- the account ids the caller supplies to client_transfer.
CREATE OR REPLACE FUNCTION client_post_transfer(p_caller_subject UUID, p_transfer_id UUID)
RETURNS transfer_status AS $$
DECLARE v_owner UUID;
BEGIN
    SELECT a.user_id INTO v_owner
    FROM transfers t JOIN accounts a ON a.id = t.debit_account_id
    WHERE t.id = p_transfer_id;
    IF v_owner IS NULL OR v_owner <> p_caller_subject THEN
        RAISE EXCEPTION 'transfer % not found', p_transfer_id;
    END IF;
    RETURN post_transfer(p_transfer_id);
END;
$$ LANGUAGE plpgsql;

CREATE OR REPLACE FUNCTION client_cancel_transfer(p_caller_subject UUID, p_transfer_id UUID, p_reason TEXT DEFAULT '')
RETURNS transfer_status AS $$
DECLARE v_owner UUID;
BEGIN
    SELECT a.user_id INTO v_owner
    FROM transfers t JOIN accounts a ON a.id = t.debit_account_id
    WHERE t.id = p_transfer_id;
    IF v_owner IS NULL OR v_owner <> p_caller_subject THEN
        RAISE EXCEPTION 'transfer % not found', p_transfer_id;
    END IF;
    RETURN cancel_transfer(p_transfer_id, p_reason);
END;
$$ LANGUAGE plpgsql;

-- +goose StatementEnd

-- +goose Down
DROP FUNCTION IF EXISTS client_cancel_transfer(UUID, UUID, TEXT);
DROP FUNCTION IF EXISTS client_post_transfer(UUID, UUID);
DROP FUNCTION IF EXISTS client_transfer(UUID, TEXT, UUID, UUID, BIGINT, TEXT);
DROP FUNCTION IF EXISTS assert_caller_owns(UUID, UUID);
