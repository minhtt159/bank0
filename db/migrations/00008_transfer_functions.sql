-- +goose Up
-- +goose StatementBegin

-- ─────────────────────────────────────────────────────────────────────────────
-- request_transfer: create a pending transfer + an authorization hold.
-- Idempotent: a replayed key returns the original result and never double-posts.
-- ─────────────────────────────────────────────────────────────────────────────
CREATE OR REPLACE FUNCTION request_transfer(
    p_idempotency_key TEXT,
    p_debit_account   UUID,
    p_credit_account  UUID,
    p_amount_minor    BIGINT,
    p_description     TEXT          DEFAULT '',
    p_kind            transfer_kind DEFAULT 'transfer',
    p_hold_ttl        INTERVAL      DEFAULT INTERVAL '15 minutes'
) RETURNS TABLE (transfer_id UUID, status transfer_status, was_replay BOOLEAN) AS $$
DECLARE
    v_hash      TEXT := encode(digest(
        coalesce(p_debit_account::text,'') || '|' ||
        coalesce(p_credit_account::text,'') || '|' ||
        p_amount_minor::text || '|' || p_kind::text, 'sha256'), 'hex');
    v_existing  idempotency_keys%ROWTYPE;
    v_debit     accounts%ROWTYPE;
    v_credit    accounts%ROWTYPE;
    v_available BIGINT;
    v_id        UUID;
BEGIN
    IF p_idempotency_key IS NULL OR p_idempotency_key = '' THEN
        RAISE EXCEPTION 'idempotency key is required' USING ERRCODE = 'check_violation';
    END IF;

    -- (a) Idempotency gate: first writer wins.
    INSERT INTO idempotency_keys (key, scope, request_hash, status)
    VALUES (p_idempotency_key, 'transfer', v_hash, 'in_progress')
    ON CONFLICT (key) DO NOTHING;

    IF NOT FOUND THEN
        SELECT * INTO v_existing FROM idempotency_keys WHERE key = p_idempotency_key;
        IF v_existing.request_hash <> v_hash THEN
            RAISE EXCEPTION 'idempotency key % reused with different parameters', p_idempotency_key
                USING ERRCODE = 'check_violation';
        END IF;
        IF v_existing.status = 'in_progress' THEN
            RAISE EXCEPTION 'request for key % is still in progress', p_idempotency_key
                USING ERRCODE = 'object_in_use';
        END IF;
        RETURN QUERY
            SELECT t.id, t.status, TRUE FROM transfers t WHERE t.id = v_existing.transfer_id;
        RETURN;
    END IF;

    -- (b) Validate. Lock both accounts in a stable order to avoid deadlocks.
    IF p_debit_account = p_credit_account THEN
        RAISE EXCEPTION 'debit and credit accounts must differ';
    END IF;

    IF p_debit_account < p_credit_account THEN
        SELECT * INTO v_debit  FROM accounts WHERE id = p_debit_account  FOR UPDATE;
        SELECT * INTO v_credit FROM accounts WHERE id = p_credit_account FOR UPDATE;
    ELSE
        SELECT * INTO v_credit FROM accounts WHERE id = p_credit_account FOR UPDATE;
        SELECT * INTO v_debit  FROM accounts WHERE id = p_debit_account  FOR UPDATE;
    END IF;

    IF v_debit.id  IS NULL THEN RAISE EXCEPTION 'debit account % not found',  p_debit_account; END IF;
    IF v_credit.id IS NULL THEN RAISE EXCEPTION 'credit account % not found', p_credit_account; END IF;
    IF v_debit.status  <> 'active' THEN RAISE EXCEPTION 'debit account % not active',  p_debit_account; END IF;
    IF v_credit.status <> 'active' THEN RAISE EXCEPTION 'credit account % not active', p_credit_account; END IF;
    IF v_debit.currency <> v_credit.currency THEN RAISE EXCEPTION 'currency mismatch'; END IF;
    IF p_amount_minor <= 0 THEN RAISE EXCEPTION 'amount must be positive'; END IF;

    IF v_debit.kind = 'customer' THEN
        IF p_amount_minor > v_debit.transfer_limit_minor THEN
            RAISE EXCEPTION 'amount % exceeds transfer limit %', p_amount_minor, v_debit.transfer_limit_minor;
        END IF;

        SELECT v_debit.balance_minor - COALESCE(SUM(amount_minor), 0) INTO v_available
        FROM holds WHERE account_id = p_debit_account AND holds.status = 'active';

        IF v_available < p_amount_minor THEN
            RAISE EXCEPTION 'insufficient available funds: have %, need %', v_available, p_amount_minor
                USING ERRCODE = 'check_violation';
        END IF;
    END IF;

    -- (c) Create the pending transfer + the hold.
    INSERT INTO transfers (debit_account_id, credit_account_id, amount_minor,
                           currency, status, kind, description, idempotency_key)
    VALUES (p_debit_account, p_credit_account, p_amount_minor,
            v_debit.currency, 'pending', p_kind, p_description, p_idempotency_key)
    RETURNING id INTO v_id;

    INSERT INTO holds (account_id, transfer_id, amount_minor, status, expires_at)
    VALUES (p_debit_account, v_id, p_amount_minor, 'active', now() + p_hold_ttl);

    -- (d) Record result against the idempotency key.
    UPDATE idempotency_keys
       SET status = 'completed', transfer_id = v_id,
           response = jsonb_build_object('transfer_id', v_id, 'status', 'pending')
     WHERE key = p_idempotency_key;

    RETURN QUERY SELECT v_id, 'pending'::transfer_status, FALSE;
END;
$$ LANGUAGE plpgsql;

-- ─────────────────────────────────────────────────────────────────────────────
-- post_transfer: pending -> posted. Writes the two ledger legs (the balance
-- trigger does the rest), captures the hold. Idempotent on an already-posted row.
-- ─────────────────────────────────────────────────────────────────────────────
CREATE OR REPLACE FUNCTION post_transfer(p_transfer_id UUID) RETURNS transfer_status AS $$
DECLARE v_t transfers%ROWTYPE;
BEGIN
    SELECT * INTO v_t FROM transfers WHERE id = p_transfer_id FOR UPDATE;
    IF NOT FOUND THEN RAISE EXCEPTION 'transfer % not found', p_transfer_id; END IF;

    IF v_t.status = 'posted' THEN RETURN 'posted'; END IF;          -- idempotent no-op
    IF v_t.status <> 'pending' THEN
        RAISE EXCEPTION 'cannot post transfer in state %', v_t.status USING ERRCODE = 'check_violation';
    END IF;

    INSERT INTO ledger_entries (transfer_id, account_id, direction, amount_minor, currency, balance_after)
    VALUES (p_transfer_id, v_t.debit_account_id,  'debit',  v_t.amount_minor, v_t.currency, 0),
           (p_transfer_id, v_t.credit_account_id, 'credit', v_t.amount_minor, v_t.currency, 0);
    -- balance_after is overwritten by the BEFORE INSERT trigger.

    UPDATE holds SET status = 'captured', released_at = now()
     WHERE transfer_id = p_transfer_id AND status = 'active';

    UPDATE transfers SET status = 'posted', posted_at = now() WHERE id = p_transfer_id;
    RETURN 'posted';
END;
$$ LANGUAGE plpgsql;

-- ─────────────────────────────────────────────────────────────────────────────
-- cancel_transfer / fail_transfer: pending -> canceled|failed, release the hold.
-- ─────────────────────────────────────────────────────────────────────────────
CREATE OR REPLACE FUNCTION cancel_transfer(p_transfer_id UUID, p_reason TEXT DEFAULT '')
RETURNS transfer_status AS $$
DECLARE v_status transfer_status;
BEGIN
    SELECT status INTO v_status FROM transfers WHERE id = p_transfer_id FOR UPDATE;
    IF NOT FOUND THEN RAISE EXCEPTION 'transfer % not found', p_transfer_id; END IF;
    IF v_status = 'canceled' THEN RETURN 'canceled'; END IF;        -- idempotent
    IF v_status <> 'pending' THEN
        RAISE EXCEPTION 'cannot cancel transfer in state %', v_status USING ERRCODE = 'check_violation';
    END IF;

    UPDATE holds SET status = 'released', released_at = now()
     WHERE transfer_id = p_transfer_id AND status = 'active';
    UPDATE transfers SET status = 'canceled', failure_reason = NULLIF(p_reason, '')
     WHERE id = p_transfer_id;
    RETURN 'canceled';
END;
$$ LANGUAGE plpgsql;

CREATE OR REPLACE FUNCTION fail_transfer(p_transfer_id UUID, p_reason TEXT DEFAULT '')
RETURNS transfer_status AS $$
DECLARE v_status transfer_status;
BEGIN
    SELECT status INTO v_status FROM transfers WHERE id = p_transfer_id FOR UPDATE;
    IF NOT FOUND THEN RAISE EXCEPTION 'transfer % not found', p_transfer_id; END IF;
    IF v_status = 'failed' THEN RETURN 'failed'; END IF;
    IF v_status <> 'pending' THEN
        RAISE EXCEPTION 'cannot fail transfer in state %', v_status USING ERRCODE = 'check_violation';
    END IF;

    UPDATE holds SET status = 'released', released_at = now()
     WHERE transfer_id = p_transfer_id AND status = 'active';
    UPDATE transfers SET status = 'failed', failure_reason = NULLIF(p_reason, '')
     WHERE id = p_transfer_id;
    RETURN 'failed';
END;
$$ LANGUAGE plpgsql;

-- ─────────────────────────────────────────────────────────────────────────────
-- transfer: the auto-post convenience (request + post in one txn). Idempotent.
-- This is what POST /transfers calls by default.
-- ─────────────────────────────────────────────────────────────────────────────
CREATE OR REPLACE FUNCTION transfer(
    p_idempotency_key TEXT,
    p_debit_account   UUID,
    p_credit_account  UUID,
    p_amount_minor    BIGINT,
    p_description     TEXT          DEFAULT '',
    p_kind            transfer_kind DEFAULT 'transfer'
) RETURNS TABLE (transfer_id UUID, status transfer_status, was_replay BOOLEAN) AS $$
DECLARE v_id UUID; v_status transfer_status; v_replay BOOLEAN;
BEGIN
    SELECT r.transfer_id, r.status, r.was_replay INTO v_id, v_status, v_replay
    FROM request_transfer(p_idempotency_key, p_debit_account, p_credit_account,
                          p_amount_minor, p_description, p_kind) r;

    IF NOT v_replay AND v_status = 'pending' THEN
        v_status := post_transfer(v_id);
    END IF;

    RETURN QUERY SELECT v_id, v_status, v_replay;
END;
$$ LANGUAGE plpgsql;

-- ─────────────────────────────────────────────────────────────────────────────
-- reverse_transfer: posted -> reversed. Appends inverse ledger entries via a new
-- 'reversal' transfer. The original is never edited. Idempotent on the key.
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

-- ─────────────────────────────────────────────────────────────────────────────
-- deposit / withdraw: money crossing the bank boundary, via external_clearing.
-- A deposit is a posted transfer external_clearing -> customer (no money minted).
-- ─────────────────────────────────────────────────────────────────────────────
CREATE OR REPLACE FUNCTION deposit(
    p_idempotency_key TEXT,
    p_account_id      UUID,
    p_amount_minor    BIGINT,
    p_description     TEXT DEFAULT 'Deposit'
) RETURNS UUID AS $$
DECLARE v_ext UUID; v_id UUID;
BEGIN
    SELECT id INTO v_ext FROM accounts WHERE system_code = 'EXTERNAL_CLEARING';
    IF NOT FOUND THEN RAISE EXCEPTION 'EXTERNAL_CLEARING system account missing (run seed)'; END IF;
    SELECT t.transfer_id INTO v_id
    FROM transfer(p_idempotency_key, v_ext, p_account_id, p_amount_minor, p_description, 'deposit') t;
    RETURN v_id;
END;
$$ LANGUAGE plpgsql;

CREATE OR REPLACE FUNCTION withdraw(
    p_idempotency_key TEXT,
    p_account_id      UUID,
    p_amount_minor    BIGINT,
    p_description     TEXT DEFAULT 'Withdrawal'
) RETURNS UUID AS $$
DECLARE v_ext UUID; v_id UUID;
BEGIN
    SELECT id INTO v_ext FROM accounts WHERE system_code = 'EXTERNAL_CLEARING';
    IF NOT FOUND THEN RAISE EXCEPTION 'EXTERNAL_CLEARING system account missing (run seed)'; END IF;
    SELECT t.transfer_id INTO v_id
    FROM transfer(p_idempotency_key, p_account_id, v_ext, p_amount_minor, p_description, 'withdrawal') t;
    RETURN v_id;
END;
$$ LANGUAGE plpgsql;

-- +goose StatementEnd

-- +goose Down
DROP FUNCTION IF EXISTS withdraw(TEXT, UUID, BIGINT, TEXT);
DROP FUNCTION IF EXISTS deposit(TEXT, UUID, BIGINT, TEXT);
DROP FUNCTION IF EXISTS reverse_transfer(UUID, TEXT, TEXT);
DROP FUNCTION IF EXISTS transfer(TEXT, UUID, UUID, BIGINT, TEXT, transfer_kind);
DROP FUNCTION IF EXISTS fail_transfer(UUID, TEXT);
DROP FUNCTION IF EXISTS cancel_transfer(UUID, TEXT);
DROP FUNCTION IF EXISTS post_transfer(UUID);
DROP FUNCTION IF EXISTS request_transfer(TEXT, UUID, UUID, BIGINT, TEXT, transfer_kind, INTERVAL);
