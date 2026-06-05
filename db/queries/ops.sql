-- NOTE: reconcile() returns a TABLE; sqlc cannot expand set-returning function
-- columns, so it lives in internal/db/bank.go (hand-written pgx).

-- name: ExpireHolds :one
SELECT expire_holds() AS expired_count;

-- name: CleanupIdempotencyKeys :one
SELECT cleanup_idempotency_keys() AS deleted_count;
