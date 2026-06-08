-- +goose Up
-- +goose StatementBegin
-- request_withdrawal: like withdraw(), but leaves the transfer PENDING (no post),
-- so high-value withdrawals can go through maker-checker. The hold reserves the
-- funds on the customer's account until a second admin approves.
CREATE OR REPLACE FUNCTION request_withdrawal(
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
    FROM request_transfer(p_idempotency_key, p_account_id, v_ext, p_amount_minor, p_description, 'withdrawal') t;
    RETURN v_id;
END;
$$ LANGUAGE plpgsql;
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DROP FUNCTION IF EXISTS request_withdrawal(TEXT, UUID, BIGINT, TEXT);
-- +goose StatementEnd
