-- 00019_login_returns_must_change_password.sql
--
-- must_change_password (00018) only bound the portal: requireSession checked it,
-- requireJWT did not, so an admin-flagged customer signed back in with the OLD
-- password and used the whole client API. Fix: the two functions that authenticate
-- also report the flag, so the API can mint it into the access token and gate the
-- client surface without a second round trip. Why: issue #99, docs/06.
--
-- RETURNS TABLE columns cannot be added with CREATE OR REPLACE (42P13), so both
-- functions are dropped and recreated. Bodies are 00018 and 00004 verbatim plus
-- the new column.

-- +goose Up
DROP FUNCTION IF EXISTS check_user_credentials(CITEXT, TEXT);
DROP FUNCTION IF EXISTS rotate_refresh_token(TEXT, TEXT, INT, INT, TEXT, TEXT);

-- +goose StatementBegin

-- check_user_credentials (00018) + must_change_password. A locked account still
-- returns no rows — same answer as a wrong password, so lockout is not an
-- enumeration oracle.
CREATE FUNCTION check_user_credentials(
    p_username CITEXT,
    p_password TEXT
) RETURNS TABLE(user_id UUID, role user_role, username CITEXT, must_change_password BOOLEAN) AS $$
DECLARE v_id UUID; v_role user_role; v_uname CITEXT; v_must BOOLEAN;
BEGIN
    SELECT u.id, u.role, u.username, u.must_change_password
      INTO v_id, v_role, v_uname, v_must
    FROM users u
    WHERE u.username = p_username
      AND u.status = 'active'
      AND (u.login_locked_until IS NULL OR u.login_locked_until <= now())
      AND u.password_hash = crypt(p_password, u.password_hash);

    IF NOT FOUND THEN
        RETURN;
    END IF;

    UPDATE users
       SET failed_login_attempts = 0, login_locked_until = NULL
     WHERE id = v_id AND (failed_login_attempts <> 0 OR login_locked_until IS NOT NULL);

    RETURN QUERY SELECT v_id, v_role, v_uname, v_must;
END;
$$ LANGUAGE plpgsql;

-- +goose StatementEnd

-- +goose StatementBegin

-- rotate_refresh_token (00004) + must_change_password: a flag raised after login
-- takes effect on the next rotation, so a refreshing client cannot outrun it.
CREATE FUNCTION rotate_refresh_token(
    p_old_hash         TEXT,
    p_new_hash         TEXT,
    p_idle_seconds     INT,
    p_absolute_seconds INT,
    p_user_agent       TEXT DEFAULT NULL,
    p_ip               TEXT DEFAULT NULL
) RETURNS TABLE(user_id UUID, family_id UUID, role user_role, username CITEXT,
                must_change_password BOOLEAN) AS $$
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
        SELECT r.user_id, r.family_id, u.role, u.username, u.must_change_password
        FROM users u WHERE u.id = r.user_id;
END;
$$ LANGUAGE plpgsql;

-- +goose StatementEnd

-- +goose Down
DROP FUNCTION IF EXISTS check_user_credentials(CITEXT, TEXT);
DROP FUNCTION IF EXISTS rotate_refresh_token(TEXT, TEXT, INT, INT, TEXT, TEXT);

-- +goose StatementBegin

-- 00018 verbatim.
CREATE FUNCTION check_user_credentials(
    p_username CITEXT,
    p_password TEXT
) RETURNS TABLE(user_id UUID, role user_role, username CITEXT) AS $$
DECLARE v_id UUID; v_role user_role; v_uname CITEXT;
BEGIN
    SELECT u.id, u.role, u.username INTO v_id, v_role, v_uname
    FROM users u
    WHERE u.username = p_username
      AND u.status = 'active'
      AND (u.login_locked_until IS NULL OR u.login_locked_until <= now())
      AND u.password_hash = crypt(p_password, u.password_hash);

    IF NOT FOUND THEN
        RETURN;
    END IF;

    UPDATE users
       SET failed_login_attempts = 0, login_locked_until = NULL
     WHERE id = v_id AND (failed_login_attempts <> 0 OR login_locked_until IS NOT NULL);

    RETURN QUERY SELECT v_id, v_role, v_uname;
END;
$$ LANGUAGE plpgsql;

-- +goose StatementEnd

-- +goose StatementBegin

-- 00004 verbatim.
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

    RETURN QUERY
        SELECT r.user_id, r.family_id, u.role, u.username
        FROM users u WHERE u.id = r.user_id;
END;
$$ LANGUAGE plpgsql;

-- +goose StatementEnd
