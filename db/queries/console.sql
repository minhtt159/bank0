-- name: DashboardStats :one
SELECT
    (SELECT count(*) FROM transfers WHERE status = 'pending')::bigint                       AS pending_count,
    (SELECT count(*) FROM holds WHERE status = 'active')::bigint                            AS active_holds,
    (SELECT COALESCE(SUM(amount_minor), 0) FROM holds WHERE status = 'active')::bigint      AS held_minor,
    (SELECT COALESCE(SUM(balance_minor), 0) FROM accounts WHERE kind = 'customer')::bigint  AS customer_money;

-- name: ListCustomerAccounts :many
SELECT a.id,
       a.user_id,
       COALESCE(u.full_name, '') AS owner,
       COALESCE(a.iban, '')      AS iban,
       a.status,
       a.balance_minor,
       account_available(a.id)   AS available_minor
FROM accounts a
LEFT JOIN users u ON u.id = a.user_id
WHERE a.kind = 'customer'
ORDER BY a.created_at DESC
LIMIT sqlc.arg(page_limit)::int;

-- name: ListPendingEnriched :many
SELECT t.id,
       COALESCE(da.iban, da.system_code, '') AS debit,
       COALESCE(ca.iban, ca.system_code, '') AS credit,
       t.kind,
       t.description,
       t.amount_minor,
       t.requested_at
FROM transfers t
JOIN accounts da ON da.id = t.debit_account_id
JOIN accounts ca ON ca.id = t.credit_account_id
WHERE t.status = 'pending'
ORDER BY t.requested_at DESC
LIMIT sqlc.arg(page_limit)::int;
