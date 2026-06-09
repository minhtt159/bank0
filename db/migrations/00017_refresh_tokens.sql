-- +goose Up
-- +goose StatementBegin

-- Client (api) refresh tokens (docs/06 §3). Mirrors the sessions discipline: the
-- PK is sha256(token) hex, so a DB leak never yields a live token. A "family" is
-- one login; rotation chains tokens via parent_id, and presenting an already-used
-- token (theft signal) revokes the whole family. Access JWTs stay stateless.
CREATE TABLE refresh_tokens (
    id             TEXT PRIMARY KEY,                       -- sha256(token) hex
    family_id      UUID NOT NULL DEFAULT uuidv7(),         -- one login = one family
    user_id        UUID NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    parent_id      TEXT REFERENCES refresh_tokens(id),     -- previous token in the chain
    issued_at      TIMESTAMPTZ NOT NULL DEFAULT now(),
    expires_at     TIMESTAMPTZ NOT NULL,                   -- idle window, slid on rotate
    rotated_at     TIMESTAMPTZ,                            -- set when consumed by rotate
    revoked_at     TIMESTAMPTZ,                            -- set on logout / reuse / force
    revoked_reason TEXT,                                   -- 'logout'|'reuse_detected'|'forced'|'expired'
    user_agent     TEXT,
    ip             TEXT
);
CREATE INDEX idx_refresh_user    ON refresh_tokens (user_id);
CREATE INDEX idx_refresh_family  ON refresh_tokens (family_id);
CREATE INDEX idx_refresh_expires ON refresh_tokens (expires_at);

-- issue_refresh_token: open a new family at login. Returns the family id.
CREATE OR REPLACE FUNCTION issue_refresh_token(
    p_user_id      UUID,
    p_token_hash   TEXT,
    p_idle_seconds INT,
    p_user_agent   TEXT DEFAULT NULL,
    p_ip           TEXT DEFAULT NULL
) RETURNS UUID AS $$
DECLARE v_family UUID;
BEGIN
    INSERT INTO refresh_tokens (id, user_id, expires_at, user_agent, ip)
    VALUES (p_token_hash, p_user_id, now() + make_interval(secs => p_idle_seconds), p_user_agent, p_ip)
    RETURNING family_id INTO v_family;
    RETURN v_family;
END;
$$ LANGUAGE plpgsql;

-- rotate_refresh_token: the heart of it, one atomic transition.
--   * live token   -> mark rotated, mint the child (same family), return the user
--   * already used  -> REUSE: revoke the whole family, RAISE 28000 (re-auth)
--   * expired/absolute-capped -> revoke family, RAISE 28P01
--   * unknown       -> RAISE 28P01
-- The API maps 28000/28P01 to 401.
CREATE OR REPLACE FUNCTION rotate_refresh_token(
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

    RETURN QUERY SELECT r.user_id, r.family_id;
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
DROP FUNCTION IF EXISTS revoke_user_refresh(UUID, TEXT);
DROP FUNCTION IF EXISTS revoke_refresh_family(TEXT);
DROP FUNCTION IF EXISTS revoke_refresh_token(TEXT, TEXT);
DROP FUNCTION IF EXISTS rotate_refresh_token(TEXT, TEXT, INT, INT, TEXT, TEXT);
DROP FUNCTION IF EXISTS issue_refresh_token(UUID, TEXT, INT, TEXT, TEXT);
DROP TABLE IF EXISTS refresh_tokens;
-- +goose StatementEnd
