-- Notification feed (events, 00008). Bare-array keyset list, matching the
-- ledger/transfers convention (cursor = last row's created_at + id).

-- name: ListMyEvents :many
SELECT id, user_id, type, title, body, related_transfer_id, related_account_id,
       data, read_at, created_at
FROM events
WHERE user_id = sqlc.arg(user_id)::uuid
  AND (sqlc.narg(cursor)::timestamptz IS NULL
       OR (created_at, id) < (sqlc.narg(cursor)::timestamptz,
                              COALESCE(sqlc.narg(cursor_id)::uuid, 'ffffffff-ffff-ffff-ffff-ffffffffffff')))
  AND (sqlc.narg(type)::event_type IS NULL OR type = sqlc.narg(type)::event_type)
  AND (NOT sqlc.arg(unread_only)::boolean OR read_at IS NULL)
ORDER BY created_at DESC, id DESC
LIMIT sqlc.arg(page_limit)::int;

-- name: CountMyUnreadEvents :one
SELECT COUNT(*)::int AS unread FROM events
WHERE user_id = sqlc.arg(user_id)::uuid AND read_at IS NULL;

-- name: MarkEventsRead :one
SELECT mark_events_read(sqlc.arg(user_id)::uuid,
    sqlc.narg(cursor)::timestamptz, sqlc.narg(cursor_id)::uuid) AS marked;
