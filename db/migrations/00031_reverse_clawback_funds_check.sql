-- +goose Up
-- +goose StatementBegin

-- ─────────────────────────────────────────────────────────────────────────────
-- reverse_transfer (clawback safety): posted -> reversed. Same behaviour as the
-- 00008 definition — same signature, idempotency handling, status guards, ledger
-- legs and status flip — EXCEPT it now verifies up front that the counterparty
-- (the original CREDIT account, which the reversal DEBITS) still holds enough to
-- be clawed back.
--
-- Without this check, reversing a transfer whose recipient has already SPENT the
-- money drives their balance_minor below zero and trips the accounts CHECK
-- (kind='system' OR balance_minor >= 0, see 00003) — surfacing the raw Postgres
-- constraint name to the operator. We instead lock that account FOR UPDATE and
-- RAISE a clear, mappable check_violation so mapDBError routes it to
-- insufficient_funds / HTTP 422.
--
-- NOTE: reversal deliberately does NOT enforce the active-account check that the
-- forward path (request_transfer) applies. A clawback is an operator remedy and
-- may legitimately credit a frozen or closed account; do not "fix" this by
-- adding a status guard.
-- ─────────────────────────────────────────────────────────────────────────────
CREATE OR REPLACE FUNCTION reverse_transfer(
    p_transfer_id     UUID,
    p_idempotency_key TEXT,
    p_reason          TEXT
) RETURNS UUID AS $$
DECLARE
    v_orig     transfers%ROWTYPE;
    v_existing idempotency_keys%ROWTYPE;
    v_rev_id   UUID;
    v_cp       accounts%ROWTYPE;
    v_hash     TEXT := encode(digest('reverse|' || p_transfer_id::text, 'sha256'), 'hex');
BEGIN
    INSERT INTO idempotency_keys (key, scope, request_hash, status)
    VALUES (p_idempotency_key, 'reversal', v_hash, 'in_progress')
    ON CONFLICT (key) DO NOTHING;

    IF NOT FOUND THEN
        SELECT * INTO v_existing FROM idempotency_keys WHERE key = p_idempotency_key;
        IF v_existing.request_hash <> v_hash THEN
            RAISE EXCEPTION 'idempotency key % reused with different parameters', p_idempotency_key
                USING ERRCODE = 'check_violation';
        END IF;
        RETURN v_existing.transfer_id;
    END IF;

    SELECT * INTO v_orig FROM transfers WHERE id = p_transfer_id FOR UPDATE;
    IF NOT FOUND THEN RAISE EXCEPTION 'transfer % not found', p_transfer_id; END IF;
    IF v_orig.status = 'reversed' THEN RAISE EXCEPTION 'transfer % already reversed', p_transfer_id; END IF;
    IF v_orig.status <> 'posted' THEN
        RAISE EXCEPTION 'can only reverse a posted transfer (state %)', v_orig.status
            USING ERRCODE = 'check_violation';
    END IF;

    -- Clawback safety: lock the account the reversal will DEBIT (the original
    -- CREDIT account) and confirm it still holds enough to be reversed. Reversal
    -- intentionally bypasses the active-account check applied on the forward path
    -- (see the header note) — only the funds check is enforced here.
    SELECT * INTO v_cp FROM accounts WHERE id = v_orig.credit_account_id FOR UPDATE;
    IF v_cp.kind <> 'system' AND v_cp.balance_minor < v_orig.amount_minor THEN
        RAISE EXCEPTION 'cannot reverse transfer %: recipient has insufficient funds to claw back', p_transfer_id
            USING ERRCODE = 'check_violation';
    END IF;

    INSERT INTO transfers (debit_account_id, credit_account_id, amount_minor, currency,
                           status, kind, reverses_id, description, idempotency_key, posted_at)
    VALUES (v_orig.credit_account_id, v_orig.debit_account_id, v_orig.amount_minor, v_orig.currency,
            'posted', 'reversal', p_transfer_id, COALESCE(p_reason, ''), p_idempotency_key, now())
    RETURNING id INTO v_rev_id;

    INSERT INTO ledger_entries (transfer_id, account_id, direction, amount_minor, currency, balance_after)
    VALUES (v_rev_id, v_orig.credit_account_id, 'debit',  v_orig.amount_minor, v_orig.currency, 0),
           (v_rev_id, v_orig.debit_account_id,  'credit', v_orig.amount_minor, v_orig.currency, 0);

    UPDATE transfers SET status = 'reversed' WHERE id = p_transfer_id;
    UPDATE idempotency_keys SET status = 'completed', transfer_id = v_rev_id WHERE key = p_idempotency_key;
    RETURN v_rev_id;
END;
$$ LANGUAGE plpgsql;

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin

-- Restore the original 00008 definition: no clawback funds check.
CREATE OR REPLACE FUNCTION reverse_transfer(
    p_transfer_id     UUID,
    p_idempotency_key TEXT,
    p_reason          TEXT
) RETURNS UUID AS $$
DECLARE
    v_orig     transfers%ROWTYPE;
    v_existing idempotency_keys%ROWTYPE;
    v_rev_id   UUID;
    v_hash     TEXT := encode(digest('reverse|' || p_transfer_id::text, 'sha256'), 'hex');
BEGIN
    INSERT INTO idempotency_keys (key, scope, request_hash, status)
    VALUES (p_idempotency_key, 'reversal', v_hash, 'in_progress')
    ON CONFLICT (key) DO NOTHING;

    IF NOT FOUND THEN
        SELECT * INTO v_existing FROM idempotency_keys WHERE key = p_idempotency_key;
        IF v_existing.request_hash <> v_hash THEN
            RAISE EXCEPTION 'idempotency key % reused with different parameters', p_idempotency_key
                USING ERRCODE = 'check_violation';
        END IF;
        RETURN v_existing.transfer_id;
    END IF;

    SELECT * INTO v_orig FROM transfers WHERE id = p_transfer_id FOR UPDATE;
    IF NOT FOUND THEN RAISE EXCEPTION 'transfer % not found', p_transfer_id; END IF;
    IF v_orig.status = 'reversed' THEN RAISE EXCEPTION 'transfer % already reversed', p_transfer_id; END IF;
    IF v_orig.status <> 'posted' THEN
        RAISE EXCEPTION 'can only reverse a posted transfer (state %)', v_orig.status
            USING ERRCODE = 'check_violation';
    END IF;

    INSERT INTO transfers (debit_account_id, credit_account_id, amount_minor, currency,
                           status, kind, reverses_id, description, idempotency_key, posted_at)
    VALUES (v_orig.credit_account_id, v_orig.debit_account_id, v_orig.amount_minor, v_orig.currency,
            'posted', 'reversal', p_transfer_id, COALESCE(p_reason, ''), p_idempotency_key, now())
    RETURNING id INTO v_rev_id;

    INSERT INTO ledger_entries (transfer_id, account_id, direction, amount_minor, currency, balance_after)
    VALUES (v_rev_id, v_orig.credit_account_id, 'debit',  v_orig.amount_minor, v_orig.currency, 0),
           (v_rev_id, v_orig.debit_account_id,  'credit', v_orig.amount_minor, v_orig.currency, 0);

    UPDATE transfers SET status = 'reversed' WHERE id = p_transfer_id;
    UPDATE idempotency_keys SET status = 'completed', transfer_id = v_rev_id WHERE key = p_idempotency_key;
    RETURN v_rev_id;
END;
$$ LANGUAGE plpgsql;

-- +goose StatementEnd
