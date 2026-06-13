-- Ownership lookups used by the client-surface (JWT) authorization checks.

-- name: AccountOwner :one
SELECT user_id FROM accounts WHERE id = $1;

-- name: TransferOwners :one
SELECT da.user_id AS debit_owner,
       ca.user_id AS credit_owner
FROM transfers t
JOIN accounts da ON da.id = t.debit_account_id
JOIN accounts ca ON ca.id = t.credit_account_id
WHERE t.id = $1;

-- name: FamilyOwnedBy :one
-- True iff the refresh-token family belongs to the caller. Lets DELETE
-- /me/sessions/{family_id} return 404 for a non-owned family while staying
-- idempotent (204) for the owner's already-revoked family.
SELECT EXISTS (
    SELECT 1 FROM refresh_tokens
     WHERE family_id = sqlc.arg(family)::uuid AND user_id = sqlc.arg(owner)::uuid
) AS owned;
