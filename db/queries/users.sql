-- name: CreateUser :one
SELECT create_user(
    sqlc.arg(username)::citext,
    sqlc.arg(password)::text,
    sqlc.arg(full_name)::text,
    sqlc.narg(email)::citext,
    sqlc.narg(phone_number)::varchar,
    sqlc.arg(role)::user_role
) AS id;

-- name: GetUserByID :one
SELECT id, username, full_name, email, phone_number, role, status, created_at, updated_at
FROM users WHERE id = sqlc.arg(id)::uuid;

-- name: UpdateUserInfo :exec
SELECT update_user_info(
    sqlc.arg(user_id)::uuid,
    sqlc.narg(full_name)::text,
    sqlc.narg(email)::citext,
    sqlc.narg(phone_number)::varchar,
    sqlc.narg(password)::text,
    sqlc.narg(status)::user_status
);
