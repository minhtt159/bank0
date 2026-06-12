# bank0 — fraudbank clients: BFF plan & feature gaps

> **Status: plan.** The [fraudbank](fraudbank) suite (web, Android,
> iOS) was built against the client API as-is — auth (login/refresh/rotation),
> accounts, ledger, beneficiaries, and transfers all work with **zero backend
> changes**. The API is sufficient for v1 clients. A *full* client suite needs more;
> this doc is the backlog, plus the BFF decision that gates the web app's production
> deploy. Companion: fraudbank `docs/02-api-contract.md` (the client's view of the
> contract).

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

**Decision needed (P0):** §1.1, §1.2, or both. Recommendation: both — they don't
conflict, 1.2 is an afternoon, 1.1 is the real security upgrade.

---

## 2. Feature gaps

Found while building (and planning past) the fraudbank clients. *Where it belongs*:
**core** = the Go API + PL/pgSQL (DB-first, per [`01-overview.md`](01-overview.md));
**BFF** = Worker-side composition over existing core endpoints.

### P0 — blocks or distorts the v1 clients

| Gap | Why fraudbank needs it | Endpoint sketch | Belongs |
|-----|------------------------|-----------------|---------|
| CORS-or-BFF decision (§1) | web has no production deploy story without it; dev needs proxy workarounds | §1.1 / §1.2 | BFF (+ small core middleware) |
| Pagination envelope + composite cursor | ledger cursor is `posted_at` only — entries tied on the same timestamp at a page boundary are **skipped** (tie-skip bug); clients also have to infer end-of-data from a short page | `GET /accounts/{id}/ledger` → `{items: [...], next_cursor, has_more}`; cursor = opaque encoding of `(posted_at, id)`, keyset `WHERE (posted_at, id) < ($1, $2)` | core |
| Password change (logged-in customer) | a banking client without "change password" isn't shippable; today only staff can reset via portal | `POST /me/password {current_password, new_password}` → 204; verify current (bcrypt), revoke all refresh families except the caller's current one | core |

### P1 — needed for a full client suite

| Gap | Why fraudbank needs it | Endpoint sketch | Belongs |
|-----|------------------------|-----------------|---------|
| Self-registration / onboarding | `createUser` is admin-tagged (staff only); clients can't sign up — demo flows fake it | `POST /auth/register {username, password, full_name, email, phone}` → 201 + pending-KYC status; needs onboarding state on `users` (deferred in [`06-client-api.md`](06-client-api.md) §7) + abuse throttling | core |
| Profile edit (client) | `update_user_info` exists DB-side but is portal-only; clients can't fix a phone number/email | `PATCH /me {email?, phone_number?, full_name?}` → 200 `User`; reuse `update_user_info`, scope to `sub` | core |
| Customer account opening | customers with one account can't open a savings/second account; `createAccount` is admin-only and takes a staff-chosen IBAN/PIN | `POST /me/accounts {kind}` → 201; server allocates IBAN, sets default limits | core |
| Transfer-limit change request | the €500/transfer limit is account data; customers will ask to raise it — natural **maker-checker** candidate (operator approves, [`05-admin-ui.md`](05-admin-ui.md)) | `POST /accounts/{id}/limit-requests {transfer_limit_minor}` → 201 pending; surfaces in the portal approvals queue | core |
| Server-side ledger filters | statement search (date range, direction, free text) currently means client-side filtering over paged fetches — wrong for 70+ row accounts, hopeless for real ones | `GET /accounts/{id}/ledger?from=&to=&direction=&q=` on top of the keyset cursor | core |
| List my transfers (across accounts) | "recent activity" and receipt lookup need per-transfer history; today only `GET /transfers/{id}` by known id, or assembling from per-account ledgers | `GET /transfers?cursor=&limit=` → transfers where caller owns either side, newest first | core |

### P2 — later

| Gap | Why fraudbank needs it | Endpoint sketch | Belongs |
|-----|------------------------|-----------------|---------|
| Notifications / webhooks | incoming-payment and transfer-status push instead of poll-on-focus | `GET /me/events?cursor=` (poll) first; push (FCM/APNs token registry) later | core (+ BFF fan-out) |
| Scheduled / standing orders | recurring rent/savings transfers — table stakes for retail | `POST /scheduled-transfers {debit, credit, amount_minor, schedule, …}` + list/cancel; runner in the maintenance sweep | core |
| Statement export (PDF/CSV) | monthly statements; deferred in [`06-client-api.md`](06-client-api.md) §7 | `GET /accounts/{id}/statement?month=&format=csv\|pdf`; CSV from the ledger query is cheap, PDF rendering could live BFF-side | core (CSV), BFF (PDF) |
| Session/device listing + selective revoke | "you're signed in on 3 devices" — the refresh-token families (`00017`) already model this; only listing is missing | `GET /me/sessions` → families (created, last used, UA); `DELETE /me/sessions/{family_id}` | core |
| Step-up auth + TOTP MFA | high-value transfer confirmation; **designed already — implement [`06-client-api.md`](06-client-api.md) §6.1/6.2 as written**, don't redesign here | `/auth/mfa/enroll` / `confirm` / `verify`; 403 `step_up_required` + same `Idempotency-Key` retry | core |
| Cards | threatbank had card UI; bank0 has no card domain | — | out of scope long-term; revisit only with a card-processor integration story |

---

## 3. Aggregation endpoints (BFF-side)

Cold start of every fraudbank client is 3 sequential round trips: `GET /me` →
`GET /users/{id}/accounts` → `GET /accounts/{id}/ledger` (default account). On mobile
latency that's the visible "skeleton screen" second. A BFF composition endpoint:

- `GET /bff/dashboard` → `{user, accounts: [...], recent: {account_id, entries: [...first ledger page]}}`
  — the Worker fans out the three upstream calls in parallel with the caller's
  bearer and merges. No new core surface, no new authority (everything remains
  ownership-scoped upstream), pure latency optimization.
- Keep it **read-only composition**. Writes (transfers) stay 1:1 pass-through —
  idempotency and error semantics must not acquire a translation layer.

Worth doing only after §1.1 lands (the Worker is then already in the auth path);
native apps can call it too if it proves out, or keep their parallel fetches.
