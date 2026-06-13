# Spec — Self-registration, onboarding state & contact verification

> Implementation spec. Closes the P1 "Self-registration / onboarding" gap in
> [`../09-fraudbank-bff-plan.md`](../09-fraudbank-bff-plan.md) and the onboarding
> item deferred in [`../06-client-api.md`](../06-client-api.md) §7. The full KYC
> vision (document capture, sanctions/PEP screening, tiered limits) is the
> architecture in [`spec-p3-roadmap.md`](spec-p3-roadmap.md) §1; **this spec is the
> shippable v1**: a public signup endpoint, an onboarding state machine on `users`,
> email/phone verification by one-time code, and abuse throttling. A model
> implementing this needs no further design decisions.

---

## 1. Summary & rationale

Today `createUser` is `admin`-tagged: only staff on the portal can mint a customer.
fraudbank's three clients (web/Android/iOS) cannot onboard a real user — demo flows
fake it. This spec adds the public client-surface path:

```
POST /auth/register  →  user in onboarding_status='pending_verification'
POST /auth/verify-contact  →  consume an emailed/SMS'd code  →  'verified' (or 'active')
```

A self-registered user is created with `role='customer'`, `status='locked'` until at
least one contact channel is verified, and a new `onboarding_status` column drives
the client's wizard. No account/IBAN is opened at signup (that is the separate
"customer account opening" gap); a verified user can be hand-opened by staff or by
the future `POST /me/accounts`. Verification codes follow the project's
hash-at-rest + DB-first-state discipline (refresh tokens, `00017`): the DB stores
only `sha256(code)`, all state transitions are PL/pgSQL, Go maps SQLSTATE → HTTP.

Design stance, consistent with the codebase:

- **Onboarding state lives on `users`** (one column + enum), not a new domain — it is
  user lifecycle, like `status`.
- **Verification challenges are their own append-friendly table**, keyed by hash,
  with attempt-throttling columns — the same shape as `refresh_tokens`/`mfa_attempts`.
- **Throttling is DB-enforced** (per-channel cooldown + max attempts), with an
  IP/global rate-limit layered in the Go middleware (signup is unauthenticated).

---

## 2. API — OpenAPI 3.1 operations

Add to `api/openapi.yaml` under `paths:` (tag `client`, all `security: []` — the
caller is not yet authenticated). Style matches the existing `/auth/*` block.

```yaml
  /auth/register:
    post:
      operationId: register
      tags: [client]
      summary: Public self-registration. Creates a locked, pending-verification customer.
      security: []
      parameters: [ { $ref: "#/components/parameters/IdempotencyKey" } ]
      requestBody:
        required: true
        content:
          application/json:
            schema: { $ref: "#/components/schemas/RegisterRequest" }
      responses:
        "201":
          description: registered; a verification code has been dispatched
          content:
            application/json:
              schema: { $ref: "#/components/schemas/RegisterResponse" }
        "409": { $ref: "#/components/responses/Error" }   # username/email/phone taken
        "422": { $ref: "#/components/responses/Error" }   # weak password / bad input
        "429": { $ref: "#/components/responses/Error" }   # rate-limited

  /auth/verify-contact:
    post:
      operationId: verifyContact
      tags: [client]
      summary: Consume a verification code for an email or phone channel
      security: []
      requestBody:
        required: true
        content:
          application/json:
            schema: { $ref: "#/components/schemas/VerifyContactRequest" }
      responses:
        "200":
          description: verified
          content:
            application/json:
              schema: { $ref: "#/components/schemas/VerifyContactResponse" }
        "401": { $ref: "#/components/responses/Error" }   # wrong/expired code
        "404": { $ref: "#/components/responses/Error" }   # no open challenge
        "422": { $ref: "#/components/responses/Error" }   # too many attempts (locked)
        "429": { $ref: "#/components/responses/Error" }

  /auth/resend-code:
    post:
      operationId: resendCode
      tags: [client]
      summary: Re-dispatch a verification code (cooldown-throttled)
      security: []
      requestBody:
        required: true
        content:
          application/json:
            schema: { $ref: "#/components/schemas/ResendCodeRequest" }
      responses:
        "202": { description: dispatched (or silently no-op if no pending challenge) }
        "429": { $ref: "#/components/responses/Error" }
```

Add to `components/schemas:`

```yaml
    RegisterRequest:
      type: object
      required: [username, password, full_name]
      properties:
        username:     { type: string }
        password:     { type: string, description: "min 10 chars; checked server-side" }
        full_name:    { type: string }
        email:        { type: string }
        phone_number: { type: string }
      # at least one of email/phone_number must be present (enforced server-side)
    RegisterResponse:
      type: object
      properties:
        user_id:            { type: string, format: uuid }
        onboarding_status:  { type: string, example: pending_verification }
        verify_channel:     { type: string, enum: [email, phone], description: "channel a code was sent to" }
        verify_token:       { type: string, description: "opaque handle to pass to /auth/verify-contact (does NOT identify the user to the client)" }
    VerifyContactRequest:
      type: object
      required: [verify_token, code]
      properties:
        verify_token: { type: string }
        code:         { type: string, description: "6-digit numeric code" }
    VerifyContactResponse:
      type: object
      properties:
        user_id:           { type: string, format: uuid }
        onboarding_status: { type: string }
        channel:           { type: string, enum: [email, phone] }
        login_ready:       { type: boolean, description: "true once the user may POST /auth/login" }
    ResendCodeRequest:
      type: object
      required: [verify_token]
      properties:
        verify_token: { type: string }
```

Also extend the existing `User` schema with the lifecycle field so `/me` and the
portal user-detail expose it:

```yaml
    # add to User.properties:
        onboarding_status: { type: string }
```

> Note: the code is **never** returned in any response — it is delivered out-of-band
> (email/SMS). In dev, the dispatcher logs it (see §4.5). `verify_token` is an opaque
> per-challenge handle (random, hashed at rest like the code) so a client can call
> `/auth/verify-contact` without re-sending the email/phone (avoids enumeration).

---

## 3. Data model — migration `00018_self_registration.sql`

Migrations are currently numbered to `00017_refresh_tokens.sql`, so `00018` is the
next free number. **Several sibling specs in this directory also claim `00018`**
(`spec-step-up-mfa.md` → `00018_mfa.sql`, `spec-change-password.md` →
`00018_change_password.sql`, `spec-notifications-events.md` → `00018_events.sql`,
`spec-disputes.md`). **Coordinate at land time**: whichever lands first takes `00018`;
renumber the rest sequentially (`00019`, `00020`, …) — goose ordering is what matters,
not the suffix. Adds: an `onboarding_status` enum + column on `users`, the
`verification_challenges` table, and the PL/pgSQL functions.

```sql
-- +goose Up
-- +goose StatementBegin

-- Onboarding lifecycle, distinct from the account-status (active/locked/closed)
-- which gates login. A self-registered user is created status='locked' +
-- onboarding_status='pending_verification'; verifying a channel advances it.
CREATE TYPE onboarding_status AS ENUM (
    'pending_verification',  -- registered, no channel verified yet
    'verified',              -- >=1 contact channel verified; staff/KYC may still gate accounts
    'active',                -- fully onboarded (login enabled)
    'rejected'               -- onboarding denied (e.g. failed KYC, abuse)
);

ALTER TABLE users
    ADD COLUMN onboarding_status onboarding_status NOT NULL DEFAULT 'active',
    ADD COLUMN email_verified_at  TIMESTAMPTZ,
    ADD COLUMN phone_verified_at  TIMESTAMPTZ;
-- DEFAULT 'active' so every pre-existing / admin-created user is unaffected
-- (admin createUser keeps minting 'active' users). Only self-registration sets
-- 'pending_verification' explicitly.

CREATE TYPE verification_channel AS ENUM ('email', 'phone');
CREATE TYPE verification_status  AS ENUM ('pending', 'verified', 'expired', 'canceled');

-- One row per dispatched challenge. Keyed by a hashed opaque token; the code is
-- also stored hashed (sha256). Same hash-at-rest discipline as refresh_tokens.
CREATE TABLE verification_challenges (
    id            UUID PRIMARY KEY DEFAULT uuidv7(),
    user_id       UUID NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    channel       verification_channel NOT NULL,
    destination   TEXT NOT NULL,                 -- email or phone snapshot at send time
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

-- register_user: like create_user, but onboarding_status='pending_verification',
-- status='locked', and it does NOT issue any code (the Go layer generates the
-- token+code, hashes them, and calls create_verification_challenge). Returns the
-- new user id. Unique violations (23505) on username/email/phone -> 409.
CREATE OR REPLACE FUNCTION register_user(
    p_username     CITEXT,
    p_password     TEXT,
    p_full_name    TEXT,
    p_email        CITEXT      DEFAULT NULL,
    p_phone_number VARCHAR(16) DEFAULT NULL
) RETURNS UUID AS $$
DECLARE v_id UUID;
BEGIN
    IF p_email IS NULL AND p_phone_number IS NULL THEN
        RAISE EXCEPTION 'at least one of email or phone is required'
            USING ERRCODE = 'check_violation';
    END IF;
    INSERT INTO users (username, password_hash, full_name, email, phone_number,
                       role, status, onboarding_status)
    VALUES (p_username, crypt(p_password, gen_salt('bf', 10)), p_full_name,
            NULLIF(p_email, ''), NULLIF(p_phone_number, ''),
            'customer', 'locked', 'pending_verification')
    RETURNING id INTO v_id;
    RETURN v_id;
END;
$$ LANGUAGE plpgsql;

-- create_verification_challenge: cancel any prior pending challenge on the same
-- (user, channel), insert a new one. Enforces a resend cooldown. The Go layer
-- supplies the already-hashed token+code (so plaintext never reaches the DB).
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
    IF FOUND AND v_prev.last_sent_at > now() - p_cooldown THEN
        RAISE EXCEPTION 'verification code recently sent; wait before retrying'
            USING ERRCODE = '53400';   -- configuration_limit_exceeded -> 429
    END IF;
    IF FOUND THEN
        UPDATE verification_challenges SET status = 'canceled' WHERE id = v_prev.id;
    END IF;

    INSERT INTO verification_challenges
        (user_id, channel, destination, token_hash, code_hash, expires_at)
    VALUES (p_user_id, p_channel, p_destination, p_token_hash, p_code_hash,
            now() + p_ttl)
    RETURNING id INTO v_id;
    RETURN v_id;
END;
$$ LANGUAGE plpgsql;

-- verify_contact: consume a code against a pending challenge (looked up by token
-- hash). Increments attempts; locks after max_attempts. On success marks the
-- challenge verified, stamps users.email_verified_at/phone_verified_at, and
-- promotes onboarding_status -> 'verified' and status 'locked' -> 'active'
-- (login becomes possible). Returns (user_id, onboarding_status, login_ready).
CREATE OR REPLACE FUNCTION verify_contact(
    p_token_hash TEXT,
    p_code_hash  TEXT
) RETURNS TABLE (user_id UUID, onboarding_status onboarding_status, login_ready BOOLEAN) AS $$
DECLARE v_c verification_challenges%ROWTYPE; v_u users%ROWTYPE;
BEGIN
    SELECT * INTO v_c FROM verification_challenges
     WHERE token_hash = p_token_hash FOR UPDATE;
    IF NOT FOUND THEN RAISE EXCEPTION 'no verification challenge found'; END IF;  -- -> 404

    IF v_c.status <> 'pending' OR v_c.expires_at < now() THEN
        IF v_c.status = 'pending' THEN
            UPDATE verification_challenges SET status = 'expired' WHERE id = v_c.id;
        END IF;
        RAISE EXCEPTION 'verification code expired or already used'
            USING ERRCODE = '28000';   -- -> 401
    END IF;

    IF v_c.attempts >= v_c.max_attempts THEN
        UPDATE verification_challenges SET status = 'expired' WHERE id = v_c.id;
        RAISE EXCEPTION 'too many verification attempts' USING ERRCODE = 'check_violation'; -- -> 422
    END IF;

    IF v_c.code_hash <> p_code_hash THEN
        UPDATE verification_challenges SET attempts = attempts + 1 WHERE id = v_c.id;
        RAISE EXCEPTION 'invalid verification code' USING ERRCODE = '28000'; -- -> 401
    END IF;

    -- success
    UPDATE verification_challenges SET status = 'verified', verified_at = now()
     WHERE id = v_c.id;

    SELECT * INTO v_u FROM users WHERE id = v_c.user_id FOR UPDATE;
    UPDATE users SET
        email_verified_at = CASE WHEN v_c.channel = 'email' THEN now() ELSE email_verified_at END,
        phone_verified_at = CASE WHEN v_c.channel = 'phone' THEN now() ELSE phone_verified_at END,
        onboarding_status = CASE WHEN onboarding_status = 'pending_verification'
                                 THEN 'verified' ELSE onboarding_status END,
        status            = CASE WHEN status = 'locked' THEN 'active' ELSE status END
     WHERE id = v_c.user_id;

    RETURN QUERY
        SELECT v_c.user_id,
               (SELECT u.onboarding_status FROM users u WHERE u.id = v_c.user_id),
               TRUE;
END;
$$ LANGUAGE plpgsql;

-- expire_verification_challenges: sweep stale pending challenges. Wired into the
-- maintenance ticker (RunMaintenance), exactly like expire_holds.
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
DROP FUNCTION IF EXISTS verify_contact(TEXT, TEXT);
DROP FUNCTION IF EXISTS create_verification_challenge(UUID, verification_channel, TEXT, TEXT, TEXT, INTERVAL, INTERVAL);
DROP FUNCTION IF EXISTS register_user(CITEXT, TEXT, TEXT, CITEXT, VARCHAR);
DROP TABLE IF EXISTS verification_challenges;
DROP TYPE IF EXISTS verification_status;
DROP TYPE IF EXISTS verification_channel;
ALTER TABLE users
    DROP COLUMN IF EXISTS phone_verified_at,
    DROP COLUMN IF EXISTS email_verified_at,
    DROP COLUMN IF EXISTS onboarding_status;
DROP TYPE IF EXISTS onboarding_status;
-- +goose StatementEnd
```

sqlc queries (`db/queries/registration.sql`):

```sql
-- name: RegisterUser :one
SELECT register_user(sqlc.arg(username)::citext, sqlc.arg(password)::text,
    sqlc.arg(full_name)::text, sqlc.narg(email)::citext,
    sqlc.narg(phone_number)::varchar) AS id;

-- name: CreateVerificationChallenge :one
SELECT create_verification_challenge(sqlc.arg(user_id)::uuid,
    sqlc.arg(channel)::verification_channel, sqlc.arg(destination)::text,
    sqlc.arg(token_hash)::text, sqlc.arg(code_hash)::text) AS id;

-- name: ChallengeUserByToken :one  -- for /auth/resend-code (re-dispatch)
SELECT user_id, channel, destination, status
FROM verification_challenges WHERE token_hash = sqlc.arg(token_hash)::text;
```

`verify_contact` RETURNS TABLE, so (per the `transfer()`/`reconcile()` convention)
hand-write it in `internal/db/bank.go` with pgx rather than via sqlc.

---

## 4. Handler logic

New file `internal/api/handlers_registration.go`. Reuse `decodeJSON`, `writeJSON`,
`writeError`, `mapDBError`, and the token helpers (`newSessionToken`, `hashToken`)
already used by login/refresh.

### 4.1 `Register` (POST /auth/register)

1. Decode `RegisterRequest`. Validate: `username`, `password`, `full_name` non-empty;
   **password length ≥ 10** (422 `weak_password` otherwise); at least one of
   `email`/`phone_number` present (422). Normalize phone to E.164-ish (strip spaces).
2. Pick the verify channel: prefer `email` if present, else `phone`.
3. `RegisterUser(...)` → new `user_id`. Unique violation (23505) → `mapDBError` → 409.
4. Generate `verify_token = newSessionToken()` and a **6-digit numeric code**
   (`crypto/rand`, zero-padded). Compute `hashToken(verify_token)` and
   `hashToken(code)` (sha256, same helper as refresh tokens).
5. `CreateVerificationChallenge(user_id, channel, destination, token_hash, code_hash)`.
   `53400` → 429.
6. Dispatch the code via the notifier (§4.5).
7. `201` with `RegisterResponse{user_id, onboarding_status:"pending_verification",
   verify_channel, verify_token}`.

`Idempotency-Key` is **required** (the generated wrapper binds it): a retried signup
with the same key returns the same `user_id`/`verify_token` instead of creating a
duplicate user. Store the response keyed by the idempotency key using the existing
`idempotency_keys` table with `scope='register'` (mirror `request_transfer`'s
INSERT … ON CONFLICT gate; wrap steps 3–5 in one txn). This prevents double-signup on
mobile retry.

### 4.2 `VerifyContact` (POST /auth/verify-contact)

1. Decode. Require `verify_token`, `code` (6 digits). 
2. `VerifyContact(hashToken(verify_token), hashToken(code))` (hand-written pgx).
3. Map errors: `404` no challenge; `401` wrong/expired code (`28000`); `422` too many
   attempts (`23514`). On success return `VerifyContactResponse{user_id,
   onboarding_status, channel, login_ready:true}`.
4. **Do not** issue tokens here — the client now calls `/auth/login` normally (the
   user's `status` is `active`). Keeps the login path single-sourced.

### 4.3 `ResendCode` (POST /auth/resend-code)

1. `ChallengeUserByToken(hashToken(verify_token))`. If absent or non-pending → `202`
   (silent no-op; do not leak whether a token is valid).
2. Generate a fresh code, `CreateVerificationChallenge` (cooldown enforced; `53400`
   → 429), dispatch. Return `202`.

### 4.4 Ownership / scoping

All three endpoints are **public** (`security: []`) and pre-auth, so there is no
`clientSubject`. They must register on the **parent router ahead of `requireJWT`**,
exactly like `/auth/login`, `/auth/refresh`, `/auth/logout` (see
[`../06-client-api.md`](../06-client-api.md) §1). The `verify_token` is the only
capability — it is unguessable and hashed at rest, so no IDOR surface.

### 4.5 Notifier seam

Add a tiny `Notifier` interface (`internal/notify`):

```go
type Notifier interface {
    SendVerification(ctx context.Context, channel, destination, code string) error
}
```

Ship a `LogNotifier` (logs `code` at INFO in dev — gated by `cfg.Env != "production"`)
as the default, so the whole flow is testable with zero external deps. A real
email/SMS provider implements the same interface later; the handler does not change.
**The code is never persisted in plaintext and never returned over the API.**

### 4.6 Error mapping additions

`mapDBError` needs one new case for the cooldown SQLSTATE:

```go
case "53400": // configuration_limit_exceeded -> rate limited
    writeError(w, http.StatusTooManyRequests, "rate_limited", msg)
    return
```

`28000` (wrong/expired code) already maps to 401 (refresh-reuse case) — reuse it.

### 4.7 Edge cases

| Case | Behavior |
|------|----------|
| Username/email/phone already taken | 409 `already_exists` (23505) |
| Password < 10 chars | 422 `weak_password` (Go-side, before DB) |
| Neither email nor phone | 422 (DB `check_violation`, also Go-side) |
| Resend within cooldown | 429 `rate_limited` (53400) |
| Wrong code | 401; `attempts++`; after 5 → challenge expired, 422 on next |
| Expired token | 401; challenge moved to `expired` |
| Verify an already-active user's stale token | 401 (challenge non-pending) |
| Replayed signup (same Idempotency-Key) | returns original user_id/verify_token, no dup |
| Admin-created users | unaffected: `onboarding_status` defaults `active` |

---

## 5. Tests to add

**DB integration (`internal/db/registration_test.go`)** — real PL/pgSQL:

- [ ] `register_user` creates `status='locked'`, `onboarding_status='pending_verification'`.
- [ ] `register_user` with neither email nor phone → `check_violation`.
- [ ] duplicate username/email/phone → `23505`.
- [ ] happy path: create challenge → `verify_contact` with correct code →
      `onboarding_status='verified'`, `status='active'`, `*_verified_at` set,
      `login_ready=true`.
- [ ] wrong code increments `attempts`; 6th attempt → `23514` (locked).
- [ ] expired challenge → `28000`; `expire_verification_challenges()` flips stale rows.
- [ ] resend within cooldown → `53400`; after cooldown → new pending row, old `canceled`.
- [ ] `Login` succeeds only **after** verification (locked user → invalid creds).

**API integration (`internal/api/registration_test.go`)** — use the existing
`helpers_test.go` server harness + a stub `Notifier` that captures the code:

- [ ] `POST /auth/register` → 201, body has `verify_token`, no `code`; stub captured a code.
- [ ] `POST /auth/verify-contact` with captured code → 200 `login_ready:true`; then
      `POST /auth/login` succeeds and returns tokens.
- [ ] wrong code → 401; 6th → 422.
- [ ] duplicate registration → 409.
- [ ] same `Idempotency-Key` replay → same `user_id`.
- [ ] `/auth/register`, `/auth/verify-contact`, `/auth/resend-code` reachable without a bearer.

---

## 6. Security considerations

- [ ] Codes & `verify_token` stored as `sha256` only — never logged in production,
      never returned over the API. Plaintext code exists only in memory + the notifier.
- [ ] Codes are 6-digit numeric, **single-use**, TTL 15m, **≤5 attempts**, then locked
      — brute force is ~5/15min per challenge.
- [ ] Resend cooldown (60s) DB-enforced; plus a **Go middleware IP rate-limit** on the
      three public `/auth/*` routes (signup is unauthenticated — DB throttle alone
      can't stop mass account creation). Reuse/extend whatever fronts `/auth/login`.
- [ ] Enumeration: `/auth/resend-code` always 202; `register` returns the same shape
      whether or not a collision occurred only after the 409 (collisions still 409 —
      acceptable, matches existing username-taken behavior; do not add timing oracles).
- [ ] Self-registered users are `role='customer'` **only** — `register_user` hardcodes
      it; the public endpoint can never mint staff (unlike admin `createUser`).
- [ ] No account/IBAN/money is created at signup — zero ledger surface here.
- [ ] PII vs. immutable ledger ([`../06-client-api.md`](../06-client-api.md) §6.4):
      verification `destination` is a snapshot for audit; subject to the same erase-PII
      policy as `users`.
- [ ] CAPTCHA / proof-of-work is **out of scope** for v1 but is the natural next abuse
      control (note it; do not build).

---

## 7. Acceptance criteria

- [ ] `00018_self_registration.sql` applies cleanly up and down on a throwaway DB.
- [ ] `users.onboarding_status` defaults `active`; existing/admin users unaffected.
- [ ] `oapi-codegen` regenerates `genclient` with `Register`/`VerifyContact`/
      `ResendCode`; handlers implement the interface (build is green).
- [ ] A user created via `POST /auth/register` **cannot** `POST /auth/login` until a
      channel is verified.
- [ ] After `POST /auth/verify-contact` with the right code, the same user logs in and
      `GET /me` shows `onboarding_status:"verified"`.
- [ ] Wrong code → 401; 6 wrong codes → challenge locked (422); resend respects cooldown (429).
- [ ] `expire_verification_challenges()` runs in the maintenance sweep and is reported
      in the tick log alongside `holds_expired`.
- [ ] No code or verify_token appears in any HTTP response body or production log.
- [ ] `reconcile()` is untouched (no ledger surface); books stay balanced.

---

## 8. Step-by-step implementation order

1. Write `db/migrations/00018_self_registration.sql` (enum + columns + table +
   functions). `goose up`/`down` against a scratch DB; eyeball the schema.
2. Add `db/queries/registration.sql`; run `sqlc generate`. Hand-write `VerifyContact`
   (RETURNS TABLE) in `internal/db/bank.go` next to `Transfer`/`Reconcile`.
3. Add `expire_verification_challenges()` to `RunMaintenance` (one more `tx.QueryRow`,
   add the count to the return tuple + the tick log line in `cmd/app/main.go`).
4. Add the `User.onboarding_status` field + the three new operations and schemas to
   `api/openapi.yaml`. Run `oapi-codegen` (both tags). Fix the compiler.
5. Add `internal/notify` with `Notifier` + `LogNotifier`; wire it into `Server`
   (config flag `notify.log_codes`, default true in non-prod).
6. Write `internal/api/handlers_registration.go` (`Register`, `VerifyContact`,
   `ResendCode`); register the three public routes on the parent router ahead of
   `requireJWT` (next to `/auth/login`).
7. Add the `53400` case to `mapDBError`.
8. Add a Go IP rate-limit middleware on the public `/auth/*` routes (or extend the
   existing one).
9. Write DB then API tests (§5). `go test ./...` green without Docker (skips), and
   green with `TEST_DATABASE_DSN`.
10. Update [`../06-client-api.md`](../06-client-api.md) §1 surface table + §7 ("Still
    deferred") to mark onboarding **done (v1)**, and the gap table in
    [`../09-fraudbank-bff-plan.md`](../09-fraudbank-bff-plan.md) P1.
