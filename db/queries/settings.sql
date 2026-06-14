-- name: GetBankSettings :one
SELECT maker_checker_threshold_minor, default_transfer_limit_minor, updated_at, updated_by
FROM bank_settings WHERE id;

-- name: UpdateBankSettings :exec
SELECT update_bank_settings(
    sqlc.arg(threshold_minor)::bigint,
    sqlc.arg(default_limit_minor)::bigint,
    sqlc.arg(actor)::uuid
);

-- requires_approval() RETURNS TABLE, which sqlc can't expand — see RequiresApproval
-- in internal/db/bank.go (hand-written pgx, like transfer()/reconcile()).
