# Spec — Banking-grade hardening & guided-transfer v2

> **Status: open recommendations only.** Everything shipped is retired from this
> spec and lives as-built in the reference docs —
> [`../03`](../03-ledger-lifecycle-idempotency.md) (ledger + idempotency),
> [`../05`](../05-admin-ui.md) (operator), [`../06`](../06-client-api.md) (client),
> [`../12`](../12-rail-readiness.md) (rail seam), `db/migrations/` (schema +
> PL/pgSQL). Rec 17 (RFC 9457) is **re-deferred at 1.0.0 (P0→P3)**. Section and Rec
> numbers are stable — other docs and code comments cite them.
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

Sections 1, 2, 3.1, 3.3, 3.8 and 5 are **tombstones** — closed work, kept only so
external references resolve; each points at its as-built home. Live content:
§3.2/§3.4/§3.5/§3.6/§3.7 (open rec tables, `P0…P3` + `S/M/L/XL`), §4 (unmet
client-UX rows), §6 next, §7 effort, §8 sources.

## 1. What is already banking-grade (do NOT re-architect)

> ✅ **Closed.** The engine — append-only ledger, trigger-guarded `balance_minor`,
> `reconcile()` invariants, authorize/capture + holds, immutable `admin_actions` +
> maker-checker — is as-built in [`../03`](../03-ledger-lifecycle-idempotency.md) §1.
> The gaps are at the edges and in the contract, not the engine.

## 2. The hardest problem — the closed-core-vs-rail dual contract

> ✅ **Resolved.** Rail-ready **additively, with no rail built**; the outbox /
> relay / idempotent-consumer / recovery-point / `pacs.004`-saga machinery is seam
> documentation in [`../12`](../12-rail-readiness.md), deliberately unbuilt — every
> trigger for building it is "a real external creditor exists," and bank0 has none.

## 3. The eight pillars

### 3.1 Idempotency & exactly-once

> ✅ **Closed** (Recs 1, 2/29, 3, 4). As-built in
> [`../03`](../03-ledger-lifecycle-idempotency.md) §2–§3 +
> [`../06`](../06-client-api.md) §5. Only descendant still open: Rec 17, in §3.5.

### 3.2 Ledger & money correctness

**Current (as-built):** production-shaped core plus schedulable maintenance (Rec 6)
and explicit `currency` on money-bearing responses (Rec 19 subset) —
[`../03`](../03-ledger-lifecycle-idempotency.md). Keep the core as-is.

**Gaps:** no settlement/finality state beyond `posted`; no partial capture (Rec 7,
deferred-YAGNI); single-currency (`CHECK currency='EUR'`) with a hard-coded
exponent-2 assumption (Rec 8, deferred-YAGNI); `reconcile()` proves only
intra-ledger invariants.

| # | Rec | P | Effort |
|---|-----|---|--------|
| 5 | **Auditor-role `admin_actions` feed** (the read-only `reconcile()` surface already ships — `GET /admin/reconcile` returns the invariant proofs; `api/openapi.yaml` `ReconcileResponse`). Open scope is only the auditor-role view over `admin_actions` so "who authorised what, with which approver" is queryable read-only alongside the existing reconcile proofs. | P1 | S |
| 7 | **Partial capture** in `post_transfer` (`amount_to_capture ≤ hold.amount_minor`; post the captured legs, release the residual). Keeps the single-transaction shape. **Deferred (YAGNI)** — no flow captures less than it authorized; build trigger + rationale in [`../12-rail-readiness.md`](../12-rail-readiness.md) §5. | P2 | M |
| 8 | **ISO-4217 currency-metadata table** carrying the minor-unit exponent so formatting/rounding are currency-driven (prerequisite for any multi-currency / FX-GL leg model). **Deferred (YAGNI)** — everything is EUR; the "surface `currency`" half shipped on responses (Rec 19). Build trigger (first non-EUR currency) in [`../12-rail-readiness.md`](../12-rail-readiness.md) §5. | P2 | M |

### 3.3 Payee verification & APP fraud (CoP / VOP, disputes, reimbursement)

> ✅ **Shipped** (Recs 9, 10, 11, 12): server-side CoP/VOP verdict + recipient risk
> on `/beneficiaries/resolve`, warning-ack evidence, and the PSR dispute claim
> machine (SLA clock, reimbursement net of cap/excess, simulated `pacs.004` recall).
> As-built in [`../06`](../06-client-api.md); regulatory anchors in §8. Nothing open.

### 3.4 SCA & transaction risk (PSD2, step-up, TRA)

**Current (as-built):** TOTP MFA + dynamically-linked step-up + the
`assess_transfer_risk()` TRA seam ship (Recs 13, 14, 15) — [`../06`](../06-client-api.md) §6.

**Gaps:** beneficiary creation isn't yet gated (RTS Art. 13). (Clients *can* now
pre-warn a step-up before submit via the `POST /transfers/intent` preflight — Rec 22,
§3.6 — which returns `decision = step_up`; Rec 16 remains only the beneficiary gate +
exposing the raw `step_up_limit_minor` constant.)

| # | Rec | P | Effort |
|---|-----|---|--------|
| 16 | **Gate beneficiary creation (RTS Art. 13) + expose `step_up_limit_minor`** so clients can pre-warn that an amount will demand step-up before submit. | P2 | S |

### 3.5 API & data standards (ISO 20022, RFC 9457, status vocabulary, rail IDs)

**Current (as-built):** rail-ready identifiers (UETR + `end_to_end_id`, Rec 18), the
computed never-stored ISO-20022 `status_iso` (Rec 20; mapping in
[`../12`](../12-rail-readiness.md) §4) and explicit ISO-4217 `currency` on responses
(Rec 19 subset) all ship. The one remaining private-dialect item is the error body:
a flat `{error, message}`, not RFC 9457 `application/problem+json` (Rec 17, deferred).

| # | Rec | P | Effort |
|---|-----|---|--------|
| 17 | **Migrate the error model to RFC 9457 `application/problem+json`**. **Re-deferred at 1.0.0 (P0→P3):** the `error` code token already gives clients machine-branchable error classes (9457 has no code member — a `code` extension would carry the same token), no client or proxy reads the error content-type, and the recorded adoption path is additive content negotiation (`Accept: application/problem+json`) per the error-contract note in [`../06-client-api.md`](../06-client-api.md) §5 — so this never requires a coordinated client bump or a major version. *(draft-07 itself cites RFC 7807; use its successor **9457**.)* | P3 | M |
| 19 | **Additive-only contract CI + cross-client DTO conformance** (the remainder of Rec 19). `currency` is now explicit on all money-bearing **responses**; **request-side `currency` is deliberately omitted** — the server derives it from the debit account ([`../12-rail-readiness.md`](../12-rail-readiness.md) §5). Open scope: enforce additive-only contract CI (fail on removed/renamed fields or narrowed enums) and **extend conformance to the hand-written iOS/Android DTOs** (only web is checked today). | P1 | S |

### 3.6 Fraud-UX backend enablers (decision/warning + events feed)

**Current (as-built):** the `GET /me/events` feed (Rec 21), the server-driven
risk-decision preflight `POST /transfers/intent` + console-tunable `warning_rules`
(Rec 22) and the `held`/`under_review` lifecycle with customer confirm/cancel
(Rec 23) all ship — [`../06`](../06-client-api.md) §1/§8,
[`../03`](../03-ledger-lifecycle-idempotency.md) §1/§2.8, [`../05`](../05-admin-ui.md) §4.4a/§4.8.

**Gaps:** velocity/daily-limit meters + new-payee cooling fields (Rec 24) are still open.

| # | Rec | P | Effort |
|---|-----|---|--------|
| 24 | **Velocity/daily-limit + new-payee cooling fields:** a limits endpoint (`daily_limit_minor/daily_used_minor/daily_remaining_minor/count_today` + the existing per-txn cap) and `beneficiaries.{added_at, payment_count, first_payment_completed, cooling_off_until}` so clients render limit meters + first-payment friction. | P2 | M |

### 3.7 Observability, audit & AML/sanctions

**Current (as-built):** audit is strong-by-construction for money (`admin_actions`,
maker-checker 4-eyes, `reconcile()`), and the AML/sanctions name-screening gate ships
(Rec 25 — `screen_payment` inside `transfer()`, hits park `under_review` into the
maker-checker queue, never auto-released; [`../03`](../03-ledger-lifecycle-idempotency.md) §2.8,
[`../05`](../05-admin-ui.md) §4.4a/§4.9). *(Wolfsberg-summarised guidance, not a quoted mandate.)*

**Gaps:** the fraud decision trail isn't in the audit feed (Rec 26); no auditor-role
read views (Rec 27); no PEP/onboarding screening (Rec 28).

| # | Rec | P | Effort |
|---|-----|---|--------|
| 26 | **Append the full fraud decision trail to the audit feed** (every warning shown, ack, step-up result, screening decision, hold action) so the decision trail feeding the PSR Consumer Standard of Caution and the reimbursement file is reconstructable. Reuses the `admin_actions` pattern. | P1 | S |
| 27 | **Auditor read-only audit views** (pure read surface; overlaps Rec 5 — the `reconcile()` surface itself already ships as `GET /admin/reconcile`, so the open part is the auditor-role `admin_actions`/audit views). | P2 | S |
| 28 | **PEP/watchlist storage + onboarding screening** (distinct from per-payment screening; runs at account opening and on list updates). | P2 | M |

### 3.8 Resilience, recovery & rail-readiness

> ✅ **Closed.** The stale-`in_progress` sweep (Rec 2/29) and schedulable
> maintenance (Rec 6) ship; the outbox / relay / idempotent rail-submit /
> recovery-point / `pacs.004`-saga checklist and the BIAN Payment Order vs Payment
> Execution seam at the `post_transfer(id, allow_from)` boundary are documented in
> [`../12`](../12-rail-readiness.md) §2/§3 (Recs 30/31, shipped as docs). Nothing
> open — the rail is deliberately unbuilt (§2 RESOLVED).

## 4. UX → backend capability map

*"What's still missing that the backend can provide?"* — **rows already met are
removed**; only the unmet set remains.

| Client fraud-UX feature | Backend capability needed | P |
|---|---|---|
| First-payment friction on a new payee (the recipient-risk badge half already ships on `/beneficiaries/resolve`) | New-payee cooling fields (Rec 24) | P1 |
| Remaining daily/transaction limit meter + pre-warn step-up | Limits endpoint (Rec 24) + `step_up_limit_minor` (Rec 16) | P2 |
| Anti-impersonation "we aren't calling you" banner | `GET /me/call-status` (Starling/Monzo pattern) | P2 |

## 5. Guided transfer v2 — SHIPPED (retired)

> ✅ **Shipped and retired.** `GET /transfers/suggestion` returns the up-to-3
> third-party "mule" options wrapper (resolver `suggest_transfer_destinations` in
> `db/migrations/00012_guided_scenarios.sql`); the PWA picks one at random and
> synthesises the own-account fallback when empty. As-built:
> [`../06-client-api.md`](../06-client-api.md) §1 + `00012_guided_scenarios.sql`.

## 6. Sequencing

**Next:**

- **Wave 4 — standards depth, edge surfaces (P1/P2, additive):** Recs 5/27, 16, 24, 26, 28,
  plus the **Rec 19 remainder** (additive-only contract CI + iOS/Android DTO conformance).
- **Deferred:** Rec 17 (RFC 9457) — re-deferred at 1.0.0; adoption path is additive
  content negotiation ([`../06-client-api.md`](../06-client-api.md) §5), no coordinated
  bump needed; Recs 7 (partial capture) and 8 (ISO-4217
  metadata table) — **deferred-as-YAGNI**, triggers in [`../12-rail-readiness.md`](../12-rail-readiness.md) §5.

## 7. Effort summary (remaining recs only)

| Priority | Recs | Rough size |
|---|---|---|
| **P3 (re-deferred at 1.0.0)** | 17 | M — additive content negotiation when ever wanted; no client bump |
| **P1** | 5/27, 19 (remainder), 26 | edge surfaces + auditor read views + contract CI |
| **P2 (active)** | 16, 24, 28 | additive standards; never blocks the closed core |
| **P2 (deferred-YAGNI)** | 7, 8 | partial capture + ISO-4217 metadata; triggers in docs/12 §5 |

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
