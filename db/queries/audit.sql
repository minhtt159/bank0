-- name: RecordAdminAction :exec
INSERT INTO admin_actions (actor_user_id, action, target_id, detail)
VALUES (
    sqlc.arg(actor_user_id)::uuid,
    sqlc.arg(action)::text,
    sqlc.narg(target_id)::uuid,
    sqlc.arg(detail)::jsonb
);

-- name: ListAuditLog :many
SELECT aa.id,
       aa.action,
       aa.target_id,
       aa.detail,
       aa.created_at,
       COALESCE(u.username::text, '')::text  AS actor,
       COALESCE(ap.username::text, '')::text AS approver
FROM admin_actions aa
LEFT JOIN users u  ON u.id  = aa.actor_user_id
LEFT JOIN users ap ON ap.id = aa.approved_by
WHERE sqlc.narg(q)::text IS NULL OR sqlc.narg(q)::text = ''
   OR aa.action ILIKE '%' || sqlc.narg(q) || '%'
   OR u.username::text ILIKE '%' || sqlc.narg(q) || '%'
   OR aa.detail::text ILIKE '%' || sqlc.narg(q) || '%'
ORDER BY aa.created_at DESC
LIMIT sqlc.arg(page_limit)::int;
