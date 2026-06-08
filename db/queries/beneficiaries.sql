-- name: AddBeneficiary :one
SELECT add_beneficiary(
    sqlc.arg(owner)::uuid,
    sqlc.arg(label)::text,
    sqlc.arg(iban)::varchar
) AS id;

-- name: GetBeneficiary :one
SELECT id, label, credit_account_id, iban, owner_name_masked, created_at
FROM beneficiaries
WHERE id = sqlc.arg(id)::uuid AND owner_user_id = sqlc.arg(owner)::uuid;

-- name: ListBeneficiaries :many
SELECT id, label, credit_account_id, iban, owner_name_masked, created_at
FROM beneficiaries
WHERE owner_user_id = sqlc.arg(owner)::uuid
ORDER BY created_at;

-- NOTE: resolve_account_by_iban() RETURNS TABLE and sqlc cannot expand
-- set-returning PL/pgSQL functions, so it is hand-written with pgx in bank.go
-- (same pattern as transfer()/reconcile()).

-- name: DeleteBeneficiary :exec
SELECT delete_beneficiary(sqlc.arg(owner)::uuid, sqlc.arg(id)::uuid);
