-- NOTE: transfer() and request_transfer() return a TABLE; sqlc cannot expand
-- set-returning function columns, so those calls live in internal/db/bank.go
-- (hand-written pgx). Everything below is plain sqlc.

-- name: ListMyTransfers :many
-- Cross-account transfer history for one customer (the JWT subject): a row shows iff
-- the caller owns the debit OR credit account. direction is caller-relative ('out' =
-- caller debits, 'in' = caller credits; a self-transfer between the caller's own two
-- accounts ties to 'out'). Composite (requested_at, id) keyset cursor (bare array, no
-- envelope); all filters narg -> omitted = no filter. counterparty_owner is masked.
SELECT t.id, t.debit_account_id, t.credit_account_id, t.amount_minor, t.currency,
       t.status, t.kind, t.description, t.hold_reason, t.hold_expires_at, t.requested_at, t.posted_at,
       CASE WHEN da.user_id = sqlc.arg(subject)::uuid THEN 'out' ELSE 'in' END AS direction,
       CASE WHEN da.user_id = sqlc.arg(subject)::uuid
            THEN COALESCE(ca.iban, ca.system_code, '')
            ELSE COALESCE(da.iban, da.system_code, '') END AS counterparty_iban,
       CASE WHEN da.user_id = sqlc.arg(subject)::uuid
            THEN mask_name(cu.full_name)
            ELSE mask_name(du.full_name) END AS counterparty_owner
FROM transfers t
JOIN accounts da ON da.id = t.debit_account_id
JOIN accounts ca ON ca.id = t.credit_account_id
LEFT JOIN users du ON du.id = da.user_id
LEFT JOIN users cu ON cu.id = ca.user_id
WHERE (da.user_id = sqlc.arg(subject)::uuid OR ca.user_id = sqlc.arg(subject)::uuid)
  AND (sqlc.narg(cursor)::timestamptz IS NULL
       OR (t.requested_at, t.id) < (sqlc.narg(cursor)::timestamptz, sqlc.narg(cursor_id)::uuid))
  AND (sqlc.narg(status)::transfer_status IS NULL OR t.status = sqlc.narg(status)::transfer_status)
  AND (sqlc.narg(kind)::transfer_kind     IS NULL OR t.kind   = sqlc.narg(kind)::transfer_kind)
  AND (sqlc.narg(from_ts)::timestamptz    IS NULL OR t.requested_at >= sqlc.narg(from_ts)::timestamptz)
  AND (sqlc.narg(to_ts)::timestamptz      IS NULL OR t.requested_at <  sqlc.narg(to_ts)::timestamptz)
  AND (sqlc.narg(dir)::text IS NULL
       OR (sqlc.narg(dir)::text = 'out' AND da.user_id = sqlc.arg(subject)::uuid)
       OR (sqlc.narg(dir)::text = 'in'  AND ca.user_id = sqlc.arg(subject)::uuid))
  AND (sqlc.narg(q)::text IS NULL
       OR t.description ILIKE '%' || sqlc.narg(q)::text || '%'
       OR COALESCE(da.iban::text, '') ILIKE '%' || sqlc.narg(q)::text || '%'
       OR COALESCE(ca.iban::text, '') ILIKE '%' || sqlc.narg(q)::text || '%')
ORDER BY t.requested_at DESC, t.id DESC
LIMIT sqlc.arg(page_limit)::int;

-- name: PostTransfer :one
SELECT post_transfer(sqlc.arg(id)::uuid) AS status;

-- name: CancelTransfer :one
SELECT cancel_transfer(sqlc.arg(id)::uuid, sqlc.arg(reason)::text) AS status;

-- name: ClientPostTransfer :one
-- Caller-scoped post: enforces debit-account ownership in the DB (TRANSFER-1).
SELECT client_post_transfer(sqlc.arg(caller_subject)::uuid, sqlc.arg(id)::uuid) AS status;

-- name: ClientCancelTransfer :one
-- Caller-scoped cancel: enforces debit-account ownership in the DB (TRANSFER-1).
SELECT client_cancel_transfer(sqlc.arg(caller_subject)::uuid, sqlc.arg(id)::uuid, sqlc.arg(reason)::text) AS status;

-- name: ClientConfirmTransfer :one
-- Caller-scoped confirm: releases the caller's own held transfer (Rec 22 cooling-off).
SELECT client_confirm_transfer(sqlc.arg(caller_subject)::uuid, sqlc.arg(id)::uuid) AS status;

-- name: ReverseTransfer :one
SELECT reverse_transfer(
    sqlc.arg(id)::uuid,
    sqlc.arg(idempotency_key)::text,
    sqlc.arg(reason)::text
) AS reversal_id;

-- name: GetTransfer :one
SELECT id, debit_account_id, credit_account_id, amount_minor, currency, status, kind,
       reverses_id, description, uetr, end_to_end_id, failure_reason, hold_reason, hold_expires_at,
       requested_at, posted_at, created_at, updated_at
FROM transfers WHERE id = sqlc.arg(id)::uuid;

-- name: ListPendingTransfers :many
SELECT id, debit_account_id, credit_account_id, amount_minor, currency, kind, description, requested_at
FROM transfers
WHERE status = 'pending'
  AND requested_at < COALESCE(sqlc.narg(cursor)::timestamptz, now())
ORDER BY requested_at DESC
LIMIT sqlc.arg(page_limit)::int;

-- name: GetAccountLedger :many
-- Client account statement. Composite (posted_at, id) keyset cursor so ties (rows
-- sharing a posted_at) page correctly — the posted_at-only cursor silently skipped
-- them at a page boundary. Optional server-side filters (all narg -> omitted = no
-- filter): date range [from, to), direction, free text, and absolute-amount range.
-- Pass cursor + cursor_id together (both from the last row of the previous page).
SELECT id, transfer_id, account_id, account_iban, direction, amount_minor, signed_amount,
       balance_after, currency, posted_at, transfer_kind, transfer_status, description,
       counterparty_iban, counterparty_owner
FROM enriched_ledger
WHERE account_id = sqlc.arg(account_id)::uuid
  AND (sqlc.narg(cursor)::timestamptz IS NULL
       OR (posted_at, id) < (sqlc.narg(cursor)::timestamptz, sqlc.narg(cursor_id)::uuid))
  AND (sqlc.narg(from_ts)::timestamptz IS NULL OR posted_at >= sqlc.narg(from_ts)::timestamptz)
  AND (sqlc.narg(to_ts)::timestamptz   IS NULL OR posted_at <  sqlc.narg(to_ts)::timestamptz)
  AND (sqlc.narg(direction)::text IS NULL OR direction::text = sqlc.narg(direction)::text)
  AND (sqlc.narg(min_minor)::bigint IS NULL OR amount_minor >= sqlc.narg(min_minor)::bigint)
  AND (sqlc.narg(max_minor)::bigint IS NULL OR amount_minor <= sqlc.narg(max_minor)::bigint)
  AND (sqlc.narg(q)::text IS NULL OR (
        description        ILIKE '%' || sqlc.narg(q)::text || '%'
     OR counterparty_iban  ILIKE '%' || sqlc.narg(q)::text || '%'
     OR counterparty_owner ILIKE '%' || sqlc.narg(q)::text || '%'))
ORDER BY posted_at DESC, id DESC
LIMIT sqlc.arg(page_limit)::int;

-- name: GetTransferDetail :one
SELECT t.id, t.amount_minor, t.currency, t.status, t.kind, t.reverses_id,
       t.description, t.failure_reason, t.hold_reason, t.hold_expires_at,
       t.requested_at, t.posted_at, t.idempotency_key,
       COALESCE(da.iban, da.system_code, '') AS debit_label,
       COALESCE(ca.iban, ca.system_code, '') AS credit_label
FROM transfers t
JOIN accounts da ON da.id = t.debit_account_id
JOIN accounts ca ON ca.id = t.credit_account_id
WHERE t.id = sqlc.arg(id)::uuid;

-- name: TransferLegs :many
SELECT account_iban, direction, signed_amount, balance_after
FROM enriched_ledger
WHERE transfer_id = sqlc.arg(transfer_id)::uuid
ORDER BY direction;

-- name: HoldForTransfer :many
SELECT amount_minor, status, expires_at, created_at
FROM holds
WHERE transfer_id = sqlc.arg(transfer_id)::uuid
ORDER BY created_at DESC
LIMIT 1;
