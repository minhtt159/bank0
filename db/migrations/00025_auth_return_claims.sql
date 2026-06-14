-- +goose Up
-- AUTH-1: have the credential-check and rotate functions return the JWT claims
-- (role + username) so the Go handlers no longer make a separate GetUserByID round
-- trip purely to mint the access token. Login drops from 3 sequential DB trips to 2,
-- Refresh from 2 to 1. The user lookup moves inside the function (same txn, indexed
-- by PK) instead of a second Go->DB hop.

-- +goose StatementBegin
DROP FUNCTION IF EXISTS check_user_credentials(CITEXT, TEXT);
-- +goose StatementEnd

-- +goose StatementBegin
-- Now set-returning: 1 row (id, role, username) on success, 0 rows on bad creds.
CREATE FUNCTION check_user_credentials(
    p_username CITEXT,
    p_password TEXT
) RETURNS TABLE(user_id UUID, role user_role, username CITEXT) AS $$
    SELECT u.id, u.role, u.username
    FROM users u
    WHERE u.username = p_username
      AND u.status = 'active'
      AND u.password_hash = crypt(p_password, u.password_hash);
$$ LANGUAGE sql;
-- +goose StatementEnd

-- +goose StatementBegin
DROP FUNCTION IF EXISTS rotate_refresh_token(TEXT, TEXT, INT, INT, TEXT, TEXT);
-- +goose StatementEnd

-- +goose StatementBegin
CREATE FUNCTION rotate_refresh_token(
    p_old_hash         TEXT,
    p_new_hash         TEXT,
    p_idle_seconds     INT,
    p_absolute_seconds INT,
    p_user_agent       TEXT DEFAULT NULL,
    p_ip               TEXT DEFAULT NULL
) RETURNS TABLE(user_id UUID, family_id UUID, role user_role, username CITEXT) AS $$
DECLARE
    r            refresh_tokens;
    v_fam_start  TIMESTAMPTZ;
BEGIN
    SELECT * INTO r FROM refresh_tokens WHERE id = p_old_hash FOR UPDATE;
    IF NOT FOUND THEN
        RAISE EXCEPTION 'unknown refresh token' USING ERRCODE = '28P01';
    END IF;

    -- reuse detection: an already-rotated or revoked token is being replayed.
    -- We only RAISE here (any UPDATE in this branch would be rolled back with the
    -- aborted statement); the API revokes the family in a separate statement via
    -- revoke_refresh_family() so the revocation actually persists.
    IF r.rotated_at IS NOT NULL OR r.revoked_at IS NOT NULL THEN
        RAISE EXCEPTION 'refresh token reuse detected' USING ERRCODE = '28000';
    END IF;

    IF r.expires_at <= now() THEN
        RAISE EXCEPTION 'refresh token expired' USING ERRCODE = '28P01';
    END IF;

    -- absolute cap on the whole family (regardless of sliding idle activity).
    SELECT min(issued_at) INTO v_fam_start
      FROM refresh_tokens WHERE refresh_tokens.family_id = r.family_id;
    IF now() > v_fam_start + make_interval(secs => p_absolute_seconds) THEN
        RAISE EXCEPTION 'refresh token family expired' USING ERRCODE = '28P01';
    END IF;

    UPDATE refresh_tokens SET rotated_at = now() WHERE id = r.id;
    INSERT INTO refresh_tokens (id, family_id, user_id, parent_id, expires_at, user_agent, ip)
    VALUES (p_new_hash, r.family_id, r.user_id, r.id,
            now() + make_interval(secs => p_idle_seconds), p_user_agent, p_ip);

    RETURN QUERY
        SELECT r.user_id, r.family_id, u.role, u.username
        FROM users u WHERE u.id = r.user_id;
END;
$$ LANGUAGE plpgsql;
-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DROP FUNCTION IF EXISTS check_user_credentials(CITEXT, TEXT);
-- +goose StatementEnd

-- +goose StatementBegin
CREATE FUNCTION check_user_credentials(
    p_username CITEXT,
    p_password TEXT
) RETURNS UUID AS $$
DECLARE
    v_id UUID;
BEGIN
    SELECT id INTO v_id
    FROM users
    WHERE username = p_username
      AND status = 'active'
      AND password_hash = crypt(p_password, password_hash);
    RETURN v_id;   -- NULL when invalid; caller maps to 401
END;
$$ LANGUAGE plpgsql;
-- +goose StatementEnd

-- +goose StatementBegin
DROP FUNCTION IF EXISTS rotate_refresh_token(TEXT, TEXT, INT, INT, TEXT, TEXT);
-- +goose StatementEnd

-- +goose StatementBegin
CREATE FUNCTION rotate_refresh_token(
    p_old_hash         TEXT,
    p_new_hash         TEXT,
    p_idle_seconds     INT,
    p_absolute_seconds INT,
    p_user_agent       TEXT DEFAULT NULL,
    p_ip               TEXT DEFAULT NULL
) RETURNS TABLE(user_id UUID, family_id UUID) AS $$
DECLARE
    r            refresh_tokens;
    v_fam_start  TIMESTAMPTZ;
BEGIN
    SELECT * INTO r FROM refresh_tokens WHERE id = p_old_hash FOR UPDATE;
    IF NOT FOUND THEN
        RAISE EXCEPTION 'unknown refresh token' USING ERRCODE = '28P01';
    END IF;

    IF r.rotated_at IS NOT NULL OR r.revoked_at IS NOT NULL THEN
        RAISE EXCEPTION 'refresh token reuse detected' USING ERRCODE = '28000';
    END IF;

    IF r.expires_at <= now() THEN
        RAISE EXCEPTION 'refresh token expired' USING ERRCODE = '28P01';
    END IF;

    SELECT min(issued_at) INTO v_fam_start
      FROM refresh_tokens WHERE refresh_tokens.family_id = r.family_id;
    IF now() > v_fam_start + make_interval(secs => p_absolute_seconds) THEN
        RAISE EXCEPTION 'refresh token family expired' USING ERRCODE = '28P01';
    END IF;

    UPDATE refresh_tokens SET rotated_at = now() WHERE id = r.id;
    INSERT INTO refresh_tokens (id, family_id, user_id, parent_id, expires_at, user_agent, ip)
    VALUES (p_new_hash, r.family_id, r.user_id, r.id,
            now() + make_interval(secs => p_idle_seconds), p_user_agent, p_ip);

    RETURN QUERY SELECT r.user_id, r.family_id;
END;
$$ LANGUAGE plpgsql;
-- +goose StatementEnd
