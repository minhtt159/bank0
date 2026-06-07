-- +goose Up
-- +goose StatementBegin
-- Maker-checker (4-eyes) for high-value console credits. A request creates a
-- PENDING deposit + an approval_request row in admin_actions; a DIFFERENT admin
-- approves (posts) or rejects (cancels). The approver is recorded in approved_by.

-- request_deposit: like deposit(), but leaves the transfer PENDING (no post).
CREATE OR REPLACE FUNCTION request_deposit(
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
    FROM request_transfer(p_idempotency_key, v_ext, p_account_id, p_amount_minor, p_description, 'deposit') t;
    RETURN v_id;
END;
$$ LANGUAGE plpgsql;

-- create_approval_request: record a pending approval for a (pending) transfer.
CREATE OR REPLACE FUNCTION create_approval_request(
    p_maker       UUID,
    p_transfer_id UUID,
    p_detail      JSONB DEFAULT '{}'
) RETURNS UUID AS $$
DECLARE v_id UUID;
BEGIN
    INSERT INTO admin_actions (actor_user_id, action, target_id, detail)
    VALUES (p_maker, 'approval_request', p_transfer_id, COALESCE(p_detail, '{}'::jsonb))
    RETURNING id INTO v_id;
    RETURN v_id;
END;
$$ LANGUAGE plpgsql;

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

CREATE INDEX IF NOT EXISTS idx_admin_actions_pending_approvals
    ON admin_actions (created_at DESC)
    WHERE action = 'approval_request' AND approved_by IS NULL;
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DROP INDEX IF EXISTS idx_admin_actions_pending_approvals;
DROP FUNCTION IF EXISTS reject_request(UUID, UUID, TEXT);
DROP FUNCTION IF EXISTS approve_request(UUID, UUID);
DROP FUNCTION IF EXISTS create_approval_request(UUID, UUID, JSONB);
DROP FUNCTION IF EXISTS request_deposit(TEXT, UUID, BIGINT, TEXT);
-- +goose StatementEnd
