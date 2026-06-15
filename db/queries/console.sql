-- name: DashboardStats :one
SELECT
    (SELECT count(*) FROM transfers WHERE status = 'pending')::bigint                       AS pending_count,
    (SELECT count(*) FROM holds WHERE status = 'active')::bigint                            AS active_holds,
    (SELECT COALESCE(SUM(amount_minor), 0) FROM holds WHERE status = 'active')::bigint      AS held_minor,
    (SELECT COALESCE(SUM(balance_minor), 0) FROM accounts WHERE kind = 'customer')::bigint  AS customer_money;
