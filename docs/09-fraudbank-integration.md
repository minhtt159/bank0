# bank0 — fraudbank client integration

> How the fraudbank clients (web, Android, iOS) integrate with bank0's client API.
> They run against the API as-is — auth (login / refresh / rotation), accounts,
> ledger, beneficiaries, transfers, disputes, and the guided-transfer suggestion
> all work with no bank0-specific backend changes. This doc describes the
> integration surface; the forward-looking backlog (self-registration, account
> opening, notifications, step-up MFA, banking-grade hardening) lives in
> [`docs/specs/`](specs/) — see [`specs/spec-p3-roadmap.md`](specs/spec-p3-roadmap.md).
> Companion on the client side: fraudbank `docs/02-api-contract.md`.

---

## 1. Auth & token handling

fraudbank web and the native apps authenticate against the client API exactly as
the bank0 PWA does ([`06-client-api.md`](06-client-api.md) §2–3):

- `POST /auth/login` → short (15m) HS256 access token **+ refresh token**.
- `POST /auth/refresh` rotates the pair, with reuse detection: a replayed refresh
  token revokes the whole family.
- `POST /auth/logout` revokes one session; `POST /auth/logout-all` revokes all.

**Web holds tokens server-side via the Worker BFF.** The same-origin seam already
exists: `worker/index.ts` serves the SPA and proxies `/api/*`
([`07-client-web-app.md`](07-client-web-app.md) §2). fraudbank web points a Worker
route (or a second assets binding on the same Worker) at its SPA and the proxy
holds the refresh token:

- **Login:** the Worker forwards `POST /api/auth/login` upstream; on 200 it strips
  `refresh_token` from the JSON before returning it to the browser and sets it as
  `Set-Cookie: rt=…; HttpOnly; Secure; SameSite=Strict; Path=/api/auth`. The SPA
  keeps only the 15-min access token in memory.
- **Refresh:** `POST /api/auth/refresh` with an empty body — the Worker reads the
  cookie, calls upstream, re-sets the rotated cookie, and returns only the new
  access token. Rotation + single-flight discipline is unchanged, just moved
  server-side. (Concurrent refreshes from multiple tabs are the family-revocation
  footgun — the Worker coalesces or tolerates one 401-and-retry.)
- **Logout:** the Worker reads the cookie, calls upstream `/auth/logout`, and
  clears the cookie.
- **Everything else:** proxied as today, `Authorization: Bearer` passed through.

"BFF" is an architecture term — it never appears in a client URL. The long-lived
refresh credential stays out of browser JS entirely.

**Native apps use direct API access** — JWT + refresh held in
Keystore/Keychain. A BFF adds nothing for them and only an extra hop, so they call
`api.bank0.hnimn.art` directly.

### 1.1 Local dev: opt-in CORS on `mode=api`

For local development without the Vite proxy, an opt-in CORS middleware (config
`server.cors_origins`, default empty = no CORS) unblocks direct
browser → `:8090` calls:

```
Access-Control-Allow-Origin: <matched origin>          # exact match from the list, no *
Access-Control-Allow-Methods: GET, POST, DELETE, OPTIONS
Access-Control-Allow-Headers: Authorization, Content-Type, Idempotency-Key
Access-Control-Max-Age: 600
Vary: Origin
```

`OPTIONS` preflight returns 204. `Idempotency-Key` is in `Allow-Headers` (else
`POST /transfers` fails preflight); no `Allow-Credentials` (bearer header, no
cookies). This is a dev convenience — production web ships same-origin via the
Worker.

---

## 2. List shape & pagination

Every list endpoint returns a **bare JSON array** and always `[]`, never `null` —
consistent across the client API, the admin/HTML surface, and disputes. There is
**no `{items, next_cursor, has_more}` envelope.** Pagination is a keyset cursor +
`limit`; end-of-data is a short page (`len < limit`). The ledger uses a
**composite keyset cursor `(posted_at, id)`** (`WHERE (posted_at, id) < ($1,$2)`),
the same pattern as the console (`AccountStatement`, `SearchTransfers`), so rows
sharing a timestamp are never skipped.

---

## 3. Guided-transfer "mule menu"

`GET /transfers/suggestion?from_account&amount_minor` powers the guided-transfer
demo flow. It returns `{"options":[…]}` with up to 3 third-party candidates drawn
at random from the active `guided_scenarios` short-list (`source=scenario`), or
`{"options":[]}` when none configured — in which case the client picks one at
random or falls back to the caller's own account. It is read-only and never
exposes more than confirmation-of-payee (a masked owner name + IBAN). Full design:
[`specs/spec-banking-grade-hardening.md`](specs/spec-banking-grade-hardening.md) §5.

---

## 4. Disputes fraud hook — flag only

`raise_dispute` emits the `admin_actions` `dispute_raised` audit row — the
fraud-engine seam — and nothing else. There is **no auto-freeze**; an opt-in
freeze toggle is documented in the spec as a future option.

---

## 5. Dashboard composition (Worker-side)

The cold start of a fraudbank client is three sequential round trips: `GET /me` →
`GET /users/{id}/accounts` → `GET /accounts/{id}/ledger` (default account). On
mobile latency that is the visible "skeleton screen" second. The Worker can
compose them into one call:

- `GET /me/dashboard` → `{user, accounts: [...], recent: {account_id, entries: [...first ledger page]}}`
  — the Worker fans the three upstream calls out in parallel with the caller's
  bearer and merges. No new core surface, no new authority (everything stays
  ownership-scoped upstream), pure latency optimization.
- It is **read-only composition**. Writes (transfers) stay 1:1 pass-through —
  idempotency and error semantics never acquire a translation layer.

The path is `GET /me/dashboard` — consistent with the `/me/*` namespace, with no
`/bff` URL segment. Native apps can call it too, or keep their parallel fetches.
