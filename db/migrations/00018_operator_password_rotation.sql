-- 00018_operator_password_rotation.sql
--
-- The bootstrap admin (00016) is seeded as admin/admin with a "change it
-- immediately" comment — but there was nowhere to change it. POST /me/password is
-- the CLIENT surface (JWT, clientSubject); the admin JSON surface refuses password
-- writes by design; the console had no screen. The only rotation path was psql.
--
-- Two halves, both here: `must_change_password` turns that comment into behaviour
-- (the console blocks every other screen until it is cleared), and change_password
-- clears it. The console screen itself is Go/Templ — this file is the policy.
--
-- The first migration after the v1.0.0 baseline froze, hence a new file rather than
-- an edit to 00003/00016.

-- +goose Up
ALTER TABLE users ADD COLUMN must_change_password BOOLEAN NOT NULL DEFAULT FALSE;

-- Flag the seeded bootstrap admin, but ONLY if it still holds the seeded password:
-- crypt() re-derives the hash using the stored hash as the salt, so an operator who
-- already rotated by hand is not forced through the screen. Idempotent on a fresh
-- install (00016 inserts, this flags) and on an existing one.
UPDATE users
   SET must_change_password = TRUE
 WHERE username = 'admin'
   AND password_hash = crypt('admin', password_hash);

-- +goose StatementBegin

-- change_password (00003) + the flag clear. Body is otherwise unchanged: it is
-- still the sole authority on password policy (>= 12 chars, must differ) and the
-- only writer of password_hash, which is exactly why the clear belongs here rather
-- than in a handler that could forget.
CREATE OR REPLACE FUNCTION change_password(
    p_user_id UUID,
    p_current TEXT,
    p_new     TEXT
) RETURNS VOID AS $$
DECLARE
    v_hash TEXT;
BEGIN
    SELECT password_hash INTO v_hash
      FROM users
     WHERE id = p_user_id AND status = 'active'
     FOR UPDATE;
    IF NOT FOUND THEN
        -- unknown / non-active user: same code as a bad password (no enumeration)
        RAISE EXCEPTION 'invalid current password' USING ERRCODE = '28P01';
    END IF;

    IF v_hash <> crypt(p_current, v_hash) THEN
        RAISE EXCEPTION 'invalid current password' USING ERRCODE = '28P01';
    END IF;

    -- policy (authority): length + must differ from current.
    IF length(p_new) < 12 THEN
        RAISE EXCEPTION 'new password too short (min 12 chars)' USING ERRCODE = 'check_violation';
    END IF;
    IF crypt(p_new, v_hash) = v_hash THEN
        RAISE EXCEPTION 'new password must differ from the current password' USING ERRCODE = 'check_violation';
    END IF;

    UPDATE users
       SET password_hash        = crypt(p_new, gen_salt('bf', 10)),
           must_change_password = FALSE
     WHERE id = p_user_id;
END;
$$ LANGUAGE plpgsql;

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin

-- Restore the 00003 body verbatim before the column goes, or the function would
-- reference a column that no longer exists.
CREATE OR REPLACE FUNCTION change_password(
    p_user_id UUID,
    p_current TEXT,
    p_new     TEXT
) RETURNS VOID AS $$
DECLARE
    v_hash TEXT;
BEGIN
    SELECT password_hash INTO v_hash
      FROM users
     WHERE id = p_user_id AND status = 'active'
     FOR UPDATE;
    IF NOT FOUND THEN
        RAISE EXCEPTION 'invalid current password' USING ERRCODE = '28P01';
    END IF;

    IF v_hash <> crypt(p_current, v_hash) THEN
        RAISE EXCEPTION 'invalid current password' USING ERRCODE = '28P01';
    END IF;

    IF length(p_new) < 12 THEN
        RAISE EXCEPTION 'new password too short (min 12 chars)' USING ERRCODE = 'check_violation';
    END IF;
    IF crypt(p_new, v_hash) = v_hash THEN
        RAISE EXCEPTION 'new password must differ from the current password' USING ERRCODE = 'check_violation';
    END IF;

    UPDATE users
       SET password_hash = crypt(p_new, gen_salt('bf', 10))
     WHERE id = p_user_id;
END;
$$ LANGUAGE plpgsql;

-- +goose StatementEnd

ALTER TABLE users DROP COLUMN must_change_password;
