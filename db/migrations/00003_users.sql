-- +goose Up
-- ─────────────────────────────────────────────────────────────────────────────
-- USER MODEL — identity, credentials, sessions, refresh tokens & onboarding
-- The people and their logins: the users table, operator (portal) cookie sessions,
-- client (api) refresh-token families, and public self-registration (onboarding
-- state + contact-verification challenges) — plus every function that creates a
-- user, checks credentials, changes a password, and mints / validates / rotates /
-- revokes sessions and refresh tokens. Password & PIN hashing is bcrypt via
-- pgcrypto (crypt / gen_salt('bf',10)); password POLICY is DB-first (rule 1). The
-- updated_at trigger on users is attached in core-banking (00004), once the shared
-- set_updated_at() trigger fn exists.
-- ─────────────────────────────────────────────────────────────────────────────

-- ─────────────────────────────────────────────────────────────────────────────
-- users
-- ─────────────────────────────────────────────────────────────────────────────
CREATE TABLE users (
    id                UUID PRIMARY KEY DEFAULT uuidv7(),
    username          CITEXT NOT NULL UNIQUE,
    password_hash     TEXT   NOT NULL,
    full_name         TEXT   NOT NULL,
    email             CITEXT UNIQUE,
    phone_number      VARCHAR(16) UNIQUE,
    role              user_role   NOT NULL DEFAULT 'customer',
    status            user_status NOT NULL DEFAULT 'active',
    -- Onboarding lifecycle (public self-registration), distinct from status which
    -- gates login. DEFAULT 'active' keeps every staff-created user unaffected;
    -- only register_user sets 'pending_verification'.
    onboarding_status onboarding_status NOT NULL DEFAULT 'active',
    email_verified_at TIMESTAMPTZ,
    phone_verified_at TIMESTAMPTZ,
    created_at        TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at        TIMESTAMPTZ NOT NULL DEFAULT now(),
    CHECK (email IS NULL OR email ~* '^[^@\s]+@[^@\s]+\.[^@\s]{2,}$')
);

-- search (substring + fuzzy). Trigram GIN powers ILIKE / word_similarity().
CREATE INDEX idx_users_username_trgm ON users USING gin ((username::text) gin_trgm_ops);
CREATE INDEX idx_users_fullname_trgm ON users USING gin (full_name        gin_trgm_ops);
CREATE INDEX idx_users_email_trgm    ON users USING gin ((email::text)    gin_trgm_ops);

-- ─────────────────────────────────────────────────────────────────────────────
-- sessions  (operator/portal cookie sessions)
-- The cookie holds an opaque random token; we store only its SHA-256 hash as the
-- PK, so a DB leak never exposes a live token. Idle timeout is enforced and slid
-- forward in the DB (validate_session), so all portal replicas share one source of
-- truth and there is no in-memory state.
-- ─────────────────────────────────────────────────────────────────────────────
CREATE TABLE sessions (
    id           TEXT PRIMARY KEY,                                   -- sha256(token) hex
    user_id      UUID NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    created_at   TIMESTAMPTZ NOT NULL DEFAULT now(),
    last_seen_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    expires_at   TIMESTAMPTZ NOT NULL,
    user_agent   TEXT,
    ip           TEXT
);
CREATE INDEX idx_sessions_user    ON sessions (user_id);
CREATE INDEX idx_sessions_expires ON sessions (expires_at);

-- ─────────────────────────────────────────────────────────────────────────────
-- refresh_tokens  (client/api refresh-token families, docs/06 §3)
-- Mirrors the sessions discipline: the PK is sha256(token) hex, so a DB leak never
-- yields a live token. A "family" is one login; rotation chains tokens via
-- parent_id, and presenting an already-used token (theft signal) revokes the whole
-- family. Access JWTs stay stateless. device_label is an optional per-family hint
-- set on the family's first token at login.
-- ─────────────────────────────────────────────────────────────────────────────
CREATE TABLE refresh_tokens (
    id             TEXT PRIMARY KEY,                       -- sha256(token) hex
    family_id      UUID NOT NULL DEFAULT uuidv7(),         -- one login = one family
    user_id        UUID NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    parent_id      TEXT REFERENCES refresh_tokens(id),     -- previous token in the chain
    issued_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    expires_at     TIMESTAMPTZ NOT NULL,                   -- idle window, slid on rotate
    rotated_at     TIMESTAMPTZ,                            -- set when consumed by rotate
    revoked_at     TIMESTAMPTZ,                            -- set on logout / reuse / force
    revoked_reason TEXT,                                   -- 'logout'|'reuse_detected'|'forced'|'expired'|'password_change'
    user_agent     TEXT,
    ip             TEXT,
    device_label   TEXT                                    -- optional per-family device hint
);
CREATE INDEX idx_refresh_user    ON refresh_tokens (user_id);
CREATE INDEX idx_refresh_family  ON refresh_tokens (family_id);
CREATE INDEX idx_refresh_expires ON refresh_tokens (expires_at);

-- ─────────────────────────────────────────────────────────────────────────────
-- verification_challenges  (self-registration contact verification)
-- One row per (user, channel) verification. Same hash-at-rest discipline as
-- refresh_tokens: only sha256 of the opaque verify_token and of the 6-digit code
-- are stored. A resend REFRESHES the pending row in place (same token, new code,
-- attempts reset) so the client's handle keeps working and UNIQUE(token_hash)
-- holds. TTL-swept by expire_verification_challenges() on the maintenance tick.
-- ─────────────────────────────────────────────────────────────────────────────
CREATE TABLE verification_challenges (
    id            UUID PRIMARY KEY DEFAULT uuidv7(),
    user_id       UUID NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    channel       verification_channel NOT NULL,
    destination   TEXT NOT NULL,                 -- email/phone snapshot at send time
    token_hash    TEXT NOT NULL UNIQUE,          -- sha256(verify_token)
    code_hash     TEXT NOT NULL,                 -- sha256(code)
    status        verification_status NOT NULL DEFAULT 'pending',
    attempts      SMALLINT NOT NULL DEFAULT 0,
    max_attempts  SMALLINT NOT NULL DEFAULT 5,
    last_sent_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    expires_at    TIMESTAMPTZ NOT NULL DEFAULT now() + INTERVAL '15 minutes',
    created_at    TIMESTAMPTZ NOT NULL DEFAULT now(),
    verified_at   TIMESTAMPTZ
);

-- at most one PENDING challenge per (user, channel)
CREATE UNIQUE INDEX uq_verif_pending ON verification_challenges (user_id, channel)
    WHERE status = 'pending';
CREATE INDEX idx_verif_expiry ON verification_challenges (expires_at) WHERE status = 'pending';

-- +goose StatementBegin

-- ─────────────────────────────────────────────────────────────────────────────
-- User / credential functions
-- ─────────────────────────────────────────────────────────────────────────────

-- create_user: passwords are hashed with bcrypt (pgcrypto). Never plaintext.
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

-- update_user_info: partial update; re-hashes password only when provided.
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

-- check_user_credentials: set-returning — 1 row (id, role, username) on success,
-- 0 rows on bad creds. Returns the JWT claims (role + username) so the API can mint
-- the access token without a second round trip.
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

-- change_password: verify the current password (bcrypt via pgcrypto) and store the
-- new hash in one statement, so a concurrent reset can't interleave. Password
-- POLICY lives here (DB-first, like every other rule): >= 12 chars and != current.
-- Typed SQLSTATEs the API maps to HTTP:
--   * wrong current password / non-active user -> 28P01          (mapped 401; no enumeration)
--   * new password fails policy                -> check_violation (mapped 422)
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

-- ─────────────────────────────────────────────────────────────────────────────
-- Self-registration & contact verification
-- ─────────────────────────────────────────────────────────────────────────────

-- create_verification_challenge: issue (or re-issue) the pending challenge for a
-- (user, channel). A resend REFRESHES the pending row in place — same token, new
-- code, attempts reset. Enforces the resend cooldown (53400 -> 429). The Go layer
-- supplies the already-hashed token+code (plaintext never reaches the DB here).
CREATE OR REPLACE FUNCTION create_verification_challenge(
    p_user_id     UUID,
    p_channel     verification_channel,
    p_destination TEXT,
    p_token_hash  TEXT,
    p_code_hash   TEXT,
    p_cooldown    INTERVAL DEFAULT INTERVAL '60 seconds',
    p_ttl         INTERVAL DEFAULT INTERVAL '15 minutes'
) RETURNS UUID AS $$
DECLARE v_prev verification_challenges%ROWTYPE; v_id UUID;
BEGIN
    SELECT * INTO v_prev FROM verification_challenges
     WHERE user_id = p_user_id AND channel = p_channel AND status = 'pending'
     FOR UPDATE;
    IF FOUND THEN
        IF v_prev.last_sent_at > now() - p_cooldown THEN
            RAISE EXCEPTION 'verification code recently sent; wait before retrying'
                USING ERRCODE = '53400';   -- configuration_limit_exceeded -> 429
        END IF;
        UPDATE verification_challenges
           SET code_hash = p_code_hash, token_hash = p_token_hash,
               destination = p_destination, attempts = 0,
               last_sent_at = now(), expires_at = now() + p_ttl
         WHERE id = v_prev.id;
        RETURN v_prev.id;
    END IF;

    INSERT INTO verification_challenges
        (user_id, channel, destination, token_hash, code_hash, expires_at)
    VALUES (p_user_id, p_channel, p_destination, p_token_hash, p_code_hash,
            now() + p_ttl)
    RETURNING id INTO v_id;
    RETURN v_id;
END;
$$ LANGUAGE plpgsql;

-- register_user: the whole public signup in ONE atomic call, gated by an
-- idempotency key exactly like request_transfer (claim-key + side effects +
-- stored response in one transaction; a concurrent duplicate blocks on the
-- in-flight insert). Creates the locked, pending-verification customer plus its
-- first verification challenge, and completes the key with the response JSONB.
-- A replayed key returns the stored response (was_replay = TRUE) so a mobile
-- retry never double-registers.
--
-- The response JSONB deliberately carries the PLAINTEXT verify_token: replay
-- must return the same token or the retrying client could never verify. The
-- token is single-use and dead after the challenge TTL (15 min), while the key
-- row lives 7 days — an acceptable, bounded exception to hash-at-rest.
--
-- Password policy matches change_password (>= 12 chars) — DB-first, one place.
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
    p_verify_token    TEXT
) RETURNS TABLE (user_id UUID, was_replay BOOLEAN, response JSONB) AS $$
DECLARE
    -- scalar vars, not idempotency_keys%ROWTYPE: %ROWTYPE resolves at CREATE
    -- time and the table lives in 00005 (this function only runs after both exist).
    v_hash      TEXT;
    v_ex_scope  TEXT;
    v_ex_hash   TEXT;
    v_ex_status ik_status;
    v_ex_resp   JSONB;
    v_id        UUID;
    v_resp      JSONB;
BEGIN
    IF p_idempotency_key IS NULL OR p_idempotency_key = '' THEN
        RAISE EXCEPTION 'idempotency key is required' USING ERRCODE = 'check_violation';
    END IF;
    v_hash := encode(digest(
        COALESCE(p_username::text,'') || '|' || COALESCE(p_email::text,'') || '|' ||
        COALESCE(p_phone_number,'')   || '|' || COALESCE(p_full_name,''), 'sha256'), 'hex');

    INSERT INTO idempotency_keys (key, scope, request_hash, status)
    VALUES (p_idempotency_key, 'register', v_hash, 'in_progress')
    ON CONFLICT (key) DO NOTHING;

    IF NOT FOUND THEN
        SELECT ik.scope, ik.request_hash, ik.status, ik.response
          INTO v_ex_scope, v_ex_hash, v_ex_status, v_ex_resp
          FROM idempotency_keys ik WHERE ik.key = p_idempotency_key;
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

    INSERT INTO users (username, password_hash, full_name, email, phone_number,
                       role, status, onboarding_status)
    VALUES (p_username, crypt(p_password, gen_salt('bf', 10)), p_full_name,
            NULLIF(p_email, ''), NULLIF(p_phone_number, ''),
            'customer', 'locked', 'pending_verification')
    RETURNING id INTO v_id;

    PERFORM create_verification_challenge(v_id, p_channel, p_destination,
                                          p_token_hash, p_code_hash);

    v_resp := jsonb_build_object(
        'user_id', v_id,
        'onboarding_status', 'pending_verification',
        'verify_channel', p_channel,
        'verify_token', p_verify_token);
    UPDATE idempotency_keys SET status = 'completed', response = v_resp
     WHERE key = p_idempotency_key;

    RETURN QUERY SELECT v_id, FALSE, v_resp;
END;
$$ LANGUAGE plpgsql;

-- verify_contact: consume a code against a pending challenge (looked up by token
-- hash). On success marks the challenge verified, stamps users.email/phone_
-- verified_at, promotes onboarding_status pending_verification -> verified and
-- status locked -> active (login becomes possible).
--
-- IMPORTANT (the RAISE-rolls-back trap, see CLAUDE.md): this function only READS
-- the attempt counter. A RAISE would roll back any increment written here, so the
-- failed-attempt increment is persisted by record_failed_verification(), which
-- the API calls in a SEPARATE statement after catching the 28000 — the same
-- pattern as rotate_refresh_token + revoke_refresh_family.
--   * unknown token          -> P0001  (-> 404)
--   * consumed/expired       -> 28000  (-> 401)
--   * attempts exhausted     -> 23514  (-> 422)
--   * wrong code             -> 28000  (-> 401; Go then records the attempt)
CREATE OR REPLACE FUNCTION verify_contact(
    p_token_hash TEXT,
    p_code_hash  TEXT
) RETURNS TABLE (user_id UUID, onboarding onboarding_status, channel verification_channel, login_ready BOOLEAN) AS $$
DECLARE v_c verification_challenges%ROWTYPE;
BEGIN
    SELECT * INTO v_c FROM verification_challenges
     WHERE token_hash = p_token_hash FOR UPDATE;
    IF NOT FOUND THEN RAISE EXCEPTION 'no verification challenge found'; END IF;  -- P0001 -> 404

    IF v_c.status <> 'pending' OR v_c.expires_at < now() THEN
        RAISE EXCEPTION 'verification code expired or already used'
            USING ERRCODE = '28000';   -- -> 401 (the TTL sweep flips the row)
    END IF;

    IF v_c.attempts >= v_c.max_attempts THEN
        RAISE EXCEPTION 'too many verification attempts' USING ERRCODE = 'check_violation'; -- -> 422
    END IF;

    IF v_c.code_hash <> p_code_hash THEN
        RAISE EXCEPTION 'invalid verification code' USING ERRCODE = '28000'; -- -> 401
    END IF;

    -- success
    UPDATE verification_challenges SET status = 'verified', verified_at = now()
     WHERE id = v_c.id;

    UPDATE users u SET
        email_verified_at = CASE WHEN v_c.channel = 'email' THEN now() ELSE u.email_verified_at END,
        phone_verified_at = CASE WHEN v_c.channel = 'phone' THEN now() ELSE u.phone_verified_at END,
        onboarding_status = CASE WHEN u.onboarding_status = 'pending_verification'
                                 THEN 'verified'::onboarding_status ELSE u.onboarding_status END,
        status            = CASE WHEN u.status = 'locked' THEN 'active'::user_status ELSE u.status END
     WHERE u.id = v_c.user_id;

    RETURN QUERY
        SELECT v_c.user_id,
               (SELECT u.onboarding_status FROM users u WHERE u.id = v_c.user_id),
               v_c.channel,
               TRUE;
END;
$$ LANGUAGE plpgsql;

-- record_failed_verification: persist one failed attempt against a pending
-- challenge. Called by the API in a separate statement after verify_contact
-- raises 'invalid verification code' (28000), because that RAISE rolled back
-- anything verify_contact itself wrote. Best-effort: unknown/consumed token is a
-- no-op (returns NULL).
CREATE OR REPLACE FUNCTION record_failed_verification(p_token_hash TEXT)
RETURNS SMALLINT AS $$
    UPDATE verification_challenges
       SET attempts = attempts + 1
     WHERE token_hash = p_token_hash AND status = 'pending'
    RETURNING attempts;
$$ LANGUAGE sql;

-- expire_verification_challenges: sweep stale pending challenges. Runs in the
-- maintenance tick alongside expire_holds.
CREATE OR REPLACE FUNCTION expire_verification_challenges() RETURNS INTEGER AS $$
DECLARE v_n INTEGER;
BEGIN
    UPDATE verification_challenges SET status = 'expired'
     WHERE status = 'pending' AND expires_at < now();
    GET DIAGNOSTICS v_n = ROW_COUNT;
    RETURN v_n;
END;
$$ LANGUAGE plpgsql;

-- ─────────────────────────────────────────────────────────────────────────────
-- Operator session functions
-- ─────────────────────────────────────────────────────────────────────────────

-- create_staff_session: verify credentials + staff role + active status, then
-- mint a session — all atomically. Raises (never silently succeeds) on failure;
-- the API maps the SQLSTATEs below to a single generic "login denied".
--   28P01 invalid_password · 28000 not active · 42501 not staff
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

-- validate_session: returns the session's user iff live, and slides the idle
-- timeout forward in the same statement. No rows => invalid/expired.
CREATE OR REPLACE FUNCTION validate_session(
    p_token_hash   TEXT,
    p_idle_seconds INT
) RETURNS TABLE(user_id UUID, username CITEXT, role user_role)
LANGUAGE sql AS $$
    WITH upd AS (
        UPDATE sessions
           SET last_seen_at = now(),
               expires_at   = now() + make_interval(secs => p_idle_seconds)
         WHERE id = p_token_hash AND expires_at > now()
        RETURNING user_id
    )
    SELECT u.id, u.username, u.role FROM upd JOIN users u ON u.id = upd.user_id;
$$;

CREATE OR REPLACE FUNCTION revoke_session(p_token_hash TEXT) RETURNS VOID
LANGUAGE sql AS $$ DELETE FROM sessions WHERE id = p_token_hash; $$;

-- cleanup_sessions: drop expired sessions (run by the maintenance sweep).
CREATE OR REPLACE FUNCTION cleanup_sessions() RETURNS INTEGER AS $$
DECLARE v_n INTEGER;
BEGIN
    DELETE FROM sessions WHERE expires_at <= now();
    GET DIAGNOSTICS v_n = ROW_COUNT;
    RETURN v_n;
END;
$$ LANGUAGE plpgsql;

-- ─────────────────────────────────────────────────────────────────────────────
-- Refresh-token functions
-- ─────────────────────────────────────────────────────────────────────────────

-- issue_refresh_token: open a new family at login, carrying the optional device
-- label. Returns the family id. Emits the 'device.new' security event in the
-- same txn (events, 00008) — one per family, deduped by the family-id partial
-- unique index, so an intrusion can't sign in silently.
CREATE OR REPLACE FUNCTION issue_refresh_token(
    p_user_id      UUID,
    p_token_hash   TEXT,
    p_idle_seconds INT,
    p_user_agent   TEXT DEFAULT NULL,
    p_ip           TEXT DEFAULT NULL,
    p_device_label TEXT DEFAULT NULL
) RETURNS UUID AS $$
DECLARE v_family UUID;
BEGIN
    INSERT INTO refresh_tokens (id, user_id, expires_at, user_agent, ip, device_label)
    VALUES (p_token_hash, p_user_id, now() + make_interval(secs => p_idle_seconds),
            p_user_agent, p_ip, p_device_label)
    RETURNING family_id INTO v_family;

    INSERT INTO events (user_id, type, title, body, data)
    VALUES (p_user_id, 'device.new', 'New sign-in',
            'A new device signed in to your account.',
            jsonb_build_object('family_id', v_family,
                               'user_agent', COALESCE(p_user_agent, ''),
                               'ip', COALESCE(p_ip, ''),
                               'device_label', COALESCE(p_device_label, '')))
    ON CONFLICT ((data->>'family_id')) WHERE type = 'device.new' DO NOTHING;

    RETURN v_family;
END;
$$ LANGUAGE plpgsql;

-- rotate_refresh_token: the heart of it, one atomic transition. Returns the JWT
-- claims (role + username) alongside the user/family so Refresh mints the access
-- token without a second round trip.
--   * live token   -> mark rotated, mint the child (same family), return the user
--   * already used  -> REUSE: RAISE 28000 (the API revokes the family via
--                       revoke_refresh_family() in a separate statement, then re-auth)
--   * expired/absolute-capped -> RAISE 28P01
--   * unknown       -> RAISE 28P01
-- The API maps 28000/28P01 to 401.
CREATE OR REPLACE FUNCTION rotate_refresh_token(
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

-- revoke_refresh_token: single-session logout. Best-effort/idempotent (no raise
-- if the token is unknown or already revoked).
CREATE OR REPLACE FUNCTION revoke_refresh_token(p_token_hash TEXT, p_reason TEXT DEFAULT 'logout')
RETURNS VOID LANGUAGE sql AS $$
    UPDATE refresh_tokens
       SET revoked_at = now(), revoked_reason = COALESCE(p_reason, 'logout')
     WHERE id = p_token_hash AND revoked_at IS NULL;
$$;

-- revoke_refresh_family: revoke every live token in the family that p_token_hash
-- belongs to. Called by the API (a separate statement) when rotate detects reuse,
-- so the revocation commits even though rotate's RAISE rolled back its own work.
CREATE OR REPLACE FUNCTION revoke_refresh_family(p_token_hash TEXT)
RETURNS VOID LANGUAGE sql AS $$
    UPDATE refresh_tokens
       SET revoked_at = now(), revoked_reason = 'reuse_detected'
     WHERE family_id = (SELECT family_id FROM refresh_tokens WHERE id = p_token_hash)
       AND revoked_at IS NULL;
$$;

-- revoke_user_refresh: "log out everywhere" / operator force-revoke. Returns the
-- number of live tokens revoked.
CREATE OR REPLACE FUNCTION revoke_user_refresh(p_user_id UUID, p_reason TEXT DEFAULT 'forced')
RETURNS INTEGER AS $$
DECLARE v_n INTEGER;
BEGIN
    UPDATE refresh_tokens
       SET revoked_at = now(), revoked_reason = COALESCE(p_reason, 'forced')
     WHERE user_id = p_user_id AND revoked_at IS NULL;
    GET DIAGNOSTICS v_n = ROW_COUNT;
    RETURN v_n;
END;
$$ LANGUAGE plpgsql;

-- revoke_user_refresh_except_family: "log out everywhere else". Revokes every live
-- refresh family for the user EXCEPT the family the current session belongs to.
-- p_keep_family may be NULL (revoke all). Returns the count revoked.
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

-- list_user_sessions: one row per ACTIVE family (device/login) for a user. A family is
-- active if its tip token (un-rotated, un-revoked, un-expired) is live. Label/UA/IP come
-- from the family's FIRST token; last_seen is the newest token's issued_at (last rotate).
CREATE OR REPLACE FUNCTION list_user_sessions(p_user_id UUID)
RETURNS TABLE (
    family_id    UUID,
    device_label TEXT,
    user_agent   TEXT,
    ip           TEXT,
    created_at   TIMESTAMPTZ,
    last_seen_at TIMESTAMPTZ
) AS $$
    WITH live AS (
        SELECT DISTINCT rt.family_id
          FROM refresh_tokens rt
         WHERE rt.user_id = p_user_id
           AND rt.revoked_at IS NULL
           AND rt.rotated_at IS NULL          -- the current tip of the chain
           AND rt.expires_at > now()
    )
    SELECT f.family_id, first.device_label, first.user_agent, first.ip,
           first.issued_at AS created_at,
           (SELECT max(rt2.issued_at) FROM refresh_tokens rt2
             WHERE rt2.family_id = f.family_id) AS last_seen_at
    FROM live f
    JOIN LATERAL (
        SELECT rt3.device_label, rt3.user_agent, rt3.ip, rt3.issued_at
          FROM refresh_tokens rt3
         WHERE rt3.family_id = f.family_id
         ORDER BY rt3.issued_at ASC
         LIMIT 1
    ) first ON TRUE
    ORDER BY last_seen_at DESC;
$$ LANGUAGE sql STABLE;

-- revoke_refresh_family_scoped: revoke one family iff it belongs to p_user_id. Returns
-- the count of live tokens revoked; 0 = not the caller's family OR already revoked (the
-- API distinguishes those via an ownership probe). Idempotent.
CREATE OR REPLACE FUNCTION revoke_refresh_family_scoped(p_user_id UUID, p_family_id UUID)
RETURNS INTEGER AS $$
DECLARE v_n INTEGER;
BEGIN
    UPDATE refresh_tokens
       SET revoked_at = now(), revoked_reason = 'logout'
     WHERE family_id = p_family_id AND user_id = p_user_id AND revoked_at IS NULL;
    GET DIAGNOSTICS v_n = ROW_COUNT;
    RETURN v_n;
END;
$$ LANGUAGE plpgsql;

-- cleanup_refresh_tokens: drop tokens long past their idle expiry or revocation
-- (run by the maintenance sweep, next to cleanup_sessions).
CREATE OR REPLACE FUNCTION cleanup_refresh_tokens() RETURNS INTEGER AS $$
DECLARE v_n INTEGER;
BEGIN
    DELETE FROM refresh_tokens
     WHERE expires_at <= now() - INTERVAL '1 day'
        OR (revoked_at IS NOT NULL AND revoked_at <= now() - INTERVAL '1 day');
    GET DIAGNOSTICS v_n = ROW_COUNT;
    RETURN v_n;
END;
$$ LANGUAGE plpgsql;

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DROP FUNCTION IF EXISTS expire_verification_challenges();
DROP FUNCTION IF EXISTS record_failed_verification(TEXT);
DROP FUNCTION IF EXISTS verify_contact(TEXT, TEXT);
DROP FUNCTION IF EXISTS register_user(TEXT, CITEXT, TEXT, TEXT, CITEXT, VARCHAR, verification_channel, TEXT, TEXT, TEXT, TEXT);
DROP FUNCTION IF EXISTS create_verification_challenge(UUID, verification_channel, TEXT, TEXT, TEXT, INTERVAL, INTERVAL);
DROP FUNCTION IF EXISTS cleanup_refresh_tokens();
DROP FUNCTION IF EXISTS revoke_refresh_family_scoped(UUID, UUID);
DROP FUNCTION IF EXISTS list_user_sessions(UUID);
DROP FUNCTION IF EXISTS revoke_user_refresh_except_family(UUID, UUID);
DROP FUNCTION IF EXISTS revoke_user_refresh(UUID, TEXT);
DROP FUNCTION IF EXISTS revoke_refresh_family(TEXT);
DROP FUNCTION IF EXISTS revoke_refresh_token(TEXT, TEXT);
DROP FUNCTION IF EXISTS rotate_refresh_token(TEXT, TEXT, INT, INT, TEXT, TEXT);
DROP FUNCTION IF EXISTS issue_refresh_token(UUID, TEXT, INT, TEXT, TEXT, TEXT);
DROP FUNCTION IF EXISTS cleanup_sessions();
DROP FUNCTION IF EXISTS revoke_session(TEXT);
DROP FUNCTION IF EXISTS validate_session(TEXT, INT);
DROP FUNCTION IF EXISTS create_staff_session(CITEXT, TEXT, TEXT, INT, TEXT, TEXT);
DROP FUNCTION IF EXISTS change_password(UUID, TEXT, TEXT);
DROP FUNCTION IF EXISTS check_user_credentials(CITEXT, TEXT);
DROP FUNCTION IF EXISTS update_user_info(UUID, TEXT, CITEXT, VARCHAR, TEXT, user_status);
DROP FUNCTION IF EXISTS create_user(CITEXT, TEXT, TEXT, CITEXT, VARCHAR, user_role);
DROP TABLE IF EXISTS verification_challenges;
DROP TABLE IF EXISTS refresh_tokens;
DROP TABLE IF EXISTS sessions;
DROP TABLE IF EXISTS users;
-- +goose StatementEnd
