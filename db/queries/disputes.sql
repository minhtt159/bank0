-- name: RaiseDispute :one
SELECT raise_dispute(
    sqlc.arg(transfer_id)::uuid,
    sqlc.arg(raiser)::uuid,
    sqlc.arg(category)::dispute_category,
    sqlc.arg(reason)::text
) AS id;

-- name: ResolveDispute :one
SELECT resolve_dispute(
    sqlc.arg(dispute_id)::uuid,
    sqlc.arg(resolver)::uuid,
    sqlc.arg(status)::dispute_status,
    sqlc.arg(note)::text
) AS status;

-- name: GetDisputeForRaiser :one
SELECT id, transfer_id, status, category, reason, resolution_note, created_at, updated_at
FROM disputes
WHERE id = sqlc.arg(id)::uuid AND raised_by_user_id = sqlc.arg(raiser)::uuid;

-- name: ListDisputesForRaiser :many
SELECT id, transfer_id, status, category, reason, resolution_note, created_at, updated_at
FROM disputes
WHERE raised_by_user_id = sqlc.arg(raiser)::uuid
  AND (sqlc.narg(cursor)::timestamptz IS NULL OR created_at < sqlc.narg(cursor)::timestamptz)
ORDER BY created_at DESC
LIMIT sqlc.arg(page_limit)::int;

-- name: GetDisputeAdmin :one
SELECT id, transfer_id, status, category, reason, resolution_note, created_at, updated_at
FROM disputes WHERE id = sqlc.arg(id)::uuid;

-- name: ListDisputesAdmin :many
SELECT d.id, d.transfer_id, d.status, d.category, d.reason,
       t.amount_minor, t.currency,
       COALESCE(u.username::text, '')        AS raised_by,
       COALESCE(da.iban, da.system_code, '') AS debit_iban,
       COALESCE(ca.iban, ca.system_code, '') AS credit_iban,
       d.created_at
FROM disputes d
JOIN transfers t  ON t.id  = d.transfer_id
LEFT JOIN users u ON u.id  = d.raised_by_user_id
JOIN accounts da  ON da.id = t.debit_account_id
JOIN accounts ca  ON ca.id = t.credit_account_id
WHERE (sqlc.narg(status)::dispute_status IS NULL OR d.status = sqlc.narg(status)::dispute_status)
  AND (sqlc.narg(cursor)::timestamptz  IS NULL OR d.created_at < sqlc.narg(cursor)::timestamptz)
ORDER BY d.created_at DESC
LIMIT sqlc.arg(page_limit)::int;
