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
-- Depends on the money paths (00008): request_money_with_approval calls
-- request_transfer; approve/reject call post_transfer/cancel_transfer.
-- default_transfer_limit() here is what create_account (00007) calls at runtime.
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
    default_page_limit            INT         NOT NULL DEFAULT 15 CHECK (default_page_limit BETWEEN 1 AND 200),
    -- cap on non-closed accounts a customer may hold (bounds self-open abuse;
    -- staff createAccount is uncapped). Bank policy, so it lives here, not in code.
    max_accounts_per_user         INT         NOT NULL DEFAULT 5 CHECK (max_accounts_per_user BETWEEN 1 AND 50),
    -- APP-scam reimbursement policy (PSR-style, Rec 12): per-claim cap (€85,000
    -- stand-in for the UK £85k) and the claim excess deducted unless the customer
    -- is flagged vulnerable (the PSR waives it for them).
    reimbursement_cap_minor       BIGINT      NOT NULL DEFAULT 8500000 CHECK (reimbursement_cap_minor >= 0),
    reimbursement_excess_minor    BIGINT      NOT NULL DEFAULT 10000   CHECK (reimbursement_excess_minor >= 0),
    -- 4-letter NL bank codes allocate_iban (00007) draws from at random — real
    -- retail-bank codes for a realistic look (accounts stay internal-only /
    -- non-routable). The CHECK: concatenation must be non-empty groups of 4
    -- uppercase letters, i.e. every element is exactly [A-Z]{4}.
    iban_bank_codes               TEXT[]      NOT NULL
        DEFAULT ARRAY['ABNA','ADYB','ARSN','ASNB','BUNQ','INGB','KNAB','RABO','RBRB','SNSB','TRIO']
        CHECK (array_to_string(iban_bank_codes, '') ~ '^([A-Z]{4})+$')
);
INSERT INTO bank_settings (id) VALUES (TRUE);  -- the one row, all defaults

-- The singleton row is load-bearing (requires_approval / default_transfer_limit /
-- max_accounts_per_user / create_account read it): deleting it would silently
-- break maker-checker and account creation, so DELETE is blocked outright.
-- +goose StatementBegin
CREATE OR REPLACE FUNCTION bank_settings_block_delete() RETURNS TRIGGER AS $$
BEGIN
    RAISE EXCEPTION 'bank_settings is a singleton (DELETE blocked)'
        USING ERRCODE = 'restrict_violation';
END;
$$ LANGUAGE plpgsql;
-- +goose StatementEnd
CREATE TRIGGER trg_bank_settings_singleton BEFORE DELETE ON bank_settings FOR EACH ROW EXECUTE FUNCTION bank_settings_block_delete();

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

CREATE OR REPLACE FUNCTION max_accounts_per_user() RETURNS INT AS $$
    SELECT max_accounts_per_user FROM bank_settings WHERE id;
$$ LANGUAGE sql STABLE;

-- p_max_accounts is optional (NULL = unchanged) so pre-existing callers of the
-- 4-value form keep working.
CREATE OR REPLACE FUNCTION update_bank_settings(
    p_threshold_minor     BIGINT,
    p_default_limit_minor BIGINT,
    p_page_limit          INT,
    p_actor               UUID,
    p_max_accounts        INT DEFAULT NULL
) RETURNS VOID AS $$
    UPDATE bank_settings
       SET maker_checker_threshold_minor = p_threshold_minor,
           default_transfer_limit_minor  = p_default_limit_minor,
           default_page_limit            = p_page_limit,
           max_accounts_per_user         = COALESCE(p_max_accounts, max_accounts_per_user),
           updated_at = now(), updated_by = p_actor
     WHERE id;
$$ LANGUAGE sql;

-- ─────────────────────────────────────────────────────────────────────────────
-- Maker-checker (4-eyes) for high-value console money moves. A request stages a
-- PENDING transfer (+ hold) and an approval_request row in admin_actions; a
-- DIFFERENT admin approves (posts) or rejects (cancels). approved_by records the approver.
-- ─────────────────────────────────────────────────────────────────────────────

-- approve_request: a DIFFERENT admin posts the transfer. Enforces 4-eyes. Handles
-- both queues: 'approval_request' (high-value staging, posts FROM pending) and
-- 'screening_hold' (Rec 25 AML review, posts FROM under_review). The screening
-- actor is the initiating customer, so the self-approval guard is always satisfied.
CREATE OR REPLACE FUNCTION approve_request(p_request_id UUID, p_approver UUID)
RETURNS UUID AS $$
DECLARE v_req admin_actions%ROWTYPE;
BEGIN
    SELECT * INTO v_req FROM admin_actions
     WHERE id = p_request_id AND action IN ('approval_request', 'screening_hold') FOR UPDATE;
    IF NOT FOUND THEN RAISE EXCEPTION 'approval request % not found', p_request_id; END IF;
    IF v_req.approved_by IS NOT NULL THEN
        RAISE EXCEPTION 'request already handled' USING ERRCODE = 'check_violation';
    END IF;
    IF v_req.actor_user_id = p_approver THEN
        RAISE EXCEPTION 'cannot approve your own request' USING ERRCODE = '42501';
    END IF;
    IF v_req.action = 'screening_hold' THEN
        PERFORM post_transfer(v_req.target_id, ARRAY['under_review']::transfer_status[]);
    ELSE
        PERFORM post_transfer(v_req.target_id);
    END IF;
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
     WHERE id = p_request_id AND action IN ('approval_request', 'screening_hold') FOR UPDATE;
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
    IF NOT FOUND THEN RAISE EXCEPTION 'EXTERNAL_CLEARING system account missing (run seed)' USING ERRCODE = 'XX000'; END IF;

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
            USING ERRCODE = 'XX000';
    END IF;

    -- (2) Enqueue the 4-eyes approval row.
    -- A failure here (e.g. the actor FK) rolls back step (1)'s hold too.
    INSERT INTO admin_actions (actor_user_id, action, target_id, detail)
    VALUES (p_maker, 'approval_request', v_tid, COALESCE(p_detail, '{}'::jsonb))
    RETURNING id INTO v_req;

    RETURN QUERY SELECT v_tid, v_req;
END;
$$ LANGUAGE plpgsql;

-- ─────────────────────────────────────────────────────────────────────────────
-- Customer transfer-limit-change requests (maker-checker over admin_actions).
-- The CUSTOMER is the maker (action = 'limit_request', requested limit in
-- detail); an operator approves (applies update_transfer_limit) or rejects. A
-- limit raise is never self-applied — a compromised customer token cannot lift
-- its own ceiling. Reuses the approval-queue shape above; no new table.
-- ─────────────────────────────────────────────────────────────────────────────

-- request_limit_change: record a pending limit-change for a customer account.
CREATE OR REPLACE FUNCTION request_limit_change(
    p_account_id UUID,
    p_maker      UUID,                 -- requesting user (the customer)
    p_new_limit  BIGINT,
    p_reason     TEXT DEFAULT ''
) RETURNS UUID AS $$
DECLARE v_id UUID; v_cur BIGINT;
BEGIN
    IF p_new_limit < 0 THEN
        RAISE EXCEPTION 'limit must be >= 0' USING ERRCODE = 'check_violation';
    END IF;
    SELECT transfer_limit_minor INTO v_cur FROM accounts
     WHERE id = p_account_id AND kind = 'customer' AND status <> 'closed'
     FOR UPDATE;
    IF NOT FOUND THEN RAISE EXCEPTION 'account % not found', p_account_id; END IF;
    IF p_new_limit = v_cur THEN
        RAISE EXCEPTION 'requested limit equals the current limit'
            USING ERRCODE = 'check_violation';
    END IF;

    INSERT INTO admin_actions (actor_user_id, action, target_id, detail)
    VALUES (p_maker, 'limit_request', p_account_id,
            jsonb_build_object('current_limit_minor', v_cur,
                               'requested_limit_minor', p_new_limit,
                               'reason', COALESCE(p_reason, '')))
    RETURNING id INTO v_id;
    RETURN v_id;
END;
$$ LANGUAGE plpgsql;

-- approve_limit_change: an operator applies the requested limit. 4-eyes: the
-- approver must differ from the maker (42501 -> 403); an already-handled
-- request raises check_violation (-> 409 in the handler's already-handled map).
CREATE OR REPLACE FUNCTION approve_limit_change(p_request_id UUID, p_approver UUID)
RETURNS UUID AS $$
DECLARE v_req admin_actions%ROWTYPE; v_new BIGINT;
BEGIN
    SELECT * INTO v_req FROM admin_actions
     WHERE id = p_request_id AND action = 'limit_request' FOR UPDATE;
    IF NOT FOUND THEN RAISE EXCEPTION 'limit request % not found', p_request_id; END IF;
    IF v_req.approved_by IS NOT NULL THEN
        RAISE EXCEPTION 'request already handled' USING ERRCODE = 'check_violation';
    END IF;
    IF v_req.actor_user_id = p_approver THEN
        RAISE EXCEPTION 'cannot approve your own request' USING ERRCODE = '42501';
    END IF;
    v_new := (v_req.detail->>'requested_limit_minor')::bigint;
    PERFORM update_transfer_limit(v_req.target_id, v_new);
    UPDATE admin_actions SET approved_by = p_approver WHERE id = p_request_id;
    RETURN v_req.target_id;
END;
$$ LANGUAGE plpgsql;

-- reject_limit_change: mark handled without applying. Mirrors reject_request.
CREATE OR REPLACE FUNCTION reject_limit_change(p_request_id UUID, p_approver UUID, p_reason TEXT DEFAULT '')
RETURNS UUID AS $$
DECLARE v_req admin_actions%ROWTYPE;
BEGIN
    SELECT * INTO v_req FROM admin_actions
     WHERE id = p_request_id AND action = 'limit_request' FOR UPDATE;
    IF NOT FOUND THEN RAISE EXCEPTION 'limit request % not found', p_request_id; END IF;
    IF v_req.approved_by IS NOT NULL THEN
        RAISE EXCEPTION 'request already handled' USING ERRCODE = 'check_violation';
    END IF;
    UPDATE admin_actions
       SET approved_by = p_approver,
           detail = detail || jsonb_build_object('rejected', true, 'reject_reason', COALESCE(p_reason,''))
     WHERE id = p_request_id;
    RETURN v_req.target_id;
END;
$$ LANGUAGE plpgsql;

-- +goose StatementEnd

-- the pending limit-request queue (mirrors idx_admin_actions_pending_approvals).
CREATE INDEX idx_admin_actions_pending_limit
    ON admin_actions (created_at DESC)
    WHERE action = 'limit_request' AND approved_by IS NULL;

-- the pending AML-screening queue (Rec 25; mirrors the approvals/limit queues).
CREATE INDEX idx_admin_actions_pending_screening
    ON admin_actions (created_at DESC)
    WHERE action = 'screening_hold' AND approved_by IS NULL;

-- +goose Down
-- +goose StatementBegin
DROP FUNCTION IF EXISTS reject_limit_change(UUID, UUID, TEXT);
DROP FUNCTION IF EXISTS approve_limit_change(UUID, UUID);
DROP FUNCTION IF EXISTS request_limit_change(UUID, UUID, BIGINT, TEXT);
DROP FUNCTION IF EXISTS request_money_with_approval(UUID, TEXT, UUID, BIGINT, transfer_kind, TEXT, JSONB);
DROP FUNCTION IF EXISTS reject_request(UUID, UUID, TEXT);
DROP FUNCTION IF EXISTS approve_request(UUID, UUID);
DROP FUNCTION IF EXISTS update_bank_settings(BIGINT, BIGINT, INT, UUID, INT);
DROP FUNCTION IF EXISTS max_accounts_per_user();
DROP FUNCTION IF EXISTS default_transfer_limit();
DROP FUNCTION IF EXISTS requires_approval(BIGINT);
DROP FUNCTION IF EXISTS bank_settings_block_delete() CASCADE;
-- +goose StatementEnd
DROP TABLE IF EXISTS bank_settings;
DROP TABLE IF EXISTS admin_actions;
