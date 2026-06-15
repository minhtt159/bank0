-- +goose Up
-- ─────────────────────────────────────────────────────────────────────────────
-- MAKER-CHECKER & BANK SETTINGS — operator policy + 4-eyes on high-value moves
-- Split out of the former core_banking monolith. Holds the admin_actions audit
-- trail ("who authorized it & why" alongside the ledger), the single-row
-- bank_settings (operator-tweakable bank policy), the policy-knob functions
-- (requires_approval / default_transfer_limit / update_bank_settings), and the
-- 4-eyes workflow: request_money_with_approval stages a PENDING transfer + the
-- approval-queue row atomically; approve_request / reject_request resolve it.
--
-- Depends on the money paths (00005): request_money_with_approval calls
-- request_transfer; approve/reject call post_transfer/cancel_transfer.
-- default_transfer_limit() here is what create_account (00004) calls at runtime.
-- ─────────────────────────────────────────────────────────────────────────────

-- ─────────────────────────────────────────────────────────────────────────────
-- admin_actions  (operator audit; the "who authorized it & why" alongside the ledger)
-- ─────────────────────────────────────────────────────────────────────────────
CREATE TABLE admin_actions (
    id            UUID PRIMARY KEY DEFAULT uuidv7(),
    actor_user_id UUID NOT NULL REFERENCES users(id),
    action        TEXT NOT NULL,
    target_id     UUID,
    detail        JSONB NOT NULL DEFAULT '{}',
    approved_by   UUID REFERENCES users(id),
    created_at    TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- audit log browsing + the maker-checker pending-approvals queue.
CREATE INDEX idx_admin_actions_created ON admin_actions (created_at DESC);
CREATE INDEX idx_admin_actions_pending_approvals
    ON admin_actions (created_at DESC)
    WHERE action = 'approval_request' AND approved_by IS NULL;

-- ─────────────────────────────────────────────────────────────────────────────
-- bank_settings  (single-row, operator-tweakable bank policy — the maker-checker
-- threshold, default transfer limit, console page size — DB is the source of truth)
-- ─────────────────────────────────────────────────────────────────────────────
CREATE TABLE bank_settings (
    id                            BOOLEAN     PRIMARY KEY DEFAULT TRUE CHECK (id),
    -- 4-eyes: money moves strictly above this need a second approver. €10,000.00.
    maker_checker_threshold_minor BIGINT      NOT NULL DEFAULT 1000000 CHECK (maker_checker_threshold_minor >= 0),
    -- per-account default transfer limit applied at account creation. €500.00.
    default_transfer_limit_minor  BIGINT      NOT NULL DEFAULT 50000   CHECK (default_transfer_limit_minor >= 0),
    updated_at                    TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_by                    UUID        REFERENCES users(id) ON DELETE SET NULL,
    -- operator-configurable console page size (default 15), bounded so a typo can't
    -- ask for a 0-row or absurd page. Kept as the trailing column to match the
    -- historical physical order (added by ALTER in the pre-split schema).
    default_page_limit            INT         NOT NULL DEFAULT 15 CHECK (default_page_limit BETWEEN 1 AND 200)
);
INSERT INTO bank_settings (id) VALUES (TRUE);  -- the one row, all defaults

-- +goose StatementBegin

-- ─────────────────────────────────────────────────────────────────────────────
-- bank_settings functions  (the maker-checker decision + policy knobs live in the DB)
-- ─────────────────────────────────────────────────────────────────────────────

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
    p_page_limit          INT,
    p_actor               UUID
) RETURNS VOID AS $$
    UPDATE bank_settings
       SET maker_checker_threshold_minor = p_threshold_minor,
           default_transfer_limit_minor  = p_default_limit_minor,
           default_page_limit            = p_page_limit,
           updated_at = now(), updated_by = p_actor
     WHERE id;
$$ LANGUAGE sql;

-- ─────────────────────────────────────────────────────────────────────────────
-- Maker-checker (4-eyes) for high-value console money moves. A request stages a
-- PENDING transfer (+ hold) and an approval_request row in admin_actions; a
-- DIFFERENT admin approves (posts) or rejects (cancels). approved_by records the approver.
-- ─────────────────────────────────────────────────────────────────────────────

-- approve_request: a DIFFERENT admin posts the transfer. Enforces 4-eyes.
CREATE OR REPLACE FUNCTION approve_request(p_request_id UUID, p_approver UUID)
RETURNS UUID AS $$
DECLARE v_req admin_actions%ROWTYPE;
BEGIN
    SELECT * INTO v_req FROM admin_actions
     WHERE id = p_request_id AND action = 'approval_request' FOR UPDATE;
    IF NOT FOUND THEN RAISE EXCEPTION 'approval request % not found', p_request_id; END IF;
    IF v_req.approved_by IS NOT NULL THEN
        RAISE EXCEPTION 'request already handled' USING ERRCODE = 'check_violation';
    END IF;
    IF v_req.actor_user_id = p_approver THEN
        RAISE EXCEPTION 'cannot approve your own request' USING ERRCODE = '42501';
    END IF;
    PERFORM post_transfer(v_req.target_id);
    UPDATE admin_actions SET approved_by = p_approver WHERE id = p_request_id;
    RETURN v_req.target_id;
END;
$$ LANGUAGE plpgsql;

-- reject_request: cancel the pending transfer + mark the request handled. The
-- maker may withdraw their own request; any admin may reject.
CREATE OR REPLACE FUNCTION reject_request(p_request_id UUID, p_approver UUID, p_reason TEXT DEFAULT '')
RETURNS UUID AS $$
DECLARE v_req admin_actions%ROWTYPE;
BEGIN
    SELECT * INTO v_req FROM admin_actions
     WHERE id = p_request_id AND action = 'approval_request' FOR UPDATE;
    IF NOT FOUND THEN RAISE EXCEPTION 'approval request % not found', p_request_id; END IF;
    IF v_req.approved_by IS NOT NULL THEN
        RAISE EXCEPTION 'request already handled' USING ERRCODE = 'check_violation';
    END IF;
    PERFORM cancel_transfer(v_req.target_id, p_reason);
    UPDATE admin_actions SET approved_by = p_approver WHERE id = p_request_id;
    RETURN v_req.target_id;
END;
$$ LANGUAGE plpgsql;

-- request_money_with_approval: atomic maker-checker staging for an above-threshold
-- console credit/withdrawal. Packs BOTH the PENDING transfer (+ hold, via the
-- request_transfer path) AND the admin_actions approval row into one transaction:
-- either both commit, or neither. This is the SOLE staging entrypoint — there are no
-- separate request_deposit/request_withdrawal/create_approval_request helpers (they
-- could orphan a hold with no queue row if the second call failed).
--   'deposit'    -> external_clearing  DEBIT, account CREDIT  (money in)
--   'withdrawal' -> account DEBIT, external_clearing CREDIT   (money out, holds funds)
CREATE OR REPLACE FUNCTION request_money_with_approval(
    p_maker           UUID,
    p_idempotency_key TEXT,
    p_account_id      UUID,
    p_amount_minor    BIGINT,
    p_kind            transfer_kind,
    p_description     TEXT  DEFAULT '',
    p_detail          JSONB DEFAULT '{}'
) RETURNS TABLE (transfer_id UUID, request_id UUID) AS $$
DECLARE
    v_ext UUID;
    v_tid UUID;
    v_req UUID;
BEGIN
    SELECT id INTO v_ext FROM accounts WHERE system_code = 'EXTERNAL_CLEARING';
    IF NOT FOUND THEN RAISE EXCEPTION 'EXTERNAL_CLEARING system account missing (run seed)'; END IF;

    -- (1) Stage the PENDING transfer + the hold via request_transfer. Direction
    -- depends on p_kind.
    IF p_kind = 'deposit' THEN
        SELECT t.transfer_id INTO v_tid
        FROM request_transfer(p_idempotency_key, v_ext, p_account_id, p_amount_minor, p_description, 'deposit') t;
    ELSIF p_kind = 'withdrawal' THEN
        SELECT t.transfer_id INTO v_tid
        FROM request_transfer(p_idempotency_key, p_account_id, v_ext, p_amount_minor, p_description, 'withdrawal') t;
    ELSE
        RAISE EXCEPTION 'unsupported kind % for maker-checker staging', p_kind
            USING ERRCODE = 'check_violation';
    END IF;

    -- (2) Enqueue the 4-eyes approval row.
    -- A failure here (e.g. the actor FK) rolls back step (1)'s hold too.
    INSERT INTO admin_actions (actor_user_id, action, target_id, detail)
    VALUES (p_maker, 'approval_request', v_tid, COALESCE(p_detail, '{}'::jsonb))
    RETURNING id INTO v_req;

    RETURN QUERY SELECT v_tid, v_req;
END;
$$ LANGUAGE plpgsql;

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DROP FUNCTION IF EXISTS request_money_with_approval(UUID, TEXT, UUID, BIGINT, transfer_kind, TEXT, JSONB);
DROP FUNCTION IF EXISTS reject_request(UUID, UUID, TEXT);
DROP FUNCTION IF EXISTS approve_request(UUID, UUID);
DROP FUNCTION IF EXISTS update_bank_settings(BIGINT, BIGINT, INT, UUID);
DROP FUNCTION IF EXISTS default_transfer_limit();
DROP FUNCTION IF EXISTS requires_approval(BIGINT);
-- +goose StatementEnd
DROP TABLE IF EXISTS bank_settings;
DROP TABLE IF EXISTS admin_actions;
