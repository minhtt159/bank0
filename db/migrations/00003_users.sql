-- +goose Up
-- ─────────────────────────────────────────────────────────────────────────────
-- USER MODEL — identity & credentials
-- The people and their logins: the users table (with the inline invitation quota
-- and onboarding lifecycle column) plus every function that creates a user, checks
-- credentials and changes a password. Password & PIN hashing is bcrypt via pgcrypto
-- (crypt / gen_salt('bf',10)); password POLICY is DB-first (rule 1). The shared
-- set_updated_at() trigger fn is defined HERE (the earliest table that needs it) and
-- the accounts/transfers/disputes/warning-rules files hang their own updated_at
-- triggers on it. Auth tokens live in 00004, self-registration/onboarding in 00005,
-- MFA in 00006.
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
    -- Invitation quota (invitation-gated registration). LIFETIME budget: minting an
    -- invitation decrements it and an expired/unused code never refunds it.
    invites_remaining INT NOT NULL DEFAULT 10 CHECK (invites_remaining >= 0),
    created_at        TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at        TIMESTAMPTZ NOT NULL DEFAULT now(),
    CHECK (email IS NULL OR email ~* '^[^@\s]+@[^@\s]+\.[^@\s]{2,}$')
);

-- search (substring + fuzzy). Trigram GIN powers ILIKE / word_similarity().
CREATE INDEX idx_users_username_trgm ON users USING gin ((username::text) gin_trgm_ops);
CREATE INDEX idx_users_fullname_trgm ON users USING gin (full_name        gin_trgm_ops);
CREATE INDEX idx_users_email_trgm    ON users USING gin ((email::text)    gin_trgm_ops);

-- +goose StatementBegin

-- ─────────────────────────────────────────────────────────────────────────────
-- Shared updated_at trigger function
-- ─────────────────────────────────────────────────────────────────────────────

-- updated_at maintenance on mutable tables (users/accounts/transfers/disputes).
-- Defined here (the first table that needs it); the accounts (00007), transfers
-- (00008), disputes (00013) and warning_rules (00015) files attach their own
-- BEFORE UPDATE triggers to it.
CREATE OR REPLACE FUNCTION set_updated_at() RETURNS TRIGGER AS $$
BEGIN
    NEW.updated_at := now();
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;

-- +goose StatementEnd

CREATE TRIGGER trg_users_updated_at BEFORE UPDATE ON users FOR EACH ROW EXECUTE FUNCTION set_updated_at();

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

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DROP FUNCTION IF EXISTS change_password(UUID, TEXT, TEXT);
DROP FUNCTION IF EXISTS check_user_credentials(CITEXT, TEXT);
DROP FUNCTION IF EXISTS update_user_info(UUID, TEXT, CITEXT, VARCHAR, TEXT, user_status);
DROP FUNCTION IF EXISTS create_user(CITEXT, TEXT, TEXT, CITEXT, VARCHAR, user_role);
-- +goose StatementEnd
DROP TRIGGER IF EXISTS trg_users_updated_at ON users;
-- +goose StatementBegin
DROP FUNCTION IF EXISTS set_updated_at();
-- +goose StatementEnd
DROP TABLE IF EXISTS users;
