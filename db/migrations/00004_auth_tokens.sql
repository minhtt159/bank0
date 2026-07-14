-- +goose Up
-- ─────────────────────────────────────────────────────────────────────────────
-- AUTH TOKENS — operator sessions & client refresh-token families
-- The stateful half of authentication: operator (portal) cookie sessions and
-- client (api) refresh-token families, plus every function that mints / validates /
-- rotates / revokes them. Both tables store only the sha256 hash of the opaque
-- token, so a DB leak never yields a live credential. issue_refresh_token emits the
-- 'device.new' security event (events, 00014) in the same txn — a late-bound
-- plpgsql reference, so this file loads fine ahead of the events table.
-- ─────────────────────────────────────────────────────────────────────────────

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

-- +goose StatementBegin

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
-- same txn (events, 00014) — one per family, deduped by the family-id partial
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

-- revoke_user_refresh: bulk refresh-token revocation for a user. With p_keep_family
-- NULL it is "log out everywhere" / operator force-revoke; with a family id it is
-- "log out everywhere else", sparing the current session's family (e.g. after a
-- password change). p_reason is recorded on each revoked token, so callers keep their
-- own audit reason ('forced' vs 'password_change'). Returns the count revoked.
CREATE OR REPLACE FUNCTION revoke_user_refresh(
    p_user_id     UUID,
    p_keep_family UUID  DEFAULT NULL,
    p_reason      TEXT  DEFAULT 'forced'
) RETURNS INTEGER AS $$
DECLARE v_n INTEGER;
BEGIN
    UPDATE refresh_tokens
       SET revoked_at = now(), revoked_reason = COALESCE(p_reason, 'forced')
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
DROP FUNCTION IF EXISTS cleanup_refresh_tokens();
DROP FUNCTION IF EXISTS revoke_refresh_family_scoped(UUID, UUID);
DROP FUNCTION IF EXISTS list_user_sessions(UUID);
DROP FUNCTION IF EXISTS revoke_user_refresh(UUID, UUID, TEXT);
DROP FUNCTION IF EXISTS revoke_refresh_family(TEXT);
DROP FUNCTION IF EXISTS revoke_refresh_token(TEXT, TEXT);
DROP FUNCTION IF EXISTS rotate_refresh_token(TEXT, TEXT, INT, INT, TEXT, TEXT);
DROP FUNCTION IF EXISTS issue_refresh_token(UUID, TEXT, INT, TEXT, TEXT, TEXT);
DROP FUNCTION IF EXISTS cleanup_sessions();
DROP FUNCTION IF EXISTS revoke_session(TEXT);
DROP FUNCTION IF EXISTS validate_session(TEXT, INT);
DROP FUNCTION IF EXISTS create_staff_session(CITEXT, TEXT, TEXT, INT, TEXT, TEXT);
-- +goose StatementEnd
DROP TABLE IF EXISTS refresh_tokens;
DROP TABLE IF EXISTS sessions;
