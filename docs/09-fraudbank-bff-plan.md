# bank0 — fraudbank clients: BFF plan & feature gaps

> **Status: plan.** The fraudbank suite (web, Android,
> iOS) was built against the client API as-is — auth (login/refresh/rotation),
> accounts, ledger, beneficiaries, and transfers all work with **zero backend
> changes**. The API is sufficient for v1 clients. A *full* client suite needs more;
> this doc is the backlog, plus the BFF decision that gates the web app's production
> deploy. Companion: fraudbank `docs/02-api-contract.md` (the client's view of the
> contract).
>
> **Implementation-ready specs** live in [`docs/specs/`](specs/) (open backlog) and
> [`docs/archive/`](archive/) (shipped) — one file per feature, each with the OpenAPI
> operation, goose migration (DDL + PL/pgSQL), handler logic, tests, and a step-by-step
> order. See [`docs/specs/spec-p3-roadmap.md`](specs/spec-p3-roadmap.md) for the index.

---

## 0. Decisions locked (2026-06-13)

Locked with the bank0 lead after rebasing `feat/bff` onto `main`. These **override**
the P1/P2 gap tables below wherever they conflict.

1. **Build order — quick wins, then features.** Shipped first and archived under
   [`../archive/`](archive/): the `null`→`[]` fix (centralized in `writeJSON`), opt-in
   dev CORS (§1.2), `PATCH /me` + `POST /me/password`, guided transfer suggestion,
   disputes (client + admin + console), `GET /transfers` (list-my-transfers), session
   listing, and the composite-cursor ledger filters. Still open: customer account
   opening, transfer-limit requests, the token-holding Worker BFF (§1.1), `/me/dashboard`
   (§3), self-registration, step-up MFA. **Out of scope:** cards.

2. **List shape — bare arrays, no envelope.** Every list endpoint returns a bare
   JSON array and **always `[]`, never `null`** (the reported bug). Consistent with
   the admin/HTML surface and the disputes spec. Pagination stays keyset-cursor +
   `limit`; end-of-data = a short page (`len < limit`). **No
   `{items, next_cursor, has_more}` envelope.** The ledger **tie-skip** bug is still
   fixed, but via a **composite keyset cursor `(posted_at, id)`**
   (`WHERE (posted_at, id) < ($1,$2)`) reusing the pattern already shipped for the
   console (`AccountStatement`, `SearchTransfers`).

3. **Disputes fraud hook — flag only.** `raise_dispute` emits the `admin_actions`
   `dispute_raised` audit row (the fraud-engine seam) and nothing else. **No
   auto-freeze** — keep the opt-in freeze documented in the spec as a future toggle,
   but don't build it now.

4. **Web auth — both CORS + Worker BFF, but no `/bff` URL.** Do both §1.2 (CORS
   dev-QoL) and §1.1 (token-holding Worker, refresh-cookie). The composition endpoint
   is **`GET /me/dashboard`** (consistent with the `/me/*` namespace), **not**
   `/bff/dashboard`. "BFF" stays an architecture term; it never appears in a client URL.

---

## 1. BFF architecture proposal

### 1.1 Extend the Worker proxy into a token-holding BFF (web production path)

The seam already exists: `worker/index.ts` serves the PWA and proxies `/api/*`
same-origin ([`07-client-web-app.md`](07-client-web-app.md) §2, §9), and
[`06-client-api.md`](06-client-api.md) §6.3 anticipated exactly this upgrade.
fraudbank web is the second SPA with the same need — point a second Worker route (or
the same Worker with a second assets binding) at it.

Upgrade the proxy to hold tokens:

- **Login:** Worker forwards `POST /api/auth/login` upstream; on 200 it strips
  `refresh_token` from the JSON before returning it to the browser and sets it as
  `Set-Cookie: rt=…; HttpOnly; Secure; SameSite=Strict; Path=/api/auth`. The SPA
  keeps only the 15-min access token in memory.
- **Refresh:** `POST /api/auth/refresh` with an empty body — the Worker reads the
  cookie, calls upstream, re-sets the rotated cookie, returns only the new access
  token. Rotation + single-flight discipline is unchanged, just moved server-side.
  (Concurrent refreshes from multiple tabs are the same family-revocation footgun —
  the Worker should coalesce or tolerate one 401-and-retry.)
- **Logout:** Worker reads the cookie, calls upstream `/auth/logout`, clears the cookie.
- **Everything else:** proxied as today, `Authorization: Bearer` passed through
  (access-token injection from a Worker-held store is a later step; the cookie for
  the *refresh* token is the part that matters — it keeps the long-lived credential
  out of browser JS entirely).

Scope: **web only.** Native apps keep direct API access (JWT + refresh in
Keystore/Keychain) — a BFF adds nothing for them and an extra hop.

### 1.2 Alternative / additional: opt-in CORS on `mode=api` (smallest change, dev QoL)

Today fraudbank web needs the Vite proxy even for local dev because the API sends no
CORS headers. An **opt-in** middleware (config `server.cors_origins`, default empty =
current behavior) unblocks direct browser→`:8090` calls in dev without any proxy:

```
Access-Control-Allow-Origin: <matched origin>          # exact match from the list, no *
Access-Control-Allow-Methods: GET, POST, DELETE, OPTIONS
Access-Control-Allow-Headers: Authorization, Content-Type, Idempotency-Key
Access-Control-Max-Age: 600
Vary: Origin
```

Plus an `OPTIONS` preflight handler (204). `Idempotency-Key` must be in
`Allow-Headers` or `POST /transfers` fails preflight; no `Allow-Credentials` needed
(bearer header, no cookies). Production web should still ship same-origin via the
Worker (§1.1) — CORS here is a dev convenience, not the prod architecture.

**Decided (§0.4): both.** §1.2 opt-in CORS has shipped (`server.cors_origins`, exact-origin
allowlist, preflight handled, no credentials); §1.1 token-holding Worker BFF is still open.

---

## 2. Remaining feature gaps

Found while building (and planning past) the fraudbank clients. *Where it belongs*:
**core** = the Go API + PL/pgSQL (DB-first, per [`01-overview.md`](01-overview.md));
**BFF** = Worker-side composition over existing core endpoints. The shipped gaps (the
`null`→`[]` fix, opt-in CORS, `POST /me/password`, `PATCH /me`, guided suggestion, the
disputes domain, list-my-transfers, the composite-cursor ledger filters, session listing,
and the admin-JSON RBAC hardening — see [`10-security-review.md`](10-security-review.md))
are archived under [`../archive/`](archive/); the rows below are what's left.

### P1 — needed for a full client suite

| Gap | Why fraudbank needs it | Endpoint sketch | Belongs |
|-----|------------------------|-----------------|---------|
| Self-registration / onboarding | `createUser` is admin-tagged (staff only); clients can't sign up — demo flows fake it | `POST /auth/register {username, password, full_name, email, phone}` → 201 + pending-KYC status; needs onboarding state on `users` (deferred in [`06-client-api.md`](06-client-api.md) §7) + abuse throttling | core |
| Customer account opening | customers with one account can't open a savings/second account; `createAccount` is admin-only and takes a staff-chosen IBAN/PIN | `POST /me/accounts {kind}` → 201; server allocates IBAN, sets default limits | core |
| Transfer-limit change request | the €500/transfer limit is account data; customers will ask to raise it — natural **maker-checker** candidate (operator approves, [`05-admin-ui.md`](05-admin-ui.md)) | `POST /accounts/{id}/limit-requests {transfer_limit_minor}` → 201 pending; surfaces in the portal approvals queue | core |

### P2 — later

| Gap | Why fraudbank needs it | Endpoint sketch | Belongs |
|-----|------------------------|-----------------|---------|
| Notifications / webhooks | incoming-payment and transfer-status push instead of poll-on-focus | `GET /me/events?cursor=` (poll) first; push (FCM/APNs token registry) later | core (+ BFF fan-out) |
| Scheduled / standing orders | recurring rent/savings transfers — table stakes for retail | `POST /scheduled-transfers {debit, credit, amount_minor, schedule, …}` + list/cancel; runner in the maintenance sweep | core |
| Statement export (PDF/CSV) | monthly statements; deferred in [`06-client-api.md`](06-client-api.md) §7 | `GET /accounts/{id}/statement?month=&format=csv\|pdf`; CSV from the ledger query is cheap, PDF rendering could live BFF-side | core (CSV), BFF (PDF) |
| Step-up auth + TOTP MFA | high-value transfer confirmation; **designed already — implement [`06-client-api.md`](06-client-api.md) §6.1/6.2 as written**, don't redesign here | `/auth/mfa/enroll` / `confirm` / `verify`; 403 `step_up_required` + same `Idempotency-Key` retry | core |
| Cards | threatbank had card UI; bank0 has no card domain | — | out of scope long-term; revisit only with a card-processor integration story |

---

## 3. Aggregation endpoint (Worker-side composition)

> **Path:** `GET /me/dashboard` (see §0.4 — no `/bff` URL segment).

Cold start of every fraudbank client is 3 sequential round trips: `GET /me` →
`GET /users/{id}/accounts` → `GET /accounts/{id}/ledger` (default account). On mobile
latency that's the visible "skeleton screen" second. A BFF composition endpoint:

- `GET /me/dashboard` → `{user, accounts: [...], recent: {account_id, entries: [...first ledger page]}}`
  — the Worker fans out the three upstream calls in parallel with the caller's
  bearer and merges. No new core surface, no new authority (everything remains
  ownership-scoped upstream), pure latency optimization.
- Keep it **read-only composition**. Writes (transfers) stay 1:1 pass-through —
  idempotency and error semantics must not acquire a translation layer.

Worth doing only after §1.1 lands (the Worker is then already in the auth path);
native apps can call it too if it proves out, or keep their parallel fetches.
