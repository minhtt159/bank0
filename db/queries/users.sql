-- name: CreateUser :one
SELECT create_user(
    sqlc.arg(username)::citext,
    sqlc.arg(password)::text,
    sqlc.arg(full_name)::text,
    sqlc.narg(email)::citext,
    sqlc.narg(phone_number)::varchar,
    sqlc.arg(role)::user_role
) AS id;

-- name: CheckCredentials :one
SELECT check_user_credentials(sqlc.arg(username)::citext, sqlc.arg(password)::text) AS user_id;

-- name: GetUserByID :one
SELECT id, username, full_name, email, phone_number, role, status, created_at, updated_at
FROM users WHERE id = sqlc.arg(id)::uuid;

-- name: GetUserByUsername :one
SELECT id, username, full_name, email, phone_number, role, status, created_at, updated_at
FROM users WHERE username = sqlc.arg(username)::citext;

-- name: ListUsers :many
SELECT id, username, full_name, email, phone_number, role, status, created_at, updated_at
FROM users
WHERE created_at < COALESCE(sqlc.narg(cursor)::timestamptz, now())
ORDER BY created_at DESC
LIMIT sqlc.arg(page_limit)::int;

-- name: UpdateUserInfo :exec
SELECT update_user_info(
    sqlc.arg(user_id)::uuid,
    sqlc.narg(full_name)::text,
    sqlc.narg(email)::citext,
    sqlc.narg(phone_number)::varchar,
    sqlc.narg(password)::text,
    sqlc.narg(status)::user_status
);
