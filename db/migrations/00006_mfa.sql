-- +goose Up
-- ─────────────────────────────────────────────────────────────────────────────
-- TOTP MFA (spec-step-up-mfa): credentials, one-time recovery codes, and the
-- attempt log that drives the DB-side throttle/lockout. The TOTP seed is
-- encrypted at rest by the Go layer (AES-256-GCM, auth.mfa_enc_key) — the DB
-- stores only ciphertext. Recovery codes are sha256-only, like refresh tokens.
-- ─────────────────────────────────────────────────────────────────────────────
CREATE TABLE mfa_credentials (
    id            UUID PRIMARY KEY DEFAULT uuidv7(),
    user_id       UUID NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    kind          mfa_kind NOT NULL DEFAULT 'totp',
    secret_enc    BYTEA NOT NULL,                 -- AEAD ciphertext of the base32 seed
    confirmed_at  TIMESTAMPTZ,                    -- NULL until /auth/mfa/confirm succeeds
    created_at    TIMESTAMPTZ NOT NULL DEFAULT now()
);
-- at most one CONFIRMED totp credential per user
CREATE UNIQUE INDEX uq_mfa_confirmed_totp
    ON mfa_credentials (user_id) WHERE kind = 'totp' AND confirmed_at IS NOT NULL;
CREATE INDEX idx_mfa_user ON mfa_credentials (user_id);

CREATE TABLE mfa_recovery_codes (
    id          UUID PRIMARY KEY DEFAULT uuidv7(),
    user_id     UUID NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    code_hash   TEXT NOT NULL,                    -- sha256(code) hex; never plaintext
    used_at     TIMESTAMPTZ,                      -- burn marker (one-time)
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE UNIQUE INDEX uq_recovery_hash ON mfa_recovery_codes (user_id, code_hash);

CREATE TABLE mfa_attempts (
    id           UUID PRIMARY KEY DEFAULT uuidv7(),
    user_id      UUID NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    succeeded    BOOLEAN NOT NULL,
    ip           TEXT,
    attempted_at TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX idx_mfa_attempts_user_time ON mfa_attempts (user_id, attempted_at DESC);
-- +goose StatementBegin

-- mfa_enabled: a confirmed totp credential exists.
CREATE OR REPLACE FUNCTION mfa_enabled(p_user_id UUID) RETURNS BOOLEAN
LANGUAGE sql STABLE AS $$
    SELECT EXISTS (
        SELECT 1 FROM mfa_credentials
         WHERE user_id = p_user_id AND kind = 'totp' AND confirmed_at IS NOT NULL);
$$;

-- mfa_begin_enroll: create (or replace) an UNCONFIRMED totp credential. Refuses
-- (23505 -> 409) if a confirmed credential already exists; a prior unconfirmed
-- one is replaced (re-enroll before confirm is fine).
CREATE OR REPLACE FUNCTION mfa_begin_enroll(p_user_id UUID, p_secret_enc BYTEA)
RETURNS UUID AS $$
DECLARE v_id UUID;
BEGIN
    IF mfa_enabled(p_user_id) THEN
        RAISE EXCEPTION 'mfa already enabled' USING ERRCODE = 'unique_violation'; -- 23505 -> 409
    END IF;
    DELETE FROM mfa_credentials
     WHERE user_id = p_user_id AND kind = 'totp' AND confirmed_at IS NULL;
    INSERT INTO mfa_credentials (user_id, kind, secret_enc)
    VALUES (p_user_id, 'totp', p_secret_enc)
    RETURNING id INTO v_id;
    RETURN v_id;
END;
$$ LANGUAGE plpgsql;

-- mfa_pending_secret: the encrypted seed of the unconfirmed credential.
CREATE OR REPLACE FUNCTION mfa_pending_secret(p_user_id UUID)
RETURNS BYTEA AS $$
DECLARE v BYTEA;
BEGIN
    SELECT secret_enc INTO v FROM mfa_credentials
     WHERE user_id = p_user_id AND kind = 'totp' AND confirmed_at IS NULL
     ORDER BY created_at DESC LIMIT 1;
    IF NOT FOUND THEN RAISE EXCEPTION 'no pending mfa enrollment found'; END IF; -- P0001 -> 404
    RETURN v;
END;
$$ LANGUAGE plpgsql;

-- mfa_confirm: mark the pending credential confirmed AND replace the recovery
-- codes, atomically. The Go layer verified the live TOTP first; this commits.
CREATE OR REPLACE FUNCTION mfa_confirm(p_user_id UUID, p_recovery_hashes TEXT[])
RETURNS VOID AS $$
DECLARE v_cred UUID; h TEXT;
BEGIN
    SELECT id INTO v_cred FROM mfa_credentials
     WHERE user_id = p_user_id AND kind = 'totp' AND confirmed_at IS NULL
     ORDER BY created_at DESC LIMIT 1 FOR UPDATE;
    IF NOT FOUND THEN RAISE EXCEPTION 'no pending mfa enrollment found'; END IF;

    UPDATE mfa_credentials SET confirmed_at = now() WHERE id = v_cred;
    DELETE FROM mfa_recovery_codes WHERE user_id = p_user_id;     -- fresh set on (re)confirm
    FOREACH h IN ARRAY p_recovery_hashes LOOP
        INSERT INTO mfa_recovery_codes (user_id, code_hash) VALUES (p_user_id, h);
    END LOOP;
END;
$$ LANGUAGE plpgsql;

-- mfa_confirmed_secret: the encrypted seed of the CONFIRMED credential.
CREATE OR REPLACE FUNCTION mfa_confirmed_secret(p_user_id UUID)
RETURNS BYTEA AS $$
DECLARE v BYTEA;
BEGIN
    SELECT secret_enc INTO v FROM mfa_credentials
     WHERE user_id = p_user_id AND kind = 'totp' AND confirmed_at IS NOT NULL;
    IF NOT FOUND THEN RAISE EXCEPTION 'mfa credential not found'; END IF;      -- P0001 -> 404
    RETURN v;
END;
$$ LANGUAGE plpgsql;

-- mfa_burn_recovery_code: consume a recovery code (one-time). TRUE iff a live
-- code matched and was burned.
CREATE OR REPLACE FUNCTION mfa_burn_recovery_code(p_user_id UUID, p_code_hash TEXT)
RETURNS BOOLEAN AS $$
DECLARE v_id UUID;
BEGIN
    SELECT id INTO v_id FROM mfa_recovery_codes
     WHERE user_id = p_user_id AND code_hash = p_code_hash AND used_at IS NULL
     FOR UPDATE;
    IF NOT FOUND THEN RETURN FALSE; END IF;
    UPDATE mfa_recovery_codes SET used_at = now() WHERE id = v_id;
    RETURN TRUE;
END;
$$ LANGUAGE plpgsql;

-- mfa_record_attempt: append an attempt; returns whether the user is now LOCKED.
-- Lockout policy lives here (DB-first): >= p_max_fail fails in the trailing window.
CREATE OR REPLACE FUNCTION mfa_record_attempt(
    p_user_id        UUID,
    p_succeeded      BOOLEAN,
    p_ip             TEXT,
    p_max_fail       INT,
    p_window_seconds INT
) RETURNS BOOLEAN AS $$
DECLARE v_fails INT;
BEGIN
    INSERT INTO mfa_attempts (user_id, succeeded, ip) VALUES (p_user_id, p_succeeded, p_ip);
    SELECT count(*) INTO v_fails
      FROM mfa_attempts
     WHERE user_id = p_user_id
       AND succeeded = FALSE
       AND attempted_at > now() - make_interval(secs => p_window_seconds);
    RETURN v_fails >= p_max_fail;
END;
$$ LANGUAGE plpgsql;

-- mfa_is_locked: read-only lock check (short-circuit BEFORE verifying).
CREATE OR REPLACE FUNCTION mfa_is_locked(p_user_id UUID, p_max_fail INT, p_window_seconds INT)
RETURNS BOOLEAN LANGUAGE sql STABLE AS $$
    SELECT count(*) >= p_max_fail
      FROM mfa_attempts
     WHERE user_id = p_user_id AND succeeded = FALSE
       AND attempted_at > now() - make_interval(secs => p_window_seconds);
$$;

-- cleanup_mfa_attempts: drop attempt rows older than a day (maintenance sweep).
CREATE OR REPLACE FUNCTION cleanup_mfa_attempts() RETURNS INTEGER AS $$
DECLARE v_n INTEGER;
BEGIN
    DELETE FROM mfa_attempts WHERE attempted_at <= now() - INTERVAL '1 day';
    GET DIAGNOSTICS v_n = ROW_COUNT;
    RETURN v_n;
END;
$$ LANGUAGE plpgsql;

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DROP FUNCTION IF EXISTS cleanup_mfa_attempts();
DROP FUNCTION IF EXISTS mfa_is_locked(UUID, INT, INT);
DROP FUNCTION IF EXISTS mfa_record_attempt(UUID, BOOLEAN, TEXT, INT, INT);
DROP FUNCTION IF EXISTS mfa_burn_recovery_code(UUID, TEXT);
DROP FUNCTION IF EXISTS mfa_confirmed_secret(UUID);
DROP FUNCTION IF EXISTS mfa_confirm(UUID, TEXT[]);
DROP FUNCTION IF EXISTS mfa_pending_secret(UUID);
DROP FUNCTION IF EXISTS mfa_begin_enroll(UUID, BYTEA);
DROP FUNCTION IF EXISTS mfa_enabled(UUID);
-- +goose StatementEnd
DROP TABLE IF EXISTS mfa_attempts;
DROP TABLE IF EXISTS mfa_recovery_codes;
DROP TABLE IF EXISTS mfa_credentials;
