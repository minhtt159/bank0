-- 00020_bcrypt_cost_12.sql
--
-- OWASP's bcrypt floor is exactly 10, which is where every gen_salt('bf', 10) in
-- this schema sits. Issue #121 raises it to 12 — four rounds' worth, 4x the work
-- per verification — before the client API is exposed to the internet (#124),
-- because a public login endpoint is where the cost actually buys something.
--
-- Two halves:
--
--   1. Every function that WRITES a password hash now writes cost 12. Bodies are
--      verbatim from 00003 / 00018 with the one literal changed; they are repeated
--      in full because CREATE OR REPLACE has no way to patch a line.
--
--   2. Every function that VERIFIES one re-hashes at cost 12 on success. A
--      migration cannot bulk-rehash — it has no plaintext — so existing hashes
--      upgrade the next time their owner signs in, and a dormant account keeps
--      its cost-10 hash until it is used. That is the standard trade and it is
--      why this is not a data migration.
--
-- Account PINs (create_account, 00007) deliberately stay at cost 10: a PIN is
-- four digits, so the search space (10^4), not the KDF cost, is what bounds an
-- offline attack there. Raising it would cost 4x per transfer for nothing.
--
-- Not covered here: no function re-costs a hash on a FAILED login, by design —
-- that would let an unauthenticated caller spend 4x CPU per guess.

-- +goose Up

-- create_user: Admin-created user. Cost 10 -> 12.
-- +goose StatementBegin

CREATE OR REPLACE FUNCTION create_user(
    p_username     CITEXT,
    p_password     TEXT,
    p_full_name    TEXT,
    p_email        CITEXT      DEFAULT NULL,
    p_phone_number VARCHAR(16) DEFAULT NULL,
    p_role         user_role   DEFAULT 'customer'
) RETURNS UUID AS $$
DECLARE
    v_user_id UUID;
BEGIN
    INSERT INTO users (username, password_hash, full_name, email, phone_number, role)
    VALUES (
        p_username,
        crypt(p_password, gen_salt('bf', 12)),
        p_full_name,
        NULLIF(p_email, ''),          -- store NULL (not '') so unique index ignores it
        NULLIF(p_phone_number, ''),
        p_role
    )
    RETURNING id INTO v_user_id;
    RETURN v_user_id;
END;
$$ LANGUAGE plpgsql;

-- +goose StatementEnd

-- update_user_info: Admin password reset (p_password NULL leaves the hash alone). Cost 10 -> 12.
-- +goose StatementBegin

CREATE OR REPLACE FUNCTION update_user_info(
    p_user_id      UUID,
    p_full_name    TEXT        DEFAULT NULL,
    p_email        CITEXT      DEFAULT NULL,
    p_phone_number VARCHAR(16) DEFAULT NULL,
    p_password     TEXT        DEFAULT NULL,
    p_status       user_status DEFAULT NULL
) RETURNS VOID AS $$
BEGIN
    IF NOT EXISTS (SELECT 1 FROM users WHERE id = p_user_id) THEN
        RAISE EXCEPTION 'user % does not exist', p_user_id;
    END IF;

    UPDATE users SET
        full_name     = COALESCE(p_full_name, full_name),
        email         = COALESCE(NULLIF(p_email, ''), email),
        phone_number  = COALESCE(NULLIF(p_phone_number, ''), phone_number),
        password_hash = CASE WHEN p_password IS NOT NULL AND p_password <> ''
                             THEN crypt(p_password, gen_salt('bf', 12))
                             ELSE password_hash END,
        status        = COALESCE(p_status, status)
    WHERE id = p_user_id;
END;
$$ LANGUAGE plpgsql;

-- +goose StatementEnd

-- change_password: Self-service change, both surfaces. Cost 10 -> 12.
-- +goose StatementBegin

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
       SET password_hash        = crypt(p_new, gen_salt('bf', 12)),
           must_change_password = FALSE,
           password_changed_at  = now()
     WHERE id = p_user_id;
END;
$$ LANGUAGE plpgsql;

-- +goose StatementEnd

-- register_user: Self-service registration. Cost 10 -> 12.
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
    VALUES (p_username, crypt(p_password, gen_salt('bf', 12)), p_full_name,
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

-- check_user_credentials: client login. Verbatim from 00019 plus the rehash.
-- +goose StatementBegin

CREATE OR REPLACE FUNCTION check_user_credentials(
    p_username CITEXT,
    p_password TEXT
) RETURNS TABLE(user_id UUID, role user_role, username CITEXT, must_change_password BOOLEAN) AS $$
DECLARE v_id UUID; v_role user_role; v_uname CITEXT; v_must BOOLEAN; v_hash TEXT;
BEGIN
    SELECT u.id, u.role, u.username, u.must_change_password, u.password_hash
      INTO v_id, v_role, v_uname, v_must, v_hash
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

    -- Opportunistic cost upgrade. The password is in hand and already verified,
    -- so this is the only moment a stored hash can be re-costed without asking
    -- the customer for anything. A hash written before 00020 stays at cost 10
    -- until its owner next signs in; there is no bulk rehash, because a migration
    -- has no plaintext to rehash with.
    IF split_part(v_hash, '$', 3)::INT < 12 THEN
        UPDATE users SET password_hash = crypt(p_password, gen_salt('bf', 12))
         WHERE id = v_id;
    END IF;

    RETURN QUERY SELECT v_id, v_role, v_uname, v_must;
END;
$$ LANGUAGE plpgsql;

-- +goose StatementEnd

-- create_staff_session: portal login. Verbatim from 00018 plus the rehash.
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
    v_hash   TEXT;
BEGIN
    SELECT u.id, u.role, u.status, u.password_hash
      INTO v_id, v_role, v_status, v_hash
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

    -- Opportunistic cost upgrade. The password is in hand and already verified,
    -- so this is the only moment a stored hash can be re-costed without asking
    -- the customer for anything. A hash written before 00020 stays at cost 10
    -- until its owner next signs in; there is no bulk rehash, because a migration
    -- has no plaintext to rehash with.
    IF split_part(v_hash, '$', 3)::INT < 12 THEN
        UPDATE users SET password_hash = crypt(p_password, gen_salt('bf', 12))
         WHERE id = v_id;
    END IF;

    INSERT INTO sessions (id, user_id, expires_at, user_agent, ip)
    VALUES (p_token_hash, v_id, now() + make_interval(secs => p_idle_seconds), p_user_agent, p_ip);

    RETURN QUERY SELECT v_id, p_username, v_role;
END;
$$ LANGUAGE plpgsql;

-- +goose StatementEnd

-- +goose Down

-- Restores the cost-10 writers and the rehash-free login functions. Hashes
-- already upgraded to cost 12 keep working: crypt() re-derives from the stored
-- hash's own cost, so a rollback is a no-op for existing rows.

-- +goose StatementBegin

CREATE OR REPLACE FUNCTION create_user(
    p_username     CITEXT,
    p_password     TEXT,
    p_full_name    TEXT,
    p_email        CITEXT      DEFAULT NULL,
    p_phone_number VARCHAR(16) DEFAULT NULL,
    p_role         user_role   DEFAULT 'customer'
) RETURNS UUID AS $$
DECLARE
    v_user_id UUID;
BEGIN
    INSERT INTO users (username, password_hash, full_name, email, phone_number, role)
    VALUES (
        p_username,
        crypt(p_password, gen_salt('bf', 10)),
        p_full_name,
        NULLIF(p_email, ''),          -- store NULL (not '') so unique index ignores it
        NULLIF(p_phone_number, ''),
        p_role
    )
    RETURNING id INTO v_user_id;
    RETURN v_user_id;
END;
$$ LANGUAGE plpgsql;

-- +goose StatementEnd

-- +goose StatementBegin

CREATE OR REPLACE FUNCTION update_user_info(
    p_user_id      UUID,
    p_full_name    TEXT        DEFAULT NULL,
    p_email        CITEXT      DEFAULT NULL,
    p_phone_number VARCHAR(16) DEFAULT NULL,
    p_password     TEXT        DEFAULT NULL,
    p_status       user_status DEFAULT NULL
) RETURNS VOID AS $$
BEGIN
    IF NOT EXISTS (SELECT 1 FROM users WHERE id = p_user_id) THEN
        RAISE EXCEPTION 'user % does not exist', p_user_id;
    END IF;

    UPDATE users SET
        full_name     = COALESCE(p_full_name, full_name),
        email         = COALESCE(NULLIF(p_email, ''), email),
        phone_number  = COALESCE(NULLIF(p_phone_number, ''), phone_number),
        password_hash = CASE WHEN p_password IS NOT NULL AND p_password <> ''
                             THEN crypt(p_password, gen_salt('bf', 10))
                             ELSE password_hash END,
        status        = COALESCE(p_status, status)
    WHERE id = p_user_id;
END;
$$ LANGUAGE plpgsql;

-- +goose StatementEnd

-- +goose StatementBegin

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
           must_change_password = FALSE,
           password_changed_at  = now()
     WHERE id = p_user_id;
END;
$$ LANGUAGE plpgsql;

-- +goose StatementEnd

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

CREATE OR REPLACE FUNCTION check_user_credentials(
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
