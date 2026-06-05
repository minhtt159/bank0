-- name: CreateAccount :one
SELECT create_account(
    sqlc.arg(user_id)::uuid,
    sqlc.arg(iban)::varchar,
    sqlc.arg(pin)::text,
    sqlc.arg(transfer_limit_minor)::bigint
) AS id;

-- name: GetAccount :one
SELECT a.id, a.user_id, a.kind, a.system_code, a.iban, a.currency,
       a.balance_minor, account_available(a.id) AS available_minor,
       a.transfer_limit_minor, a.is_default, a.status, a.created_at, a.updated_at
FROM accounts a
WHERE a.id = sqlc.arg(id)::uuid;

-- name: ListAccountsByUser :many
SELECT id, user_id, kind, iban, currency, balance_minor,
       account_available(id) AS available_minor,
       transfer_limit_minor, is_default, status, created_at
FROM accounts
WHERE user_id = sqlc.arg(user_id)::uuid
ORDER BY created_at;

-- name: Deposit :one
SELECT deposit(
    sqlc.arg(idempotency_key)::text,
    sqlc.arg(account_id)::uuid,
    sqlc.arg(amount_minor)::bigint,
    sqlc.arg(description)::text
) AS transfer_id;

-- name: Withdraw :one
SELECT withdraw(
    sqlc.arg(idempotency_key)::text,
    sqlc.arg(account_id)::uuid,
    sqlc.arg(amount_minor)::bigint,
    sqlc.arg(description)::text
) AS transfer_id;

-- name: SetAccountStatus :exec
SELECT set_account_status(sqlc.arg(account_id)::uuid, sqlc.arg(status)::account_status);

-- name: SetDefaultAccount :exec
SELECT set_default_account(sqlc.arg(user_id)::uuid, sqlc.arg(account_id)::uuid);

-- name: UpdateTransferLimit :exec
SELECT update_transfer_limit(sqlc.arg(account_id)::uuid, sqlc.arg(transfer_limit_minor)::bigint);
