-- Self-registration & contact verification (00011). register_user and
-- verify_contact are RETURNS TABLE -> hand-written pgx in internal/db/registration.go.

-- name: CreateVerificationChallenge :one
SELECT create_verification_challenge(sqlc.arg(user_id)::uuid,
    sqlc.arg(channel)::verification_channel, sqlc.arg(destination)::text,
    sqlc.arg(token_hash)::text, sqlc.arg(code_hash)::text) AS id;

-- name: ChallengeByToken :one
SELECT user_id, channel, destination, status
FROM verification_challenges WHERE token_hash = sqlc.arg(token_hash)::text;
