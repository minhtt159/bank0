-- +goose Up
-- +goose StatementBegin
-- request_money_with_approval: atomic maker-checker staging for an above-threshold
-- console credit/withdrawal (MAKER-CHECKER-ATOMICITY). Before this, the console
-- handler made TWO autocommitted calls — request_deposit/request_withdrawal (which
-- places a hold) and then create_approval_request — so a failure or crash between
-- them could leave a PENDING transfer + hold with NO approval-queue row: funds
-- held, nothing in the 4-eyes queue.
--
-- This packs BOTH steps into one function body (one transaction): the hold is
-- created by the SAME path request_deposit/request_withdrawal use (it calls
-- request_transfer with the external_clearing leg, never reimplementing the hold),
-- and the admin_actions approval row is inserted in the same statement. Either both
-- the pending transfer (with hold) and the approval row commit, or neither does.
--
-- p_kind selects the direction, matching request_deposit / request_withdrawal:
--   'deposit'    -> external_clearing  DEBIT, account CREDIT  (money in)
--   'withdrawal' -> account DEBIT, external_clearing CREDIT   (money out, holds funds)
-- Any other kind is rejected (this function is only for the boundary money paths).
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

    -- (1) Stage the PENDING transfer + the hold, via the same request_transfer path
    -- request_deposit/request_withdrawal use. Direction depends on p_kind.
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

    -- (2) Enqueue the 4-eyes approval row — same shape as create_approval_request.
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
-- +goose StatementEnd
