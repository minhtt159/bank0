# bank0 — fraudbank clients: BFF plan & feature gaps

> **Status: plan.** The [fraudbank](../../../WORK-TF/fraudbank) suite (web, Android,
> iOS) was built against the client API as-is — auth (login/refresh/rotation),
> accounts, ledger, beneficiaries, and transfers all work with **zero backend
> changes**. The API is sufficient for v1 clients. A *full* client suite needs more;
> this doc is the backlog, plus the BFF decision that gates the web app's production
> deploy. Companion: fraudbank `docs/02-api-contract.md` (the client's view of the
> contract).
>
> **Implementation-ready specs** for the items below now live in
> [`docs/specs/`](specs/) — one file per feature, each with the OpenAPI operation,
> goose migration (DDL + PL/pgSQL), handler logic, tests, and a step-by-step order.
> See [`docs/specs/spec-p3-roadmap.md`](specs/spec-p3-roadmap.md) for the index and
> the larger product-domain roadmap. Highest priority (fraudbank fraud demo):
> [`spec-guided-transfer-suggestion.md`](specs/spec-guided-transfer-suggestion.md)
> and [`spec-disputes.md`](specs/spec-disputes.md).

---

## 0. Decisions locked (2026-06-13)

Refinement with the bank0 lead after rebasing `feat/bff` onto `main`. These
**override** the P0/P1 tables and the envelope proposal in
[`spec-ledger-pagination-and-filters.md`](specs/spec-ledger-pagination-and-filters.md)
wherever they conflict.

1. **Build order — quick wins, then features.**
   - **Wave 0 (quick wins):** the `null`→`[]` fix on every bare-array handler (XS,
     non-breaking) + the opt-in CORS dev-QoL middleware (§1.2). ✅ **landed** —
     `null`→`[]` centralized in `writeJSON` (reflect-coerces a nil top-level slice,
     so disputes/list-my-transfers inherit it); CORS via opt-in `server.cors_origins`
     wrapped outside the mux so preflight `OPTIONS` is answered before routing.
   - **Pulled forward (landed early):** profile edit (`PATCH /me`) + change password
     (`POST /me/password`) shipped as one client self-service PR ahead of Wave 1.
     Change-password took the next free migration number **`00018_change_password.sql`**,
     so the numbers below shift up by one.
   - **Wave 1:** guided transfer suggestion
     ([`spec-guided-transfer-suggestion.md`](specs/spec-guided-transfer-suggestion.md))
     — highest priority. Migration **`00019_guided_scenarios.sql`**. ✅ **landed** —
     `GET /transfers/suggestion` (registered before `/transfers/{id}` so `all` mode
     doesn't shadow it), `guided_scenarios` table + `suggest_transfer_destination()`,
     `bank.go` pgx wrapper, masked owner name, 403 on foreign `from_account`, 204 on none.
   - **Wave 2:** disputes ([`spec-disputes.md`](specs/spec-disputes.md)), flag-only.
     Migration **`00020_disputes.sql`**. ✅ **landed** — client `POST /transfers/{id}/dispute`,
     `GET /disputes`, `GET /disputes/{id}` (raiser-scoped) + admin `GET /admin/disputes`,
     `POST /admin/disputes/{id}/resolve` (state machine; illegal → 409 via plain `P0001`
     "cannot…", not `check_violation`, so it maps to `invalid_state`). Fraud hook =
     `admin_actions` `dispute_raised` row only (no auto-freeze). Operator **console
     screen** (`/console/disputes`) renders the queue + resolve actions (gated to
     operators/admins; auditors read-only) — see [`05-admin-ui.md`](05-admin-ui.md) §4.7.
   - **Ledger composite-cursor tie-skip fix + server-side filters** ✅ **landed**
     (2026-06-14): `GET /accounts/{id}/ledger` now takes `cursor`+`cursor_id`
     (composite `(posted_at, id)` keyset) and `from`/`to`/`direction`/`q`/`min_minor`/
     `max_minor`; still a bare array. No migration.
   - **Wave 3 (P1 suite, remaining):** customer account opening, transfer-limit
     request (maker-checker), list-my-transfers, the token-holding Worker auth
     upgrade (§1.1), and the `/me/dashboard` aggregation (§3).
   - **Deferred:** self-registration, step-up/TOTP MFA. **Out of scope:** cards.
     **P2/later:** notifications, scheduled transfers, statement export, session listing.

2. **List shape — bare arrays, no envelope.** Every list endpoint returns a bare
   JSON array and **always `[]`, never `null`** (the reported bug). Consistent with
   the admin/HTML surface and the disputes spec. Pagination stays keyset-cursor +
   `limit`; end-of-data = a short page (`len < limit`). **No
   `{items, next_cursor, has_more}` envelope.** The ledger **tie-skip** bug is still
   fixed, but via a **composite keyset cursor `(posted_at, id)`**
   (`WHERE (posted_at, id) < ($1,$2)`) reusing the pattern already shipped for the
   console (`AccountStatement`, `SearchTransfers`) — scheduled with the server-side
   ledger filters in Wave 3, since they touch the same query.

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

**Decided (§0.4): both.** ✅ §1.2 opt-in CORS landed (`server.cors_origins`, exact-origin
allowlist, preflight handled, no credentials). ⏳ §1.1 token-holding Worker BFF still
pending (Wave 3).

---

## 2. Feature gaps

Found while building (and planning past) the fraudbank clients. *Where it belongs*:
**core** = the Go API + PL/pgSQL (DB-first, per [`01-overview.md`](01-overview.md));
**BFF** = Worker-side composition over existing core endpoints.

> **Status (2026-06-13).** The rows below are the original gap analysis; current
> wave/shipped status lives in §0. ✅ **Shipped on `feat/bff`:** the `null`→`[]` fix,
> opt-in CORS (§1.2), `POST /me/password`, `PATCH /me`, `GET /transfers/suggestion`
> (guided), the disputes domain (client + admin), and the **admin-JSON RBAC** hardening
> ([`10-security-review.md`](10-security-review.md)), and the **ledger composite-cursor
> tie-skip fix + server-side filters**. **Open:** token-holding Worker BFF (§1.1),
> customer account opening, transfer-limit requests, device/session
> list. **Deferred:** self-registration, step-up MFA.

### P0 — blocks or distorts the v1 clients

| Gap | Why fraudbank needs it | Endpoint sketch | Belongs |
|-----|------------------------|-----------------|---------|
| CORS-or-BFF decision (§1) | web has no production deploy story without it; dev needs proxy workarounds | §1.1 / §1.2 | BFF (+ small core middleware) |
| Pagination envelope + composite cursor | ledger cursor is `posted_at` only — entries tied on the same timestamp at a page boundary are **skipped** (tie-skip bug); clients also have to infer end-of-data from a short page | `GET /accounts/{id}/ledger` → `{items: [...], next_cursor, has_more}`; cursor = opaque encoding of `(posted_at, id)`, keyset `WHERE (posted_at, id) < ($1, $2)` | core |
| Empty ledger page returns `null`, not `[]` | `GET /accounts/{id}/ledger` returns HTTP 200 with body **`null`** when a cursor is exhausted or an account has zero entries. A typed client (iOS `[LedgerEntry]`, Android `List<LedgerEntry>`) throws a decode error on `null` — fraudbank had to add per-client null-tolerance as a workaround (item 7, 2026-06-13). A JSON array endpoint should always return `[]`. Fix: marshal the empty slice as `[]` (e.g. initialize the slice non-nil before `json.Marshal`). Subsumed by the envelope above if adopted. | core |
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
