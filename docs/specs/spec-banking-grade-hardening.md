# Spec — Banking-grade hardening & guided-transfer v2

> **Status: recommendation spec — partly SHIPPED, remainder open.** Shipped and
> now as-built (see the reference docs, not this spec): all of Waves 0–2 (Recs 1,
> 2/29, 9, 10, 13, 18, 21), the Wave-3 subset (Recs 11, 12, 14, 15), Rec 3
> (per-owner idempotency namespace), and guided-transfer v2 (former §5). As-built
> homes: the client surfaces in [`../06-client-api.md`](../06-client-api.md), the
> operator surfaces in [`../05-admin-ui.md`](../05-admin-ui.md), the ledger +
> idempotency engine in [`../03-ledger-lifecycle-idempotency.md`](../03-ledger-lifecycle-idempotency.md),
> and the schema + PL/pgSQL in `db/migrations/`. The retired companion line-level
> specs (spec-step-up-mfa, spec-notifications-events) are folded into those docs.
> The tables below keep **only the open recommendations**; each pillar's "Current"
> prose is updated to the as-built baseline. Rec 17 (RFC 9457) stays deferred: it
> changes the error content-type for all three fraudbank clients and needs a
> coordinated bump.
>
> **Confidence & hedges (qualify the remaining open recs).** EUR-Lex, IETF, UK PSR
> and EU Instant Payments Regulation facts are high-confidence. EPC primary PDFs
> (rulebooks, VOP API spec, R-transaction reason codes) 403'd to direct fetch and
> were corroborated via secondaries — so **exact EPC article locators and VOP
> wire-level outcome code tokens are hedged** (the four *semantic* outcomes are
> certain; the literal tokens are not). **PSD3 / the new PSR are only at
> provisional trilogue agreement (27 Nov 2025), not adopted** — all PSD3/PSR
> specifics are forward-looking; SEPA-Instant Verification of Payee under Reg (EU)
> 2024/886 **is** binding and stated as fact. AML "screen before
> settlement" is industry guidance (Wolfsberg-summarised), not a quoted mandate.

---

## 0. How to read this

Section 1 says what is already banking-grade and must not be touched. Section 2
is the single hardest architectural problem. Section 3 is the eight pillars, each
with bank0's **current state → target → gaps → recommendations** (every
recommendation carries a `P0/P1/P2` priority and an `S/M/L/XL` effort). Section 4
maps each client-side fraud-UX feature to the backend capability it needs (the
"what's still missing that the backend must provide" question). Section 5 is a
tombstone — the guided-transfer v2 design shipped and is now as-built. Section 6
sequences the remaining work into waves; Section 7 is the effort summary; Section 8
is sources + confidence.

## 1. What is already banking-grade (do NOT re-architect)

bank0's closed single-Postgres core is strikingly correct where it counts:

- `request_transfer` (`00008`) **claims the idempotency key** (`INSERT … ON CONFLICT DO NOTHING`, first-writer-wins) **+ validates** accounts/limits/available-funds **+ writes the double-entry ledger legs + the hold + the completion response — all in ONE transaction.** Because the side effect commits atomically with the key-claim, bank0 needs **none** of the distributed machinery (outbox, saga, inbox, recovery-points) the industry built to paper over non-atomic side effects.
- The ledger is **append-only** with a mutation-blocking trigger; `balance_minor` is a **cache writable only by the ledger trigger** and guarded against any non-ledger write. `reconcile()` (`00009`) continuously asserts cache==ledger, per-transfer zero-sum, and global zero-sum.
- Genuine **authorize/capture**: `request_transfer` = authorize (pending + active hold, 15-min TTL); `post_transfer` = capture; `available = balance − Σ active holds`; holds auto-expire.
- **Append-only idempotent reversal** with a clawback funds-check; money as `BIGINT` minor units; cross-bank money modelled against an `EXTERNAL_CLEARING` GL so the books stay zero-sum.
- **Immutable operator audit** (`admin_actions`) + **maker-checker 4-eyes** for above-threshold console money moves.

The fingerprint `sha256(debit│credit│amount│kind)` is, in the IETF draft's own
terms, a "selected-elements checksum" — exactly right, and deliberately excluding
cosmetic fields. **The gaps are at the edges and in the contract, not the engine.**

## 2. The hardest problem — the closed-core-vs-rail dual contract

bank0's entire correctness story is load-bearing **only because the core is
closed.** The strength of `request_transfer` — claim-key + mutate-ledger +
record-completion in one atomic transaction — becomes **impossible the moment a
real external payment rail is interleaved**, because you cannot put a network call
inside a DB transaction. At that point every guarantee bank0 gets for free (no
outbox, no saga, no inbox, no visible `in_progress→completed` crash window) must
be rebuilt with the full distributed stack: a transactional outbox written in the
ledger txn, an at-least-once relay, an idempotent rail consumer keyed by a
deterministic UETR/`EndToEndId` derived from `transfer_id`, recovery-point
checkpoints, and a saga whose compensations are **asymmetric** (a settled credit
cannot be unilaterally clawed back — it becomes a `pacs.004` recall/return
governed by scheme SLAs, not an `UPDATE`).

The trap is twofold: **(a)** building any of that now needlessly complicates the
demo and breaks the "do not re-architect" verdict; but **(b)** shipping
client-facing features whose semantics *silently assume synchronous atomicity*
(instant-final `posted`, client-computed CoP verdicts, a single transfer status,
no UETR) bakes in a contract the rail will later violate — forcing breaking client
changes exactly when the system is most fragile.

**Resolution:** make the contract **rail-ready additively without building the
rail** — mint a UETR + `end_to_end_id` now, model status with an ISO-20022-aligned
parallel field that already distinguishes `posted` from a future `settled`, move
the fraud verdict + warning evidence server-side (so they survive an async
future), and write the outbox/saga/recovery-point seam as **documentation**. The
day a rail is added, the core converges on the Stripe/brandur design behind a
contract the clients already speak — zero breaking change.

## 3. The eight pillars

### 3.1 Idempotency & exactly-once

**Current (as-built):** best-in-class for a closed core (see §1). The HTTP contract
is now mapped (Rec 1 shipped — ERRCODE→status map + replay of the stored `response`
JSONB + `Idempotency-Replayed: true`); the stale-`in_progress` sweep exists in
`cleanup_idempotency_keys` (Rec 2/29); and the namespace is **per-owner** —
`PK(owner_id, key)`, sentinel-namespaced for pre-auth/operator paths (Rec 3 shipped;
as-built in [`../03-ledger-lifecycle-idempotency.md`](../03-ledger-lifecycle-idempotency.md) §3). Header required on
`/transfers`, `/transfers/{id}/reverse`, deposit/withdraw; 7-day `expires_at`.

**Target:** wire the IETF `Idempotency-Key` draft-07 contract at the HTTP layer —
largely done; the open piece is extending the documented contract to the remaining
mutating POSTs.

**Gaps:** the header/OpenAPI response semantics aren't yet spelled out on
`post`/`cancel`/`dispute`, and a second key against an already-reversed original
still raises rather than idempotently returning the existing reversal.

| # | Rec | P | Effort |
|---|-----|---|--------|
| 4 | **Document the semantics + extend the header to all mutating POSTs.** Express 422/409/TTL in the OpenAPI responses for `post`/`cancel`/`dispute` too; make a second key against an already-reversed original return the **existing** reversal id idempotently instead of raising. | P2 | S |

### 3.2 Ledger & money correctness

**Current:** production-shaped (append-only ledger, trigger-guarded balance cache,
`reconcile()` invariants, real authorize/capture + holds, append-only reversal).

**Target:** keep the core exactly as-is; add only the edge surfaces auditors/clients need.

**Gaps:** no settlement/finality state beyond `posted`; no partial capture;
single-currency (`CHECK currency='EUR'`) with a hard-coded exponent-2 assumption;
`reconcile()` proves only intra-ledger invariants.

| # | Rec | P | Effort |
|---|-----|---|--------|
| 5 | **Auditor-role `admin_actions` feed** (the read-only `reconcile()` surface already ships — `GET /admin/reconcile` returns the invariant proofs; `api/openapi.yaml` `ReconcileResponse`). Open scope is only the auditor-role view over `admin_actions` so "who authorised what, with which approver" is queryable read-only alongside the existing reconcile proofs. | P1 | S |
| 6 | **Make the maintenance runners** (`expire_holds()`, `cleanup_idempotency_keys()`) **independently schedulable** — a one-shot subcommand (cron/systemd-timer friendly) and surfaced run results — so sweeps don't depend solely on the in-process advisory-locked ticker. *(pg_cron is deliberately NOT a dependency — the app owns scheduling.)* | P1 | S |
| 7 | **Partial capture** in `post_transfer` (`amount_to_capture ≤ hold.amount_minor`; post the captured legs, release the residual). Keeps the single-transaction shape. | P2 | M |
| 8 | **ISO-4217 currency-metadata table** carrying the minor-unit exponent so formatting/rounding are currency-driven (prerequisite for any multi-currency / FX-GL leg model). Surface `currency` on every money-bearing schema. | P2 | M |

### 3.3 Payee verification & APP fraud (CoP / VOP, disputes, reimbursement)

**Regulatory anchors.** EU **Verification of Payee (VOP)** under the Instant
Payments Regulation (Reg (EU) 2024/886) is in force, free to the payer, on all
SEPA credit transfers, IBAN-keyed, with **four outcomes** and a liability pivot:
*if the payer is warned of a mismatch and proceeds anyway, the payer bears the
loss.* UK **Confirmation of Payee** likewise has four outcomes (match / close
match **with the real name returned** / no match / unavailable). UK **PSR
mandatory APP-scam reimbursement** is live (7 Oct 2024) with a maximum
reimbursement cap, a 50/50 split between sending and receiving PSP, a business-day
SLA clock, and a consumer-standard-of-caution exception. *(Exact EPC VOP code
tokens and rulebook article locators are hedged; the semantics are certain.)*

**Current (as-built):** the regulatory substance shipped. `/beneficiaries/resolve`
now returns the **server-side** CoP/VOP verdict (`match_result` ∈
match/close_match/no_match/unable + `reason_code` + `suggested_name` on close_match
+ a server `gate`) plus **recipient risk** (`recipient_risk`, `mule_suspected`,
`signals[]`, `is_first_payment_to_payee`) — clients render, never decide (Rec 9,
Rec 11; as-built [`../06-client-api.md`](../06-client-api.md)). Warning-shown /
-acknowledged evidence persists via `POST /me/warning-acks` (Rec 10). Disputes are
a PSR claim machine — `scam_type`, business-day `sla_due_at`, decision with a real
clearing→victim reimbursement net of the `bank_settings` cap/excess, vulnerable
waiver, simulated `pacs.004` recall states (Rec 12). bank0 is its own (intra-bank,
simulated) VOP responder and can only *simulate* the interbank recall.

**Gaps:** none tracked in this pillar — the remaining adaptive-fraud surfaces
(server-driven warning copy, held/under-review lifecycle, AML screening) live in
§3.6/§3.7 (Recs 22, 23, 25).

### 3.4 SCA & transaction risk (PSD2, step-up, TRA)

**Current (as-built):** TOTP MFA + step-up shipped (Rec 13) — RFC 6238, AES-256-GCM
seed at rest, hashed recovery codes, `mfa_required` login branch + `/auth/mfa/verify`,
`amr`/`auth_time` claims, 403 `step_up_required` before the key is claimed, same-key
retry (as-built [`../06-client-api.md`](../06-client-api.md) §6). The step-up
challenge is **dynamically linked** to `(debit│credit│amount)` via the JWT `txn_link`
— a generic fresh OTP no longer authorises any payment (Rec 14, PSD2 RTS Art. 5). The
**server-side TRA seam** ships too — `assess_transfer_risk()` scores
velocity/first-payment/flagged-destination/account-age and ORs `high` into the gate's
trigger set (Rec 15).

**Gaps:** beneficiary creation isn't yet gated (RTS Art. 13) and clients can't
pre-warn that an amount will demand step-up before submit.

| # | Rec | P | Effort |
|---|-----|---|--------|
| 16 | **Gate beneficiary creation (RTS Art. 13) + expose `step_up_limit_minor`** so clients can pre-warn that an amount will demand step-up before submit. | P2 | S |

### 3.5 API & data standards (ISO 20022, RFC 9457, status vocabulary, rail IDs)

**Current (as-built):** the idempotency design is strongly standards-aligned. Rail-ready
identifiers shipped (Rec 18) — a bank-minted **UETR** UUIDv4 + originator `end_to_end_id`
on `transfers`, surfaced on the contract and folded into the idempotency fingerprint.
The surrounding contract is still a **private dialect** otherwise: a flat
`{error, message}` body (not RFC 9457 `application/problem+json`); a private status set
(`pending|posted|failed|canceled|reversed`), not the ISO 20022 family UK OBIE and Berlin
Group constrain to; `currency` is a free string omitted from `CreateTransferRequest`.

| # | Rec | P | Effort |
|---|-----|---|--------|
| 17 | **Migrate the error model to RFC 9457 `application/problem+json`** `{type (stable URI per class), title, status, detail, instance}` so clients branch on `type`, not prose. **Deferred** pending a coordinated bump across all three fraudbank clients (it changes the error content-type). *(draft-07 itself cites RFC 7807; use its successor **9457** — do not attribute 9457 to the draft.)* | P0 | M |
| 19 | **Make `currency` explicit (ISO-4217) on every money-bearing schema + enforce additive-only contract CI** (fail on removed/renamed fields or narrowed enums) and **extend conformance to the hand-written iOS/Android DTOs** (only web is checked today). | P1 | S |
| 20 | **Map status onto an ISO-20022-aligned parallel `status_iso`** (`pending→PDNG/RCVD`, `posted→ACSC`, `failed→RJCT`, `canceled→CANC`, reverse via `pacs.004`) while keeping the flat `status` for back-compat. *(Berlin Group status-code list is medium-confidence.)* | P2 | M |

### 3.6 Fraud-UX backend enablers (decision/warning + events feed)

**Current (as-built):** the `GET /me/events` feed shipped (Rec 21; as-built
[`../06-client-api.md`](../06-client-api.md) §1) — per-user append-only, keyset-paginated,
`unread_count` + `/me/events/read`, **written in the same txn as the cause**, replacing
poll-on-focus and enabling a badge + "new sign-in" alert. Four event types exist today —
`transfer.posted`, `payment.incoming`, `device.new`, `dispute.updated`; the spec's
`fraud-alert`/`hold` types are **not** implemented (they belong to the still-open adaptive
surfaces, Recs 22/23). The mule risk signal on resolve also shipped (Rec 11, §3.3). Still
missing: a risk-decision endpoint, warning taxonomy, held/under-review state, and
velocity/daily-limit fields; warning copy + a11y labels remain hardcoded ×3.

| # | Rec | P | Effort |
|---|-----|---|--------|
| 22 | **Warning/decision endpoint backed by server-driven copy:** `POST /transfers/intent` (preflight) → `{decision: allow\|step_up\|review\|block\|warn, risk_band, reason_codes[], warning:{warning_id, category, severity, headline, body, required_ack, cooling_off_seconds}, step_up_method}`. Generalise `guided_scenarios` into a rule table. **Plain-language copy + machine outcome codes let clients attach correct ARIA labels** (the client audit found the dictated IBAN was `aria-hidden` on web and colour-only on iOS/Android). **Do not** expose a raw numeric score. | P1 | L |
| 23 | **Held / under-review transfer lifecycle state:** add `held` + `under_review` with `hold_reason`, `hold_expires_at` (a risk-based delay clock, cf. FCA FG24/6 up-to-4-business-day delay), `action_required`, and a customer release/confirm action, routed to the existing maker-checker queue. Enables a demo payment-hold / cooling-off. | P1 | M |
| 24 | **Velocity/daily-limit + new-payee cooling fields:** a limits endpoint (`daily_limit_minor/daily_used_minor/daily_remaining_minor/count_today` + the existing per-txn cap) and `beneficiaries.{added_at, payment_count, first_payment_completed, cooling_off_until}` so clients render limit meters + first-payment friction. | P2 | M |

### 3.7 Observability, audit & AML/sanctions

**Current:** audit is strong-by-construction for money (`admin_actions`,
maker-checker 4-eyes, `reconcile()`), but there is **zero AML/sanctions/PEP/
watchlist screening** (grep-confirmed) — the only "gate" is per-amount limits +
4-eyes.

| # | Rec | P | Effort |
|---|-----|---|--------|
| 25 | **Sanctions/AML screening gate between authorize and capture:** a `screen_payment` hook that screens debtor/creditor names, amount, currency, routing; on a potential true match leave the transfer `pending+held` and route to the maker-checker queue **rather than auto-posting** (never auto-release a potential match). The `transfer()` auto-post convenience must respect the gate. *(Industry — Wolfsberg-summarised — guidance, not a quoted mandate.)* | P1 | L |
| 26 | **Append the full fraud decision trail to the audit feed** (every warning shown, ack, step-up result, screening decision, hold action) so the decision trail feeding the PSR Consumer Standard of Caution and the reimbursement file is reconstructable. Reuses the `admin_actions` pattern. | P1 | S |
| 27 | **Auditor read-only audit views** (pure read surface; overlaps Rec 5 — the `reconcile()` surface itself already ships as `GET /admin/reconcile`, so the open part is the auditor-role `admin_actions`/audit views). | P2 | S |
| 28 | **PEP/watchlist storage + onboarding screening** (distinct from per-payment screening; runs at account opening and on list updates). | P2 | M |

### 3.8 Resilience, recovery & rail-readiness

**Current (as-built):** strongest-possible for a closed core *because* the side effect
commits atomically with the key-claim (the degenerate ideal — no recovery point to
manage). The operational durability hole is **closed** (Rec 2/29): the stale-`in_progress`
sweep now reaps wedged keys. The only residual nicety is making the sweeps
schedulable independently of the in-process ticker — tracked as the open Rec 6 (§3.2).

| # | Rec | P | Effort |
|---|-----|---|--------|
| 30 | **Document the rail-readiness checklist (do NOT build yet):** transactional outbox written in the ledger txn, an at-least-once relay, an idempotent rail-submit keyed by a deterministic UETR/`EndToEndId` derived from `transfer_id`, recovery-point checkpoints, and a saga with compensating reversals for `pacs.004`. The UETR/`end_to_end_id` fields (Rec 18) are the cheap additive pre-work. | P2 | S |
| 31 | **Adopt BIAN-style boundary seams in docs/schema** — conceptually split Payment Order (instruction + lifecycle/holds) from Payment Execution (ledger settlement) so the one-transaction `request_transfer` can later separate a held/pending instruction from settlement without a breaking client change. | P2 | M |

## 4. UX → backend capability map

The client question — *"what's still missing that the backend can provide?"* —
answered as a feature→capability table:

| Client fraud-UX feature | Backend capability needed | P |
|---|---|---|
| CoP/VOP 4-state badge (match / close-match **with revealed name** / no-match / unable), colour **+ text** (a11y) | `/beneficiaries/resolve` returns `{match_result, reason_code, suggested_name, account_type, checked_at}` — verdict **server-side** | P0 |
| Continue gated identically across web/iOS/Android (no `copBlocks` drift) | Server-driven gate `status = ok\|awaiting_acknowledgement\|blocked` | P0 |
| "I was warned and chose to proceed" ack that holds up for liability | Warning-evidence capture tied to the transfer attempt | P0 |
| Replay-safe retry after a network failure / after step-up (charge once) | Replay stored `response` body + `Idempotency-Replayed: true`; `403 step_up_required` **before** the key is claimed | P0 |
| Notification badge + incoming-payment + "new sign-in" alerts | `GET /me/events` feed (**shipped**) | P0 |
| Branch on error class without string-matching prose | RFC 9457 `problem+json` with a stable `type` URI per class | P0 |
| High-value / new-payee step-up, code bound to this exact amount+payee | Step-up MFA + dynamic-linking challenge `hash(debit│credit│amount│kind)` | P0 |
| "High-risk / newly-opened / reported" destination badge; first-payment friction | Recipient-risk on resolve + new-payee cooling fields | P1 |
| Category-specific scam interstitial copy, tunable without an app release | Warning/decision endpoint with server-side rule table | P1 |
| "Payment under review / held" with a clock + release action | `held`/`under_review` statuses + hold metadata | P1 |
| Dispute / scam-claim timeline with the regulatory clock + reimbursement/recall | Dispute enrichment (SLA, decision, recall, scam_type, cap/excess) | P1 |
| Remaining daily/transaction limit meter + pre-warn step-up | Limits endpoint + `step_up_limit_minor` | P2 |
| Anti-impersonation "we aren't calling you" banner | `GET /me/call-status` (Starling/Monzo pattern) | P2 |
| End-to-end trace reference on a payment / for support | Server-minted UETR + `end_to_end_id` on `Transfer` + events | P2 |

## 5. Guided transfer v2 — SHIPPED (retired)

> ✅ **Shipped and retired.** `GET /transfers/suggestion` returns the up-to-3
> third-party "mule" options wrapper (resolver `suggest_transfer_destinations` in
> `db/migrations/00008_features.sql`); the PWA picks one at random and synthesises
> the own-account fallback when empty. As-built:
> [`../06-client-api.md`](../06-client-api.md) §1 + `00008_features.sql`.

## 6. Sequencing

**Done (collapsed):** Wave 0 (Recs 1, 2/29) — ERRCODE→HTTP map + replay stored body,
stale-`in_progress` sweep. Wave 1 (Recs 9, 10, 21) — server-side CoP verdict, warning
evidence, `/me/events` feed. Wave 2 (Recs 13, 14) — TOTP MFA + dynamically-linked
step-up. Wave-3 subset (Recs 11, 12, 15) — recipient/mule risk on resolve, PSR dispute
claim machine, TRA seam. Plus Rec 18 (UETR/`end_to_end_id`), Rec 3 (per-owner
idempotency namespace), and guided-transfer v2 (former §5).

**Next:**

- **Wave-3 remainder (P1, adaptive fraud UX):** Recs 22 (server-driven warning/decision
  copy), 23 (held/under-review lifecycle), 25 (sanctions/AML screening gate). These add
  the `fraud-alert`/`hold` event types the feed doesn't yet emit.
- **Wave 4 — standards depth, edge surfaces & rail pre-work (P1/P2, additive):** Recs 4,
  5/27, 6, 7, 8, 16, 19, 20, 24, 26, 28.
- **Wave 5 — docs-only, defer until a real rail exists:** Recs 30, 31 — document the seam
  now (the shipped UETR/`end_to_end_id` is the cheap pre-work), build nothing.
- **Deferred:** Rec 17 (RFC 9457) — waits on a coordinated bump across all three fraudbank
  clients (it changes the error content-type).

## 7. Effort summary (remaining recs only)

| Priority | Recs | Rough size |
|---|---|---|
| **P0 (deferred)** | 17 | M — coordinated three-client bump |
| **P1** | 5/27, 6, 19, 22, 23, 25, 26 | edge surfaces + adaptive UX + AML; 1 × L (AML screening) |
| **P2** | 4, 7, 8, 16, 20, 24, 28, 30, 31 | additive standards/rail-readiness + docs; never blocks the closed core |

## 8. Sources & confidence

High-confidence (primary): IETF, EUR-Lex, UK PSR, SWIFT, FCA, RFC 9457/8470.
Hedged (secondary corroboration; EPC PDFs 403'd): exact EPC article locators, VOP
wire-code tokens, Berlin Group status list, card-hold windows (per Stripe's
summary), rarer CoP reason codes. Forward-looking (not adopted): PSD3 / new PSR
(provisional trilogue 27 Nov 2025). Binding: SEPA-Instant VOP under Reg (EU) 2024/886.

- IETF `draft-ietf-httpapi-idempotency-key-header-07` (15 Oct 2025) — https://www.ietf.org/archive/id/draft-ietf-httpapi-idempotency-key-header-07.html *(cites RFC 7807; use 9457)*
- RFC 9457 Problem Details (obsoletes 7807) — https://www.rfc-editor.org/rfc/rfc9457.html
- RFC 8470 Using Early Data in HTTP — https://datatracker.ietf.org/doc/html/rfc8470 *(425 = 0-RTT, not in-flight; use 409)*
- brandur.org — Implementing Stripe-like Idempotency Keys in Postgres — https://brandur.org/idempotency-keys
- Stripe — idempotent requests — https://docs.stripe.com/api/idempotent_requests
- Commission Delegated Regulation (EU) 2018/389 (PSD2 RTS, SCA & dynamic linking, TRA) — https://eur-lex.europa.eu/legal-content/EN/TXT/?uri=CELEX:32018R0389
- EU Instant Payments Regulation (EU) 2024/886 / EPC VOP (VOP in force, payer-warned-then-proceeds = payer liable) — https://legal.pwc.de/en/news/articles/verification-of-payee-requirements-vop-under-the-eus-instant-payments-regulation-ipr
- UK PSR mandatory APP-scam reimbursement (live 7 Oct 2024; cap, 50/50, business-day clock) — https://www.psr.org.uk/publications/policy-statements/ps247-faster-payments-app-scams-reimbursement-requirement-confirming-the-maximum-level-of-reimbursement/
- UK Confirmation of Payee — four outcomes — https://www.natwest.com/support-centre/banking-from-home/make-payments/what-is-confirmation-of-payee-cop-and-how-does-it-work.html
- SEPA SCT R-transactions / recall reason codes (10-BBD initiate, 15-BBD answer) — https://www.europeanpaymentscouncil.eu/document-library/guidance-documents/guidance-reason-codes-sepa-credit-transfer-r-transactions *(exact locator hedged)*
- SWIFT UETR (UUIDv4, minted by the debtor agent) — https://www.swift.com/payments/what-unique-end-end-transaction-reference-uetr
- FCA FG24/6 risk-based payment delay (up to 4 business days) — https://www.fca.org.uk/publications/finalised-guidance/fg24-6-guidance-firms-enables-risk-based-approach-payments
- Stripe Radar risk-evaluation outcome model — https://docs.stripe.com/radar/risk-evaluation
- Microservices.io — Transactional Outbox — https://microservices.io/patterns/data/transactional-outbox.html
