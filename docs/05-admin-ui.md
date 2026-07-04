# bank0 — Operator Console (Admin UI/UX)

> The internal tool support/ops/finance staff use to run the bank. Server-rendered
> with **Go Templ + HTMX**, organized around roles, safety, and the ledger truths
> from [`03-...md`](03-ledger-lifecycle-idempotency.md). It runs on the portal
> surface behind session auth (§7); the client-facing app is a separate surface —
> its API is [`06-client-api.md`](06-client-api.md), its PWA is
> [`07-client-web-app.md`](07-client-web-app.md).

The console covers users, accounts, credit/withdraw, maker-checker approvals,
transfers with drill-down, statements, audit log, reconciliation, fuzzy search,
keyset pagination, auto-refreshing views, disputes triage, and an admin-only
**"Revoke app sessions"** action. Mutations fire `HX-Trigger: bank0:refresh` so
the main-panel lists self-refresh.

---

## 1. What a banking back office needs

A single shared login over flat tables doesn't suffice for a banking back office.
The console is built around these requirements:

| Requirement | Why | How |
|-----|-------------|--------------|
| Per-user accountability, least privilege | attribution + scoped authority | per-user sessions + 4 roles |
| Real session lifecycle | logout, expiry, no creds on the wire | cookie session, idle timeout |
| Drill-down to investigate | follow money from account to entry | account → statement → transfer detail |
| Visibility into the lifecycle | holds / pending / available are first-class | pending queue, holds panel, available vs ledger |
| No direct balance edit | money is never silent or untraceable | "credit/debit" = a ledger `deposit`/`withdraw` |
| Reconciliation surface | prove the books are right | dashboard `reconcile()` badge |
| Guardrails on big actions | one mis-click can't move real money | confirm modals + maker-checker |

---

## 2. Roles (least privilege)

Maps to `users.role`. Enforced **in each handler** (a `requireRole` check on the
gated action — there is no route→role middleware; the portal subrouter carries only
`requireSession` + `csrfGuard`) **and** mirrored in the UI (hide what you can't do).

| Role | Can | Cannot |
|------|-----|--------|
| `auditor` | read everything: accounts, ledgers, transfers, reconcile, audit log | change anything |
| `operator` | + create accounts, freeze/unfreeze, cancel *pending* transfers, post credits/withdrawals up to the maker-checker threshold | reverse posted transfers, post above-threshold moves directly (they route to Approvals), manage users |
| `admin` | + reverse posted transfers, approve maker-checker items (a different admin than the maker), manage all users | (nothing app-level; still audited) |
| `customer` | no console access | — |

> Every state-changing screen calls a DB function that already enforces its own
> invariants — the role check is defense-in-depth and UX, not the primary control.

---

## 3. Information architecture

```
┌ Top bar: bank0 · role badge · operator name · logout ─────────────┐
│ Left nav            │  Main panel (HTMX-swapped)                   │
│ • Dashboard         │                                              │
│ • Users             │                                              │
│ • Accounts          │   [ context-specific content ]               │
│ • Transfers         │                                              │
│ • Reconciliation    │   right rail: detail / actions               │
│ • Approvals (N)     │                                              │
│ • Disputes          │                                              │
│ • Audit log         │                                              │
│ • Settings          │                                              │
└─────────────────────┴──────────────────────────────────────────────┘
```

Each nav item loads into the main panel via `hx-get`; drill-downs open in the
right rail so the operator never loses their list context.

---

## 4. Screen by screen

### 4.1 Dashboard

The "is the bank healthy?" glance:

- **Reconciliation badge** — green if `reconcile()` returns 0 rows, red with the
  failing checks otherwise. This is the single most important widget; it proves
  I1–I3 hold right now.
- **Money-in-the-bank** — `−SUM(balance of system accounts)` = total customer
  money; `external_clearing` balance = net flows across the boundary.
- **Operational counters** — pending transfers, active holds (count + reserved
  total), failed/expired today, reversals today.
- **Pending-approvals** count (maker-checker queue) with a jump link.

```mermaid
graph TD
    D[Dashboard] --> R[Reconcile badge ✅/❌]
    D --> M[Customer money total]
    D --> Q[Pending: 12  Holds: €3,200  Failed today: 3]
    D --> A[Approvals waiting: 2 →]
```

### 4.2 Accounts (list → detail)

- **List**: search bar (IBAN / owner / username — the search-feature TODO),
  cursor-paginated, columns: owner, IBAN, status chip, **available** and
  **ledger balance** side by side, default-account star.
- **Detail (right rail)**:
  - Header: owner, IBAN, status, currency.
  - **Available vs Ledger balance** explained inline:
    `available €90.00 = ledger €100.00 − holds €10.00`. Demystifies the lifecycle.
  - **Statement**: `ledger_entries` newest-first with `balance_after` as a real
    running balance, each row linking to its transfer. Cursor pagination on
    `(posted_at, id)`.
  - **Active holds** list with expiry countdown.
  - **Actions** (role-gated, each a confirm modal):
    `Credit` (deposit), `Withdraw`, `Freeze/Unfreeze`, `Set default`,
    `Adjust transfer limit`, `Close`.

### 4.3 Transfers

- **Pending queue** (the operational heart): every `status='pending'` transfer
  with age and hold expiry, plus inline `Post` / `Cancel`. Double-click safe — the
  button sends an `Idempotency-Key` and disables on submit.
- **History**: cursor-paginated, filterable by status/kind/account/amount/date.
- **Transfer detail**: both ledger legs, the hold, the idempotency key, and — for
  reversals — a link to/from the original (`reverses_id`). A posted transfer shows
  a `Reverse` action (admin only, reason required, idempotency key auto-generated).

### 4.4 Approvals (maker-checker)

For high-risk actions (see §5), the maker submits and the action lands here as
*requested*; a different admin approves or rejects. The acting and approving user
are both recorded in `admin_actions` (`actor_user_id`, `approved_by`). An admin
cannot approve their own request.

### 4.4b Limit requests (customer maker-checker)

Customer-initiated transfer-limit changes (`POST /accounts/{id}/limit-requests`
on the client surface) land in a **Limit requests** queue — same
`admin_actions` shape (`action = 'limit_request'`), same rules: an admin
applies (`approve_limit_change` runs `update_transfer_limit`) or rejects, the
requester can never apply their own, and a raise is therefore never
self-service. JSON twins: `GET /admin/limit-requests`,
`POST /admin/limit-requests/{id}/approve|reject`.

### 4.5 Audit log

`admin_actions` joined to operators: who did what, to which target, when, with the
JSON detail and the approver. Filterable, read-only, exportable. Pairs with the
ledger to answer "who authorized this movement and why."

### 4.6 Reconciliation

Runs `reconcile()` on demand, lists any failing invariant with the offending
account/transfer and the drift amount. In a healthy system this is an empty,
green page — and that emptiness is the product.

### 4.7 Disputes

A **Disputes** nav screen renders the triage queue (newest first) and drives the
resolve state machine, backed by the same endpoints the JSON admin API exposes
([`06-client-api.md`](06-client-api.md) §1):

- **Queue** (`GET /console/disputes` → `/console/disputes/results`): each row shows
  raised-at, raiser, category, status, from/to IBANs, and amount. Backed by
  `ListDisputesAdmin` (cursor-paginated; the JSON `GET /admin/disputes?status=` adds
  the status filter).
- **Decide / Recall (JSON, Rec 12)**: `POST /admin/disputes/{id}/decide`
  (`reimbursed` / `partially_reimbursed` — a REAL clearing→victim `adjustment`
  transfer, capped + excess-adjusted by `bank_settings`, excess waived for
  vulnerable customers — or `declined`) and `POST /admin/disputes/{id}/recall`
  (simulated pacs.004: `requested` → `funds_returned` | `refused`). Both audit
  to `admin_actions` and notify the filer on the events feed.
- **Resolve** (`POST /console/disputes/{id}/resolve?status=` + optional note): inline
  per-row actions — *Reviewing* (open → under_review), *Resolve*, *Reject* — with an
  optional resolution note. Terminal rows show their final status, no actions. The
  state machine (terminal transitions → 409) lives in `resolve_dispute`; the resolver
  is the session operator, audited in `admin_actions`.

Resolving is gated to **operators/admins** (`canActOnMoney`); auditors see the queue
read-only (no action buttons, and a direct resolve POST → 403). Raising a dispute
emits an `admin_actions` `dispute_raised` row — the flag-only fraud-engine seam (no
auto-freeze).

> **Admin-JSON RBAC.** The JSON admin API enforces roles **per handler**, not just
> a valid session: money / account / dispute mutations require `canActOnMoney`,
> user creation requires `canManageUsers`; reads stay open to any staff. See
> [`10-security-review.md`](10-security-review.md).

---

## 5. Safety patterns (the UX that protects money)

1. **Confirm modals** for every money/destructive action, restating the concrete
   effect: *"Credit €250.00 to IBAN …7821 (Alice Smith). This posts a ledger
   entry from external_clearing. Reason: ___"*.
2. **Idempotency keys are automatic.** The UI generates a key per action attempt
   and sends it; a retried/double-clicked submit reuses the key → the DB replays
   the original result. The operator literally cannot create a duplicate movement.
3. **Optimistic disable**: action buttons disable on click (`hx-disabled-elt`),
   re-enable on response — kills the double-submit instinct even before the key
   does.
4. **Maker-checker threshold**: deposits/withdrawals strictly above a
   configurable amount (**€10,000** by default,
   `bank_settings.maker_checker_threshold_minor` — DB-resident, console-editable)
   require a second admin via the Approvals queue. Smaller ops stay one-click.
5. **No raw balance field anywhere.** "Credit/Debit" always means a ledger
   `deposit`/`withdraw`; there is no input that writes `balance_minor`. An
   "edit balance" field cannot exist by design.
6. **Reason required** on reverse, freeze, close, and any maker-checker action —
   stored in `admin_actions.detail` / `transfers.failure_reason`.
7. **Toasts + inline errors**: the DB error mapping (§5 of `03-...md`) surfaces as
   human messages ("Insufficient available funds: have €90.00, need €100.00").

---

## 6. HTMX interaction model

One handler feeds both the JSON API and HTML. The interaction patterns:

| Pattern | HTMX | Use |
|---------|------|-----|
| Drill-down | `hx-get` → right rail target | account/transfer detail |
| Live search | `hx-get` + `hx-trigger="input changed delay:300ms"` | account/transfer search |
| Safe action | `hx-post` + `hx-confirm` + `hx-disabled-elt="this"` + `Idempotency-Key` | credit, post, reverse |
| Auto-refresh | `hx-trigger="every 10s"` on the pending queue & reconcile badge | keep ops view live |
| Partial swap | `hx-target` + `hx-swap="outerHTML"` | update one row after an action, not the whole table |

Components (Templ): `Shell`, `Dashboard`, `AccountList`, `AccountDetail`,
`Statement`, `TransferQueue`, `TransferDetail`, `ApprovalQueue`, `AuditLog`,
`ReconcilePanel`, plus shared `ConfirmModal`, `StatusChip`, `Money` (formats
minor units → `€x.xx`).

---

## 7. Auth & session

Portal auth is **DB-backed sessions** (the `sessions` table and session functions
in [`00003_users.sql`](../db/migrations/00003_users.sql)), consistent with the
"logic in the DB" principle:

- **Login** (`GET/POST /login`, public) → `create_staff_session(...)` verifies
  `crypt(pw, password_hash)` **and** staff role **and** `status='active'` in one
  function. The cookie holds an opaque 256-bit token; the DB stores only its
  **SHA-256** (a DB leak never exposes a live token).
- **Cookie**: `bank0_session`, HttpOnly, SameSite=Strict, `Secure` in production.
- **Idle timeout 30 min**, slid forward in `validate_session(...)` on every request
  — so all portal replicas share one source of truth, no in-memory state.
- **Logout** (`POST /logout`) calls `revoke_session(...)` (deletes the row).
- **Role in session** (`operator`/`admin`/`auditor`; customers are rejected at
  login) is injected into request context for per-action gating.
- Expired sessions are swept by the advisory-locked maintenance loop
  (`cleanup_sessions()`).
- **Revoke app sessions** (user-detail rail, admin only): `revoke_user_refresh`
  force-revokes every refresh token of any user — the operator-side control that
  complements the customer's own "log out everywhere" ([`06-client-api.md`](06-client-api.md) §3.3).
- Every portal route (admin JSON API **and** console HTML) sits behind the
  `requireSession` middleware; browsers/HTMX get a redirect to `/login`,
  programmatic callers get `401`. `/health`, `/docs`, `/openapi.yaml`, `/login`
  stay public.

---

## 8. Settings & defaults

The console's safety thresholds are DB-resident in `bank_settings`
([`00006_maker_checker.sql`](../db/migrations/00006_maker_checker.sql)) and
console-editable:

- **Maker-checker threshold** (§5.4): **€10,000**
  (`bank_settings.maker_checker_threshold_minor = 1000000`). Money moves strictly
  above this route to the Approvals queue for a second approver; smaller ops stay
  one-click.
- **Idle session timeout** (§7): **30 min** (`admin.session_idle_timeout = 30m`).
- **Auto-post**: `POST /transfers` and the console "send" settle immediately. The
  pending queue still exists for deferred and maker-checker cases — above-threshold
  money moves call `request_transfer` to enqueue a pending deposit + an
  `approval_request`, which the Approvals screen lets a *different* admin Approve
  (posts) or Reject (cancels). `approve_request` enforces approver ≠ maker
  (`approved_by` recorded).

Search across users/accounts/transfers uses `pg_trgm`; list pagination uses a
composite `(timestamp, id)` keyset cursor — correct even when many rows share a
timestamp. Dashboard and Approvals auto-refresh every 15s.
