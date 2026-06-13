# bank0 — API security review (pentest pass 1)

> A focused adversarial review of the client (JWT) and admin (session) HTTP surfaces,
> 2026-06-13. Protective regression tests live in
> [`internal/api/security_test.go`](../internal/api/security_test.go) (plus the
> per-feature IDOR/scope tests in `*_test.go`). DSN-gated like the rest of the suite.

## Findings

| # | Severity | Finding | Status |
|---|----------|---------|--------|
| 1 | **High** | **Broken access control on the JSON admin API.** `requireSession` authenticates a portal session but checks **no role**; the genadmin mutation handlers didn't call `requireRole`. An `auditor` (read-only) session could `deposit`/`withdraw`/`reverse`/`set-account-status`/`create-user`/`resolve-dispute` via the JSON endpoints, bypassing the RBAC the console HTML enforces. | **Fixed** — `requireRole(canActOnMoney)` on money/account/dispute mutations + `requireRole(canManageUsers)` on `createUser`. Reads (reconcile, queues, getUser) stay open to any staff. Tested: `TestSecurityAdminMutationsRequireRole`. |
| 2 | Info (verified safe) | JWT forgery / algorithm confusion. | **No issue** — `parseJWT` pins HMAC, `WithValidMethods(["HS256"])`, `WithExpirationRequired()`, issuer+audience. Tampered / wrong-secret / `alg=none` / garbage all 401. Tested: `TestSecurityJWTForgery`. |
| 3 | Info (verified safe) | Client/admin surface separation in `mode=all`. | **No issue** — admin-only routes sit behind `requireSession`; a client bearer (no cookie) gets 401. Tested: `TestSecurityClientCannotReachAdminJSON`. |
| 4 | Info (verified safe) | IDOR / ownership scoping (accounts, ledger, transfers, beneficiaries, disputes, guided-suggestion `from_account`). | **No issue** — every client read/write is `clientSubject`-scoped; cross-user ids return 404 (or 403 for an unowned debit/`from_account`). Tested across `integration_test.go`, `disputes_test.go`, `suggestion_test.go`. |
| 5 | Info (verified safe) | User enumeration on auth. | **No issue** — login returns a generic `invalid_credentials`; `change_password` raises `28P01` for both wrong-password and non-active/unknown user (identical 401). |
| 6 | Info (verified safe) | SQL injection on free-text (`dispute.reason`, beneficiary search, IBAN). | **No issue** — all DB access is parameterized via sqlc / PL/pgSQL functions; free text is stored/compared as a bound value, never concatenated. |

## Documented gaps (accepted / deferred — no fix this pass)

- **Rate limiting.** `POST /auth/login`, `/auth/refresh`, and `/me/password` are credential
  oracles with no throttle (brute-force / credential-stuffing). Tracked in
  [`06-client-api.md`](06-client-api.md) §6.4; the shared limiter middleware is the fix.
  `/me/password` already documents a `429` for when it lands.
- **Raw DB messages to clients.** `mapDBError` forwards the PL/pgSQL exception message
  (e.g. `insufficient available funds: have X, need Y`) on the P0001/`23514` paths. Only
  ever about the caller's own resource (cross-user is 404), but it is internal detail;
  consider a message allowlist if tightening.
- **Request body size.** Handlers don't wrap the body in `http.MaxBytesReader`; a huge JSON
  body is read unbounded. Low risk behind Cloudflare, worth adding a global cap.
- **Cookie flags in dev.** The portal session cookie is not `Secure` in `app.env=development`
  (intentional for local http). Production must run with a non-dev env so `Secure` is set.

## Re-run

```bash
export TEST_DATABASE_DSN='postgres://admin:admin@localhost:5432/bank0_test?sslmode=disable'
go test ./internal/api/ -run TestSecurity -count=1 -v
```
