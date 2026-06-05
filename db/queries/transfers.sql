-- NOTE: transfer() and request_transfer() return a TABLE; sqlc cannot expand
-- set-returning function columns, so those calls live in internal/db/bank.go
-- (hand-written pgx). Everything below is plain sqlc.

-- name: PostTransfer :one
SELECT post_transfer(sqlc.arg(id)::uuid) AS status;

-- name: CancelTransfer :one
SELECT cancel_transfer(sqlc.arg(id)::uuid, sqlc.arg(reason)::text) AS status;

-- name: ReverseTransfer :one
SELECT reverse_transfer(
    sqlc.arg(id)::uuid,
    sqlc.arg(idempotency_key)::text,
    sqlc.arg(reason)::text
) AS reversal_id;

-- name: GetTransfer :one
SELECT id, debit_account_id, credit_account_id, amount_minor, currency, status, kind,
       reverses_id, description, failure_reason, requested_at, posted_at, created_at, updated_at
FROM transfers WHERE id = sqlc.arg(id)::uuid;

-- name: ListPendingTransfers :many
SELECT id, debit_account_id, credit_account_id, amount_minor, currency, kind, description, requested_at
FROM transfers
WHERE status = 'pending'
  AND requested_at < COALESCE(sqlc.narg(cursor)::timestamptz, now())
ORDER BY requested_at DESC
LIMIT sqlc.arg(page_limit)::int;

-- name: GetAccountLedger :many
SELECT id, transfer_id, account_id, account_iban, direction, amount_minor, signed_amount,
       balance_after, currency, posted_at, transfer_kind, transfer_status, description,
       counterparty_iban, counterparty_owner
FROM enriched_ledger
WHERE account_id = sqlc.arg(account_id)::uuid
  AND posted_at < COALESCE(sqlc.narg(cursor)::timestamptz, now())
ORDER BY posted_at DESC, id DESC
LIMIT sqlc.arg(page_limit)::int;
