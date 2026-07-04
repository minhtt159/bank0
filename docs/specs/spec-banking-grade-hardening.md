# Spec — Banking-grade hardening & guided-transfer v2

> **Status: recommendation spec / roadmap. Waves 0 AND 1 + the Rec-18 pre-work
> are SHIPPED (2026-07-04); the rest is open.** Wave 1 as-built: Rec 21 (the
> `GET /me/events` feed + unread badge + mark-read, per spec-notifications-events
> — emissions ride the source txns in post_transfer / issue_refresh_token /
> resolve_dispute; append-only), Rec 9 (server-side CoP verdict on
> `/beneficiaries/resolve?name=` — match/close_match/no_match/unable +
> reason_code + suggested_name on close_match + server gate), and Rec 10
> (`POST /me/warning-acks` — append-only warned-and-proceeded evidence).
> Wave 0 as-built: Rec 1 (ERRCODE →
> HTTP map incl. 55006→409 / fingerprint→422 / 42501→403 / 28000→401, replay
> honoured + `Idempotency-Replayed: true` on `/transfers`, `/auth/register`,
> `/me/accounts`), Rec 2/29 (`cleanup_idempotency_keys` reaps committed
> `in_progress` orphans >15 min — note bank0's claims complete inside ONE
> transaction, so such orphans are unreachable today; the reap guards future
> multi-statement flows like step-up MFA), and Rec 18 (bank-minted `uetr`
> UUIDv4 + originator `end_to_end_id` on `transfers`, surfaced on the contract
> and folded into the idempotency fingerprint). Rec 17 (RFC 9457) is
> deliberately deferred: it changes the error content-type for all three
> fraudbank clients and needs a coordinated bump. Consolidated from a
> 2026-06-15 multi-track research pass (idempotency & exactly-once, ledger/money
> correctness, payee verification & APP-fraud regulation, SCA & transaction risk,
> ISO 20022 / API standards, anti-fraud UX → backend enablers, observability /
> AML). It is **additive**: every recommendation is a vendored-OpenAPI edit + a
> goose migration (DDL + PL/pgSQL) consistent with the DB-first architecture —
> **not a re-architecture**. The companion line-level specs it leans on are
> the retired spec-step-up-mfa and spec-notifications-events (both now as-built); the shipped
> guided endpoint it evolves (v1) is documented as-built in [`../06-client-api.md`](../06-client-api.md) §1.
>
> **Confidence & hedges.** Facts from EUR-Lex, IETF, the UK PSR and the EU Instant
> Payments Regulation are high-confidence. EPC primary PDFs (rulebooks, VOP API
> spec, R-transaction reason-code guidance) returned HTTP 403 to direct fetch and
> were corroborated via strong secondaries — so **exact EPC article locators and
> the VOP wire-level outcome code tokens are hedged** (the four *semantic*
> outcomes are certain; the literal token strings are not). **PSD3 / the new PSR
> are only at provisional trilogue agreement (27 Nov 2025), not adopted** — all
> PSD3/PSR specifics are forward-looking; SEPA-Instant Verification of Payee under
> Reg (EU) 2024/886 **is** binding and stated as fact. AML "screen before
> settlement" is industry guidance (Wolfsberg-summarised), not a quoted mandate.

---

## 0. How to read this

Section 1 says what is already banking-grade and must not be touched. Section 2
is the single hardest architectural problem. Section 3 is the eight pillars, each
with bank0's **current state → target → gaps → recommendations** (every
recommendation carries a `P0/P1/P2` priority and an `S/M/L/XL` effort). Section 4
maps each client-side fraud-UX feature to the backend capability it needs (the
"what's still missing that the backend must provide" question). Section 5 is the
concrete **guided-transfer v2** design (3 options, client picks 1, own-account
fallback). Section 6 sequences the work into waves; Section 7 is the effort
summary; Section 8 is sources + confidence.

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

**Current:** best-in-class for a closed core (see §1). Header required on
`/transfers`, `/transfers/{id}/reverse`, deposit/withdraw; 7-day `expires_at`,
reaped by `cleanup_idempotency_keys`.

**Target:** wire the IETF `Idempotency-Key` draft-07 contract at the HTTP layer.

**Gaps:** the PL/pgSQL primitives are right but the **HTTP contract isn't mapped** —
in-flight surfaces as a raw `object_in_use`, fingerprint-mismatch as a raw
`check_violation`, and a replay returns only `transfer_id+status` rather than the
stored `response` JSONB; the reaper **never reaps `in_progress`**, so a
crash-mid-claim key is wedged at 409 forever; `idempotency_keys.key` is a **global
PK** with no owner binding.

| # | Rec | P | Effort |
|---|-----|---|--------|
| 1 | **Map ERRCODEs → standards-correct HTTP + replay the stored body.** `object_in_use`→**409** (+ optional `Retry-After`); fingerprint-mismatch `check_violation`→**422**; funds/limit→**422**; illegal state transition→**409**; missing key→**400**. On `was_replay`, return the stored `response` JSONB with the original status code + an `Idempotency-Replayed: true` header. Handler + contract only. | P0 | M |
| 2 | **Stale-`in_progress` sweep.** Extend `cleanup_idempotency_keys` with an `in_progress` age threshold (e.g. `created_at < now()-'15 min'`) to reap/flag wedged keys; optional `locked_at` heartbeat if a re-drive (vs delete) is wanted. Run on **pg_cron**, not an app ticker. | P0 | S |
| 3 | **Bind the key namespace to the principal.** Change uniqueness from `PK(key)` to per-owner (`PK(owner_user_id, key)` or fold the JWT subject into the fingerprint) so one customer's key can never collide with or surface another's stored result. Clients unaffected. | P1 | M |
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
| 5 | **Auditor-facing read-only `reconcile()` + `admin_actions` feed** so the "who authorised what, with which approver, and the invariant proofs" are queryable read-only. | P1 | S |
| 6 | **Schedule the maintenance runners on pg_cron** (`expire_holds()`, `cleanup_idempotency_keys()`) so available balances stay correct independent of the app ticker; surface run results. | P1 | S |
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

**Current:** skeleton without the regulatory substance. `/beneficiaries/resolve`
returns **only** `{account_id, iban, owner_name_masked}`; the match/close/no-match
**verdict is computed client-side** (the `matchPayeeName` heuristic, ported ×3).
`guided_scenarios` drives the APP-scam mule steer (the one genuinely
backend-driven fraud surface). Disputes exist but are **flag-only** (audit row, no
auto-freeze, no reimbursement machine).

**Gaps:** no server verdict enum, no reason code, **no returned actual name on a
close match** (CoP/VOP both require it); no server-driven gate status (each app
recomputes `copBlocks`/`canContinue` — the exact close-match drift the team
already had to fix once); **no warning-shown/acknowledged evidence** (the IPR/PSR
liability pivot); no recipient/mule risk signal on resolve; disputes carry no
reimbursement amount, SLA clock, recall status, or scam-type mapping.

| # | Rec | P | Effort |
|---|-----|---|--------|
| 9 | **Move the CoP/VOP verdict server-side onto `/beneficiaries/resolve`.** Return `{match_result: match\|close_match\|no_match\|unable, reason_code, suggested_name (the actual registered name on close_match, ≤140 chars), account_type: personal\|business, checked_at}` + a **gate status** `ok\|awaiting_acknowledgement\|blocked`. All three clients then gate identically *by construction.* bank0 is its own (intra-bank, simulated) VOP responder. *Hedge the literal VOP code tokens.* | P0 | M |
| 10 | **Persist warning-shown / warning-acknowledged evidence** `{warning_id, category, reason_code, acked_at, device}` tied to the transfer attempt (reuse the `admin_actions` pattern). This is the IPR/PSR liability pivot and the input to the Consumer Standard of Caution; today only the local client risk seam fires. | P0 | M |
| 11 | **Add a recipient-risk / mule signal to resolve:** `{recipient_risk: low\|medium\|high, mule_suspected, signals[]: new_payee\|guided_steer\|mule_flagged\|recently_changed_details, is_first_payment_to_payee}` — the seeded mule target resolves **high**. Generalise `guided_scenarios` into the rule seam. | P1 | M |
| 12 | **Enrich disputes into a PSR claim machine:** status timeline + `sla_due_at` (business-day clock, stop-the-clock), `decision`, `reimbursed_amount_minor`, `recall_status` (requested/funds_returned/refused/none) + `recall_reason` (e.g. `FRAD`), `scam_type`, cap / excess / `vulnerable_flag`. A closed core can only *simulate* the interbank recall. **Two SEPA deadlines:** originator initiates the recall within **10 banking business days**; the beneficiary bank answers within **15 banking business days** *(exact EPC locator hedged).* | P1 | M |

### 3.4 SCA & transaction risk (PSD2, step-up, TRA)

**Current:** **Rec 13 SHIPPED (2026-07-04)** — TOTP MFA (RFC 6238, AES-GCM seed
at rest, hashed one-time recovery codes, DB-side lockout), `mfa_required` login
branch + `/auth/mfa/verify`, `amr`/`auth_time` claims, and the 403
`step_up_required` gate on high-value/new-payee transfers (same-key retry). The
planning spec (spec-step-up-mfa.md) is retired; as-built in ../06-client-api.md §6.

**Gaps:** no second factor; even once the spec ships, bare TOTP is **not
dynamically linked** (PSD2 RTS Art. 5 — the same 6 digits authorise any
amount/payee in the window); no server-side TRA; new-payee detection exists in
data but isn't wired to a gate.

| # | Rec | P | Effort |
|---|-----|---|--------|
| 13 | **SHIPPED — TOTP MFA + step-up as specced** (retired spec; as-built in ../06-client-api.md §6): RFC 6238, AES-256-GCM seed at rest, hashed recovery codes, 403 `step_up_required` before the key is claimed, `amr`/`auth_time`, same-key retry. | P0 | L |
| 14 | **Bind the step-up challenge to `(debit│credit│amount)` for dynamic linking** (PSD2 RTS Art. 5 / WYSIWYS) — reuse the existing idempotency tuple so changing amount or payee invalidates the code. The reserved `webauthn` mfa_kind gives a future passkey-bound path. | P1 | M |
| 15 | **Server-side TRA seam at `request_transfer` time** scoring prior pattern, velocity (count/value trailing 24h–90d), device/location anomaly, and known-mule lists (`guided_scenarios` already models a mule), emitting a risk decision the gate ORs into its trigger set. The client SDK is **advisory input only**; the authoritative decision lives server-side. *(TRA exemption ETV thresholds €500/€250/€100 → 0.005/0.010/0.015 % fraud-rate are conditional and revocable.)* | P1 | L |
| 16 | **Gate beneficiary creation (RTS Art. 13) + expose `step_up_limit_minor`** so clients can pre-warn that an amount will demand step-up before submit. | P2 | S |

### 3.5 API & data standards (ISO 20022, RFC 9457, status vocabulary, rail IDs)

**Current:** the idempotency design is strongly standards-aligned (its biggest
strength). But the surrounding contract is a **private dialect**: a flat
`{error, message}` body (not RFC 9457 `application/problem+json`); a private status
set (`pending|posted|failed|canceled|reversed`), not the ISO 20022 family UK OBIE
and Berlin Group constrain to; **no UETR, no `EndToEndId`**; `currency` is a free
string omitted from `CreateTransferRequest`.

| # | Rec | P | Effort |
|---|-----|---|--------|
| 17 | **Migrate the error model to RFC 9457 `application/problem+json`** `{type (stable URI per class), title, status, detail, instance}` so clients branch on `type`, not prose. *(draft-07 itself cites RFC 7807; use its successor **9457** — do not attribute 9457 to the draft.)* | P0 | M |
| 18 | **Add rail-ready identifiers:** a server-minted **UETR-shaped UUIDv4** per transfer (stable across replays, decoupled from the internal id) + an originator-supplied **`end_to_end_id`** (~35 chars) on `CreateTransferRequest`, echoed on `Transfer` and every event. *(UETR is minted by the debtor agent — the bank — not the customer.)* | P1 | M |
| 19 | **Make `currency` explicit (ISO-4217) on every money-bearing schema + enforce additive-only contract CI** (fail on removed/renamed fields or narrowed enums) and **extend conformance to the hand-written iOS/Android DTOs** (only web is checked today). | P1 | S |
| 20 | **Map status onto an ISO-20022-aligned parallel `status_iso`** (`pending→PDNG/RCVD`, `posted→ACSC`, `failed→RJCT`, `canceled→CANC`, reverse via `pacs.004`) while keeping the flat `status` for back-compat. *(Berlin Group status-code list is medium-confidence.)* | P2 | M |

### 3.6 Fraud-UX backend enablers (decision/warning + events feed)

**Current:** clients can show a (client-computed) masked-name CoP, the
server-decided guided recipient, a single per-transfer limit, a flag-only dispute,
and a per-device sessions list. **Missing: everything that powers *adaptive* fraud
UX** — no risk-decision endpoint, no warning taxonomy, no held/under-review state,
no velocity/daily-limit fields, no payee mule signal, no server-driven warning
copy (warning text + a11y labels are hardcoded ×3 and must be hand-synced), and
the **events feed is spec'd but unbuilt** (clients poll-on-focus and diff).

| # | Rec | P | Effort |
|---|-----|---|--------|
| 21 | **SHIPPED — the `GET /me/events` feed** (spec retired; as-built in ../06-client-api.md §1): per-user append-only (`transfer.posted\|payment.incoming\|device.new\|dispute.updated` + `fraud-alert\|hold`), keyset-paginated, `unread_count` + `/me/events/read`, **written in the same txn as the cause** so a notification never exists without its cause. Replaces poll-on-focus; enables a badge + "new sign-in" alert. | P0 | M |
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
| 27 | **Auditor read-only `reconcile()` + audit views** (pure read surface; overlaps Rec 5). | P2 | S |
| 28 | **PEP/watchlist storage + onboarding screening** (distinct from per-payment screening; runs at account opening and on list updates). | P2 | M |

### 3.8 Resilience, recovery & rail-readiness

**Current:** strongest-possible for a closed core *because* the side effect commits
atomically with the key-claim (the degenerate ideal — no recovery point to
manage). The **one durability hole** is operational: the reaper never reaps
`in_progress` and the sweepers ride an app ticker.

| # | Rec | P | Effort |
|---|-----|---|--------|
| 29 | **Close the operational durability hole now** (= Recs 2 + 6: stale-`in_progress` sweep + pg_cron). The only resilience gap that matters while the core stays closed. | P0 | S |
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
| Notification badge + incoming-payment + "new sign-in" alerts | `GET /me/events` feed (spec'd, unbuilt) | P0 |
| Branch on error class without string-matching prose | RFC 9457 `problem+json` with a stable `type` URI per class | P0 |
| High-value / new-payee step-up, code bound to this exact amount+payee | Step-up MFA + dynamic-linking challenge `hash(debit│credit│amount│kind)` | P0 |
| "High-risk / newly-opened / reported" destination badge; first-payment friction | Recipient-risk on resolve + new-payee cooling fields | P1 |
| Category-specific scam interstitial copy, tunable without an app release | Warning/decision endpoint with server-side rule table | P1 |
| "Payment under review / held" with a clock + release action | `held`/`under_review` statuses + hold metadata | P1 |
| Dispute / scam-claim timeline with the regulatory clock + reimbursement/recall | Dispute enrichment (SLA, decision, recall, scam_type, cap/excess) | P1 |
| Remaining daily/transaction limit meter + pre-warn step-up | Limits endpoint + `step_up_limit_minor` | P2 |
| Anti-impersonation "we aren't calling you" banner | `GET /me/call-status` (Starling/Monzo pattern) | P2 |
| End-to-end trace reference on a payment / for support | Server-minted UETR + `end_to_end_id` on `Transfer` + events | P2 |

## 5. Guided transfer v2 — three options, client picks one, own-account fallback

> ✅ **IMPLEMENTED (`feat/guided-mule-menu`, migration `00032`).** `GET /transfers/suggestion`
> returns `{"options":[…]}` (resolver `suggest_transfer_destinations`); the PWA picks one at
> random and synthesises the own-account fallback when empty. The rest of this doc remains a
> roadmap. Open question 5.6 resolved per the recommendation: the client picks once per
> `from_account`/amount change (matches the existing clear-on-change behaviour).

> Evolves the shipped guided-transfer v1 endpoint (as-built: [`../06-client-api.md`](../06-client-api.md) §1).
> Still **read-only; never moves money** — selecting a suggestion still goes
> through `POST /transfers` (idempotency-key). Same privacy envelope as
> `/beneficiaries/resolve` (masked name + iban + account_id only).

### 5.1 Rationale

Today `GET /transfers/suggestion` returns **one** dictated account, and bank0's
resolver falls back to the caller's *own* other account when no scenario applies.
v2 makes the demo a sharper APP-scam simulation and removes the "steered to my own
account" smell: the backend offers a **short-list of up-to-3 third-party "mule"
accounts** chosen at random, the client **dictates one of the three at random**,
and the **own-account path becomes the explicit client-side fallback** used only
when the backend has no mule to offer. This also reconciles the fraudbank client
guard added 2026-06-15 (reject any own-account suggestion): the backend now returns
**only third parties**, so the guard never trips on a backend option; the
own-account dictation is a deliberate client fallback, not a backend response.

### 5.2 API change (breaking — coordinated bump)

`GET /transfers/suggestion` response changes from a single object to an
**options wrapper**:

```jsonc
// 200
{
  "options": [            // 0..3 entries, each a TransferSuggestion (existing shape)
    { "account_id": "…", "iban": "NL…", "owner_name_masked": "M**** E*****",
      "reason": "Recommended payee", "scenario": "app-scam-demo", "source": "scenario" }
  ]
}
```

- `source` on every backend option is `scenario` (mule). `own_account` is **no
  longer returned by the backend**; the client synthesises an own-account
  dictation locally when `options` is empty.
- **`200 {"options": []}`** replaces the old `204` for "nothing to suggest". (Keep
  `204` accepted by clients for one release if a softer migration is wanted.)
- This is the **deliberate one-time exception** to the additive-only contract gate
  (Rec 19): a wrapper is introduced once so future fields (a selection token, a
  per-option risk band) are additive thereafter. Bump the vendored OpenAPI →
  regenerate Go (oapi-codegen) → update the three client DTOs + the random-pick +
  the own-account fallback + reconcile the existing client guard.

### 5.3 Backend selection algorithm

A new resolver `suggest_transfer_destinations(p_caller, p_from_account, p_amount_minor)`
returning **up to 3 rows** (parallel to today's single-row function):

1. Build the **eligible pool** = active `guided_scenarios` targets matching the
   caller + amount (per-user targeting beats global, as today), whose target is an
   **active customer account**, **excluding the debit account** and **excluding any
   account owned by the caller** (so every option is a third party).
2. `SELECT … ORDER BY random() LIMIT 3` over the eligible pool (distinct accounts).
   If the pool has 1–2, return those; if empty, return **no rows** → handler emits
   `{"options": []}`.
3. Mask owner names via `mask_name()` exactly as `/beneficiaries/resolve`.

> **Priority vs random (decision):** today the single-row resolver orders by
> per-user-first, then priority, then recency. v2 samples **at random** per the
> requirement. Keep per-user/amount targeting as a *filter* for which scenarios are
> eligible, then randomise *within* the eligible set. (Open question 5.6.)

### 5.4 Client behaviour (web / iOS / Android)

- Fetch `options`. If non-empty, **pick one at random** to dictate; the
  force-type gate (`Iban.normalizedExactEqual` / `matchIban().complete` /
  `guidedIbanMatches`) is unchanged.
- If `options` is empty → **fall back to dictating one of the caller's own other
  active accounts** (the prior safe default), marked `source: own_account`
  client-side. The own-account guard applies only to *backend* options, so it does
  not block this deliberate fallback.
- The client already clears the typed IBAN when the suggestion changes — so a
  re-fetch that yields a different pick re-arms the field cleanly.

### 5.5 Data model

`guided_scenarios` already supports multiple rows, so no schema change is strictly
required — the pool is "all active eligible scenario targets". Optional additions
for richer demos: a `weight`/`priority` to bias the random draw, or a dedicated
`mule_pool` flag. The mule short-list seeded 2026-06-15 (`app-scam-demo` →
`NL80KNAB9000000099`, "Markus Eklund") becomes one member of the pool; seed 2–3
more mule targets so a full 3-option draw is possible.

### 5.6 Edge cases & open questions

- **Re-fetch instability:** random selection means a re-fetch yields a different
  3 and the client a different pick. Acceptable for a demo, but decide: **(a)** the
  client picks once and caches per `from_account` until From changes (recommended —
  matches the existing clear-on-change behaviour), or **(b)** seed the server
  randomness per `(caller, from_account, day)` for stability.
- **Pool < 3:** return what exists (1–2). Single-option pool degenerates to v1
  behaviour (one dictated mule) — still correct.
- **Amount gating:** an amount-thresholded scenario only enters the pool once
  `amount_minor` meets its `min_amount_minor` — so the option set can change as the
  user types an amount; the client re-fetches on amount change (as today).

### 5.7 Effort

**M** — one new PL/pgSQL resolver, a handler change, the OpenAPI wrapper bump +
regenerate, and the three client updates (DTO + random pick + own-account
fallback). No engine/ledger change; no money-movement change.

## 6. Sequencing

- **Wave 0 — correctness & operational safety (P0, S/M, no new features):** Recs
  1, 2/29, 17. Map ERRCODEs + replay stored body; stale-`in_progress` sweep +
  pg_cron; RFC 9457 errors. Widest leverage, no engine change — unblocks every
  client's ability to branch on outcome.
- **Wave 1 — server-authoritative payee verification + the feed (P0):** Recs 9,
  10, 21. The regulatory liability pivot + the biggest UX-consistency win (kills
  client-side CoP drift) + replaces poll-on-focus.
- **Wave 2 — SCA (P0/P1, heaviest single item):** Rec 13 then 14. Do MFA *after*
  Wave 0 so the 403/replay contract it relies on is already correct.
- **Wave 3 — risk-driven adaptive fraud UX (P1):** Recs 22, 23, 11, 15, 12, 25.
- **Wave 4 — standards depth & rail pre-work (P1/P2, additive):** Recs 18, 19, 20,
  5/27, 7, 8, 24. **Guided transfer v2 (§5) slots here** (additive contract bump).
- **Wave 5 — defer until a real rail exists:** Recs 30, 31 — document the seam now
  (UETR/`end_to_end_id` from Wave 4 is the cheap pre-work), build nothing.

## 7. Effort summary

| Priority | Recs | Rough size |
|---|---|---|
| **P0** | 1, 2/29, 9, 10, 13, 17, 21 | 1 × L (MFA) + the rest S/M — the highest-leverage, mostly contract+handler |
| **P1** | 3, 5, 6, 11, 12, 14, 15, 18, 19, 22, 23, 25, 26 | the regulator-facing substance + adaptive UX; 2 × L (TRA, AML) |
| **P2** | 4, 7, 8, 16, 20, 24, 27, 28, 30, 31, **guided v2 (M)** | additive standards/rail-readiness; never blocks the closed core |

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
