# bank0 - Client API (`api.bank0.hnimn.art`)

**TL;DR.** Log in for a 15-minute access token and a rotating refresh token.
Send the access token as `Authorization: Bearer`. Everything you can reach is
scoped to your own user: reading another customer's account is a 404, not a
403, and paying *from* one is a 403. Money
moves carry an `Idempotency-Key` and may be warned on, stepped up, or parked by
the fraud gate before they post. Errors are always
`{"error": <code>, "message": <text>}` - branch on `error` and the status, never
on the message.

> The customer-facing JSON API: the same Go binary as the portal, run in
> `server.mode=api`. JWT bearer auth, ownership-scoped to the token subject, and
> **fronted by a Cloudflare proxy** (see [`04-deployment.md`](04-deployment.md)).
> The browser never calls this host directly - the PWA's Worker proxies `/api/*`
> here ([`07-client-web-app.md`](07-client-web-app.md)). MFA and step-up auth are
> shipped (§6).

---

## 1. The surface

`api/openapi.yaml` is the source of truth; `oapi-codegen` generates the
`genclient.ServerInterface` (tag `client`), and the handlers implement it, so
spec/handler drift is a build error ([`08-development.md`](08-development.md) §3).
Every route except the public ones is wrapped by `requireJWT` and scoped to the
JWT subject.

| Area | Method | Path | Auth | Notes |
|------|--------|------|------|-------|
| Auth | POST | `/auth/login` | public | username+password -> access JWT **+ refresh token**; `password_change_required: true` and no refresh token when the account is under forced rotation (§2.1) |
| Auth | POST | `/auth/refresh` | refresh token | rotate -> new access + refresh pair |
| Auth | POST | `/auth/logout` | refresh token | revoke one refresh token |
| Auth | POST | `/auth/logout-all` | bearer | revoke every refresh token for the caller |
| Onboarding | POST | `/auth/register` | public | **invitation-gated** self-registration -> a locked `pending_verification` customer. Needs a single-use `invitation_code` and an `Idempotency-Key`; the verification code is dispatched out of band. |
| Onboarding | POST | `/auth/verify-contact` | public | consume the 6-digit code via the opaque `verify_token`; unlocks login (401 wrong/expired, 422 after 5 attempts, 404 unknown token) |
| Onboarding | POST | `/auth/resend-code` | public | re-dispatch (60s DB cooldown -> 429; unknown token -> silent 202) |
| MFA | POST | `/auth/mfa/enroll` | bearer | begin TOTP enrollment -> otpauth URI + base32 secret (shown once); 409 if already enabled |
| MFA | POST | `/auth/mfa/confirm` | bearer | first live code -> MFA on + 10 one-time recovery codes (shown once, stored hashed) |
| MFA | POST | `/auth/mfa/verify` | public | exchange the login-issued `mfa_token` + TOTP/recovery code -> real token pair (`amr=["pwd","otp"]`); lockout -> 429 |
| Profile | GET | `/me` | bearer | the caller's own `User` (no password hash); includes `invites_remaining` (the caller's lifetime invite quota) |
| Profile | PATCH | `/me` | bearer | self-service edit of name/email/phone (password/status/role can't be set here) |
| Profile | POST | `/me/password` | bearer | change password (verify current); revokes **every** session and refresh family, the caller's included -> sign in again with the new password |
| Invitations | POST | `/me/invitations` | bearer | mint a single-use invite code -> 201 `{code, expires_at, invites_remaining}`. Verified-active callers only (403); the lifetime quota decrements and never refunds. |
| Invitations | GET | `/me/invitations` | bearer | the caller's issued invites - bare array `[{code, status, created_at, expires_at, consumed_at}]`; `status` is derived `pending`/`consumed`/`expired` |
| Sessions | GET | `/me/sessions` | bearer | active devices (refresh-token families); `X-Refresh-Token` header flags the current one |
| Sessions | DELETE | `/me/sessions/{family_id}` | bearer | selective sign-out of one device (idempotent; 404 if not the caller's) |
| Accounts | GET | `/users/{id}/accounts` | bearer | own accounts only (404 otherwise) |
| Accounts | GET | `/accounts/{id}` | bearer | account + available balance |
| Accounts | POST | `/me/accounts` | bearer | open an account for the caller: server-minted NL IBAN (internal-only, never routable), limits from `bank_settings`. `Idempotency-Key` required; per-user cap -> 409 `account_limit`. |
| Accounts | POST | `/accounts/{id}/limit-requests` | bearer | ask for a transfer-limit change on an OWNED account (403 otherwise); lands in the operator maker-checker queue - never self-applied |
| Statement | GET | `/accounts/{id}/ledger?cursor&cursor_id&limit&from&to&direction&q&min_minor&max_minor` | bearer | the account's entries with a running balance and counterparty, composite-keyset paged, filterable by date, direction, text and amount. |
| Beneficiaries | GET | `/beneficiaries` | bearer | saved payees (fuzzy search is client-side) |
| Beneficiaries | GET | `/beneficiaries/resolve?iban=&name=` | bearer | confirmation of payee: masked owner name, the server-side match verdict, and recipient risk signals. Clients render the verdict, never compute it. |
| Beneficiaries | POST | `/beneficiaries` | bearer | resolve an IBAN + save |
| Beneficiaries | DELETE | `/beneficiaries/{id}` | bearer | scoped removal |
| Transfers | GET | `/transfers/suggestion?from_account&amount_minor` | bearer | guided-transfer candidates: up to 3 third-party options drawn from the active scenario short-list, `{"options":[]}` when there are none. Read-only. |
| Transfers | GET | `/transfers?cursor&cursor_id&limit&from&to&status&kind&direction&q` | bearer | caller's cross-account history, newest first; composite-keyset cursor; caller-relative `direction` (out/in); masked counterparty; filterable. Bare array. Each item carries `status_iso` (ISO-20022 parallel status) |
| Transfers | POST | `/transfers/intent` | bearer | read-only fraud preflight (§8): `decision`, `risk_band`, `reason_codes[]`, optional `warning{}`, `step_up_method`. Writes nothing, and never returns a numeric score. |
| Transfers | POST | `/transfers` | bearer | create and auto-post. `Idempotency-Key` required. The fraud gate may park it as `held` or `under_review`, or refuse it with `422 payment_blocked` / `409 ack_required` (§8). |
| Transfers | GET | `/transfers/{id}` | bearer | transfer status (a party must be owned); every transfer carries `status_iso` (ISO-20022 parallel status); `held`/`under_review` carry `hold_reason` + `hold_expires_at` |
| Transfers | POST | `/transfers/{id}/post` and `/transfers/{id}/cancel` | bearer | deferred-settlement lifecycle; `cancel` also releases a `held` transfer, but refuses `under_review` (409, operator-only) |
| Transfers | POST | `/transfers/{id}/confirm` | bearer | release a `held` transfer to `posted` (§8.3). Owner only, idempotent; not-held, `under_review` or a lapsed window -> 409. |
| Notifications | GET | `/me/events?cursor&cursor_id&limit&type&unread_only` | bearer | append-only feed (`transfer.posted`/`payment.incoming`/`transfer.held`/`device.new`/`dispute.updated`), written in the same txn as its cause; bare array, composite keyset |
| Notifications | GET | `/me/events/unread` | bearer | unread count (badge) |
| Notifications | POST | `/me/events/read` | bearer | mark read up to a cursor (or all); idempotent |
| Fraud evidence | POST | `/me/warning-acks` | bearer | "warned and proceeded / backed out" liability evidence (CoP/VOP pivot); append-only, debit account must be the caller's |
| Disputes | POST | `/transfers/{id}/dispute` | bearer | "I don't recognise this" - party-only, one open per (transfer, caller); optional `scam_type` starts the PSR claim (15-BBD `sla_due_at`) |
| Disputes | GET | `/disputes` and `/disputes/{id}` | bearer | track own disputes (raiser-scoped; foreign id -> 404); each carries the disputed transfer's `currency` |
| Health | GET | `/health` | public | DB-blind liveness/version |
| Health | GET | `/readyz` | public | DB-aware readiness (pings the DB) |
| Metrics | GET | `/metrics` | public | RED counters |

`/readyz` and `/metrics` are real routes on every surface but deliberately **absent
from `api/openapi.yaml`**: they are the ops surface (probes and scrape target), not
part of the customer contract - so the spec stays the source of truth for what
clients may call.

Public routes (`/auth/login`, `/auth/refresh`, `/auth/logout`, `/auth/register`,
`/auth/verify-contact`, `/auth/resend-code`, `/auth/mfa/verify`, `/health`,
`/readyz`, `/metrics`, `/docs`, `/openapi.yaml`) are registered on the parent router
ahead of the JWT-guarded subrouter, so they aren't shadowed. `logout-all` needs the subject,
so it stays behind `requireJWT`. The three onboarding routes share the strict
per-IP login limiter; every `Transfer` carries the rail-ready `uetr`
(bank-minted UUIDv4) and optional originator `end_to_end_id`.

---

## 2. Authentication - access tokens

`POST /auth/login` verifies credentials (bcrypt, in the DB) and mints an **HS256
JWT** (`internal/api/jwt.go`):

- Claims: `sub` (user id), `role`, `username`, `iss=bank0`, `aud=bank0-client`, `exp`,
  and `pwc` when the account is under forced rotation (§2.1).
- TTL `auth.jwt_ttl` (**default 15m** - short, because clients rotate; see §3).
- Secret `auth.jwt_secret` (`APP_AUTH_JWT_SECRET`); empty => insecure dev fallback + warn.
- `requireJWT` validates `WithIssuer`/`WithAudience`/`WithExpirationRequired`/
  `WithValidMethods([HS256])` on every client route and injects the subject.

`aud=bank0-client` isolates client tokens from the portal's cookie session - the
two are never interchangeable.

### 2.1 Forced password rotation

An operator can require a customer to change their password (console user detail,
[`05-admin-ui.md`](05-admin-ui.md) §4.6a). That raises `users.must_change_password`
and signs the customer out of every session.

The flag then rides the token. `check_user_credentials` and `rotate_refresh_token`
return it alongside the claims they already return, so login, `/auth/mfa/verify`
and `/auth/refresh` all mint it as the `pwc` claim. What the client sees:

- Login still succeeds - the customer has the right password, it is simply one the
  bank no longer trusts - and the response carries `password_change_required: true`
  with **no** `refresh_token`.
- That access token reaches exactly two operations: `POST /me/password` and
  `POST /auth/logout-all`. Every other bearer route answers
  **403 `password_change_required`**.
- Changing the password clears the flag (`change_password()` sets it false) and
  revokes everything, so the next login returns a normal pair.

Route the customer straight to a change-password screen on that field; a client
that ignores it will see 403 on every screen it opens.

One window is open by design: an access token minted *before* the operator raised
the flag stays valid until it expires (`auth.jwt_ttl`, 15m by default). The
refresh families are revoked when the flag is raised, so the window cannot be
extended past that single token's lifetime.

---

## 3. Authentication - refresh tokens

Short access tokens need a way to stay logged in without a long-lived bearer.
The refresh token is an **opaque random string**; the DB stores only
`sha256(token)` (the `refresh_tokens` table in
[`00004_auth_tokens.sql`](../db/migrations/00004_auth_tokens.sql)), so a DB leak never yields
a live token. All state and transitions live in PL/pgSQL - the Go layer calls one
function and maps typed errors to HTTP, the project's standard discipline
([`01-overview.md`](01-overview.md)).

### 3.1 Model

`refresh_tokens` is keyed by the token hash, with a **`family_id`** (one login =
one family) and `parent_id` chaining each rotation. Lifetime state - `expires_at`
(idle, slid on rotate), `rotated_at`, `revoked_at`/`revoked_reason` - lives on the
row. Config: `auth.refresh_ttl` (30d idle) and `auth.refresh_absolute_ttl` (90d
hard cap per family).

### 3.2 Rotation with reuse detection

`POST /auth/refresh` takes the token in the request body as
`{"refresh_token": "..."}` - it is not a header, and no bearer is needed, since
the access token may already have expired. It calls
`rotate_refresh_token(old, new, ...)`, one atomic transition:

1. **Live token** -> mark it `rotated_at`, insert the child (`parent_id=old`, same
   family, new idle expiry), return the user -> new access + refresh pair.
2. **Already rotated/revoked** (a replay - theft signal) -> `RAISE 28000`. The API
   then revokes the **whole family** in a *separate, committing* statement
   (`revoke_refresh_family`), because a `RAISE` rolls back the function's own
   writes. The client must re-authenticate.
3. **Expired / past the absolute cap / unknown** -> `RAISE 28P01`.

`mapDBError` maps `28000`/`28P01` -> **401**.

```mermaid
sequenceDiagram
    participant C as Client
    participant API as client API
    participant DB as Postgres
    C->>API: POST /auth/refresh (refresh token)
    API->>DB: rotate_refresh_token(old,new,...)
    alt token live
        DB-->>API: user_id
        API-->>C: new access JWT + new refresh token
    else replay (already rotated)
        DB-->>API: RAISE 28000
        API->>DB: revoke_refresh_family(old)  (separate stmt)
        API-->>C: 401 - re-authenticate
    end
```

### 3.3 The whole auth lifecycle

The states a client moves through, and what ends each one. The two transitions
clients get wrong are the ones on the right: a replayed refresh token kills the
entire family, and a password change kills every session including the one that
made the call.

```mermaid
stateDiagram-v2
    [*] --> SignedOut
    SignedOut --> MfaPending: POST /auth/login (MFA enrolled)
    MfaPending --> Active: POST /auth/mfa/verify (code)
    SignedOut --> Active: POST /auth/login
    SignedOut --> MustChangePassword: POST /auth/login (flagged)
    Active --> Active: POST /auth/refresh (rotates, same family)
    Active --> MustChangePassword: POST /auth/refresh (flagged since login)
    MustChangePassword --> SignedOut: POST /me/password (flag cleared, all revoked)
    Active --> SignedOut: POST /me/password (every session revoked)
    Active --> SignedOut: POST /auth/logout (this family)
    Active --> SignedOut: POST /auth/logout-all (every family)
    Active --> SignedOut: replayed refresh token (family revoked, 401)
    Active --> SignedOut: access token expires, refresh expired or capped
```

Diagram: client auth states. Signed out leads to active (directly, or via the
MFA-pending state); active refreshes into itself; logout, password change,
refresh-token replay and expiry all lead back to signed out; a flagged account
passes through a must-change-password state that only the password change exits.

### 3.4 Logout & operator revoke

- `POST /auth/logout` -> `revoke_refresh_token` (single session; idempotent).
- `POST /auth/logout-all` -> `revoke_user_refresh(subject)` (every family).
- **Operators** can force-revoke a user's app sessions from the console
  (user-detail -> "Revoke app sessions" -> `revoke_user_refresh`, admin-only,
  audited; [`05-admin-ui.md`](05-admin-ui.md)).
- `cleanup_refresh_tokens()` runs in the advisory-locked maintenance sweep.

---

## 4. Ownership scoping

Every client request is scoped to the JWT `sub` (the `clientSubject` helper):

- `GET /accounts/{id}`, `/accounts/{id}/ledger`, `/users/{id}/accounts`, `/me` ->
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
- **Documented header semantics.** The OpenAPI spec spells the
  `Idempotency-Key` contract out on every mutating money POST that carries one -
  `postTransfer`, `confirmTransfer`, `cancelTransfer`, `raiseDispute`,
  `reverseTransfer`: a replay with the same key + same parameters returns the
  original result with **`Idempotency-Replayed: true`**; the same key with different
  parameters is **`422 idempotency_key_conflict`**; a duplicate racing an in-flight
  request is **`409 in_progress`**; keys are retained **~7 days**, after which the
  same key is a fresh request. (`raiseDispute` is naturally idempotent on
  `(transfer, caller)` - at most one open dispute per pair - rather than on a header.)
- **Reverse is idempotent on the transfer, not just the key.** A second
  reverse of an already-reversed transfer - even under a **different** key - returns
  the **existing** reversal id (`200`), never a second inverse pair
  ([`03-...md`](03-ledger-lifecycle-idempotency.md) §2.4).
- `mapDBError` is the only place HTTP status is derived from DB SQLSTATEs - every
  business rule still lives in the database. New codes: `422 payment_blocked` and
  `409 ack_required` from the fraud gate (§8).
- **Error contract (stable at 1.0).** Every non-2xx JSON response is
  `{"error": <code>, "message": <human text>}` with `Content-Type:
  application/json` - including errors minted by the Worker proxy (`bad_gateway`)
  and by request binding (`bad_request`). Clients MUST branch on `error` and the
  HTTP status only; `message` is display text and may change without notice. The
  `error` code tokens are a registry: existing tokens are never renamed or removed
  in 1.x, and new tokens may be added at any time (treat an unknown token as a
  generic failure for its status class). The envelope is additive: new top-level
  members may appear in 1.x and MUST be ignored if unrecognised. **Path to
  RFC 9457:** if `application/problem+json` is ever adopted, it is
  served via content negotiation (`Accept: application/problem+json`) with the
  `error` token carried as a `code` extension member, leaving the default
  response above untouched - never as an in-place replacement.
- Money is **int64 minor units** end to end - the smallest unit of the currency,
  so EUR 12.34 travels as `1234`. Never a decimal, never a float; `currency` is single (EUR) for now,
  and ships explicitly on every money-bearing **response** - including
  `Dispute`/`DecideDisputeResponse`. Requests **inherit** the debit account's
  currency by design (no request-side `currency`; see
  [`12-rail-readiness.md`](12-rail-readiness.md) §5).
- **Rail-ready additive fields.** Transfer responses (`Transfer`,
  `TransferListItem`, `TransferResult`) carry `status_iso`, an ISO-20022
  ExternalPaymentTransactionStatus (`PDNG`/`ACSC`/`RJCT`/`CANC`) **computed** from
  `status`, never stored, added alongside the flat `status` (never replacing it).
  See [`12-rail-readiness.md`](12-rail-readiness.md).

---

## 5.1 Transfer statuses

There is one status vocabulary, and a client should switch on it exhaustively.
`status_iso` rides alongside it as an ISO-20022 projection, computed from
`status` and never stored - ignore it unless you are mapping to a payment rail.

| `status` | `status_iso` | Means | Customer action |
|---|---|---|---|
| `pending` | `PDNG` | above the maker-checker threshold, waiting for a second operator | wait |
| `held` | `PDNG` | the fraud gate parked it for a cooling-off period | **confirm** (`POST /transfers/{id}/confirm`) or **cancel**, before `hold_expires_at` |
| `under_review` | `PDNG` | an AML watchlist hit; an operator must decide | none - confirm and cancel both answer 409 |
| `posted` | `ACSC` | the ledger entries are written; the money has moved | none |
| `canceled` | `CANC` | withdrawn by the customer or auto-canceled when a window lapsed | none |
| `reversed` | `ACSC` | a later reversing pair undid it; the original entries remain | none |

A lapsed `held` or `under_review` window is auto-canceled by the maintenance
sweep. That is the fail-safe direction: an unanswered payment does not post
itself.

---

## 6. MFA & step-up

MFA hardens login; step-up hardens individual money moves. Both keep the DB-first
discipline - the tables and `mfa_*` functions live in
[`00006_mfa.sql`](../db/migrations/00006_mfa.sql), the handlers in
`internal/api/handlers_mfa.go`, and `requireJWT` is untouched by either.

### 6.1 TOTP MFA

- `mfa_credentials` (kind `totp`/`webauthn`, encrypted seed, `confirmed_at`),
  `mfa_recovery_codes` (stored `sha256` only, one-time), `mfa_attempts`
  (throttle/lockout). "MFA enabled" = a confirmed credential exists.
- Endpoints: `/auth/mfa/enroll` (-> otpauth URI), `/auth/mfa/confirm` (first code ->
  recovery codes), `/auth/mfa/verify` (exchange a short-lived `mfa_token` + code ->
  tokens; public - the token, audience `bank0-mfa`, is the credential; shares the
  login rate limiter). The HMAC-SHA1 TOTP math lives in Go (`pquerna/otp`,
  SHA1/6/30s, ±1-step drift); the **seed is encrypted at rest** (AES-256-GCM,
  `auth.mfa_enc_key`; unset key => MFA endpoints 503).
- `LoginResponse` gains `mfa_required` + `mfa_token`; when required, **no** access
  token is issued until `/auth/mfa/verify`.

### 6.2 Step-up

The access JWT carries `amr` (`["pwd","otp"]`), `auth_time` and - from a linked
verify - `txn_link`. For an MFA-enabled caller, a transfer >=
`auth.step_up_limit_minor`, **or to a new payee** (not among their saved
beneficiaries), **or scored `high` by the server-side TRA seam**
(`assess_transfer_risk()`: flagged/reported destination, 24h velocity count &
value, first payment, fresh debit account) returns **403 `step_up_required`**
unless the token carries a **fresh otp dynamically linked to this exact
payment** (PSD2 RTS Art. 5 / WYSIWYS): the client re-runs `/auth/mfa/verify`
with `link: {debit_account, credit_account, amount_minor}` and retries with the
**same `Idempotency-Key`** (the gate runs before the key is claimed, so the
retry posts exactly once). A generic fresh OTP - including the login-time
verify - does NOT authorize a gated transfer; changing amount or payee
invalidates the factor. Freshness is per-verify - deliberately NOT preserved
across `/auth/refresh`. Users without MFA are not gated (they could never
satisfy it); limits + maker-checker still apply. Customer control,
complementing the operator-side maker-checker.

### 6.3 Where this is heading

If a managed identity provider ever fronts this API, the Worker runs the
OAuth2/OIDC authorization-code + PKCE flow and `parseJWT` moves from the HS256
shared secret to RS256/JWKS. `aud=bank0-client` and `sub -> users.id` do not
change, so ownership scoping and the ledger stay where they are.

---

## 7. Why the surface is shaped this way

- **Not a second backend.** The client surface is an auth + ownership layer over
  the *same* ledger the portal uses. There is no second source of truth.
- **Customers are `role=customer`.** Admin operations exist only on the portal's
  cookie surface, and `aud=bank0-client` cannot be replayed against it.
- **A Cloudflare-fronted single binary, not a separate BFF service.** The Worker
  gives the PWA a same-origin seam and somewhere to hold refresh cookies without
  a second deployment to run ([`07-client-web-app.md`](07-client-web-app.md)).
- **What this surface adds over the core ledger API**: `GET /me`, saved
  beneficiaries with confirmation-of-payee masking, invitation-gated
  self-registration with a per-customer invite quota, self-service account
  opening, and the refresh-token tables - schema in
  [`00004_auth_tokens.sql`](../db/migrations/00004_auth_tokens.sql),
  [`00005_onboarding.sql`](../db/migrations/00005_onboarding.sql) and
  [`00011_beneficiaries.sql`](../db/migrations/00011_beneficiaries.sql).
- **Security rules that hold across the surface.** Refresh tokens and recovery
  codes are stored as `sha256` only and never logged; the TOTP seed is encrypted
  at rest; step-up is decided server-side from `amr`/`auth_time`, never from a
  client claim; `/auth/login`, `/auth/refresh` and `/auth/mfa/verify` are rate
  limited per IP on top of the per-account DB throttles.
- **What is not built** (full KYC and document capture, statement export,
  multi-currency) is tracked in [`specs/spec-p3-roadmap.md`](specs/spec-p3-roadmap.md).

---

## 8. Fraud preflight, warnings & held payments

These endpoints let the client see - and act on - the **same** server-side risk
decision the ledger enforces at submit. As
always the logic lives in the DB (`evaluate_transfer`, `screen_payment`,
`assert_warning_ack`, `place_transfer_hold`, `client_confirm_transfer`); the client
renders and the engine decides. The mechanics are in
[`03-ledger-lifecycle-idempotency.md`](03-ledger-lifecycle-idempotency.md) §2.8/§1;
this is the client contract.

### 8.1 The preflight - `POST /transfers/intent`

A **read-only** preview of what would happen if the caller submitted a given
transfer. It runs the exact evaluation `POST /transfers` applies but reserves
nothing, posts nothing, and writes no row - call it as often as you like (e.g. on
amount/payee change). It requires the caller to own the debit account (**403**
otherwise). The response:

```jsonc
{
  "decision": "warn",                       // allow | warn | step_up | review | block
  "risk_band": "medium",                    // low | medium | high (server-authoritative)
  "reason_codes": ["first_payment_to_payee"],// machine tokens; ALWAYS an array, never null
  "warning": {                               // present only when a warning rule matched, else null
    "warning_id": "...", "category": "risk_warning",
    "severity": "warning",                   // info | warning | critical
    "headline": "...", "body": "...",
    "required_ack": true, "cooling_off_seconds": 15
  },
  "step_up_method": null                     // "otp" when decision = step_up, else null
}
```

**There is no numeric risk score in the response, by design** - the score never
leaves the database. `decision` is the single collapsed outcome (precedence
`block > review > step_up > warn > allow`):

| `decision` | Meaning for the client |
|---|---|
| `allow` | Proceed; submit will post. |
| `warn` | Show `warning`; the customer may proceed (record the ack if `required_ack`). |
| `step_up` | Re-verification **dynamically linked to this exact payment** is required before submit - run the step-up flow in §6.2 (`step_up_method` says how, e.g. `otp`). |
| `review` | Submitting will **park** the payment as `held` for the customer to confirm (§8.3). |
| `block` | Submitting will be refused `422 payment_blocked`. |

Because both the preflight and the submit gate call `evaluate_transfer`, and the
submit path excludes its own just-created pending row from the velocity math, the
two agree at a boundary. One deliberate divergence: the preflight **downgrades
`step_up -> allow`** (dropping `step_up_method`) when the caller could never be gated
- they have no MFA enrolled, or their token already carries a fresh OTP linked to
exactly this `(debit, credit, amount)` - so the client isn't told to step up for a
payment it can already make. `warn`/`review`/`block` are never downgraded.

### 8.2 The acknowledgement rule (liability evidence)

When `warning.required_ack` is true, the customer must record a
"warned-and-proceeded" acknowledgement via `POST /me/warning-acks` (§1 Fraud
evidence; category `risk_warning` joins the existing CoP/VOP categories) **before**
submitting. At submit time the DB (`assert_warning_ack`) requires a matching
`warning_acks` row on all of:

- `user` = the caller, `category` = the warning's category, `acknowledged = true`;
- `debit account` = the debit, `counterparty IBAN` = the credit party's IBAN,
  `amount_minor` = the **exact** amount;
- **aged** at least `cooling_off_seconds` old, yet still **fresh** - within
  `cooling_off_seconds + 30 minutes`.

The dual bound is deliberate: the age floor stops a customer pre-clicking "I
understand" far in advance, and the 30-minute ceiling stops a stale ack from a prior
session authorising a later payment. Change the amount or the payee and the old ack
no longer matches. A missing / too-fresh / too-old / mismatched ack is
**`409 ack_required`**; the client re-acknowledges (respecting the cooling-off) and
retries with the **same** `Idempotency-Key` - a blocked/ack-required attempt leaves
no claimed key (it rolls back), so the retry posts exactly once
([`03-...md`](03-ledger-lifecycle-idempotency.md) §3).

### 8.3 Held & under-review payments (the customer's view)

The submit gate can return a **parked** transfer instead of `posted` - funds
reserved, no ledger entry yet, `hold_reason` + `hold_expires_at` populated on the
`Transfer`, and a `transfer.held` event pushed to the payer's feed:

- **`held`** - the customer's own cooling-off (a `review` decision, 1 business day).
  The owner releases it with **`POST /transfers/{id}/confirm`** (-> `posted`) or
  withdraws it with `POST /transfers/{id}/cancel`. Confirming an already-posted
  transfer is an idempotent no-op; a lapsed confirmation window is `409`.
- **`under_review`** - operator AML screening (a `screen_payment` watchlist hit, 4
  business days). The customer can **neither confirm nor cancel** it (both -> `409`);
  an operator releases or refuses it from the console screening queue
  ([`05-admin-ui.md`](05-admin-ui.md) §4.4a). It is **never auto-released**.

Either way, if the window lapses the maintenance sweep **auto-cancels** the transfer
(`'confirmation window expired'` / `'review window expired'`) - the fail-safe
direction. `GET /transfers` and `GET /transfers/{id}` accept and return `held` /
`under_review` alongside the other statuses.

### 8.4 PWA behaviour

The Transfer flow (`web/app/src/routes/Transfer.tsx`) calls `/transfers/intent` on
the confirm screen and renders the `warning` as a severity-styled card with the
correct ARIA role (`alert` for `critical`, `aria-live="polite"` otherwise). When
`required_ack`, it shows an acknowledgement checkbox that posts the ack, then runs a
live **cooling-off countdown** (`lib/duration.ts`) off the ack timestamp and keeps
the **Send** button disabled until the countdown elapses. Submit-time
`422 payment_blocked` / `409 ack_required` responses are mapped back into the same
warning card (rather than a raw banner), so the DB stays the source of truth even if
the advisory preflight was skipped or raced. `held`/`under_review` results route to
the receipt like any other status.
