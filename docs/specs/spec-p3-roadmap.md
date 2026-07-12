# bank0 — P3 product-domain roadmap & architecture

> **Status: architecture / planning.** Not line-level specs. For each big "P3"
> domain this is a credible plan for building it on bank0 *as it stands*: rationale,
> core data-model changes (reusing bank0 conventions — UUIDv7 ids, BIGINT minor
> units, append-only ledger, PL/pgSQL business logic, sqlc + oapi-codegen, DB-first
> error mapping), the main endpoints, the hardest problem, an S/M/L/XL effort
> estimate, and recommended sequencing.
>
> **This doc is the index for `docs/specs/`.** Only the **open backlog** lives here as
> line-level specs. Once a feature ships, its as-built behaviour becomes the source of
> truth — [`../06-client-api.md`](../06-client-api.md) / [`../05-admin-ui.md`](../05-admin-ui.md)
> for the surfaces and `db/migrations/` for the schema + PL/pgSQL — and the planning spec
> is retired rather than kept as a separate file.
>
> **Open backlog** (`docs/specs/`):
>
> | Spec | Covers | Relates to P3 § |
> |------|--------|-----------------|
> | [`spec-banking-grade-hardening.md`](spec-banking-grade-hardening.md) | banking-grade roadmap (server-side CoP/VOP, SCA, RFC 9457, fraud-UX backend enablers, AML gate) + guided-transfer v2 (3 options → pick 1, own-account fallback) | cross-cutting; consolidates the fraud + payment surfaces |
>
> **Already shipped** items are no longer re-listed here — the as-built truth lives in
> the reference docs ([`../06-client-api.md`](../06-client-api.md) /
> [`../05-admin-ui.md`](../05-admin-ui.md)) and `db/migrations/`. For the fraud/payment
> hardening roadmap specifically, the shipped-vs-open status (Waves 0–2, the Wave-3
> subset, per-owner idempotency, guided-transfer v2, and what remains) is tracked at the
> top of [`spec-banking-grade-hardening.md`](spec-banking-grade-hardening.md).
>
> The gap backlog and BFF decision are in
> [`../09-fraudbank-integration.md`](../09-fraudbank-integration.md); the auth/MFA design in
> [`../06-client-api.md`](../06-client-api.md) §6; the ledger invariants in
> [`../03-ledger-lifecycle-idempotency.md`](../03-ledger-lifecycle-idempotency.md).

---

## 0. The invariants every domain must respect

bank0's correctness lives in the database. Any P3 domain that moves money plugs into
the **existing ledger** — it does not invent a parallel one. Non-negotiable:

| Invariant | Where enforced | Consequence for new domains |
|-----------|----------------|-----------------------------|
| Money moves only by inserting balanced `ledger_entries` | `ledger_apply_to_balance` trigger; `reconcile()` I1–I3 | New money flows call `transfer()`/`request_transfer()`, never write balances |
| Ledger is append-only | `ledger_block_mutation` trigger | Corrections = `reverse_transfer`, never UPDATE/DELETE |
| `balance_minor` is a cache only the ledger trigger writes | `account_guard_balance` trigger | No new code may UPDATE balances |
| Replay safety on money moves | `idempotency_keys` + `Idempotency-Key` header | Every new money endpoint takes the header and gates with the same pattern |
| Customer balances ≥ 0; only system (GL) accounts go negative | accounts CHECK | New "GL"-like accounts (FX, fees, card settlement) are `kind='system'` |
| Currency is single (EUR) | CHECK on accounts/transfers/entries | Multi-currency (§6) is the one domain that rewrites this |
| Ownership scoping on the client surface | `clientSubject` / `ownsAccount` | Every new client endpoint scopes to the JWT subject |
| Background work is advisory-locked & idempotent | `RunMaintenance` + `pg_try_advisory_xact_lock` | New workers reuse the maintenance ticker, not a new scheduler |

**Heuristic used throughout:** *extend an existing domain* (a column, an `account_kind`,
an `admin_actions.action`, a maintenance step) before *adding a new domain* (a new
table family + handler file + worker). Flagged per section.

Snapshot of the effort scale: **S** ≈ days (1 migration + a handler + tests);
**M** ≈ 1–2 weeks (new table family, several endpoints, a worker step); **L** ≈
multiple weeks (new domain, cross-cutting changes, careful migration); **XL** ≈
months and/or a third-party integration / regulatory surface.

---

## 1. Self-registration / onboarding / KYC

**Today:** `createUser` is `admin`-only; there is no public signup, no onboarding
state, no contact verification. (`users.status` gates login; nothing models "partway
onboarded".)

**Rationale.** The **v1 is SHIPPED** (as-built in
[`../06-client-api.md`](../06-client-api.md) §1 + `00003_users.sql`): public
`POST /auth/register`, `onboarding_status` enum on `users`,
`verification_challenges` table (hash-at-rest codes), email/phone verification,
DB cooldown + Go IP rate-limit. This section is the **KYC continuation** beyond
that v1.

**Extends existing domain** (users lifecycle) — *not* a new domain. KYC document
capture is the one piece that is genuinely new and best **outsourced**.

### Core data-model changes (beyond the v1 spec)

| Change | Shape |
|--------|-------|
| `kyc_status` on `users` | enum `none / pending / in_review / approved / rejected`; advances independent of `onboarding_status` |
| `kyc_verifications` table | `id uuidv7, user_id, provider TEXT, provider_ref TEXT, level SMALLINT, status, decided_at, detail JSONB` — one row per KYC attempt; **no raw documents stored** (provider holds them) |
| `account_tier` / limits by tier | a `tier SMALLINT` on `users` or `accounts`; tiered `transfer_limit_minor` defaults; unverified users get a low cap |
| Onboarding events | reuse the SHIPPED `/me/events` feed (as-built: [`../06-client-api.md`](../06-client-api.md) §1) for "KYC approved" |

### Endpoints (sketch)

| Method | Path | Tag | Note |
|--------|------|-----|------|
| POST | `/me/kyc` | client | start a KYC session → returns a provider redirect/SDK token |
| POST | `/webhooks/kyc/{provider}` | (public, signed) | provider callback → set `kyc_status`, raise tier |
| GET | `/me/kyc` | client | current KYC status/tier |
| POST | `/admin/users/{id}/kyc` | admin | manual override (approve/reject) — audited via `admin_actions` |

### Hardest problem / risk

**Identity verification is not a thing you build — it is a vendor + a regulated
process** (document OCR, liveness, sanctions/PEP screening, audit retention).
bank0 should integrate a provider (Onfido/Sumsub/Stripe Identity-class) via a signed
webhook and store only a reference + decision, never the documents. Risks: webhook
authenticity (verify signatures, idempotent application), PII retention vs. the
immutable ledger ([`../06-client-api.md`](../06-client-api.md) §6.4 — erase PII in
`users`, keep pseudonymous ledger rows), and **tier→limit enforcement** living in
PL/pgSQL so the cap can't be bypassed at the API.

### Effort

- v1 self-registration + verification: **M** (already specced).
- KYC provider integration + tiers: **L** (vendor integration, webhooks, compliance).

### Sequencing

Ship the v1 spec now (it unblocks all three fraudbank clients). KYC follows only when
there's a real provider and a compliance owner — until then, staff `approve` via the
admin override endpoint is the bridge.

---

## 2. Pots / Spaces / savings sub-accounts

**Today:** every account is `kind='customer'` with an IBAN; `is_default` picks one.
Transfers move between any two active same-currency accounts.

**Rationale.** "Pots" (Monzo) / "Spaces" (N26) are sub-balances a user moves money
into for goals. The clean realization on bank0: **a pot is just another account**,
owned by the same user, with a new `kind` — and "move to pot" is an internal
`transfer()` between two of the user's own accounts. The ledger, holds, reconcile, and
statement machinery all work **unchanged**. This is the highest-leverage P3 because it
reuses everything.

**Extends existing domain** (accounts) — mostly. The only real change is relaxing the
"customer accounts must have an IBAN" CHECK so pots can be IBAN-less.

### Core data-model changes

| Change | Shape |
|--------|-------|
| `account_kind` += `'pot'` | `ALTER TYPE account_kind ADD VALUE 'pot'` |
| Relax the accounts CHECK | pots are owned (`user_id` set) but may have **no IBAN** (not externally addressable) and **no PIN**; keep `balance ≥ 0`. Rework the big `CHECK` into: system ⇒ code+no-owner+no-iban; customer ⇒ owner+iban; pot ⇒ owner + **no iban** |
| `parent_account_id` on accounts | optional self-FK: a pot belongs to a main account (for grouping + auto-close); `ON DELETE RESTRICT` |
| `goal_minor`, `label` on accounts (or pots) | savings target + display name; cache-only, no ledger impact |
| `transfer_kind` += `'pot_transfer'` (optional) | so statements label internal moves distinctly |

`create_pot(p_user_id, p_parent_account_id, p_label, p_goal_minor)` mirrors
`create_account` but inserts `kind='pot'`, no IBAN/PIN, never default.

### Endpoints (sketch)

| Method | Path | Tag | Note |
|--------|------|-----|------|
| POST | `/me/pots` | client | create a pot (server: no IBAN, balance 0) |
| GET | `/me/pots` | client | caller's pots (+ goal progress) |
| POST | `/me/pots/{id}/transfer` | client | move money main↔pot via `transfer()`; `Idempotency-Key` |
| PATCH | `/me/pots/{id}` | client | rename / change goal |
| DELETE | `/me/pots/{id}` | client | sweep remaining balance to parent, then close |

`/me/pots/{id}/transfer` is the only money mover — it calls the existing `transfer()`
between two owned accounts; ownership check is `ownsAccount` on **both** legs (a pot
move must stay within the caller's own accounts).

### Hardest problem / risk

**Pots must not leak into the external-payment surface.** A pot has no IBAN, so it can
never be a transfer destination from outside; the "no iban" CHECK guarantees it. The
subtle risk is the **available-balance and transfer-limit semantics**: does spending
from the main account see pot balances? (No — pots are separate accounts, so "ring
fenced" naturally.) And `transfer_limit_minor` on pot moves — internal moves between a
user's own accounts probably shouldn't count against the external limit, so
`request_transfer` may need a `kind='pot_transfer'` carve-out that skips the limit
check (it already skips checks for system debit accounts). Also: the **default-account
uniqueness index** and any "list accounts" UI must filter `kind='pot'` so pots don't
appear as payable accounts.

### Effort

**M.** One enum value, one CHECK rework (needs care — `ALTER TYPE ADD VALUE` can't run
in a txn with its use; split migration), a `create_pot` + a scoped transfer wrapper,
~5 client endpoints. No ledger changes.

### Sequencing

Customer account opening is **shipped** — **server-side IBAN allocation
(`allocate_iban`, real ISO SE IBANs) and `open_customer_account`** live in
`00004_accounts.sql`, and pots reuse them (minus the IBAN). Do pots **before**
multi-currency: pots in EUR are trivial; pots across currencies inherit §6's
hard problems.

---

## 3. Cards

**Today:** no card domain at all. [`../09-fraudbank-integration.md`](../09-fraudbank-integration.md)
marks cards **out of scope long-term, revisit only with a card-processor integration
story.** threatbank had card UI; bank0 has no card entity.

**Rationale.** Cards are the canonical retail feature, but they are also the one P3
domain that **cannot be built purely inside bank0** — a card needs a BIN sponsor and a
processor (Marqeta/Stripe Issuing/Adyen-class) to authorize at the network. What bank0
*can* own: the card **entity**, its controls (freeze/limits), PAN **tokenization**
(store only a token + last4, never the PAN), and the **card-transaction feed** as
ledger postings driven by processor webhooks.

**Entirely new domain.** New tables, new handler file, **and** an external integration.

### Core data-model changes

| Table | Shape |
|-------|-------|
| `cards` | `id uuidv7, account_id (FK), user_id, processor_card_id TEXT, last4 CHAR(4), brand, exp_month, exp_year, status (active/frozen/canceled/expired), kind (virtual/physical), created_at`. **No PAN, no CVV — ever.** |
| `card_controls` | per-card limits: `daily_limit_minor, per_tx_limit_minor, atm_enabled, online_enabled, contactless_enabled` |
| `card_auth_holds` | a network authorization places a **hold** on `account_id` (reuse the existing `holds` table — auths are exactly holds with a TTL); capture posts a transfer, reversal/expiry releases |
| `card_transactions` | feed enrichment over ledger entries: `transfer_id (FK), card_id, merchant_name, mcc, network_ref, auth/clearing status` — like `enriched_ledger` but card-aware |
| `processor_events` | raw signed webhook log (idempotent application, audit) |

Money path: **authorization → `request_transfer` (pending + hold)** on the card's
`account_id` to a `CARD_SETTLEMENT` system GL account; **clearing → `post_transfer`**;
**reversal/expiry → existing cancel/hold-expiry**. Zero new ledger mechanics — a card
auth *is* a hold, a capture *is* a posted transfer. This is the elegant part.

### Endpoints (sketch)

| Method | Path | Tag | Note |
|--------|------|-----|------|
| POST | `/me/cards` | client | issue a (virtual) card for an owned account → processor create |
| GET | `/me/cards` | client | caller's cards (last4, status) |
| POST | `/me/cards/{id}/freeze` · `/unfreeze` | client | toggles `status`; pushed to processor |
| PATCH | `/me/cards/{id}/controls` | client | limits/toggles |
| GET | `/me/cards/{id}/transactions` | client | card feed (cursor-paginated) |
| POST | `/webhooks/cards/{processor}` | (public, signed) | auth/clearing/reversal events → ledger |

### Hardest problem / risk

**The processor integration and PCI scope.** bank0 must never touch a PAN (keep PCI
scope minimal — store tokens + last4 only; card creation/display goes
processor-SDK-direct to the client). The genuinely hard engineering is the
**asynchronous auth→clearing lifecycle**: authorizations arrive as holds, may partially
or never clear, may reverse, may expire — and every one must map to bank0's
hold/transfer states **idempotently** under webhook retries and out-of-order delivery
(`processor_events` keyed by the processor's event id, applied once). Settlement
reconciliation against the processor's daily file is a second hard problem
(`reconcile()` would gain a card-settlement invariant). Regulatory: card issuing is a
licensed activity (the BIN sponsor's program).

### Effort

**XL.** New domain + a card-processor integration + PCI considerations + async
settlement reconciliation. The bank0-internal data model is **M**; the integration and
compliance are what make it XL.

### Sequencing

**Last, and only with a processor partner.** Do the bank0-internal scaffolding (cards,
controls, holds-as-auths against a *mock* processor) as an **M** spike to prove the
ledger mapping, but do not ship customer cards without the real integration. The
holds-as-auths design should be validated early because it's reusable and low-risk.

---

## 4. Spending insights / categorization

**Today:** the ledger has `description` and a counterparty (`enriched_ledger`), but no
category, no aggregation, no insights endpoint.

**Rationale.** "Where did my money go" is table-stakes retail. The realization: a
**categorization layer over existing ledger entries** (never mutating them — the ledger
is append-only) plus **aggregation queries** and one insights endpoint. Rules first
(deterministic, explainable, cheap); ML is optional and lives behind the same
category field.

**Extends existing domain** (a read/annotation layer over the ledger). The category is
**not** on `ledger_entries` (append-only) — it's a side table keyed by `transfer_id`.

### Core data-model changes

| Table | Shape |
|-------|-------|
| `categories` | taxonomy: `id SMALLINT PK, slug, label, parent_id, icon`. Seeded (groceries, transport, eating_out, income, transfers, …). Small fixed set, not user-defined for v1 |
| `transaction_categories` | `transfer_id (FK, PK), category_id, source (rule/ml/user), confidence, created_at`. One category per transfer; `source='user'` overrides auto |
| `categorization_rules` | `id, priority, match JSONB (mcc / counterparty / description regex / amount sign), category_id`. Evaluated in priority order |
| (optional) `merchant_aliases` | normalize raw descriptions → merchant + category |

Categorization runs **in the maintenance sweep** (a new step,
`categorize_uncategorized()`): pick up posted transfers lacking a
`transaction_categories` row, apply rules, insert. Idempotent (PK on `transfer_id`).
ML, if added, is an out-of-process scorer that writes the same table with
`source='ml'`; the rules path stays the deterministic fallback.

### Endpoints (sketch)

| Method | Path | Tag | Note |
|--------|------|-----|------|
| GET | `/me/insights?from=&to=&group_by=category\|month` | client | aggregated spend; `SUM(signed_amount)` grouped, scoped to owned accounts |
| GET | `/accounts/{id}/ledger` (extend) | client | include `category` per entry |
| PATCH | `/transfers/{id}/category` | client | user override (`source='user'`) |
| GET | `/categories` | client | the taxonomy (for filter UI) |

Insights is a pure read: `SUM`/`GROUP BY` over `ledger_entries` joined to
`transaction_categories`, filtered to the caller's accounts and the date range. Keyset
pagination not needed — it's an aggregate.

### Hardest problem / risk

**Categorization quality without rich merchant data.** bank0's transfers are
bank-to-bank with a free-text `description` and a counterparty name — there's no MCC
(that only exists once Cards, §3, lands). So rules are weak until card data arrives;
v1 insights are mostly "transfers in / out / by counterparty", which is honest but not
the glossy Monzo pie chart. The second risk is **performance**: insights aggregates can
scan large ledger ranges — needs an index on `(account_id, posted_at)` (exists) and
possibly a **materialized monthly rollup** table refreshed in the sweep for accounts
with long histories. Don't prematurely build the rollup; add it when a real account is
slow.

### Effort

**M** for rules + aggregation + insights endpoint. ML is a separate **L** (out-of-process
model, training data, serving) and explicitly optional — the field/endpoint shape is
identical, so ML can be added later without API change.

### Sequencing

Do the **categories taxonomy + rules + insights endpoint** early (it's cheap and
demos well). Hold ML until there's volume and a reason. Categorization gets
dramatically better **after Cards** (MCC), so sequence rich insights after §3 if cards
ever happen; ship the transfer-based version now.

---

## 5. Scheduled / recurring payments / standing orders

**Today:** transfers are immediate (`POST /transfers` auto-posts). No scheduling.
But bank0 **already has the worker**: the advisory-locked maintenance ticker
(`RunMaintenance` → `expire_holds` etc., `cmd/app/main.go` `runMaintenanceLoop`).

**Rationale.** Recurring rent/savings is retail table-stakes. The realization is almost
free: a `schedules` table + **one new step in the existing maintenance sweep** that
finds due schedules and creates transfers with a **deterministic idempotency key per
occurrence** — so the ticker can run on every replica, re-run after a crash, and never
double-pay.

**Extends existing domain** (transfers + the maintenance worker). No new scheduler, no
new process — this is the cleanest fit of any P3 item to bank0's architecture.

### Core data-model changes

| Table | Shape |
|-------|-------|
| `payment_schedules` | `id uuidv7, owner_user_id, debit_account_id, credit_account_id, amount_minor BIGINT, currency, description, cadence (enum: once/daily/weekly/monthly), interval_n SMALLINT, next_run_at TIMESTAMPTZ, end_at TIMESTAMPTZ NULL, max_occurrences NULL, occurrences_done INT, status (active/paused/completed/canceled), created_at` |
| `schedule_runs` | `id, schedule_id, scheduled_for, transfer_id NULL, status (posted/failed/skipped), ran_at, detail`. One row per occurrence — the audit + the dedupe anchor |

`run_due_schedules(p_now, p_batch)` (new PL/pgSQL, called from `RunMaintenance`):

```
FOR each active schedule WHERE next_run_at <= now() (FOR UPDATE SKIP LOCKED, batched):
    occ_key := 'sched:' || schedule_id || ':' || scheduled_for   -- deterministic
    -- transfer() is idempotent on occ_key: a re-run after a crash replays, never doubles
    SELECT transfer(occ_key, debit, credit, amount_minor, description, 'transfer')
    INSERT schedule_runs(...)               -- record outcome (posted / insufficient_funds)
    next_run_at := advance(next_run_at, cadence, interval_n)
    mark completed if end_at passed / max_occurrences hit
```

Insufficient funds on a due date → record a `failed` run + (future) notify; **do not**
retry-spam — advance to the next occurrence (or a small bounded retry window). The
advisory lock in `RunMaintenance` already guarantees one replica runs the batch per
tick; `SKIP LOCKED` lets a future sharded worker parallelize safely.

### Endpoints (sketch)

| Method | Path | Tag | Note |
|--------|------|-----|------|
| POST | `/me/schedules` | client | create; ownership on `debit_account_id`; validate cadence |
| GET | `/me/schedules` | client | caller's schedules + `next_run_at` |
| GET | `/me/schedules/{id}/runs` | client | occurrence history |
| PATCH | `/me/schedules/{id}` | client | pause/resume/amend amount or cadence |
| DELETE | `/me/schedules/{id}` | client | cancel future runs |

### Hardest problem / risk

**Exactly-once execution across crashes, replicas, and clock skew.** Solved by the
**deterministic per-occurrence idempotency key** (`sched:{id}:{scheduled_for}`) feeding
the already-idempotent `transfer()` — a re-run replays the same key and never
double-pays. The remaining judgment calls: **failed occurrences** (insufficient funds —
skip vs. bounded retry; product decision, default skip + notify), **DST / month-end**
semantics for monthly cadences (Jan 31 → Feb 28?), and **timezone** of `next_run_at`
(store UTC, compute "due" in the user's tz only at creation). Catch-up after long
downtime: cap how many missed occurrences fire at once (or fire only the latest) to
avoid a thundering herd of back-dated transfers.

### Effort

**M.** One table pair, one PL/pgSQL batch fn, one extra `RunMaintenance` step + tick
log line, ~5 client endpoints. The worker already exists — that's the expensive part
bank0 doesn't have to build.

### Sequencing

**First among the "new feature" P3 items** — it's the best architecture fit, demos
strongly, and is low-risk because the ledger/idempotency/worker are all reused. Depends
on nothing but the existing transfer path. (Pairs naturally with the events feed in
the shipped `/me/events` feed for "your standing order
ran / failed".)

---

## 6. Multi-currency / FX

**Today:** `currency CHAR(3)` is `CHECK = 'EUR'` on `accounts`, `transfers`, and
`ledger_entries`; `request_transfer` rejects cross-currency (`currency mismatch`).
Every amount is EUR minor units.

**Rationale.** Multi-currency accounts + FX is the deepest P3 change because it touches
**the ledger's core assumption** that a transfer's two legs are the same currency and
net to zero. This is the one domain that **rewrites invariants**, so it's the riskiest.

**Entirely new domain in effect** — even though it edits existing tables, it changes
the meaning of the ledger and reconcile, and adds FX rate management + an FX GL.

### Core data-model changes

| Change | Shape |
|--------|-------|
| Drop the `currency = 'EUR'` CHECKs | replace with a `currencies` reference table (`code CHAR(3) PK, minor_units SMALLINT, active`) and FK; minor-unit scale becomes per-currency (JPY=0, EUR/USD=2, …) — **amounts are no longer uniformly /100** |
| Multi-currency accounts | a user holds **one account per currency** (simplest; keeps each account single-currency so the ledger stays per-currency-balanced). A "wallet" groups them |
| `fx_rates` table | `base CHAR(3), quote CHAR(3), rate NUMERIC(18,8), as_of TIMESTAMPTZ, source` — time-series; pick the latest at quote time |
| FX as a **four-leg** transfer | a cross-currency move is **two same-currency transfers** through an `FX_TRADING` system GL pair: debit customer EUR → credit FX GL EUR; debit FX GL USD → credit customer USD. Each transfer still nets to zero *within its currency*; the FX GL absorbs the rate spread (the bank's FX P&L) |
| `transfer.fx_rate`, `fx_quote_id` | record the rate used + a quote reference for audit |
| `reconcile()` becomes **per-currency** | I1–I3 (`SUM(signed_amount)=0`) must hold **per currency**, not globally — a cross-currency global sum is meaningless. Add a currency dimension to every invariant |

The pivotal design choice: **never put two currencies in one transfer's legs.** Keep
every `transfers` row and every balanced leg-pair single-currency; model FX as a pair
of single-currency transfers linked by an `fx_quote_id`, with the rate spread landing
in a system FX account. This preserves the append-only, zero-sum ledger *per currency*
and means the trigger/holds/reverse machinery is unchanged — only `reconcile` gains a
`GROUP BY currency`.

### Endpoints (sketch)

| Method | Path | Tag | Note |
|--------|------|-----|------|
| GET | `/currencies` | client | supported currencies + scale |
| POST | `/me/accounts {currency}` | client | open an account in a currency (extends the open-account spec) |
| GET | `/fx/quote?from=&to=&amount_minor=` | client | a short-lived, signed FX quote (rate + fees + expiry) |
| POST | `/fx/exchange` | client | execute a quote → the four-leg transfer; `Idempotency-Key` |

### Hardest problem / risk

**Rounding, scale, and the zero-sum invariant under FX.** Minor units stop being
uniformly /100 (per-currency scale), so every amount path must consult
`currencies.minor_units` — a pervasive change. FX rounding must be **deterministic and
conservative** (round so the bank never loses fractions; the spread covers it) and the
rounding residue must land in a GL account so `reconcile` stays exact *per currency*.
Rate **staleness/quote expiry** (a quote must be honored or rejected, never silently
re-priced) and rate-source trust are operational risks. And the migration itself is
delicate: relaxing a CHECK that the whole ledger has assumed since `00005_transfers.sql`.

### Effort

**XL.** It edits the ledger's core invariants, makes minor-unit scale per-currency
(touching every amount path), adds FX rate management + a GL + quote lifecycle, and
rewrites `reconcile`. High blast radius; do it deliberately or not at all.

### Sequencing

**Last of the "extend bank0" items, before/independent of Cards.** Do **everything
single-currency first** (pots, schedules, insights, P2P) — they're all cheaper and
unaffected. Only take on FX when there's a concrete product need; when you do, land the
`currencies` table + per-currency scale + per-currency `reconcile` as a foundation
*before* exposing any FX endpoint, and prove the four-leg model with system GL tests
against `reconcile` before any customer touches it.

---

## 7. P2P-by-handle, request-money, bill-splitting

**Today:** transfers are by `credit_account_id` (a UUID); beneficiaries
(the `beneficiaries` table in `00008_features.sql`) save payees by IBAN with confirmation-of-payee masking.
There are no handles/aliases and no "request money" direction.

**Rationale.** Consumer P2P (Venmo/Twint/Revolut) is: pay a **handle** (@alice) not an
account number; **request** money (the inverse of a transfer — a payable claim); and
**split a bill** (one request fanned to several people). bank0 already has the money
rail (`transfer()`); this adds an **addressing layer** (handles) and a **request/claim
object** (a new lightweight domain), with payment still flowing through the existing,
idempotent transfer path.

**Mixed:** handles **extend** the users/accounts domain (an alias table + resolver,
like beneficiaries). Payment-requests are a **small new domain** (a claims table) —
but they create money moves through the existing ledger, never a new one.

### Core data-model changes

| Table | Shape |
|-------|-------|
| `handles` | `handle CITEXT PK, user_id (FK), default_account_id (FK), created_at`. `@alice` → a default credit account. Resolver mirrors `resolve_account_by_iban` (masked name, confirmation-of-payee) |
| `payment_requests` | `id uuidv7, requester_user_id, requester_account_id, payer_user_id NULL, payer_handle TEXT NULL, amount_minor BIGINT, currency, description, status (pending/paid/declined/canceled/expired), expires_at, group_id NULL, created_at, settled_transfer_id NULL` |
| `payment_request_groups` | bill-split parent: `id, creator_user_id, total_minor, description`; children are `payment_requests` sharing `group_id` (equal or custom shares) |

Paying a request = the payer calls `transfer()` (requester_account as credit), then the
request flips to `paid` with `settled_transfer_id` — **idempotent on the transfer's
key**, so double-tap can't double-pay. A request is a *claim*, never a hold on the
payer (you can't reserve someone else's funds), so it has no ledger effect until paid.

### Endpoints (sketch)

| Method | Path | Tag | Note |
|--------|------|-----|------|
| GET | `/handles/{handle}` | client | resolve → masked owner + account (confirmation-of-payee) |
| POST | `/me/handle` | client | claim a unique handle for the caller |
| POST | `/transfers` (extend) | client | accept `credit_handle` as an alternative to `credit_account` |
| POST | `/me/payment-requests` | client | request money from a handle/user (or fan-out a split via `group`) |
| GET | `/me/payment-requests?direction=incoming\|outgoing` | client | claims to pay / awaiting payment |
| POST | `/me/payment-requests/{id}/pay` | client | pay via `transfer()`; `Idempotency-Key`; flips to `paid` |
| POST | `/me/payment-requests/{id}/decline` · `/cancel` | client | requester cancels / payer declines |

### Hardest problem / risk

**Handles are an abuse and impersonation surface.** A handle is a public, memorable
alias resolving to a real person — so handle **squatting**, **impersonation** (@bank0,
@support), and **enumeration/scraping** (harvesting names by guessing handles) are the
real risks. Mitigations: reserved-handle denylist, rate-limited resolution, the
existing **masked-name confirmation-of-payee** so resolution leaks minimal PII, and
**no balance/existence oracle** beyond a yes/no. Payment-requests add a **spam/phishing**
vector ("pay this request" social engineering) — cap outstanding requests per sender,
require the payer's explicit confirm (the request never auto-debits), and expire stale
requests in the maintenance sweep. The money safety itself is easy (it's just
`transfer()`); the social/abuse layer is the hard part.

### Effort

**M.** Handles = an alias table + a resolver (clone the beneficiary pattern) + a
`credit_handle` branch in the transfer handler. Payment-requests = one table (plus a
group table) + ~5 endpoints + an expiry sweep step. No ledger changes; all money flows
through existing `transfer()`.

### Sequencing

Do **handles first** (small, reuses confirmation-of-payee, immediately useful for
"pay @alice"), then **request-money**, then **bill-split** (just grouped requests) on
top. Sits naturally after the P1 client gaps and pairs with the events feed in
the shipped `/me/events` feed ("@bob requested €20").
Independent of pots/cards/FX.

---

## 8. Summary & recommended global sequencing

| # | Domain | New vs. extend | Hardest problem | Effort | Order |
|---|--------|----------------|-----------------|--------|-------|
| 5 | Scheduled / recurring | extend (worker + transfers) | exactly-once across crashes (det. idempotency key) | **M** | **1st** — best fit, low risk |
| 2 | Pots / spaces | extend (accounts) | keeping pots off the external-pay surface | **M** | 2nd — reuses IBAN allocator |
| 7 | P2P handles / requests | mixed | handle abuse/impersonation/enumeration | **M** | 3rd — addressing + claims layer |
| 4 | Insights / categorization | extend (ledger read layer) | weak data until cards (no MCC) | **M** (ML: L) | 4th — cheap, demos well |
| 1 | Onboarding / KYC | extend (users) | KYC is a vendor + compliance, not code | **M** (v1) / **L** (KYC) | v1 now; KYC when there's a provider |
| 6 | Multi-currency / FX | new-in-effect | per-currency zero-sum + rounding + scale | **XL** | late — rewrites ledger invariants |
| 3 | Cards | new + integration | async auth→clearing, PCI, processor | **XL** | last — needs a processor partner |

**Why this order.** The three **M**-effort, ledger-reusing, integration-free domains
(schedules, pots, P2P) come first: each is mostly a table + a thin PL/pgSQL function +
client endpoints over the *existing* transfer/worker machinery, so they ship fast and
carry little risk. Insights is similarly cheap but its quality is gated on card data,
so it slots in after but doesn't block. Onboarding's **v1 is already specced** and
should land immediately to unblock fraudbank; full KYC waits on a vendor. The two
**XL** domains — FX (rewrites the ledger's currency invariant) and Cards (a regulated
external integration with async settlement) — are deliberately last: highest blast
radius, lowest reuse, and each needs a partner/owner beyond engineering. Validate their
*internal* models early as low-risk spikes (the four-leg FX model against `reconcile`;
holds-as-card-auths against a mock processor) so that when the business case arrives,
the ledger mapping is already proven.
