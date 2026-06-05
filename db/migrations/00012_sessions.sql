-- +goose Up
-- +goose StatementBegin

-- Operator (portal) sessions. The cookie holds an opaque random token; we store
-- only its SHA-256 hash as the PK, so a DB leak never exposes a live token.
-- Idle timeout is enforced and slid forward in the DB (validate_session), so all
-- portal replicas share one source of truth and there is no in-memory state.
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

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DROP FUNCTION IF EXISTS cleanup_sessions();
DROP FUNCTION IF EXISTS revoke_session(TEXT);
DROP FUNCTION IF EXISTS validate_session(TEXT, INT);
DROP FUNCTION IF EXISTS create_staff_session(CITEXT, TEXT, TEXT, INT, TEXT, TEXT);
DROP TABLE IF EXISTS sessions;
-- +goose StatementEnd
