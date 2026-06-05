-- +goose Up

-- enriched_ledger: the human-readable read model for statements and the operator
-- console. One row per posting, with the account, the transfer, and the
-- counterparty resolved. Replaces tf-backend's enriched_transaction_audit_log,
-- but reads from the single source of truth instead of a duplicate audit table.
CREATE VIEW enriched_ledger AS
SELECT
    le.id,
    le.transfer_id,
    le.account_id,
    a.iban                AS account_iban,
    a.system_code        AS account_system_code,
    au.full_name         AS account_owner,
    le.direction,
    le.amount_minor,
    le.signed_amount,
    le.balance_after,
    le.currency,
    le.posted_at,
    t.kind               AS transfer_kind,
    t.status             AS transfer_status,
    t.description,
    t.reverses_id,
    cp.id                AS counterparty_account_id,
    cp.iban              AS counterparty_iban,
    cp.system_code       AS counterparty_system_code,
    cu.full_name         AS counterparty_owner
FROM ledger_entries le
JOIN transfers t ON t.id = le.transfer_id
JOIN accounts  a ON a.id = le.account_id
LEFT JOIN users au ON au.id = a.user_id
JOIN accounts cp ON cp.id = CASE WHEN le.account_id = t.debit_account_id
                                 THEN t.credit_account_id ELSE t.debit_account_id END
LEFT JOIN users cu ON cu.id = cp.user_id;

-- +goose Down
DROP VIEW IF EXISTS enriched_ledger;
