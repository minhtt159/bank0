-- +goose Up
-- ─────────────────────────────────────────────────────────────────────────────
-- ONBOARDING — public self-registration & contact verification
-- The invitation-gated signup flow: the verification_challenges table (one row per
-- (user, channel) contact-verification), the invitations table (single-use codes
-- spending a user's lifetime quota), and the functions that issue challenges,
-- register a user atomically, mint/consume invitations and verify a contact. Same
-- hash-at-rest discipline as the auth tokens (00004): only sha256 of the opaque
-- verify_token and 6-digit code are stored. register_user shares request_transfer's
-- idempotency-key gate (idempotency_keys, 00008).
-- ─────────────────────────────────────────────────────────────────────────────

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

-- ─────────────────────────────────────────────────────────────────────────────
-- invitations  (invitation-gated registration)
-- A verified customer mints a single-use code (create_invitation), spending one
-- unit of their lifetime users.invites_remaining budget. register_user consumes
-- it. Status is DERIVED, never stored: consumed (consumed_at set), else expired
-- (expires_at < now()), else pending. inviter_id CASCADEs (the invites die with
-- the account); invitee_id is SET NULL so a consumed row survives its user's
-- deletion as an audit trace.
-- ─────────────────────────────────────────────────────────────────────────────
CREATE TABLE invitations (
    id          UUID PRIMARY KEY DEFAULT uuidv7(),
    code        TEXT NOT NULL UNIQUE,
    inviter_id  UUID NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    invitee_id  UUID REFERENCES users(id) ON DELETE SET NULL,
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now(),
    expires_at  TIMESTAMPTZ NOT NULL DEFAULT now() + INTERVAL '14 days',
    consumed_at TIMESTAMPTZ
);
CREATE INDEX idx_invitations_inviter ON invitations (inviter_id);

-- +goose StatementBegin

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
    -- The invite code is part of the fingerprint: a replay of the same key with a
    -- DIFFERENT code is a parameter mismatch (-> 23514), not a silent success.
    v_hash := encode(digest(
        COALESCE(p_username::text,'') || '|' || COALESCE(p_email::text,'') || '|' ||
        COALESCE(p_phone_number,'')   || '|' || COALESCE(p_full_name,'')   || '|' ||
        COALESCE(p_invite_code,''), 'sha256'), 'hex');

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

-- create_invitation: mint one single-use invitation for a verified customer,
-- spending one unit of their lifetime quota atomically (the inviter row is locked
-- FOR UPDATE so concurrent calls can't overspend). Returns the code, its expiry,
-- and the caller's remaining balance.
--   * unknown inviter          -> P0001           (-> 404)
--   * not verified / not active -> 42501           (-> 403)
--   * quota exhausted          -> check_violation  (-> 409, "invitation limit")
-- Quota is LIFETIME: a code that later EXPIRES unused does NOT refund the
-- decrement (bounds total invites per user; see users.invites_remaining).
CREATE OR REPLACE FUNCTION create_invitation(p_inviter UUID)
RETURNS TABLE(code TEXT, expires_at TIMESTAMPTZ, invites_remaining INT) AS $$
DECLARE
    v_status     user_status;
    v_onboarding onboarding_status;
    v_remaining  INT;
    v_code       TEXT;
    v_expires    TIMESTAMPTZ;
BEGIN
    SELECT u.status, u.onboarding_status, u.invites_remaining
      INTO v_status, v_onboarding, v_remaining
      FROM users u WHERE u.id = p_inviter FOR UPDATE;
    IF NOT FOUND THEN
        RAISE EXCEPTION 'user not found';   -- P0001 -> 404
    END IF;

    IF NOT (v_status = 'active' AND v_onboarding IN ('verified', 'active')) THEN
        RAISE EXCEPTION 'account must be verified to create invitations'
            USING ERRCODE = '42501';        -- -> 403
    END IF;

    IF v_remaining <= 0 THEN
        RAISE EXCEPTION 'invitation limit reached' USING ERRCODE = 'check_violation'; -- -> 409
    END IF;

    -- base64url-ish code from 12 random bytes: translate +/ to -_ and strip = pad.
    v_code := translate(encode(gen_random_bytes(12), 'base64'), '+/=', '-_');

    INSERT INTO invitations (code, inviter_id)
    VALUES (v_code, p_inviter)
    RETURNING invitations.expires_at INTO v_expires;

    -- qualify the RHS: the RETURNS TABLE OUT param 'invites_remaining' shadows the column.
    UPDATE users SET invites_remaining = users.invites_remaining - 1 WHERE id = p_inviter;

    RETURN QUERY SELECT v_code, v_expires, v_remaining - 1;
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

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DROP FUNCTION IF EXISTS expire_verification_challenges();
DROP FUNCTION IF EXISTS record_failed_verification(TEXT);
DROP FUNCTION IF EXISTS verify_contact(TEXT, TEXT);
DROP FUNCTION IF EXISTS create_invitation(UUID);
DROP FUNCTION IF EXISTS register_user(TEXT, CITEXT, TEXT, TEXT, CITEXT, VARCHAR, verification_channel, TEXT, TEXT, TEXT, TEXT, TEXT);
DROP FUNCTION IF EXISTS create_verification_challenge(UUID, verification_channel, TEXT, TEXT, TEXT, INTERVAL, INTERVAL);
-- +goose StatementEnd
DROP TABLE IF EXISTS invitations;                     -- FK to users; drop before users
DROP TABLE IF EXISTS verification_challenges;
