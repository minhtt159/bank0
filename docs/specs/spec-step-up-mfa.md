# spec — TOTP MFA + step-up for high-value / new-payee transfers

> Implementation-ready. Implement as written; no further design decisions.
> **Completes** the design sketched in [`06-client-api.md`](../06-client-api.md)
> §6.1 (TOTP MFA) and §6.2 (step-up) — read those first; this fills in tables,
> SQLSTATEs, the TOTP math, JWT claims, and the exact step-up gate. Backlog
> reference: [`09-fraudbank-bff-plan.md`](../09-fraudbank-bff-plan.md) P2
> ("Step-up auth + TOTP MFA … implement 06 §6.1/6.2 as written, don't redesign").

---

## 1. Summary & rationale

Two related increments, one migration:

1. **TOTP MFA (RFC 6238).** A customer enrolls an authenticator app
   (`/auth/mfa/enroll` → otpauth URI + QR seed), confirms it with a live code
   (`/auth/mfa/confirm` → one-time recovery codes), and from then on **login requires
   a second factor**: `POST /auth/login` returns `mfa_required:true` + a short-lived
   **`mfa_token`** and **no** access/refresh tokens; the client exchanges
   `mfa_token + code` at `/auth/mfa/verify` for the real token pair.

2. **Step-up.** The access JWT carries `amr` (`["pwd"]` or `["pwd","otp"]`) and
   `auth_time`. A `POST /transfers` that is **high-value** (≥ `auth.step_up_limit_minor`)
   **or to a new payee** is rejected with **403 `step_up_required`** unless the JWT
   shows a *fresh OTP* (`amr` contains `otp` **and** `auth_time` within
   `auth.step_up_max_age`). The client then runs `/auth/mfa/verify` to mint a fresh
   `otp`-stamped token and **retries the transfer with the SAME `Idempotency-Key`**.

**fraudbank already reuses the idempotency key on retry** (the web/iOS/Android clients
cache the `Idempotency-Key` per pending transfer and resend it verbatim after any
recoverable error — see fraudbank `docs/02-api-contract.md` and the unified transfer
flow from review item 14). So the step-up retry costs nothing client-side: the same
key hits `transfer()`'s idempotency gate and, because the first attempt never created
a row (it was rejected at the handler before `transfer()` ran), the retry proceeds
normally. **Call this out in the client guide; do not require a new key.**

DB-first discipline holds: TOTP *secrets and verification state* live in the DB;
the HMAC-SHA1 OTP math lives in Go (`github.com/pquerna/otp/totp`), and the seed is
**encrypted at rest** with an app-side AEAD key (`auth.mfa_enc_key`). The access-token
path (`requireJWT`) is unchanged except for two extra claims.

---

## 2. API — OpenAPI 3.1 operations

### 2.1 New paths (all client tag)

```yaml
  /auth/mfa/enroll:
    post:
      operationId: mfaEnroll
      tags: [client]
      summary: Begin TOTP enrollment — returns an otpauth:// URI + base32 secret
      responses:
        "200":
          description: enrollment started (unconfirmed credential created)
          content:
            application/json:
              schema: { $ref: "#/components/schemas/MfaEnrollResponse" }
        "401": { $ref: "#/components/responses/Error" }
        "409": { $ref: "#/components/responses/Error" }   # already has a confirmed credential

  /auth/mfa/confirm:
    post:
      operationId: mfaConfirm
      tags: [client]
      summary: Confirm TOTP enrollment with the first code; returns one-time recovery codes
      requestBody:
        required: true
        content:
          application/json:
            schema: { $ref: "#/components/schemas/MfaConfirmRequest" }
      responses:
        "200":
          description: MFA enabled; recovery codes (shown ONCE)
          content:
            application/json:
              schema: { $ref: "#/components/schemas/MfaRecoveryCodes" }
        "401": { $ref: "#/components/responses/Error" }   # bad code
        "404": { $ref: "#/components/responses/Error" }   # no pending enrollment
        "429": { $ref: "#/components/responses/Error" }   # throttled/locked

  /auth/mfa/verify:
    post:
      operationId: mfaVerify
      tags: [client]
      summary: Exchange an mfa_token + TOTP/recovery code for an access + refresh pair
      security: []                                         # mfa_token IS the credential
      requestBody:
        required: true
        content:
          application/json:
            schema: { $ref: "#/components/schemas/MfaVerifyRequest" }
      responses:
        "200":
          description: verified; access + refresh issued (amr includes "otp")
          content:
            application/json:
              schema: { $ref: "#/components/schemas/LoginResponse" }
        "401": { $ref: "#/components/responses/Error" }   # bad/expired mfa_token or code
        "429": { $ref: "#/components/responses/Error" }   # throttled/locked
```

### 2.2 Changed: `LoginResponse` and `POST /transfers`

`LoginResponse` gains two optional fields. When `mfa_required` is true the token
fields are **absent**:

```yaml
    LoginResponse:
      type: object
      properties:
        user_id: { type: string, format: uuid }
        token: { type: string, description: "JWT access token (HS256). Absent when mfa_required." }
        token_type: { type: string, example: Bearer }
        expires_at: { type: string, format: date-time }
        refresh_token: { type: string, description: "Opaque refresh token. Absent when mfa_required." }
        mfa_required:
          type: boolean
          description: "When true, no tokens are issued; exchange mfa_token + code at /auth/mfa/verify."
        mfa_token:
          type: string
          description: "Short-lived (default 5m) one-time token identifying this pending login. Present only when mfa_required."
```

`POST /transfers` gains a **403** response (the step-up signal). Add to its
`responses:` block:

```yaml
        "403": { $ref: "#/components/responses/Error" }   # step_up_required (see body below)
```

The 403 body is the standard `Error` envelope with a **machine-readable code** the
client keys on:

```json
{ "error": "step_up_required", "message": "this transfer requires re-verification (high value or new payee)" }
```

### 2.3 New schemas (`components/schemas`)

```yaml
    MfaEnrollResponse:
      type: object
      properties:
        secret: { type: string, description: "base32 TOTP secret (also embedded in the URI)" }
        otpauth_uri: { type: string, description: "otpauth://totp/bank0:<username>?secret=...&issuer=bank0&algorithm=SHA1&digits=6&period=30" }
    MfaConfirmRequest:
      type: object
      required: [code]
      properties:
        code: { type: string, description: "6-digit TOTP from the authenticator app" }
    MfaRecoveryCodes:
      type: object
      properties:
        recovery_codes:
          type: array
          items: { type: string }
          description: "One-time recovery codes; shown ONCE, stored hashed server-side."
    MfaVerifyRequest:
      type: object
      required: [mfa_token, code]
      properties:
        mfa_token: { type: string }
        code: { type: string, description: "6-digit TOTP, or a recovery code" }
```

---

## 3. Data model & migration

`db/migrations/00018_mfa.sql` (the number reserved by
[`06-client-api.md`](../06-client-api.md) §6 — "Tables land in `00018_mfa.sql`").
Note `spec-change-password.md` also eyes `00018`; **this file takes `00018`**, the
password spec falls to `00019`. Coordinate at implement time.

```sql
-- +goose Up
-- +goose StatementBegin

-- MFA credentials. One confirmed TOTP credential per user = "MFA enabled".
-- The TOTP seed is encrypted at rest by the Go layer (AEAD with auth.mfa_enc_key);
-- the DB stores only the ciphertext (bytea). kind is forward-looking (webauthn later).
CREATE TYPE mfa_kind AS ENUM ('totp', 'webauthn');

CREATE TABLE mfa_credentials (
    id            UUID PRIMARY KEY DEFAULT uuidv7(),
    user_id       UUID NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    kind          mfa_kind NOT NULL DEFAULT 'totp',
    secret_enc    BYTEA NOT NULL,                 -- AEAD ciphertext of the base32 seed
    confirmed_at  TIMESTAMPTZ,                    -- NULL until /auth/mfa/confirm succeeds
    created_at    TIMESTAMPTZ NOT NULL DEFAULT now()
);
-- at most one CONFIRMED totp credential per user (partial unique index)
CREATE UNIQUE INDEX uq_mfa_confirmed_totp
    ON mfa_credentials (user_id) WHERE kind = 'totp' AND confirmed_at IS NOT NULL;
CREATE INDEX idx_mfa_user ON mfa_credentials (user_id);

-- One-time recovery codes. Stored sha256 only (never plaintext). used_at marks burn.
CREATE TABLE mfa_recovery_codes (
    id          UUID PRIMARY KEY DEFAULT uuidv7(),
    user_id     UUID NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    code_hash   TEXT NOT NULL,                    -- sha256(code) hex
    used_at     TIMESTAMPTZ,
    created_at  TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE UNIQUE INDEX uq_recovery_hash ON mfa_recovery_codes (user_id, code_hash);

-- Verification attempt log for throttle + lockout (the only mutable security state).
CREATE TABLE mfa_attempts (
    id          UUID PRIMARY KEY DEFAULT uuidv7(),
    user_id     UUID NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    succeeded   BOOLEAN NOT NULL,
    ip          TEXT,
    attempted_at TIMESTAMPTZ NOT NULL DEFAULT now()
);
CREATE INDEX idx_mfa_attempts_user_time ON mfa_attempts (user_id, attempted_at DESC);

-- mfa_enabled: a confirmed totp credential exists.
CREATE OR REPLACE FUNCTION mfa_enabled(p_user_id UUID) RETURNS BOOLEAN
LANGUAGE sql STABLE AS $$
    SELECT EXISTS (
        SELECT 1 FROM mfa_credentials
         WHERE user_id = p_user_id AND kind = 'totp' AND confirmed_at IS NOT NULL);
$$;

-- mfa_begin_enroll: create (or replace) an UNCONFIRMED totp credential, returning its
-- id. Refuses (raises 23505 -> 409) if a confirmed credential already exists. Any
-- prior unconfirmed credential is deleted first (re-enroll is idempotent-ish).
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

-- mfa_pending_secret: the encrypted seed of the user's unconfirmed totp credential
-- (for /auth/mfa/confirm). Raises if none pending.
CREATE OR REPLACE FUNCTION mfa_pending_secret(p_user_id UUID)
RETURNS BYTEA AS $$
DECLARE v BYTEA;
BEGIN
    SELECT secret_enc INTO v FROM mfa_credentials
     WHERE user_id = p_user_id AND kind = 'totp' AND confirmed_at IS NULL
     ORDER BY created_at DESC LIMIT 1;
    IF NOT FOUND THEN RAISE EXCEPTION 'no pending mfa enrollment'; END IF; -- P0001 -> 404 (msg "no ...")
    RETURN v;
END;
$$ LANGUAGE plpgsql;

-- mfa_confirm: mark the pending credential confirmed AND replace the recovery-code
-- set with the supplied hashes, atomically. The Go layer has already verified the
-- TOTP code; this is the commit. Raises if no pending enrollment.
CREATE OR REPLACE FUNCTION mfa_confirm(p_user_id UUID, p_recovery_hashes TEXT[])
RETURNS VOID AS $$
DECLARE v_cred UUID; h TEXT;
BEGIN
    SELECT id INTO v_cred FROM mfa_credentials
     WHERE user_id = p_user_id AND kind = 'totp' AND confirmed_at IS NULL
     ORDER BY created_at DESC LIMIT 1 FOR UPDATE;
    IF NOT FOUND THEN RAISE EXCEPTION 'no pending mfa enrollment'; END IF;

    UPDATE mfa_credentials SET confirmed_at = now() WHERE id = v_cred;
    DELETE FROM mfa_recovery_codes WHERE user_id = p_user_id;     -- fresh set on (re)confirm
    FOREACH h IN ARRAY p_recovery_hashes LOOP
        INSERT INTO mfa_recovery_codes (user_id, code_hash) VALUES (p_user_id, h);
    END LOOP;
END;
$$ LANGUAGE plpgsql;

-- mfa_confirmed_secret: the encrypted seed of the user's CONFIRMED credential (for
-- /auth/mfa/verify). Raises (P0001, msg "not found") if MFA is not enabled.
CREATE OR REPLACE FUNCTION mfa_confirmed_secret(p_user_id UUID)
RETURNS BYTEA AS $$
DECLARE v BYTEA;
BEGIN
    SELECT secret_enc INTO v FROM mfa_credentials
     WHERE user_id = p_user_id AND kind = 'totp' AND confirmed_at IS NOT NULL;
    IF NOT FOUND THEN RAISE EXCEPTION 'mfa credential not found'; END IF;
    RETURN v;
END;
$$ LANGUAGE plpgsql;

-- mfa_burn_recovery_code: consume a recovery code (one-time). Returns TRUE if a live
-- code matched and was burned; FALSE otherwise. Used by /auth/mfa/verify fallback.
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

-- mfa_record_attempt: append an attempt and return whether the user is now LOCKED.
-- Lockout policy lives here (DB-first): >= p_max_fail failed attempts in the trailing
-- p_window_seconds => locked. A success resets the practical window (older fails age out).
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

-- mfa_is_locked: read-only lock check (called BEFORE verifying, to short-circuit).
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
DROP TABLE IF EXISTS mfa_attempts;
DROP TABLE IF EXISTS mfa_recovery_codes;
DROP TABLE IF EXISTS mfa_credentials;
DROP TYPE IF EXISTS mfa_kind;
-- +goose StatementEnd
```

Conventions honored: `uuidv7()` PKs; sha256-only recovery codes (mirrors
`refresh_tokens` / `sessions`); typed SQLSTATEs into `mapDBError`; lockout policy in
PL/pgSQL (DB-first); a `cleanup_mfa_attempts()` joined to the maintenance sweep
(`internal/db/bank.go` `RunMaintenance`, next to `cleanup_refresh_tokens()`).

### 3.1 The `mfa_token` (pending-login) store

The `mfa_token` is a short-lived, single-use credential issued by `/auth/login` when
MFA is required, redeemed at `/auth/mfa/verify`. **Reuse the `refresh_tokens`-style
hashed-token discipline**, but it is its own concern. Two equally acceptable options
— pick **(a)** for simplicity:

**(a) Stateless signed token (chosen).** Mint a second short-lived HS256 JWT with a
distinct audience `bank0-mfa`, `sub = user_id`, `exp = now + auth.mfa_token_ttl`
(default 5m), and a random `jti`. `/auth/mfa/verify` parses it with
`WithAudience("bank0-mfa")`. Single-use is enforced by the fact that a *successful*
verify immediately issues real tokens and the client discards the `mfa_token`; the
5-minute TTL bounds replay. No table needed. **This is the chosen design** — it mirrors
`issueJWT`/`parseJWT` exactly with a different audience and TTL, so no new storage.

> (b) DB-backed `mfa_login_tokens(id sha256, user_id, expires_at, used_at)` if you want
> hard single-use semantics. Not required for v1; note it as a hardening follow-up.

### 3.2 Config (`internal/config/config.go`, `AuthConfig`)

Add fields + defaults (mirroring the existing `auth.*` block):

```go
StepUpLimitMinor int64         `mapstructure:"step_up_limit_minor"`  // transfers >= this need fresh otp
StepUpMaxAge     time.Duration `mapstructure:"step_up_max_age"`      // how fresh auth_time must be
MFAEncKey        string        `mapstructure:"mfa_enc_key"`          // base64 32-byte AEAD key (APP_AUTH_MFA_ENC_KEY)
MFATokenTTL      time.Duration `mapstructure:"mfa_token_ttl"`        // pending-login token TTL
MFALockMaxFail   int           `mapstructure:"mfa_lock_max_fail"`    // failed attempts before lockout
MFALockWindow    time.Duration `mapstructure:"mfa_lock_window"`      // trailing window for the count
```

```go
v.SetDefault("auth.step_up_limit_minor", 100000)   // €1,000.00
v.SetDefault("auth.step_up_max_age", "5m")
v.SetDefault("auth.mfa_enc_key", "")               // empty => MFA disabled with a loud warn
v.SetDefault("auth.mfa_token_ttl", "5m")
v.SetDefault("auth.mfa_lock_max_fail", 5)
v.SetDefault("auth.mfa_lock_window", "15m")
```

`auth.mfa_enc_key` empty ⇒ same posture as `jwt_secret`: warn loudly; enroll/confirm/
verify all return **503 `mfa_unavailable`** (refuse rather than store unencrypted).

---

## 4. Handler logic

### 4.1 TOTP + crypto helpers (new file `internal/api/mfa.go`)

- **TOTP**: `github.com/pquerna/otp/totp` (add to `go.mod`). RFC 6238 defaults:
  HMAC-**SHA1**, **6** digits, **30s** period. Verify with a **±1 step drift window**
  (`totp.ValidateOpts{Skew:1, Period:30, Digits:6, Algorithm:SHA1}`) — tolerates ~±30s
  clock skew, the standard choice. Generate the secret with `totp.Generate(...)`
  (Issuer `bank0`, AccountName = username) which yields both the base32 secret and the
  `otpauth://` URI.
- **Seed encryption at rest**: AES-256-GCM (`crypto/aes`+`crypto/cipher`), key =
  base64-decoded `auth.mfa_enc_key` (must be 32 bytes; refuse otherwise). Store
  `nonce || ciphertext` as `secret_enc BYTEA`. Helpers `encryptSeed([]byte) ([]byte,error)`
  / `decryptSeed([]byte) ([]byte,error)` on `*Server`.
- **Recovery codes**: 10 codes, each 10 chars from an unambiguous alphabet
  (`crypto/rand`, e.g. base32 of 6 random bytes → 10 chars). Return plaintext to the
  client **once**; store `hashToken(code)` (the existing sha256 hex helper in `auth.go`).
- Add `mfaTokenAudience = "bank0-mfa"`; add `issueMFAToken(userID)` /
  `parseMFAToken(raw)` to `jwt.go` mirroring `issueJWT`/`parseJWT` with the distinct
  audience + `s.cfg.Auth.MFATokenTTL`.

### 4.2 JWT claims (`internal/api/jwt.go`)

Extend `clientClaims` and `issueJWT`:

```go
type clientClaims struct {
	jwt.RegisteredClaims
	Role     string   `json:"role"`
	Username string   `json:"username"`
	AMR      []string `json:"amr,omitempty"`       // ["pwd"] or ["pwd","otp"]
	AuthTime int64    `json:"auth_time,omitempty"` // unix seconds of the factor event
}
```

Change `issueJWT` to take `amr []string` and stamp `AuthTime = now.Unix()`:
- **Login without MFA** (or any plain refresh): `amr = ["pwd"]`.
- **`/auth/mfa/verify` success**: `amr = ["pwd","otp"]`, `auth_time = now`.
- **`/auth/refresh`**: preserve the *kind* of the prior session — carry `amr`/`auth_time`
  forward by **re-stamping `auth_time = now`** only if the prior token had `otp`?
  **No.** Keep it simple and safe: refresh issues `amr=["pwd"]` with a fresh
  `auth_time`. Rationale: a refresh is not a re-authentication of the second factor;
  forcing a step-up re-verify after the access token rotates (≤15m) is the *correct*
  security posture for high-value moves. Document: "step-up freshness is per
  `/auth/mfa/verify`, not preserved across refresh." (`StepUpMaxAge` default 5m < jwt
  TTL 15m, so this is consistent — a fresh OTP is required close to the money move.)

Helper on claims: `func (c *clientClaims) hasFreshOTP(maxAge time.Duration) bool` →
`contains(c.AMR,"otp") && time.Since(time.Unix(c.AuthTime,0)) <= maxAge`.

### 4.3 `Login` change (`internal/api/handlers_users.go`)

After credentials verify and **before** issuing tokens:

```go
if enabled, err := s.pg.MFAEnabled(r.Context(), id); err != nil {
	mapDBError(w, err); return
} else if enabled {
	mfaTok, err := s.issueMFAToken(id)
	if err != nil { /* 500 */ }
	writeJSON(w, http.StatusOK, map[string]any{
		"user_id": id, "mfa_required": true, "mfa_token": mfaTok,
	})
	return
}
// ...else existing flow, but issueJWT(id, role, username, []string{"pwd"})
```

### 4.4 MFA handlers (new file `internal/api/handlers_mfa.go`)

All four are client-surface. `enroll`/`confirm` are **behind `requireJWT`** (you must be
logged in to manage your own MFA); `verify` is **public** (the `mfa_token` is the
credential). Guard every handler with an `mfaConfigured()` check (`auth.mfa_enc_key`
set) → else 503 `mfa_unavailable`.

- **`MfaEnroll`**: `subj := clientSubject`. Generate TOTP secret+URI; `encryptSeed`;
  `s.pg.MFABeginEnroll(subj, enc)` (23505 → 409 already-enabled). Return
  `{secret, otpauth_uri}`. **Never** return the raw seed again after this.
- **`MfaConfirm`**: `subj`; load pending seed via `s.pg.MFAPendingSecret(subj)`
  (P0001 "no pending" → 404). Throttle check (`mfa_is_locked`) → 429. Verify `code`
  against decrypted seed with drift window. `mfa_record_attempt(...)` (records + returns
  locked). On bad code → 401 `invalid_code`. On good code: generate 10 recovery codes,
  hash them, `s.pg.MFAConfirm(subj, hashes)`; return plaintext codes **once**.
- **`MfaVerify`** (public): parse `mfa_token` (`parseMFAToken`) → `userID` (401 on
  bad/expired). `mfa_is_locked` → 429. Load confirmed seed (`MFAConfirmedSecret`).
  Try `code` as TOTP (drift window); if that fails, try it as a recovery code
  (`mfa_burn_recovery_code(user, hashToken(code))`). `mfa_record_attempt`. On failure
  → 401 `invalid_code` (429 if it tipped into lockout). On success: issue the **real**
  pair via the existing login tail, but with `issueJWT(userID, role, username,
  []string{"pwd","otp"})` and `IssueRefreshToken(...)`. Same `LoginResponse` body as
  `/auth/login`.

### 4.5 Step-up gate in `CreateTransfer` (`internal/api/handlers_transfers.go`)

Insert the gate **after** the debit-ownership check and **before** calling
`s.pg.Transfer(...)`, so a rejected step-up never creates a transfer or consumes the
idempotency key:

```go
if subj, ok := clientSubject(r.Context()); ok {
	claims, _ := r.Context().Value(subjectCtxKey).(*clientClaims) // already injected by requireJWT
	highValue := req.AmountMinor >= s.cfg.Auth.StepUpLimitMinor
	newPayee := false
	if !highValue { // only the cheaper check when not already gated
		// "new payee": the credit account is NOT among the caller's saved beneficiaries.
		known, err := s.pg.IsKnownPayee(r.Context(), subj, credit)
		if err != nil { mapDBError(w, err); return }
		newPayee = !known
	}
	if (highValue || newPayee) && s.mfaEligible(r.Context(), subj) && !claims.hasFreshOTP(s.cfg.Auth.StepUpMaxAge) {
		writeError(w, http.StatusForbidden, "step_up_required",
			"this transfer requires re-verification (high value or new payee)")
		return
	}
}
```

- `s.mfaEligible(ctx, subj)` = `MFAEnabled(subj)`. **Step-up only applies to users who
  have MFA enabled.** A user without MFA cannot satisfy a step-up, so gating them would
  brick high-value transfers — instead they transfer normally (the limit/maker-checker
  controls still apply server-side). This is intentional and matches "customer control,
  complementing the operator-side maker-checker" ([`06`](../06-client-api.md) §6.2):
  step-up is an *opt-in* hardening for MFA users. Document clearly.
- **New-payee** check: a payee is "known" if the credit account id is the
  `credit_account_id` of one of the caller's `beneficiaries` (migration `00016`),
  **or** the caller has a prior *posted* transfer to it. v1: beneficiaries only (cheap,
  add `IsKnownPayee` query below); the prior-transfer relaxation is an optional follow-up.

`IsKnownPayee` — add to `db/queries/beneficiaries.sql`:

```sql
-- name: IsKnownPayee :one
SELECT EXISTS (
    SELECT 1 FROM beneficiaries
     WHERE owner_user_id = sqlc.arg(owner)::uuid
       AND credit_account_id = sqlc.arg(credit)::uuid
) AS known;
```

### 4.6 DB methods (`internal/db/mfa.go`, new — hand-written pgx, matching `auth.go`)

`MFAEnabled`, `MFABeginEnroll`, `MFAPendingSecret`, `MFAConfirm`, `MFAConfirmedSecret`,
`MFABurnRecoveryCode`, `MFARecordAttempt`, `MFAIsLocked` — each a thin `QueryRow`/`Exec`
over the matching function, scanning the documented return. `bytea` ↔ `[]byte`.
`p_recovery_hashes TEXT[]` ↔ `[]string` (pgx encodes slices to arrays directly).
Add `cleanup_mfa_attempts()` to `RunMaintenance` in `bank.go`.

### Error mapping summary

| Cause | Source | HTTP |
|-------|--------|------|
| Not logged in (enroll/confirm) | handler | 401 |
| MFA already enabled (enroll) | `23505` | 409 |
| No pending enrollment (confirm) | P0001 "no ..." | 404 |
| Bad TOTP / recovery code | handler | 401 `invalid_code` |
| Bad/expired `mfa_token` (verify) | handler (parse) | 401 |
| Throttled / locked | handler (`mfa_is_locked`) | 429 `mfa_locked` |
| `auth.mfa_enc_key` unset | handler | 503 `mfa_unavailable` |
| Step-up required (transfer) | handler | 403 `step_up_required` |

`mapDBError` already covers `23505`→409 and P0001+"not found"/"no"→404. Add **429**
and **503** as explicit `writeError` calls (no DB SQLSTATE needed).

---

## 5. Tests to add

### DB integration (`internal/db/mfa_test.go`)
- enroll → `MFAEnabled` false (unconfirmed); confirm → true; second `MFABeginEnroll` → 23505.
- `MFAPendingSecret` returns the stored ciphertext; `MFAConfirmedSecret` raises before confirm.
- `MFABurnRecoveryCode`: a valid hash burns once (true), again → false; unknown → false.
- `MFARecordAttempt`: N-1 fails → not locked; Nth fail → locked true; `MFAIsLocked` agrees;
  a success appended then window check.
- `IsKnownPayee`: false before adding a beneficiary, true after.

### API integration (`internal/api/mfa_test.go`)
Helper to compute a live TOTP for a returned base32 secret (`totp.GenerateCode`).
- `TestHTTPMfaEnrollConfirmThenLoginRequiresMfa`:
  1. login (no MFA) → tokens. enroll → secret. confirm with live code → recovery codes.
  2. login again → `mfa_required:true`, no `token`, has `mfa_token`.
  3. `/auth/mfa/verify {mfa_token, code:liveTOTP}` → 200 with token+refresh; `/me` works.
- `TestHTTPMfaVerifyBadCode`: verify with wrong code → 401; repeat to lockout → 429.
- `TestHTTPMfaRecoveryCode`: verify with a recovery code → 200; same code again → 401.
- `TestHTTPStepUpRequiredHighValue`: MFA-enabled user, fresh `pwd`-only token (i.e. via
  the verify flow but then let `auth_time` be stale — simulate by setting `StepUpMaxAge`
  tiny, or by minting a `pwd`-only token in a test helper). `POST /transfers` ≥
  `step_up_limit_minor` → 403 `step_up_required`. After `/auth/mfa/verify`, retry with
  the **same Idempotency-Key** → 200, exactly one transfer posted.
- `TestHTTPStepUpNewPayee`: amount below limit but credit account not a beneficiary →
  403; add beneficiary OR step-up → 200.
- `TestHTTPStepUpSkippedWhenNoMfa`: user WITHOUT MFA, high-value transfer → 200 (no gate).
- `TestHTTPStepUpFreshOtpPasses`: immediately after `/auth/mfa/verify`, a high-value
  transfer → 200 (fresh `otp`).

Idempotency assertion: after a 403-then-verify-then-retry with the same key, query the
ledger / `GetTransfer` and assert a single posted transfer (no double-post).

---

## 6. Security considerations

- **Seed encrypted at rest** (AES-256-GCM, `auth.mfa_enc_key`); plaintext seed exists
  only transiently in Go during verify. Empty key ⇒ refuse (503), never store plaintext.
- **Recovery codes**: sha256-only at rest, one-time (`used_at`), shown once. Hash with
  the same `hashToken` helper used for refresh tokens.
- **Throttle + lockout** server-side (`mfa_record_attempt` / `mfa_is_locked`), per user;
  `429` on lockout. Combine with the per-IP login limiter from
  [`06`](../06-client-api.md) §6.4 when it lands.
- **Step-up enforced server-side** via `amr` + `auth_time` in the *signed* JWT — never
  trust a client header. Freshness is per-`/auth/mfa/verify` and explicitly **not**
  preserved across `/auth/refresh` (§4.2), so a stale access token can't satisfy a
  money move.
- **`mfa_token` isolation**: distinct audience `bank0-mfa`; `parseMFAToken` validates
  it, so it can't be replayed against `requireJWT` (`bank0-client`) routes, and a client
  access token can't be used at `/auth/mfa/verify`.
- **No enumeration**: bad code and locked both return generic messages; enroll on an
  already-enabled account 409s without revealing the seed.
- **No secrets logged**: seeds, codes, `mfa_token` never logged.
- **Append-only/ledger untouched**: the gate runs *before* `transfer()`, so a rejected
  step-up writes nothing and consumes no idempotency key — the retry is clean.

---

## 7. Acceptance criteria

- [ ] `api/openapi.yaml`: `/auth/mfa/enroll|confirm|verify` added (client); `LoginResponse`
      gains `mfa_required`+`mfa_token`; `POST /transfers` gains `403`. `task generate:oapi` clean.
- [ ] Migration `00018_mfa.sql`: tables `mfa_credentials`/`mfa_recovery_codes`/`mfa_attempts`
      + functions; `goose up`/`down` succeed.
- [ ] `internal/config`: `step_up_limit_minor`, `step_up_max_age`, `mfa_enc_key`,
      `mfa_token_ttl`, `mfa_lock_max_fail`, `mfa_lock_window` with defaults.
- [ ] JWT carries `amr` + `auth_time`; verify stamps `["pwd","otp"]`, login/refresh `["pwd"]`.
- [ ] `Login` returns `mfa_required` for MFA-enabled users; `/auth/mfa/verify` exchanges
      `mfa_token`+code for the real pair (TOTP or recovery code).
- [ ] TOTP verified per RFC 6238 (SHA1/6/30) with a ±1-step drift window; seed AES-GCM
      encrypted at rest; recovery codes sha256-only + one-time.
- [ ] `POST /transfers` returns **403 `step_up_required`** for MFA-enabled callers on
      high-value OR new-payee transfers without a fresh `otp`; a fresh OTP passes.
- [ ] Step-up retry with the **same `Idempotency-Key`** succeeds and posts exactly one
      transfer (no double-post). Documented for fraudbank clients.
- [ ] Throttle/lockout returns 429; missing `mfa_enc_key` returns 503.
- [ ] DB + API tests pass under `task test:db`.

---

## 8. Implementation order

1. `api/openapi.yaml`: add the three operations, the new schemas, the `LoginResponse`
   fields, and the `403` on `/transfers`. `task generate:oapi` (build breaks — handlers
   missing — as intended).
2. `internal/config`: add the six fields + defaults.
3. `db/migrations/00018_mfa.sql`; `goose up`/`down` on a scratch DB.
4. `go get github.com/pquerna/otp`; write `internal/api/mfa.go` (TOTP + AES-GCM + recovery
   codes + `issueMFAToken`/`parseMFAToken`); extend `clientClaims`/`issueJWT` in `jwt.go`.
5. `internal/db/mfa.go` (hand-written pgx methods); add `IsKnownPayee` to
   `db/queries/beneficiaries.sql` + `task generate:sqlc`; hook `cleanup_mfa_attempts()`
   into `RunMaintenance`.
6. `internal/api/handlers_mfa.go` (enroll/confirm/verify) + the `Login` branch; build passes.
7. Step-up gate in `handlers_transfers.go`.
8. DB tests (`internal/db/mfa_test.go`) then API tests (`internal/api/mfa_test.go`).
9. `task test:db`; fix; done. Update fraudbank `docs/02-api-contract.md` with the
   `mfa_required` login branch and the `step_up_required` + same-key retry.
