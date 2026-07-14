# Spec — Banking-grade hardening & guided-transfer v2

> **Status: recommendation spec — partly SHIPPED, remainder open.** Shipped and
> now as-built (see the reference docs, not this spec): all of Waves 0–2 (Recs 1,
> 2/29, 9, 10, 13, 18, 21), the **full Wave-3 set** (Recs 11, 12, 14, 15 **plus the
> adaptive-fraud remainder 22, 23, 25**), Rec 3 (per-owner idempotency namespace),
> Rec 4 (idempotency semantics documented on all mutating money POSTs + idempotent
> second reverse), and guided-transfer v2 (former §5). As-built
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
- The ledger is **append-only** with a mutation-blocking trigger; `balance_minor` is a **cache writable only by the ledger trigger** and guarded against any non-ledger write. `reconcile()` (`00010`) continuously asserts cache==ledger, per-transfer zero-sum, and global zero-sum.
- Genuine **authorize/capture**: `request_transfer` = authorize (pending + active hold, 15-min TTL); `post_transfer` = capture; `available = balance − Σ active holds`; holds auto-expire.
- **Append-only idempotent reversal** with a clawback funds-check; money as `BIGINT` minor units; cross-bank money modelled against an `EXTERNAL_CLEARING` GL so the books stay zero-sum.
- **Immutable operator audit** (`admin_actions`) + **maker-checker 4-eyes** for above-threshold console money moves.

The fingerprint `sha256(debit│credit│amount│kind)` is, in the IETF draft's own
terms, a "selected-elements checksum" — exactly right, and deliberately excluding
cosmetic fields. **The gaps are at the edges and in the contract, not the engine.**

## 2. The hardest problem — the closed-core-vs-rail dual contract

> ✅ **Resolved.** The dual contract is made **rail-ready additively, with no rail
> built** — the resolution the problem itself prescribed (correctness stays
> load-bearing only because the core is closed; a real rail would force the full
> distributed stack — outbox, relay, idempotent consumer, recovery points, an
> asymmetric `pacs.004` saga — that a closed core needs none of).
>
> Shipped as the cheap additive pre-work: a bank-minted `uetr` + originator
> `end_to_end_id` (Rec 18); an ISO-20022-aligned parallel **`status_iso`**,
> computed-never-stored (Rec 20, §3.5); and the fraud verdict + warning evidence
> already moved server-side (Recs 22/23/25) so they survive an async future. The
> outbox / at-least-once relay / idempotent rail-submit / recovery-point /
> asymmetric-`pacs.004`-saga machinery (Recs 30/31) is written as the **seam
> documentation** — [`../12-rail-readiness.md`](../12-rail-readiness.md) — and the
> rail itself is **deliberately unbuilt**: every trigger for building it is "a real
> external creditor exists," and bank0 has none. The day a rail is added, the core
> converges on the Stripe/brandur design behind a contract the clients already
> speak — **zero breaking change**.

## 3. The eight pillars

### 3.1 Idempotency & exactly-once

**Current (as-built):** best-in-class for a closed core (see §1). The HTTP contract
is now mapped (Rec 1 shipped — ERRCODE→status map + replay of the stored `response`
JSONB + `Idempotency-Replayed: true`); the stale-`in_progress` sweep exists in
`cleanup_idempotency_keys` (Rec 2/29); and the namespace is **per-owner** —
`PK(owner_id, key)`, sentinel-namespaced for pre-auth/operator paths (Rec 3 shipped;
as-built in [`../03-ledger-lifecycle-idempotency.md`](../03-ledger-lifecycle-idempotency.md) §3). Header required on
`/transfers`, `/transfers/{id}/reverse`, deposit/withdraw; 7-day `expires_at`. The
**IETF draft-07 semantics are now documented** across the surface (Rec 4 shipped):
the OpenAPI spec spells out the replay + `Idempotency-Replayed: true`, `422
idempotency_key_conflict`, `409 in_progress` and 7-day TTL on `postTransfer`,
`confirmTransfer`, `cancelTransfer`, `raiseDispute` and `reverseTransfer`; and a
second reverse of an already-reversed original — even under a **different** key —
now returns the **existing** reversal id idempotently (200) instead of raising
(as-built [`../03-ledger-lifecycle-idempotency.md`](../03-ledger-lifecycle-idempotency.md) §2.4, [`../06-client-api.md`](../06-client-api.md) §5).

**Target:** wire the IETF `Idempotency-Key` draft-07 contract at the HTTP layer —
**done** for the closed core; only the content-type migration to RFC 9457 (Rec 17,
§3.5) remains, and it is deferred pending a coordinated three-client bump.

**Gaps:** none tracked in this pillar.

### 3.2 Ledger & money correctness

**Current (as-built):** production-shaped (append-only ledger, trigger-guarded
balance cache, `reconcile()` invariants, real authorize/capture + holds,
append-only reversal). The maintenance runners are now **independently
schedulable** (Rec 6 shipped): a one-shot `bank0 maintenance` subcommand
(`cmd/app/main.go`; CronJob/Cloud-Scheduler-friendly, [`../08-deployment-cloud-run-supabase.md`](../08-deployment-cloud-run-supabase.md) §3.4)
runs `expire_holds` + the cleanups + a `reconcile()` pass and logs the counts. Two
accepted nits: a **reconcile-drift** result and a **lock-held** run (another
replica holds the advisory lock) both **exit 0** — alerting keys on the emitted
logs (`reconcile drift detected …`), not the process exit code. `currency` now
ships explicitly on every money-bearing **response** (Rec 19, §3.5).

**Target:** keep the core exactly as-is; add only the edge surfaces auditors/clients need.

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

**Gaps:** none tracked in this pillar — the adaptive-fraud surfaces (server-driven
warning copy, held/under-review lifecycle, AML screening) have since shipped and are
described in §3.6/§3.7 (Recs 22, 23, 25).

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

**Gaps:** beneficiary creation isn't yet gated (RTS Art. 13). (Clients *can* now
pre-warn a step-up before submit via the `POST /transfers/intent` preflight — Rec 22,
§3.6 — which returns `decision = step_up`; Rec 16 remains only the beneficiary gate +
exposing the raw `step_up_limit_minor` constant.)

| # | Rec | P | Effort |
|---|-----|---|--------|
| 16 | **Gate beneficiary creation (RTS Art. 13) + expose `step_up_limit_minor`** so clients can pre-warn that an amount will demand step-up before submit. | P2 | S |

### 3.5 API & data standards (ISO 20022, RFC 9457, status vocabulary, rail IDs)

**Current (as-built):** the idempotency design is strongly standards-aligned. Rail-ready
identifiers shipped (Rec 18) — a bank-minted **UETR** UUIDv4 + originator `end_to_end_id`
on `transfers`, surfaced on the contract and folded into the idempotency fingerprint. The
status vocabulary is now **dual** (Rec 20 shipped): a computed, never-stored `status_iso`
maps the private status set onto the ISO-20022 ExternalPaymentTransactionStatus family
(`PDNG`/`ACSC`/`RJCT`/`CANC`; `iso_status()` in `00008_transfers.sql`), surfaced additively
on `Transfer`/`TransferListItem`/`TransferResult` alongside the flat `status` (mapping +
rationale in [`../12-rail-readiness.md`](../12-rail-readiness.md) §4). `currency` is now
explicit (ISO-4217) on every money-bearing **response** (Rec 19 subset shipped). The one
remaining private-dialect item is the error body: a flat `{error, message}`, not RFC 9457
`application/problem+json` (Rec 17, deferred).

| # | Rec | P | Effort |
|---|-----|---|--------|
| 17 | **Migrate the error model to RFC 9457 `application/problem+json`** `{type (stable URI per class), title, status, detail, instance}` so clients branch on `type`, not prose. **Deferred** pending a coordinated bump across all three fraudbank clients (it changes the error content-type). *(draft-07 itself cites RFC 7807; use its successor **9457** — do not attribute 9457 to the draft.)* | P0 | M |
| 19 | **Additive-only contract CI + cross-client DTO conformance** (the remainder of Rec 19). `currency` is now explicit on all money-bearing **responses**; **request-side `currency` is deliberately omitted** — the server derives it from the debit account ([`../12-rail-readiness.md`](../12-rail-readiness.md) §5). Open scope: enforce additive-only contract CI (fail on removed/renamed fields or narrowed enums) and **extend conformance to the hand-written iOS/Android DTOs** (only web is checked today). | P1 | S |

### 3.6 Fraud-UX backend enablers (decision/warning + events feed)

**Current (as-built):** the `GET /me/events` feed shipped (Rec 21; as-built
[`../06-client-api.md`](../06-client-api.md) §1) — per-user append-only, keyset-paginated,
`unread_count` + `/me/events/read`, **written in the same txn as the cause**, replacing
poll-on-focus and enabling a badge + "new sign-in" alert. The **adaptive-fraud surfaces
now ship** (Recs 22 & 23): a `transfer.held` event type notifies the payer when a payment
is parked, and the risk-decision endpoint `POST /transfers/intent` (read-only preflight,
Rec 22) returns `{decision: allow|warn|step_up|review|block, risk_band, reason_codes[],
warning:{warning_id, category, severity, headline, body, required_ack, cooling_off_seconds},
step_up_method}` — server-driven copy from a console-tunable `warning_rules` table
(generalising the fixed `assess_transfer_risk` weights), **with the numeric score never
surfaced**; the PWA renders the warning card with correct ARIA roles + an ack checkbox +
cooling-off countdown. The **held / under_review lifecycle** (Rec 23) adds both parked
states with `hold_reason`/`hold_expires_at` (business-day delay clock, cf. FCA FG24/6), a
customer confirm/cancel action for `held`, and screening routed to the maker-checker queue
for `under_review` (as-built [`../03-ledger-lifecycle-idempotency.md`](../03-ledger-lifecycle-idempotency.md) §1/§2.8,
[`../06-client-api.md`](../06-client-api.md) §8, [`../05-admin-ui.md`](../05-admin-ui.md) §4.4a/§4.8). The mule risk signal on
resolve also shipped (Rec 11, §3.3).

**Gaps:** velocity/daily-limit meters + new-payee cooling fields (Rec 24) are still open.

| # | Rec | P | Effort |
|---|-----|---|--------|
| 24 | **Velocity/daily-limit + new-payee cooling fields:** a limits endpoint (`daily_limit_minor/daily_used_minor/daily_remaining_minor/count_today` + the existing per-txn cap) and `beneficiaries.{added_at, payment_count, first_payment_completed, cooling_off_until}` so clients render limit meters + first-payment friction. | P2 | M |

### 3.7 Observability, audit & AML/sanctions

**Current (as-built):** audit is strong-by-construction for money (`admin_actions`,
maker-checker 4-eyes, `reconcile()`), and the **AML/sanctions name-screening gate now
ships** (Rec 25). A console-managed, demo-seeded `watchlist_entries` list (ILIKE
patterns against a party's registered name) is checked by `screen_payment`, which runs
**inside `transfer()` between authorize and capture**; a hit parks the payment
`under_review` (`hold_reason='screening'`, a 4-business-day window) and files a
`screening_hold` row into the existing maker-checker queue **rather than auto-posting** —
and it is **never auto-released**: an operator releases (`approve_request`, posting via
`post_transfer` allow-from `under_review`) or refuses (cancels), all audited. The
`transfer()` auto-post convenience respects the gate (sentinel system/operator callers
bypass it) (as-built [`../03-ledger-lifecycle-idempotency.md`](../03-ledger-lifecycle-idempotency.md) §2.8,
[`../05-admin-ui.md`](../05-admin-ui.md) §4.4a/§4.9). *(Industry — Wolfsberg-summarised — guidance, not a
quoted mandate.)* Still open: PEP/onboarding screening (Rec 28) and the auditor-role
read views (Recs 26/27).

| # | Rec | P | Effort |
|---|-----|---|--------|
| 26 | **Append the full fraud decision trail to the audit feed** (every warning shown, ack, step-up result, screening decision, hold action) so the decision trail feeding the PSR Consumer Standard of Caution and the reimbursement file is reconstructable. Reuses the `admin_actions` pattern. | P1 | S |
| 27 | **Auditor read-only audit views** (pure read surface; overlaps Rec 5 — the `reconcile()` surface itself already ships as `GET /admin/reconcile`, so the open part is the auditor-role `admin_actions`/audit views). | P2 | S |
| 28 | **PEP/watchlist storage + onboarding screening** (distinct from per-payment screening; runs at account opening and on list updates). | P2 | M |

### 3.8 Resilience, recovery & rail-readiness

**Current (as-built):** strongest-possible for a closed core *because* the side effect
commits atomically with the key-claim (the degenerate ideal — no recovery point to
manage). The operational durability hole is **closed** (Rec 2/29): the stale-`in_progress`
sweep now reaps wedged keys. Sweeps are also independently schedulable (Rec 6 shipped,
§3.2). The rail-readiness seam is now **documented** (Recs 30/31 shipped as docs):
[`../12-rail-readiness.md`](../12-rail-readiness.md) writes the outbox / at-least-once
relay / idempotent rail-submit (keyed by the deterministic UETR/`end_to_end_id`) /
recovery-point / asymmetric-`pacs.004`-saga checklist (§2) and the BIAN Payment
Order vs Payment Execution seam at the `post_transfer(id, allow_from)` boundary (§3) —
**building nothing**, since bank0 has no external creditor to trigger it.

**Gaps:** none tracked — the rail itself is deliberately unbuilt (§2 RESOLVED).

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
| End-to-end trace reference on a payment / for support | Server-minted UETR + `end_to_end_id` on `Transfer` + events (**shipped**, Rec 18); ISO-20022 `status_iso` also on the contract (**shipped**, Rec 20) | P2 |

## 5. Guided transfer v2 — SHIPPED (retired)

> ✅ **Shipped and retired.** `GET /transfers/suggestion` returns the up-to-3
> third-party "mule" options wrapper (resolver `suggest_transfer_destinations` in
> `db/migrations/00012_guided_scenarios.sql`); the PWA picks one at random and synthesises
> the own-account fallback when empty. As-built:
> [`../06-client-api.md`](../06-client-api.md) §1 + `00012_guided_scenarios.sql`.

## 6. Sequencing

**Done (collapsed):** Wave 0 (Recs 1, 2/29) — ERRCODE→HTTP map + replay stored body,
stale-`in_progress` sweep. Wave 1 (Recs 9, 10, 21) — server-side CoP verdict, warning
evidence, `/me/events` feed. Wave 2 (Recs 13, 14) — TOTP MFA + dynamically-linked
step-up. Wave 3, in full (Recs 11, 12, 15 — recipient/mule risk on resolve, PSR dispute
claim machine, TRA seam; **plus the adaptive-fraud remainder** Recs 22 — server-driven
warning/decision endpoint `/transfers/intent` + `warning_rules` + `transfer.held` event,
23 — held/under_review lifecycle + customer confirm, 25 — sanctions/AML `screen_payment`
gate + watchlist + console screening queue). Plus Rec 18 (UETR/`end_to_end_id`), Rec 3
(per-owner idempotency namespace), **Rec 4 (documented idempotency semantics on all
mutating money POSTs + idempotent second reverse)**, and guided-transfer v2 (former §5).
**Plus, now collapsed in:** Rec 6 (schedulable `bank0 maintenance` one-shot), Rec 19
subset (`currency` on all money-bearing responses), Rec 20 (computed `status_iso` on the
transfer contract), and **all of former Wave 5** — Recs 30/31 shipped as the
rail-readiness seam **documentation** ([`../12-rail-readiness.md`](../12-rail-readiness.md)),
building nothing (§2 RESOLVED).

**Next:**

- **Wave 4 — standards depth, edge surfaces (P1/P2, additive):** Recs 5/27, 16, 24, 26, 28,
  plus the **Rec 19 remainder** (additive-only contract CI + iOS/Android DTO conformance).
- **Deferred:** Rec 17 (RFC 9457) — waits on a coordinated bump across all three fraudbank
  clients (it changes the error content-type); Recs 7 (partial capture) and 8 (ISO-4217
  metadata table) — **deferred-as-YAGNI**, triggers in [`../12-rail-readiness.md`](../12-rail-readiness.md) §5.

## 7. Effort summary (remaining recs only)

| Priority | Recs | Rough size |
|---|---|---|
| **P0 (deferred)** | 17 | M — coordinated three-client bump |
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
