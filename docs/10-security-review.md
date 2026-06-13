# bank0 — API security review (pentest pass 1)

> A focused adversarial review of the client (JWT) and admin (session) HTTP surfaces.
> **Pass 1 (2026-06-13)** covered the JWT/JSON surface (auth, IDOR, RBAC). **Pass 2
> (2026-06-14)** added CSRF hardening on the cookie console + a rate-limit backstop.
> Protective tests live in [`internal/api/security_test.go`](../internal/api/security_test.go),
> [`csrf_test.go`](../internal/api/csrf_test.go), [`ratelimit_test.go`](../internal/api/ratelimit_test.go),
> plus the per-feature IDOR/scope tests in `*_test.go`.

## Findings

| # | Severity | Finding | Status |
|---|----------|---------|--------|
| 1 | **High** | **Broken access control on the JSON admin API.** `requireSession` authenticates a portal session but checks **no role**; the genadmin mutation handlers didn't call `requireRole`. An `auditor` (read-only) session could `deposit`/`withdraw`/`reverse`/`set-account-status`/`create-user`/`resolve-dispute` via the JSON endpoints, bypassing the RBAC the console HTML enforces. | **Fixed** — `requireRole(canActOnMoney)` on money/account/dispute mutations + `requireRole(canManageUsers)` on `createUser`. Reads (reconcile, queues, getUser) stay open to any staff. Tested: `TestSecurityAdminMutationsRequireRole`. |
| 2 | Info (verified safe) | JWT forgery / algorithm confusion. | **No issue** — `parseJWT` pins HMAC, `WithValidMethods(["HS256"])`, `WithExpirationRequired()`, issuer+audience. Tampered / wrong-secret / `alg=none` / garbage all 401. Tested: `TestSecurityJWTForgery`. |
| 3 | Info (verified safe) | Client/admin surface separation in `mode=all`. | **No issue** — admin-only routes sit behind `requireSession`; a client bearer (no cookie) gets 401. Tested: `TestSecurityClientCannotReachAdminJSON`. |
| 4 | Info (verified safe) | IDOR / ownership scoping (accounts, ledger, transfers, beneficiaries, disputes, guided-suggestion `from_account`). | **No issue** — every client read/write is `clientSubject`-scoped; cross-user ids return 404 (or 403 for an unowned debit/`from_account`). Tested across `integration_test.go`, `disputes_test.go`, `suggestion_test.go`. |
| 5 | Info (verified safe) | User enumeration on auth. | **No issue** — login returns a generic `invalid_credentials`; `change_password` raises `28P01` for both wrong-password and non-active/unknown user (identical 401). |
| 6 | Info (verified safe) | SQL injection on free-text (`dispute.reason`, beneficiary search, IBAN). | **No issue** — all DB access is parameterized via sqlc / PL/pgSQL functions; free text is stored/compared as a bound value, never concatenated. |
| 7 | Low→Med (pass 2) | **CSRF on the cookie-authed console.** Baseline was already `SameSite=Lax` (the cookie is dropped on a cross-site POST). | **Hardened** — session cookie bumped to `SameSite=Strict`; added an Origin/Referer same-origin guard (`csrfGuard`) on every portal (console + admin JSON) mutation. A missing Origin/Referer (non-browser) is allowed — not a CSRF vector. Tested: `TestCSRFGuard`, `TestSecurityCSRFOnPortal`. |
| 8 | Low→Med (pass 2) | **No rate limiting** on the credential oracles (`/auth/login`, `/auth/refresh`). | **Fixed (backstop)** — in-app sliding-window limiter per client IP (`server.rate_limit_per_min`, default 60; `0` disables, so tests are unaffected). 429 + `Retry-After`. Cloudflare edge remains the primary control; this is a per-instance backstop. Tested: `TestRateLimiterAllow`, `TestRateLimitMiddleware429`. |
| 9 | Low (pass 2) | Unbounded request bodies. | **Fixed** — `decodeJSON` wraps the body in `http.MaxBytesReader` (1 MiB). |

## Remaining gaps (accepted / deferred)

- **Multi-replica rate limiting.** The pass-2 limiter (finding #8) is per-instance
  (in-memory). A global limit across Cloud Run replicas needs a shared store (Cloudflare
  edge rules — the primary control — or a DB/Redis-backed counter). `/me/password` is not
  yet limited: it sits behind a valid JWT, so it is a lower-priority oracle; add it when
  the limiter generalises.
- **Raw DB messages to clients.** `mapDBError` forwards the PL/pgSQL exception message
  (e.g. `insufficient available funds: have X, need Y`) on the P0001/`23514` paths. Only
  ever about the caller's own resource (cross-user is 404), but it is internal detail;
  consider a message allowlist if tightening.
- **Cookie flags in dev.** The portal session cookie is not `Secure` in `app.env=development`
  (intentional for local http). Production must run with a non-dev env so `Secure` is set.

## Re-run

```bash
export TEST_DATABASE_DSN='postgres://admin:admin@localhost:5432/bank0_test?sslmode=disable'
go test ./internal/api/ -run 'TestSecurity|TestCSRFGuard|TestRateLimit' -count=1 -v
```
