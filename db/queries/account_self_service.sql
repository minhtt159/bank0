-- Customer self-service: account opening + transfer-limit-change requests.
-- open_customer_account is RETURNS TABLE -> hand-written pgx in internal/db/bank.go.

-- name: RequestLimitChange :one
SELECT request_limit_change(sqlc.arg(account_id)::uuid, sqlc.arg(maker)::uuid,
    sqlc.arg(new_limit)::bigint, sqlc.arg(reason)::text) AS id;

-- name: ListLimitRequests :many
SELECT aa.id AS request_id, aa.target_id AS account_id, a.iban AS account_iban,
       a.user_id, u.username,
       (aa.detail->>'current_limit_minor')::bigint   AS current_limit_minor,
       (aa.detail->>'requested_limit_minor')::bigint AS requested_limit_minor,
       COALESCE(aa.detail->>'reason','') AS reason, aa.created_at AS requested_at
FROM admin_actions aa
JOIN accounts a ON a.id = aa.target_id
JOIN users u    ON u.id = a.user_id
WHERE aa.action = 'limit_request' AND aa.approved_by IS NULL
  AND (sqlc.narg(cursor)::timestamptz IS NULL OR aa.created_at < sqlc.narg(cursor)::timestamptz)
ORDER BY aa.created_at DESC
LIMIT sqlc.arg(page_limit)::int;

-- name: ApproveLimitChange :one
SELECT approve_limit_change(sqlc.arg(request_id)::uuid, sqlc.arg(approver)::uuid) AS account_id;

-- name: RejectLimitChange :one
SELECT reject_limit_change(sqlc.arg(request_id)::uuid, sqlc.arg(approver)::uuid,
    sqlc.arg(reason)::text) AS account_id;
