-- +goose Up
-- +goose StatementBegin

-- Optional per-family device label, set once on the family's first token at login
-- (a client hint). Nullable; the API falls back to a user_agent summary when absent.
ALTER TABLE refresh_tokens ADD COLUMN device_label TEXT;

-- Replace issue_refresh_token with a 6-arg form carrying the optional device label.
-- Drop the 00017 5-arg version so a 5-positional call can't become ambiguous; the Go
-- layer always calls the 6-arg form now.
DROP FUNCTION IF EXISTS issue_refresh_token(UUID, TEXT, INT, TEXT, TEXT);
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
    RETURN v_family;
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
-- API distinguishes those via an ownership probe). Idempotent; append-only-safe (only
-- sets revoked_at/revoked_reason, allowed by 00017).
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

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DROP FUNCTION IF EXISTS revoke_refresh_family_scoped(UUID, UUID);
DROP FUNCTION IF EXISTS list_user_sessions(UUID);
DROP FUNCTION IF EXISTS issue_refresh_token(UUID, TEXT, INT, TEXT, TEXT, TEXT);
-- restore the original 5-arg issue_refresh_token from 00017
CREATE OR REPLACE FUNCTION issue_refresh_token(
    p_user_id UUID, p_token_hash TEXT, p_idle_seconds INT,
    p_user_agent TEXT DEFAULT NULL, p_ip TEXT DEFAULT NULL
) RETURNS UUID AS $$
DECLARE v_family UUID;
BEGIN
    INSERT INTO refresh_tokens (id, user_id, expires_at, user_agent, ip)
    VALUES (p_token_hash, p_user_id, now() + make_interval(secs => p_idle_seconds), p_user_agent, p_ip)
    RETURNING family_id INTO v_family;
    RETURN v_family;
END;
$$ LANGUAGE plpgsql;
ALTER TABLE refresh_tokens DROP COLUMN IF EXISTS device_label;
-- +goose StatementEnd
