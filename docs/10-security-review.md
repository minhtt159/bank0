# bank0 — Security

> The security model of the two HTTP surfaces: the client (JWT bearer) API and the
> admin (cookie session) portal. Protective tests live in
> [`internal/api/security_test.go`](../internal/api/security_test.go),
> [`csrf_test.go`](../internal/api/csrf_test.go),
> [`ratelimit_test.go`](../internal/api/ratelimit_test.go), plus the per-feature
> IDOR/scope tests in `*_test.go`.

## Controls in place

| Area | Control |
|---|---|
| **RBAC on the admin JSON API** | Roles are enforced **per handler**, not just by a valid session. Money / account / dispute mutations require `requireRole(canActOnMoney)`; user creation requires `requireRole(canManageUsers)`. Reads (reconcile, queues, getUser) stay open to any staff. A `requireSession` (authentication) is never mistaken for authorization. Tested: `TestSecurityAdminMutationsRequireRole`. |
| **JWT integrity** | `parseJWT` pins HMAC with `WithValidMethods(["HS256"])`, `WithExpirationRequired()`, and issuer + audience checks. Tampered, wrong-secret, `alg=none`, and garbage tokens all 401. Tested: `TestSecurityJWTForgery`. |
| **Fail-closed JWT secret** | `Config.Validate` (called at startup) refuses to boot when `app.env≠development` and `auth.jwt_secret` is empty — no silent fallback to a hardcoded dev constant in production. Tested: `TestConfigValidate`. |
| **Surface separation** | Admin-only routes sit behind `requireSession`; a client bearer (no cookie) gets 401, even in `mode=all`. Tested: `TestSecurityClientCannotReachAdminJSON`. |
| **Ownership scoping (IDOR)** | Every client read/write is `clientSubject`-scoped: cross-user ids return 404, or 403 for an unowned debit / `from_account`. Covers accounts, ledger, transfers, beneficiaries, disputes, and the guided-suggestion `from_account`. Tested across `integration_test.go`, `disputes_test.go`, `suggestion_test.go`. |
| **Refresh-token rotation + reuse detection** | Refresh tokens are stored as `sha256` only. Each `/auth/refresh` rotates the pair; a replayed (already-rotated) token revokes the whole family ([`06-client-api.md`](06-client-api.md) §3.2). |
| **No user enumeration on auth** | Login returns a generic `invalid_credentials`; `change_password` raises the same `28P01` (→ 401) for wrong-password and for non-active/unknown user. |
| **CSRF on the cookie console** | The session cookie is `SameSite=Strict`, and a `csrfGuard` Origin/Referer same-origin check runs on every portal (console + admin JSON) mutation. A missing Origin/Referer (non-browser caller) is allowed — not a CSRF vector. Tested: `TestCSRFGuard`, `TestSecurityCSRFOnPortal`. |
| **Rate limiting + trusted-proxy IP** | An in-app sliding-window limiter keys per client IP on the credential paths (`/auth/login`, `/auth/refresh`, `/auth/logout`), config `server.rate_limit_per_min` (default 60; `0` disables). Forwarded headers are trusted only when `server.trust_proxy_headers=true` (set behind Cloudflare), then `CF-Connecting-IP` is preferred, else the first XFF hop, else `RemoteAddr` — so the limiter key can't be spoofed by rotating `X-Forwarded-For`. 429 + `Retry-After`. The Cloudflare edge is the primary control; this is a per-instance backstop. Tested: `TestRateLimiterAllow`, `TestRateLimitMiddleware429`, `TestClientIP`. |
| **Bounded request bodies** | `decodeJSON` wraps the body in `http.MaxBytesReader` (1 MiB). |
| **Security headers** | A `securityHeaders` middleware sets `X-Frame-Options: DENY`, `X-Content-Type-Options: nosniff`, `Referrer-Policy`, and a `frame-ancestors 'none'; base-uri 'self'; object-src 'none'` CSP on every surface. The PWA additionally gets a full CSP + HSTS from its Worker. |
| **DB errors don't leak** | `mapDBError` returns curated, stable messages for raw constraint trips (unique violation, generic `23514`, restrict violation, auth) rather than echoing Postgres text. It still surfaces developer-authored business `RAISE`s (`P0001`, crafted `insufficient`/idempotency messages) since those are meaningful and caller-scoped. |
| **Parameterized queries** | All DB access is parameterized via sqlc / PL/pgSQL functions; free text (`dispute.reason`, beneficiary search, IBAN) is stored/compared as a bound value, never concatenated. |

## Known limitations

- **Multi-replica rate limiting.** The in-app limiter is per-instance (in-memory).
  A global limit across replicas needs a shared store — the Cloudflare edge (the
  primary control) or a DB/Redis-backed counter. `/me/password` is not yet rate
  limited: it sits behind a valid JWT, so it is a lower-priority oracle.
- **Raw-error server-side logging.** The 500 path returns a curated message but
  does not yet **log** the raw error server-side; that needs `mapDBError` to carry
  the request logger (a signature change across all handlers).
- **Stricter console CSP.** The `securityHeaders` CSP omits a `script-src`
  lockdown so the CDN-loaded htmx keeps working. Self-hosting htmx (with SRI)
  would allow `script-src 'self'`.
- **No distributed tracing.** `/metrics` covers RED + pool saturation, and
  request-scoped logs carry `request_id`, but OpenTelemetry spans across the
  proxy → api → DB hops are not in place.
- **Cookie flags in dev.** The portal session cookie is not `Secure` in
  `app.env=development` (intentional for local http). Production runs with a
  non-dev env so `Secure` is set.

## Re-run the security tests

```bash
export TEST_DATABASE_DSN='postgres://admin:admin@localhost:5432/bank0_test?sslmode=disable'
go test ./internal/api/ -run 'TestSecurity|TestCSRFGuard|TestRateLimit' -count=1 -v
```
