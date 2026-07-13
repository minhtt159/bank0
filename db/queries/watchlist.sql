-- Sanctions/AML watchlist (watchlist_entries, 00008) — console maintenance CRUD.
-- The screening seam itself is screen_payment, called inside transfer().

-- name: ListWatchlistEntries :many
-- All entries (incl. inactive) for the console.
SELECT id, pattern, reason, active, created_at
FROM watchlist_entries
ORDER BY active DESC, created_at DESC;

-- name: CreateWatchlistEntry :one
INSERT INTO watchlist_entries (pattern, reason, active)
VALUES (sqlc.arg(pattern)::text, sqlc.arg(reason)::text, sqlc.arg(active)::boolean)
RETURNING id;

-- name: SetWatchlistEntryActive :exec
UPDATE watchlist_entries SET active = sqlc.arg(active)::boolean WHERE id = sqlc.arg(id)::uuid;
