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
| **Rate limiting + trusted-proxy IP** | An in-app sliding-window limiter keys per client IP on every public `/auth/*` path (login, refresh, logout, register, verify-contact, resend-code, mfa/verify), config `server.rate_limit_per_min` (default 60; `0` disables). Forwarded headers are trusted only when `server.trust_proxy_headers=true`. Then `CF-Connecting-IP` wins (Cloudflare **replaces** it), and `X-Forwarded-For` is read **right-to-left**, `server.trusted_proxy_hops` entries in (default 1). Right-to-left is the load-bearing part: a proxy running with `use_remote_address` (Envoy, nginx, Traefik) **appends** the true downstream address rather than replacing the header, so the left-most entry is whatever the client sent — reading it let an attacker rotate the limiter key per request even with trust enabled. Untrusted mode keys on `RemoteAddr`. 429 + `Retry-After`. Tested: `TestRateLimiterAllow`, `TestRateLimitMiddleware429`, `TestClientIP` (forged leading entries, multi-hop, short chains). |
| **Bounded request bodies** | `decodeJSON` wraps the body in `http.MaxBytesReader` (1 MiB). |
| **Security headers** | A `securityHeaders` middleware sets `X-Frame-Options: DENY`, `X-Content-Type-Options: nosniff`, `Referrer-Policy`, and a `frame-ancestors 'none'; base-uri 'self'; object-src 'none'` CSP on every surface. The PWA additionally gets a full CSP + HSTS from its Worker. |
| **DB errors don't leak** | `mapDBError` returns curated, stable messages for raw constraint trips (unique violation, generic `23514`, restrict violation, auth) rather than echoing Postgres text. It still surfaces developer-authored business `RAISE`s (`P0001`, crafted `insufficient`/idempotency messages) since those are meaningful and caller-scoped. An **unmapped** error returns a generic `500 internal` to the client while the raw error + SQLSTATE are logged server-side (`s.mapDBError` → `s.logFor`, correlated by `request_id`), so it stays debuggable without leaking. |
| **Parameterized queries** | All DB access is parameterized via sqlc / PL/pgSQL functions; free text (`dispute.reason`, beneficiary search, IBAN) is stored/compared as a bound value, never concatenated. |

## Password policy (and why there is no expiry)

Rules live in one place, `assert_password_policy()` (00018), called by both
`change_password` and `register_user`:

| Rule | Value | Why |
|---|---|---|
| Minimum length | 12 chars | NIST SP 800-63B Rev 4 §3.1.1.2 sets 15 for a **single-factor** password and permits ≥ 8 when the password is only ever one factor of MFA. 12 is our middle ground; see the open item below. |
| Maximum length | **72 bytes** | bcrypt hashes only the first 72 bytes. Without this cap a longer passphrase is silently truncated and anyone knowing its first 72 bytes authenticates. Verified against pgcrypto: 72 `a`s and 72 `a`s + a different tail produce the same hash. Bytes, not characters — 25 three-byte runes already exceed it. |
| Must differ from current | yes | Cheap, and the only reuse control worth having without expiry. |
| Composition rules | **none** | NIST SHALL NOT impose character-class rules (Rev 4 §3.1.1.2); OWASP ASVS 5.0 6.2.5 agrees. |
| Password history | **none** | Not in NIST Rev 4, absent from ASVS 5.0 V6. Only PCI DSS 8.3.7 wants it, and PCI scopes to cardholder-data environments — bank0 stores no card data. |
| Periodic expiry | **none** | See below. |

**No periodic expiry, deliberately.** NIST SP 800-63B **Rev 4** (final, 2025-07-31)
§3.1.1.2: verifiers *SHALL NOT* require periodic password changes, and *SHALL* force
a change on evidence of compromise. Rev 4 hardened this from Rev 3's "SHOULD NOT" —
forced rotation drives predictable variations, and a calendar is not evidence.
OWASP ASVS 5.0 6.2.10 and the UK NCSC say the same.

`must_change_password` is the mechanism the standard *does* ask for: rotation on an
event, not on a schedule. Today the only event that sets it is the seeded bootstrap
credential (§4.6a in [`05-admin-ui.md`](05-admin-ui.md)).

PCI DSS 4.0 §8.3.9 is the one framework still requiring 90-day rotation, and it is
inapplicable twice over: it covers cardholder-data environments (bank0 holds no PAN),
and it exempts accounts protected by MFA regardless.

Sources: [NIST SP 800-63B Rev 4](https://pages.nist.gov/800-63-4/sp800-63b.html) ·
[OWASP ASVS 5.0 V6](https://github.com/OWASP/ASVS/blob/master/5.0/en/0x15-V6-Authentication.md) ·
[NCSC on password expiry](https://www.ncsc.gov.uk/blog-post/problems-forcing-regular-password-expiry)

### Open, in priority order

1. **Per-account failed-attempt throttling.** NIST §3.2.2 requires limiting
   consecutive failures *per account*; the current limiter is per-IP, which
   distributed credential stuffing walks straight past. (MFA verify already has a
   per-account lockout — logins do not.)
2. **Breached-password check** on set/change (NIST SHALL, ASVS 6.2.12). The
   Pwned Passwords range API sends only the first 5 hex chars of the SHA-1, so the
   password never leaves the cluster; it belongs in the Go handler, not in
   `change_password` — PL/pgSQL has no egress and a DB transaction must not block on
   a third party. Needs an egress allowlist, and should fail open.
3. **TOTP on portal login.** The customer surface already has TOTP; the portal is
   single-factor today, which is also what forces the higher minimum length under
   Rev 4.
4. **bcrypt cost 10 → 12.** OWASP's floor is exactly 10; rehash on next login.

## Known limitations

- **Multi-replica rate limiting.** The in-app limiter is per-instance (in-memory).
  A global limit across replicas needs a shared store — the Cloudflare edge (the
  primary control) or a DB/Redis-backed counter. `/me/password` is not yet rate
  limited: it sits behind a valid JWT, so it is a lower-priority oracle.
- **Stricter console CSP.** The prerequisite is **done** — htmx is vendored,
  embedded and served same-origin from `/static/htmx.min.js`
  ([`web/static/htmx.min.js`](../web/static/htmx.min.js), `htmxSrc` in
  [`web/template/components.templ`](../web/template/components.templ); regression
  test `TestHTMXSelfHosted`). What remains is adding the `script-src 'self'`
  directive to the `securityHeaders` CSP (`internal/api/middleware.go`) — which
  first needs the console's remaining inline handlers (`onclick=` in
  `shell.templ`/`layout.templ`, `hx-on:click` in `pending.templ`) moved into
  `console.js`, since `script-src 'self'` blocks them.
- **No distributed tracing.** `/metrics` covers RED + pool saturation, and
  request-scoped logs carry `request_id`, but OpenTelemetry spans across the
  proxy → api → DB hops are not in place.
- **Cookie flags in dev.** The portal session cookie is not `Secure` in
  `app.env=development` (intentional for local http). Production runs with a
  non-dev env so `Secure` is set.

## Re-run the security tests

```bash
# no DB needed — fail-closed JWT secret (TestConfigValidate)
go test ./internal/config/ -run 'TestConfigValidate' -count=1 -v

export TEST_DATABASE_DSN='postgres://admin:admin@localhost:5432/bank0_test?sslmode=disable'
go test ./internal/api/ \
  -run 'TestSecurity|TestCSRFGuard|TestRateLimit|TestClientIP|TestHTMXSelfHosted' \
  -count=1 -v
```

`TestClientIP` (trusted-proxy IP) and `TestHTMXSelfHosted` live in
`internal/api/helpers_test.go`, and `TestConfigValidate` in
`internal/config/config_test.go` — hence the two commands. The `internal/api`
suite derives its own database from the DSN (drops/recreates `bank0_test_api` in
`TestMain`), so the DSN's role needs `CREATE DATABASE`.
