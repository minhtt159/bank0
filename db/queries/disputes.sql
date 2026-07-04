-- name: RaiseDispute :one
SELECT raise_dispute(
    sqlc.arg(transfer_id)::uuid,
    sqlc.arg(raiser)::uuid,
    sqlc.arg(category)::dispute_category,
    sqlc.arg(reason)::text,
    sqlc.narg(scam_type)::scam_type
) AS id;

-- name: ResolveDispute :one
SELECT resolve_dispute(
    sqlc.arg(dispute_id)::uuid,
    sqlc.arg(resolver)::uuid,
    sqlc.arg(status)::dispute_status,
    sqlc.arg(note)::text
) AS status;

-- name: GetDisputeForRaiser :one
SELECT id, transfer_id, status, category, reason, resolution_note,
       scam_type, sla_due_at, decision, reimbursed_amount_minor, vulnerable_flag, recall_status, recall_reason, created_at, updated_at
FROM disputes
WHERE id = sqlc.arg(id)::uuid AND raised_by_user_id = sqlc.arg(raiser)::uuid;

-- name: ListDisputesForRaiser :many
SELECT id, transfer_id, status, category, reason, resolution_note,
       scam_type, sla_due_at, decision, reimbursed_amount_minor, vulnerable_flag, recall_status, recall_reason, created_at, updated_at
FROM disputes
WHERE raised_by_user_id = sqlc.arg(raiser)::uuid
  AND (sqlc.narg(cursor)::timestamptz IS NULL OR created_at < sqlc.narg(cursor)::timestamptz)
ORDER BY created_at DESC
LIMIT sqlc.arg(page_limit)::int;

-- name: GetDisputeAdmin :one
SELECT id, transfer_id, status, category, reason, resolution_note,
       scam_type, sla_due_at, decision, reimbursed_amount_minor, vulnerable_flag, recall_status, recall_reason, created_at, updated_at
FROM disputes WHERE id = sqlc.arg(id)::uuid;

-- name: ListDisputesAdmin :many
SELECT d.id, d.transfer_id, d.status, d.category, d.reason,
       d.scam_type, d.sla_due_at, d.decision, d.reimbursed_amount_minor,
       d.vulnerable_flag, d.recall_status, d.recall_reason,
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
  AND (sqlc.narg(cursor)::timestamptz IS NULL
       OR (d.created_at, d.id) < (sqlc.narg(cursor)::timestamptz, sqlc.narg(cursor_id)::uuid))
ORDER BY d.created_at DESC, d.id DESC
LIMIT sqlc.arg(page_limit)::int;

-- name: DecideDispute :one
SELECT decide_dispute(sqlc.arg(dispute_id)::uuid, sqlc.arg(resolver)::uuid,
    sqlc.arg(decision)::dispute_decision, sqlc.narg(reimburse_minor)::bigint,
    sqlc.narg(vulnerable)::boolean, sqlc.arg(note)::text) AS payout_minor;

-- name: SetDisputeRecall :one
SELECT set_dispute_recall(sqlc.arg(dispute_id)::uuid, sqlc.arg(actor)::uuid,
    sqlc.arg(status)::recall_status, sqlc.arg(reason)::text) AS recall_status;
