-- name: RequestDeposit :one
SELECT request_deposit(
    sqlc.arg(idempotency_key)::text,
    sqlc.arg(account_id)::uuid,
    sqlc.arg(amount_minor)::bigint,
    sqlc.arg(description)::text
) AS transfer_id;

-- name: CreateApprovalRequest :one
SELECT create_approval_request(
    sqlc.arg(maker)::uuid,
    sqlc.arg(transfer_id)::uuid,
    sqlc.arg(detail)::jsonb
) AS id;

-- name: ApproveRequest :one
SELECT approve_request(sqlc.arg(request_id)::uuid, sqlc.arg(approver)::uuid) AS transfer_id;

-- name: RejectRequest :one
SELECT reject_request(sqlc.arg(request_id)::uuid, sqlc.arg(approver)::uuid, sqlc.arg(reason)::text) AS transfer_id;

-- name: CountPendingApprovals :one
SELECT count(*)::int FROM admin_actions
WHERE action = 'approval_request' AND approved_by IS NULL;

-- name: ListPendingApprovals :many
SELECT aa.id,
       aa.created_at,
       aa.detail,
       COALESCE(u.username::text, '')::text  AS maker,
       t.amount_minor,
       COALESCE(ca.iban, ca.system_code, '') AS credit_iban,
       COALESCE(da.iban, da.system_code, '') AS debit_iban
FROM admin_actions aa
JOIN transfers t  ON t.id  = aa.target_id
LEFT JOIN users u ON u.id  = aa.actor_user_id
JOIN accounts da  ON da.id = t.debit_account_id
JOIN accounts ca  ON ca.id = t.credit_account_id
WHERE aa.action = 'approval_request' AND aa.approved_by IS NULL AND t.status = 'pending'
ORDER BY aa.created_at DESC
LIMIT sqlc.arg(page_limit)::int;
