-- 00018_operator_password_rotation.sql
--
-- Seeded admin/admin (00016) had no rotation path: /me/password is client-surface,
-- admin JSON refuses password writes, console had no screen. Adds the flag that
-- forces it, plus one shared password policy. Why: docs/05 §4.6a, docs/10.
--
-- First migration after the v1.0.0 freeze.

-- +goose Up
ALTER TABLE users ADD COLUMN must_change_password  BOOLEAN  NOT NULL DEFAULT FALSE;
-- Per-account login throttling (NIST SP 800-63B Rev 4 §3.2.2). The per-IP limiter
-- is the outer layer; distributed stuffing walks past it. docs/10.
ALTER TABLE users ADD COLUMN failed_login_attempts SMALLINT NOT NULL DEFAULT 0;
ALTER TABLE users ADD COLUMN login_locked_until    TIMESTAMPTZ;

-- Only if it STILL holds the seeded password — crypt() re-derives using the stored
-- hash as salt. An operator who already rotated by hand is not nagged.
UPDATE users
   SET must_change_password = TRUE
 WHERE username = 'admin'
   AND password_hash = crypt('admin', password_hash);

-- +goose StatementBegin

-- assert_password_policy: ONE place for password rules, called by change_password
-- and register_user.
--   min 12 chars.
--   max 72 BYTES: bcrypt truncates there, so a longer passphrase silently keeps
--   only its first 72 bytes and anyone knowing that prefix authenticates.
CREATE OR REPLACE FUNCTION assert_password_policy(p_password TEXT) RETURNS VOID AS $$
BEGIN
    IF length(p_password) < 12 THEN
        RAISE EXCEPTION 'new password too short (min 12 chars)' USING ERRCODE = 'check_violation';
    END IF;
    IF octet_length(p_password) > 72 THEN
        RAISE EXCEPTION 'new password too long (max 72 bytes)' USING ERRCODE = 'check_violation';
    END IF;
END;
$$ LANGUAGE plpgsql IMMUTABLE;

-- +goose StatementEnd

-- +goose StatementBegin

-- change_password (00003) + the flag clear. Clearing lives here, not in a handler:
-- this is the only writer of password_hash.
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

    PERFORM assert_password_policy(p_new);
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

-- +goose StatementBegin

-- register_user (00005), unchanged except that its inline min-length check becomes
-- the shared policy call — registration was the other bcrypt-truncation entry point.
CREATE OR REPLACE FUNCTION register_user(
    p_idempotency_key TEXT,
    p_username        CITEXT,
    p_password        TEXT,
    p_full_name       TEXT,
    p_email           CITEXT,
    p_phone_number    VARCHAR(16),
    p_channel         verification_channel,
    p_destination     TEXT,
    p_token_hash      TEXT,
    p_code_hash       TEXT,
    p_verify_token    TEXT,
    p_invite_code     TEXT
) RETURNS TABLE (user_id UUID, was_replay BOOLEAN, response JSONB) AS $$
DECLARE
    -- scalar vars, not idempotency_keys%ROWTYPE: %ROWTYPE resolves at CREATE
    -- time and the table lives in 00008 (this function only runs after both exist).
    v_hash      TEXT;
    v_ex_scope  TEXT;
    v_ex_hash   TEXT;
    v_ex_status ik_status;
    v_ex_resp   JSONB;
    v_id        UUID;
    v_resp      JSONB;
    v_inv       RECORD;
BEGIN
    IF p_idempotency_key IS NULL OR p_idempotency_key = '' THEN
        RAISE EXCEPTION 'idempotency key is required' USING ERRCODE = 'check_violation';
    END IF;
    -- The invite code AND the password are part of the fingerprint: a replay of the
    -- same key with ANY different parameter is a mismatch (-> 23514), not a silent
    -- success. Without the password, a client retrying with a corrected one would
    -- get back the original account, still holding the typo'd password.
    v_hash := encode(digest(
        COALESCE(p_username::text,'') || '|' || COALESCE(p_email::text,'') || '|' ||
        COALESCE(p_phone_number,'')   || '|' || COALESCE(p_full_name,'')   || '|' ||
        COALESCE(p_invite_code,'')    || '|' || COALESCE(p_password,''), 'sha256'), 'hex');

    -- Pre-auth: there is no authenticated principal yet. Registration claims live in
    -- a DEDICATED sentinel namespace, 0…01 — distinct from the all-zero UUID, which
    -- is the money/system namespace. Namespacing them apart keeps a client-chosen
    -- register key from squatting a deterministic system transfer key (e.g. the
    -- 'dispute-reimburse-<id>' keys minted under the all-zero owner).
    INSERT INTO idempotency_keys (owner_id, key, scope, request_hash, status)
    VALUES ('00000000-0000-0000-0000-000000000001', p_idempotency_key, 'register', v_hash, 'in_progress')
    ON CONFLICT (owner_id, key) DO NOTHING;

    IF NOT FOUND THEN
        SELECT ik.scope, ik.request_hash, ik.status, ik.response
          INTO v_ex_scope, v_ex_hash, v_ex_status, v_ex_resp
          FROM idempotency_keys ik
         WHERE ik.owner_id = '00000000-0000-0000-0000-000000000001' AND ik.key = p_idempotency_key;
        IF v_ex_scope <> 'register' OR v_ex_hash <> v_hash THEN
            RAISE EXCEPTION 'idempotency key reused with different parameters'
                USING ERRCODE = 'check_violation';
        END IF;
        IF v_ex_status = 'in_progress' THEN
            RAISE EXCEPTION 'request with this idempotency key is in progress'
                USING ERRCODE = 'object_in_use';   -- -> 409
        END IF;
        RETURN QUERY SELECT (v_ex_resp->>'user_id')::uuid, TRUE, v_ex_resp;
        RETURN;
    END IF;

    -- fresh key: validate + create
    IF p_email IS NULL AND p_phone_number IS NULL THEN
        RAISE EXCEPTION 'at least one of email or phone is required'
            USING ERRCODE = 'check_violation';
    END IF;
    PERFORM assert_password_policy(p_password);

    -- Invitation gate (fresh path only): the code must exist, be unconsumed and
    -- unexpired. Locked FOR UPDATE so two concurrent fresh registrations can't
    -- consume the same single-use code.
    IF p_invite_code IS NULL OR p_invite_code = '' THEN
        RAISE EXCEPTION 'invitation code required' USING ERRCODE = 'check_violation';
    END IF;
    SELECT i.id, i.inviter_id, i.consumed_at, i.expires_at INTO v_inv
      FROM invitations i WHERE i.code = p_invite_code FOR UPDATE;
    IF NOT FOUND THEN
        RAISE EXCEPTION 'invitation code not found';   -- P0001 -> 404
    END IF;
    IF v_inv.consumed_at IS NOT NULL THEN
        RAISE EXCEPTION 'invitation code already used' USING ERRCODE = 'check_violation'; -- -> 409
    END IF;
    IF v_inv.expires_at < now() THEN
        RAISE EXCEPTION 'invitation code expired' USING ERRCODE = 'check_violation';      -- -> 409
    END IF;

    INSERT INTO users (username, password_hash, full_name, email, phone_number,
                       role, status, onboarding_status)
    VALUES (p_username, crypt(p_password, gen_salt('bf', 10)), p_full_name,
            NULLIF(p_email, ''), NULLIF(p_phone_number, ''),
            'customer', 'locked', 'pending_verification')
    RETURNING id INTO v_id;

    -- Burn the invitation onto the new user (single-use; the row locked above).
    UPDATE invitations SET consumed_at = now(), invitee_id = v_id WHERE id = v_inv.id;

    PERFORM create_verification_challenge(v_id, p_channel, p_destination,
                                          p_token_hash, p_code_hash);

    v_resp := jsonb_build_object(
        'user_id', v_id,
        'onboarding_status', 'pending_verification',
        'verify_channel', p_channel,
        'verify_token', p_verify_token);
    UPDATE idempotency_keys SET status = 'completed', response = v_resp
     WHERE owner_id = '00000000-0000-0000-0000-000000000001' AND key = p_idempotency_key;

    RETURN QUERY SELECT v_id, FALSE, v_resp;
END;
$$ LANGUAGE plpgsql;

-- +goose StatementEnd

-- +goose StatementBegin

-- note_failed_login: one consecutive-failure step for an EXISTING user. Called by
-- Go AFTER a denial, never from inside the login functions — create_staff_session
-- RAISEs on denial and a RAISE rolls back its own writes, so the counter would
-- vanish with it (same pattern as revoke_refresh_family).
--
-- Unknown username is a no-op: no rows to grow, and nothing an attacker can probe.
-- At the threshold the account locks for a fixed window and the counter resets, so
-- each further lock costs another full run of attempts.
CREATE OR REPLACE FUNCTION note_failed_login(p_username CITEXT) RETURNS VOID AS $$
DECLARE
    c_max_attempts CONSTANT SMALLINT  := 10;
    c_lock_window  CONSTANT INTERVAL  := INTERVAL '15 minutes';
BEGIN
    UPDATE users
       SET failed_login_attempts = failed_login_attempts + 1
     WHERE username = p_username;

    UPDATE users
       SET login_locked_until    = now() + c_lock_window,
           failed_login_attempts = 0
     WHERE username = p_username
       AND failed_login_attempts >= c_max_attempts;
END;
$$ LANGUAGE plpgsql;

-- +goose StatementEnd

-- +goose StatementBegin

-- check_user_credentials (00003) + the lockout. plpgsql now, because success has to
-- clear the counter. A locked account returns no rows — same answer as a wrong
-- password, so lockout is not an enumeration oracle.
CREATE OR REPLACE FUNCTION check_user_credentials(
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

-- create_staff_session (00004) + the lockout. The lock is checked with the password
-- so a locked account cannot be told apart from a wrong one (both 28P01).
CREATE OR REPLACE FUNCTION create_staff_session(
    p_username     CITEXT,
    p_password     TEXT,
    p_token_hash   TEXT,
    p_idle_seconds INT,
    p_user_agent   TEXT DEFAULT NULL,
    p_ip           TEXT DEFAULT NULL
) RETURNS TABLE(user_id UUID, username CITEXT, role user_role) AS $$
DECLARE
    v_id     UUID;
    v_role   user_role;
    v_status user_status;
BEGIN
    SELECT u.id, u.role, u.status
      INTO v_id, v_role, v_status
      FROM users u
     WHERE u.username = p_username
       AND (u.login_locked_until IS NULL OR u.login_locked_until <= now())
       AND u.password_hash = crypt(p_password, u.password_hash);

    IF NOT FOUND THEN
        RAISE EXCEPTION 'invalid credentials' USING ERRCODE = '28P01';
    END IF;
    IF v_status <> 'active' THEN
        RAISE EXCEPTION 'account not active' USING ERRCODE = '28000';
    END IF;
    IF v_role NOT IN ('operator', 'admin', 'auditor') THEN
        RAISE EXCEPTION 'not authorized for console' USING ERRCODE = '42501';
    END IF;

    UPDATE users
       SET failed_login_attempts = 0, login_locked_until = NULL
     WHERE id = v_id AND (failed_login_attempts <> 0 OR login_locked_until IS NOT NULL);

    INSERT INTO sessions (id, user_id, expires_at, user_agent, ip)
    VALUES (p_token_hash, v_id, now() + make_interval(secs => p_idle_seconds), p_user_agent, p_ip);

    RETURN QUERY SELECT v_id, p_username, v_role;
END;
$$ LANGUAGE plpgsql;

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin

CREATE OR REPLACE FUNCTION check_user_credentials(
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

CREATE OR REPLACE FUNCTION create_staff_session(
    p_username     CITEXT,
    p_password     TEXT,
    p_token_hash   TEXT,
    p_idle_seconds INT,
    p_user_agent   TEXT DEFAULT NULL,
    p_ip           TEXT DEFAULT NULL
) RETURNS TABLE(user_id UUID, username CITEXT, role user_role) AS $$
DECLARE
    v_id     UUID;
    v_role   user_role;
    v_status user_status;
BEGIN
    SELECT u.id, u.role, u.status
      INTO v_id, v_role, v_status
      FROM users u
     WHERE u.username = p_username
       AND u.password_hash = crypt(p_password, u.password_hash);

    IF NOT FOUND THEN
        RAISE EXCEPTION 'invalid credentials' USING ERRCODE = '28P01';
    END IF;
    IF v_status <> 'active' THEN
        RAISE EXCEPTION 'account not active' USING ERRCODE = '28000';
    END IF;
    IF v_role NOT IN ('operator', 'admin', 'auditor') THEN
        RAISE EXCEPTION 'not authorized for console' USING ERRCODE = '42501';
    END IF;

    INSERT INTO sessions (id, user_id, expires_at, user_agent, ip)
    VALUES (p_token_hash, v_id, now() + make_interval(secs => p_idle_seconds), p_user_agent, p_ip);

    RETURN QUERY SELECT v_id, p_username, v_role;
END;
$$ LANGUAGE plpgsql;

-- +goose StatementEnd

DROP FUNCTION IF EXISTS note_failed_login(CITEXT);

-- +goose StatementBegin

CREATE OR REPLACE FUNCTION register_user(
    p_idempotency_key TEXT,
    p_username        CITEXT,
    p_password        TEXT,
    p_full_name       TEXT,
    p_email           CITEXT,
    p_phone_number    VARCHAR(16),
    p_channel         verification_channel,
    p_destination     TEXT,
    p_token_hash      TEXT,
    p_code_hash       TEXT,
    p_verify_token    TEXT,
    p_invite_code     TEXT
) RETURNS TABLE (user_id UUID, was_replay BOOLEAN, response JSONB) AS $$
DECLARE
    -- scalar vars, not idempotency_keys%ROWTYPE: %ROWTYPE resolves at CREATE
    -- time and the table lives in 00008 (this function only runs after both exist).
    v_hash      TEXT;
    v_ex_scope  TEXT;
    v_ex_hash   TEXT;
    v_ex_status ik_status;
    v_ex_resp   JSONB;
    v_id        UUID;
    v_resp      JSONB;
    v_inv       RECORD;
BEGIN
    IF p_idempotency_key IS NULL OR p_idempotency_key = '' THEN
        RAISE EXCEPTION 'idempotency key is required' USING ERRCODE = 'check_violation';
    END IF;
    -- The invite code AND the password are part of the fingerprint: a replay of the
    -- same key with ANY different parameter is a mismatch (-> 23514), not a silent
    -- success. Without the password, a client retrying with a corrected one would
    -- get back the original account, still holding the typo'd password.
    v_hash := encode(digest(
        COALESCE(p_username::text,'') || '|' || COALESCE(p_email::text,'') || '|' ||
        COALESCE(p_phone_number,'')   || '|' || COALESCE(p_full_name,'')   || '|' ||
        COALESCE(p_invite_code,'')    || '|' || COALESCE(p_password,''), 'sha256'), 'hex');

    -- Pre-auth: there is no authenticated principal yet. Registration claims live in
    -- a DEDICATED sentinel namespace, 0…01 — distinct from the all-zero UUID, which
    -- is the money/system namespace. Namespacing them apart keeps a client-chosen
    -- register key from squatting a deterministic system transfer key (e.g. the
    -- 'dispute-reimburse-<id>' keys minted under the all-zero owner).
    INSERT INTO idempotency_keys (owner_id, key, scope, request_hash, status)
    VALUES ('00000000-0000-0000-0000-000000000001', p_idempotency_key, 'register', v_hash, 'in_progress')
    ON CONFLICT (owner_id, key) DO NOTHING;

    IF NOT FOUND THEN
        SELECT ik.scope, ik.request_hash, ik.status, ik.response
          INTO v_ex_scope, v_ex_hash, v_ex_status, v_ex_resp
          FROM idempotency_keys ik
         WHERE ik.owner_id = '00000000-0000-0000-0000-000000000001' AND ik.key = p_idempotency_key;
        IF v_ex_scope <> 'register' OR v_ex_hash <> v_hash THEN
            RAISE EXCEPTION 'idempotency key reused with different parameters'
                USING ERRCODE = 'check_violation';
        END IF;
        IF v_ex_status = 'in_progress' THEN
            RAISE EXCEPTION 'request with this idempotency key is in progress'
                USING ERRCODE = 'object_in_use';   -- -> 409
        END IF;
        RETURN QUERY SELECT (v_ex_resp->>'user_id')::uuid, TRUE, v_ex_resp;
        RETURN;
    END IF;

    -- fresh key: validate + create
    IF p_email IS NULL AND p_phone_number IS NULL THEN
        RAISE EXCEPTION 'at least one of email or phone is required'
            USING ERRCODE = 'check_violation';
    END IF;
    IF length(p_password) < 12 THEN
        RAISE EXCEPTION 'password must be at least 12 characters'
            USING ERRCODE = 'check_violation';
    END IF;

    -- Invitation gate (fresh path only): the code must exist, be unconsumed and
    -- unexpired. Locked FOR UPDATE so two concurrent fresh registrations can't
    -- consume the same single-use code.
    IF p_invite_code IS NULL OR p_invite_code = '' THEN
        RAISE EXCEPTION 'invitation code required' USING ERRCODE = 'check_violation';
    END IF;
    SELECT i.id, i.inviter_id, i.consumed_at, i.expires_at INTO v_inv
      FROM invitations i WHERE i.code = p_invite_code FOR UPDATE;
    IF NOT FOUND THEN
        RAISE EXCEPTION 'invitation code not found';   -- P0001 -> 404
    END IF;
    IF v_inv.consumed_at IS NOT NULL THEN
        RAISE EXCEPTION 'invitation code already used' USING ERRCODE = 'check_violation'; -- -> 409
    END IF;
    IF v_inv.expires_at < now() THEN
        RAISE EXCEPTION 'invitation code expired' USING ERRCODE = 'check_violation';      -- -> 409
    END IF;

    INSERT INTO users (username, password_hash, full_name, email, phone_number,
                       role, status, onboarding_status)
    VALUES (p_username, crypt(p_password, gen_salt('bf', 10)), p_full_name,
            NULLIF(p_email, ''), NULLIF(p_phone_number, ''),
            'customer', 'locked', 'pending_verification')
    RETURNING id INTO v_id;

    -- Burn the invitation onto the new user (single-use; the row locked above).
    UPDATE invitations SET consumed_at = now(), invitee_id = v_id WHERE id = v_inv.id;

    PERFORM create_verification_challenge(v_id, p_channel, p_destination,
                                          p_token_hash, p_code_hash);

    v_resp := jsonb_build_object(
        'user_id', v_id,
        'onboarding_status', 'pending_verification',
        'verify_channel', p_channel,
        'verify_token', p_verify_token);
    UPDATE idempotency_keys SET status = 'completed', response = v_resp
     WHERE owner_id = '00000000-0000-0000-0000-000000000001' AND key = p_idempotency_key;

    RETURN QUERY SELECT v_id, FALSE, v_resp;
END;
$$ LANGUAGE plpgsql;

-- +goose StatementEnd

-- +goose StatementBegin

-- 00003 body verbatim: it must stop referencing the column before the column goes.
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

DROP FUNCTION IF EXISTS assert_password_policy(TEXT);
ALTER TABLE users DROP COLUMN login_locked_until;
ALTER TABLE users DROP COLUMN failed_login_attempts;
ALTER TABLE users DROP COLUMN must_change_password;
