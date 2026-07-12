-- name: ListInvitationsByInviter :many
-- Raw rows for the caller's invitations, newest first. Status (pending/consumed/
-- expired) is DERIVED in Go from consumed_at/expires_at — not stored.
SELECT code, created_at, expires_at, consumed_at
FROM invitations
WHERE inviter_id = sqlc.arg(inviter_id)::uuid
ORDER BY created_at DESC;
