-- +goose Up
-- +goose StatementBegin

-- change_password: verify the current password (bcrypt via pgcrypto) and store the
-- new hash in one statement, so a concurrent reset can't interleave. Password
-- POLICY lives here (DB-first, like every other rule): >= 12 chars and != current.
-- Typed SQLSTATEs the API maps to HTTP:
--   * wrong current password / non-active user -> 28P01          (mapped 401; no enumeration)
--   * new password fails policy                -> check_violation (mapped 422)
-- bcrypt discipline (crypt / gen_salt('bf',10)) is identical to 00006.
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
       SET password_hash = crypt(p_new, gen_salt('bf', 10))
     WHERE id = p_user_id;
END;
$$ LANGUAGE plpgsql;

-- revoke_user_refresh_except_family: "log out everywhere else". Revokes every live
-- refresh family for the user EXCEPT the family the current session belongs to.
-- p_keep_family may be NULL (revoke all). Returns the count revoked. The reason
-- 'password_change' extends the revoked_reason vocabulary documented in 00017
-- (the column is plain TEXT, no constraint to alter).
CREATE OR REPLACE FUNCTION revoke_user_refresh_except_family(
    p_user_id     UUID,
    p_keep_family UUID DEFAULT NULL
) RETURNS INTEGER AS $$
DECLARE v_n INTEGER;
BEGIN
    UPDATE refresh_tokens
       SET revoked_at = now(), revoked_reason = 'password_change'
     WHERE user_id = p_user_id
       AND revoked_at IS NULL
       AND (p_keep_family IS NULL OR family_id <> p_keep_family);
    GET DIAGNOSTICS v_n = ROW_COUNT;
    RETURN v_n;
END;
$$ LANGUAGE plpgsql;

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DROP FUNCTION IF EXISTS revoke_user_refresh_except_family(UUID, UUID);
DROP FUNCTION IF EXISTS change_password(UUID, TEXT, TEXT);
-- +goose StatementEnd
