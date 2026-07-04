-- Fraud-warning evidence (warning_acks, 00008).

-- name: RecordWarningAck :one
SELECT record_warning_ack(sqlc.arg(user_id)::uuid, sqlc.arg(category)::text,
    sqlc.arg(reason_code)::text, sqlc.arg(acknowledged)::boolean,
    sqlc.narg(debit_account_id)::uuid, sqlc.arg(counterparty_iban)::text,
    sqlc.narg(amount_minor)::bigint, sqlc.arg(device)::text) AS id;
