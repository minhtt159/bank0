# bank0 — Client API (`api.bank0.hnimn.art`)

> The customer-facing JSON API: the same Go binary as the portal, run in
> `server.mode=api`. JWT bearer auth, ownership-scoped to the token subject, and
> **fronted by a Cloudflare proxy** (see [`04-deployment.md`](04-deployment.md)).
> The browser never calls this host directly — the PWA's Worker proxies `/api/*`
> here ([`07-client-web-app.md`](07-client-web-app.md)). MFA and step-up auth are
> a designed extension (§6).

---

## 1. The surface

`api/openapi.yaml` is the source of truth; `oapi-codegen` generates the
`genclient.ServerInterface` (tag `client`), and the handlers implement it, so
spec/handler drift is a build error ([`04-deployment.md`](04-deployment.md) §4).
Every route except the public ones is wrapped by `requireJWT` and scoped to the
JWT subject.

| Area | Method | Path | Auth | Notes |
|------|--------|------|------|-------|
| Auth | POST | `/auth/login` | public | username+password → access JWT **+ refresh token** |
| Auth | POST | `/auth/refresh` | refresh token | rotate → new access + refresh pair |
| Auth | POST | `/auth/logout` | refresh token | revoke one refresh token |
| Auth | POST | `/auth/logout-all` | bearer | revoke every refresh token for the caller |
| Onboarding | POST | `/auth/register` | public | self-registration → locked `pending_verification` customer; `Idempotency-Key` required (replay returns the original body + `Idempotency-Replayed: true`); code dispatched out-of-band |
| Onboarding | POST | `/auth/verify-contact` | public | consume the 6-digit code via the opaque `verify_token`; unlocks login (401 wrong/expired, 422 after 5 attempts, 404 unknown token) |
| Onboarding | POST | `/auth/resend-code` | public | re-dispatch (60s DB cooldown → 429; unknown token → silent 202) |
| Profile | GET | `/me` | bearer | the caller's own `User` (no password hash) |
| Profile | PATCH | `/me` | bearer | self-service edit of name/email/phone (password/status/role can't be set here) |
| Profile | POST | `/me/password` | bearer | change password (verify current); revokes other refresh families, spares the current session |
| Sessions | GET | `/me/sessions` | bearer | active devices (refresh-token families); `X-Refresh-Token` header flags the current one |
| Sessions | DELETE | `/me/sessions/{family_id}` | bearer | selective sign-out of one device (idempotent; 404 if not the caller's) |
| Accounts | GET | `/users/{id}/accounts` | bearer | own accounts only (404 otherwise) |
| Accounts | GET | `/accounts/{id}` | bearer | account + available balance |
| Accounts | POST | `/me/accounts` | bearer | open a new account for the caller — server-minted ISO SE IBAN, default limit + per-user cap from `bank_settings`; `Idempotency-Key` required; cap → 409 `account_limit` |
| Accounts | POST | `/accounts/{id}/limit-requests` | bearer | ask for a transfer-limit change on an OWNED account (403 otherwise); lands in the operator maker-checker queue — never self-applied |
| Statement | GET | `/accounts/{id}/ledger?cursor&cursor_id&limit&from&to&direction&q&min_minor&max_minor` | bearer | composite-keyset cursor (`cursor`+`cursor_id`, fixes same-timestamp tie-skip), running balance, counterparty; server-side filters (date range, direction, free text, amount range) |
| Beneficiaries | GET | `/beneficiaries` | bearer | saved payees (fuzzy search is client-side) |
| Beneficiaries | GET | `/beneficiaries/resolve?iban=&name=` | bearer | confirmation-of-payee: masked owner name + the **server-side CoP/VOP verdict** (`match_result`, `reason_code`, `suggested_name` on close_match only, `gate`) — clients render, never decide |
| Beneficiaries | POST | `/beneficiaries` | bearer | resolve an IBAN + save |
| Beneficiaries | DELETE | `/beneficiaries/{id}` | bearer | scoped removal |
| Transfers | GET | `/transfers/suggestion?from_account&amount_minor` | bearer | guided-transfer "mule menu": `{"options":[…]}` with up to 3 third-party candidates drawn at random from the active `guided_scenarios` short-list (`source=scenario`); `{"options":[]}` when none → the client picks one at random, or falls back to the caller's own account. Read-only ([spec](specs/spec-banking-grade-hardening.md) §5) |
| Transfers | GET | `/transfers?cursor&cursor_id&limit&from&to&status&kind&direction&q` | bearer | caller's cross-account history, newest first; composite-keyset cursor; caller-relative `direction` (out/in); masked counterparty; filterable. Bare array |
| Transfers | POST | `/transfers` | bearer | create (auto-post); `Idempotency-Key` required; optional `end_to_end_id` (ISO 20022, fingerprinted); replay → `Idempotency-Replayed: true` |
| Transfers | GET | `/transfers/{id}` | bearer | transfer status (a party must be owned) |
| Transfers | POST | `/transfers/{id}/post` · `/cancel` | bearer | deferred-settlement lifecycle |
| Notifications | GET | `/me/events?cursor&cursor_id&limit&type&unread_only` | bearer | append-only feed (`transfer.posted`/`payment.incoming`/`device.new`/`dispute.updated`), written in the same txn as its cause; bare array, composite keyset |
| Notifications | GET | `/me/events/unread` | bearer | unread count (badge) |
| Notifications | POST | `/me/events/read` | bearer | mark read up to a cursor (or all); idempotent |
| Fraud evidence | POST | `/me/warning-acks` | bearer | "warned and proceeded / backed out" liability evidence (CoP/VOP pivot); append-only, debit account must be the caller's |
| Disputes | POST | `/transfers/{id}/dispute` | bearer | "I don't recognise this" — party-only, one open per (transfer, caller) |
| Disputes | GET | `/disputes` · `/disputes/{id}` | bearer | track own disputes (raiser-scoped; foreign id → 404) |
| Health | GET | `/health` | public | DB-blind liveness/version |
| Health | GET | `/readyz` | public | DB-aware readiness (pings the DB) |
| Metrics | GET | `/metrics` | public | RED counters |

Public routes (`/auth/login`, `/auth/refresh`, `/auth/logout`, `/auth/register`,
`/auth/verify-contact`, `/auth/resend-code`, `/health`, `/readyz`, `/metrics`,
`/docs`, `/openapi.yaml`) are registered on the parent router ahead of the
JWT-guarded subrouter, so they aren't shadowed. `logout-all` needs the subject,
so it stays behind `requireJWT`. The three onboarding routes share the strict
per-IP login limiter; every `Transfer` carries the rail-ready `uetr`
(bank-minted UUIDv4) and optional originator `end_to_end_id`.

---

## 2. Authentication — access tokens

`POST /auth/login` verifies credentials (bcrypt, in the DB) and mints an **HS256
JWT** (`internal/api/jwt.go`):

- Claims: `sub` (user id), `role`, `username`, `iss=bank0`, `aud=bank0-client`, `exp`.
- TTL `auth.jwt_ttl` (**default 15m** — short, because clients rotate; see §3).
- Secret `auth.jwt_secret` (`APP_AUTH_JWT_SECRET`); empty ⇒ insecure dev fallback + warn.
- `requireJWT` validates `WithIssuer`/`WithAudience`/`WithExpirationRequired`/
  `WithValidMethods([HS256])` on every client route and injects the subject.

`aud=bank0-client` isolates client tokens from the portal's cookie session — the
two are never interchangeable.

---

## 3. Authentication — refresh tokens

Short access tokens need a way to stay logged in without a long-lived bearer.
The refresh token is an **opaque random string**; the DB stores only
`sha256(token)` (the `refresh_tokens` table in
[`00003_users.sql`](../db/migrations/00003_users.sql)), so a DB leak never yields
a live token. All state and transitions live in PL/pgSQL — the Go layer calls one
function and maps typed errors to HTTP, the project's standard discipline
([`01-overview.md`](01-overview.md)).

### 3.1 Model

`refresh_tokens` is keyed by the token hash, with a **`family_id`** (one login =
one family) and `parent_id` chaining each rotation. Lifetime state — `expires_at`
(idle, slid on rotate), `rotated_at`, `revoked_at`/`revoked_reason` — lives on the
row. Config: `auth.refresh_ttl` (30d idle) and `auth.refresh_absolute_ttl` (90d
hard cap per family).

### 3.2 Rotation with reuse detection

`POST /auth/refresh` calls `rotate_refresh_token(old, new, …)`, one atomic
transition:

1. **Live token** → mark it `rotated_at`, insert the child (`parent_id=old`, same
   family, new idle expiry), return the user → new access + refresh pair.
2. **Already rotated/revoked** (a replay — theft signal) → `RAISE 28000`. The API
   then revokes the **whole family** in a *separate, committing* statement
   (`revoke_refresh_family`), because a `RAISE` rolls back the function's own
   writes. The client must re-authenticate.
3. **Expired / past the absolute cap / unknown** → `RAISE 28P01`.

`mapDBError` maps `28000`/`28P01` → **401**.

```mermaid
sequenceDiagram
    participant C as Client
    participant API as client API
    participant DB as Postgres
    C->>API: POST /auth/refresh (refresh token)
    API->>DB: rotate_refresh_token(old,new,…)
    alt token live
        DB-->>API: user_id
        API-->>C: new access JWT + new refresh token
    else replay (already rotated)
        DB-->>API: RAISE 28000
        API->>DB: revoke_refresh_family(old)  (separate stmt)
        API-->>C: 401 — re-authenticate
    end
```

### 3.3 Logout & operator revoke

- `POST /auth/logout` → `revoke_refresh_token` (single session; idempotent).
- `POST /auth/logout-all` → `revoke_user_refresh(subject)` (every family).
- **Operators** can force-revoke a user's app sessions from the console
  (user-detail → "Revoke app sessions" → `revoke_user_refresh`, admin-only,
  audited; [`05-admin-ui.md`](05-admin-ui.md)).
- `cleanup_refresh_tokens()` runs in the advisory-locked maintenance sweep.

---

## 4. Ownership scoping

Every client request is scoped to the JWT `sub` (the `clientSubject` helper):

- `GET /accounts/{id}`, `/accounts/{id}/ledger`, `/users/{id}/accounts`, `/me` →
  **404** for anything not owned by the caller.
- `POST /transfers` requires the **debit** account to belong to the caller
  (**403** otherwise); `GET /transfers/{id}` and the post/cancel lifecycle check
  that the caller is a party.
- Beneficiaries are always scoped to `owner_user_id = subject`.

Scoping applies only on the client surface (a `clientSubject` is present);
operators on the portal are deliberately unscoped (they act on the bank's
behalf). One customer can never read or debit another's account.

---

## 5. Idempotency, errors & money

- `POST /transfers` **requires** an `Idempotency-Key` header; replays return the
  original result and never double-post ([`03-ledger-lifecycle-idempotency.md`](03-ledger-lifecycle-idempotency.md)).
- `mapDBError` is the only place HTTP status is derived from DB SQLSTATEs — every
  business rule still lives in the database.
- Money is **int64 minor units** end to end; `currency` is single (EUR) for now.

---

## 6. MFA & step-up (designed extension)

The MFA/step-up increment hardens login and money moves. Same DB-first discipline;
the access-token path (`requireJWT`) barely changes. Its tables land in a new
migration after the baseline; the full design is in
[`specs/spec-step-up-mfa.md`](specs/spec-step-up-mfa.md).

### 6.1 TOTP MFA

- `mfa_credentials` (kind `totp`/`webauthn`, encrypted seed, `confirmed_at`),
  `mfa_recovery_codes` (stored `sha256` only, one-time), `mfa_attempts`
  (throttle/lockout). "MFA enabled" = a confirmed credential exists.
- Endpoints: `/auth/mfa/enroll` (→ otpauth URI), `/auth/mfa/confirm` (first code →
  recovery codes), `/auth/mfa/verify` (exchange a short-lived `mfa_token` + code →
  tokens). The HMAC-SHA1 TOTP math lives in Go (`pquerna/otp`); the **seed is
  encrypted at rest** with an app-side AEAD key (`auth.mfa_enc_key`).
- `LoginResponse` gains `mfa_required` + `mfa_token`; when required, **no** access
  token is issued until `/auth/mfa/verify`.

### 6.2 Step-up

The access JWT carries `amr` (`["pwd","otp"]`) and `auth_time`. A transfer ≥
`auth.step_up_limit_minor` with a stale `auth_time` returns **403
`step_up_required`**; the client re-runs `/auth/mfa/verify` and retries with the
**same `Idempotency-Key`**. Customer control, complementing the operator-side
maker-checker.

### 6.3 Toward OIDC / asymmetric keys

When a managed IdP arrives, the Cloudflare Worker can run OAuth2/OIDC
authorization-code + PKCE and hold tokens in httpOnly cookies (the BFF in
[`07-client-web-app.md`](07-client-web-app.md)), and `parseJWT` switches from the
HS256 shared secret to **RS256/JWKS**. `aud=bank0-client` and `sub → users.id`
are unchanged, so the ledger and ownership logic don't move.

### 6.4 Security requirements for MFA

- Refresh tokens & recovery codes stored as `sha256` only; never logged.
- TOTP seed encrypted at rest; recovery codes one-time; 6-digit verify throttled/locked.
- Step-up enforced server-side via `amr`/`auth_time`, never client-trusted.
- Rate-limit `/auth/login`, `/auth/refresh`, `/auth/mfa/verify` per subject + IP.
- PII handling vs. the immutable ledger: erase PII in `users`, keep pseudonymous ledger rows.

---

## 7. Design notes

- **Not a separate backend.** The client surface is an auth/identity + ownership
  layer on the *same* ledger API — no second source of truth.
- **Authentication.** Login mints a short HS256 access token plus a rotating
  refresh token with reuse detection (§2–3); MFA/step-up is the designed
  extension (§6).
- **Authorization model.** Customers are `role=customer`; admin ops live only on
  the portal cookie surface, and the client JWT's `aud=bank0-client` can't be
  replayed against an admin audience.
- **Why a Cloudflare-fronted single binary rather than a separate BFF service:**
  the Worker provides a same-origin seam and a place to hold refresh cookies
  without standing up another deployment. The BFF/OIDC path is described in
  [`07-client-web-app.md`](07-client-web-app.md) and §6.3.
- **Beyond the core ledger API**, this surface adds `GET /me`, saved
  **beneficiaries** (with confirmation-of-payee masking), and the refresh-token
  tables/functions — schema in
  [`00003_users.sql`](../db/migrations/00003_users.sql) and
  [`00008_features.sql`](../db/migrations/00008_features.sql).
- **Onboarding v1 is shipped**: public self-registration + contact verification
  (§1 Onboarding rows; schema/functions in
  [`00003_users.sql`](../db/migrations/00003_users.sql)) and customer
  self-service account opening / limit requests (§1 Accounts rows). A
  self-registered user is `locked` + `pending_verification` until a code is
  verified; codes and the `verify_token` are stored hashed; failed attempts are
  persisted from Go in a second statement (a `RAISE` rolls back the function's
  own writes).
- **Open backlog** (full KYC/document capture, notifications, statement
  export, multi-currency) lives in [`specs/`](specs/) — see
  [`specs/spec-p3-roadmap.md`](specs/spec-p3-roadmap.md).
