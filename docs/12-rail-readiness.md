# bank0 — Rail-readiness (the closed-core → real-rail seam)

> How bank0 stays honest about the one architectural fact it cannot engineer
> away: its correctness is **load-bearing only because the core is closed**. This
> doc is the seam — what breaks the day a real external payment rail is attached,
> what the contract has already been shaped to absorb, and what is deliberately
> **left unbuilt**. Read [`03-ledger-lifecycle-idempotency.md`](03-ledger-lifecycle-idempotency.md)
> first; this is the resolution of the spec's hardest problem
> ([`specs/spec-banking-grade-hardening.md`](specs/spec-banking-grade-hardening.md) §2).

---

## 1. Why this document exists

bank0's whole correctness story rests on one move: `request_transfer` (in
[`00008_transfers.sql`](../db/migrations/00008_transfers.sql)) **claims the
idempotency key, validates, writes the double-entry ledger legs, records the
completion — all in ONE Postgres transaction.** Because the side effect commits
atomically with the key-claim, bank0 needs **none** of the distributed machinery
the industry built to paper over non-atomic side effects: no transactional
outbox, no at-least-once relay, no inbox/consumer-dedup, no recovery-point
checkpoints, no saga. `post_transfer` = capture; `reverse_transfer` = an
append-only inverse. It is correct *by construction* (§1 of the spec).

That guarantee **evaporates the moment a real external rail is interleaved**,
because you cannot put a network call (SEPA/SCT-Inst submission, a card
authorization, a `pacs.008` hand-off) inside a database transaction. At that
point every property bank0 gets for free must be rebuilt with the full
distributed stack, and — worse — the compensations become **asymmetric**: a
settled interbank credit **cannot be unilaterally clawed back** with an `UPDATE`;
it becomes a scheme-governed `pacs.004` recall/return under an SLA.

The trap is twofold:

- **(a)** Building any of that *now* needlessly complicates a demo whose closed
  core is explicitly "do not re-architect." So we build none of it.
- **(b)** Shipping client-facing semantics that *silently assume synchronous
  atomicity* — instant-final `posted`, a single flat status, no trace id, a
  client-computed CoP verdict — would bake in a contract the rail later violates,
  forcing a **breaking** client change at the worst possible moment.

**The resolution (spec §2): make the contract rail-ready *additively*, build no
rail.** The cheap, additive pre-work has shipped — a bank-minted `uetr` +
originator `end_to_end_id` (Rec 18), an ISO-20022-aligned parallel `status_iso`
(Rec 20), the fraud verdict + warning evidence moved server-side so they survive
an async future — and the outbox/saga/recovery-point machinery lives here as
**documentation**. The day a rail is added, the core converges on the
Stripe/brandur design behind a contract the clients already speak — **zero
breaking change**.

---

## 2. Rec 30 — the rail-readiness checklist (do NOT build yet)

Each item below is what a real rail would demand, **where in bank0 it would
attach**, and the **trigger** that would justify building it. Until a trigger
fires, **build none of it** — the closed core is strictly better without it.

| # | Capability | What it is | Where it attaches | Build trigger |
|---|---|---|---|---|
| 1 | **Transactional outbox** | An `outbox` table written **in the same txn** as the ledger legs, carrying the rail instruction (debtor/creditor agent, UETR, amount). The atomic write is the whole point — it inherits `post_transfer`'s transaction. | A new table + one `INSERT` inside `post_transfer` ([`00008`](../db/migrations/00008_transfers.sql)), gated on `kind`/destination being external. | First transfer whose `credit_account` is **not** an internal `accounts` row (a real external creditor agent). |
| 2 | **At-least-once relay** | A worker that reads unsent `outbox` rows and submits them to the rail, marking them sent; crash-safe because the row is durable and the submit is idempotent (#3). | A new relay loop next to the maintenance sweep (`RunMaintenance`, [`internal/db/bank.go`](../internal/db/bank.go)); reuses the advisory-lock pattern so only one replica relays per tick. | Alongside #1 — an outbox with no relay is inert. |
| 3 | **Idempotent rail-submit keyed by UETR/EndToEndId** | The rail consumer must dedup retries. The key is the **deterministic** `uetr` (bank-minted at insert, stable across replays) plus the originator `end_to_end_id`; the submit is safe to retry because the rail dedups on it. | The relay's submit call; the key material already exists on `transfers` (Rec 18). | Alongside #2. |
| 4 | **Recovery-point checkpoints** | Durable markers of "how far the relay got" so a mid-flight crash resumes without double-submitting. In a closed core there is **no** recovery point to manage (the degenerate ideal, spec §3.8); a rail introduces the first one. | Relay bookkeeping (last-sent cursor / per-row state machine on the `outbox`). | When #2 exists and the rail's ack is asynchronous (submit ≠ settle). |
| 5 | **Asymmetric saga (pacs.004, never UPDATE)** | Once the rail settles a credit, a "reversal" is **not** an inverse ledger write — it is a scheme recall/return request the counterparty may **refuse**. The saga's compensation is therefore a request with its own lifecycle, not a guaranteed rollback. | bank0 **already models this shape**: `disputes.recall_status` (`none → requested → funds_returned \| refused`) + `set_dispute_recall` ([`00013_disputes.sql`](../db/migrations/00013_disputes.sql)) is the simulated `pacs.004`. A rail wires it to a real scheme message. | When #1–#4 exist and a settled external credit must be recalled. |

**Why documentation is the right deliverable now:** every trigger above is
"a real external creditor exists." bank0 has none — every `credit_account_id` is
an internal `accounts` row (customer or the `EXTERNAL_CLEARING` GL, §4). Building
the outbox/relay/saga against a rail that doesn't exist adds crash windows and
distributed-failure modes to a core that currently has neither.

---

## 3. Rec 31 — the BIAN boundary seam (Payment Order vs Payment Execution)

BIAN splits a payment into two service domains that bank0 today fuses into one
transaction:

- **Payment Order** — the *instruction* and its lifecycle: request, validate,
  reserve funds, hold for confirmation/screening, cancel. In bank0 this is
  `request_transfer` + `place_transfer_hold` + `cancel_transfer` +
  `client_confirm_transfer` (all in [`00008`](../db/migrations/00008_transfers.sql)):
  the `transfers` row is the **order**, carrying `status`/`hold_reason`/
  `hold_expires_at`.
- **Payment Execution** — *settlement*: writing the ledger and moving the balance
  cache. In bank0 this is `post_transfer` + the `ledger_apply_to_balance` trigger.

**The seam is already a real function boundary: `post_transfer(id, allow_from)`.**
The `p_allow_from` argument is a safety fence (docs/03 §2.2) that names exactly
which order-states may cross into execution — `{pending}` by default, `{held}` for
`client_confirm_transfer`, `{under_review}` for the operator's `approve_request`.
That is precisely the Order→Execution handoff drawn as a guarded edge.

```
Payment Order (instruction + lifecycle)        │  Payment Execution (settlement)
──────────────────────────────────────────────┼───────────────────────────────
request_transfer → pending (+ hold)            │
place_transfer_hold → held / under_review      │
   client_confirm_transfer  ───{held}────────► │  post_transfer → ledger legs
   approve_request          ─{under_review}──► │       ↳ trigger writes balance
cancel_transfer → canceled                     │
                              ──{pending}─────► │  post_transfer (auto-post)
```

Today both sides run in **one** transaction, so the seam is invisible at runtime
— but it is a clean conceptual cut. A future rail separates them: the **order**
commits synchronously (funds reserved, client gets a `pending`/`PDNG` receipt),
and **execution** becomes the async rail submit + settlement ack that later flips
the order to `posted`/`ACSC`. Crucially, **the client contract does not move**:
the client already receives a `status` + `status_iso` and already tolerates a
non-`posted` outcome (`held`/`under_review` today; a future `pending`-then-settled
tomorrow). The seam can be pulled apart without a breaking change because it was
named, not smeared.

---

## 4. Seam inventory — what is already pre-shaped

The additive pre-work already in the tree, and the rail role each field plays:

- **`EXTERNAL_CLEARING` GL account** — cross-bank money is modelled against a
  system clearing account (`deposit`/`withdraw` in
  [`00008`](../db/migrations/00008_transfers.sql), seeded in
  [`00016_system_seed.sql`](../db/migrations/00016_system_seed.sql)) so the books
  stay zero-sum even for money "entering" or "leaving" the bank. This is the
  natural attach point for the outbox (§2 #1): a transfer whose external leg is
  the clearing account is the first candidate for real rail submission.
- **`uetr` + `end_to_end_id` + the idempotency fingerprint** — `uetr` is a
  bank-minted UUIDv4 (SWIFT UETR), minted once at insert and **stable across
  idempotent replays** (a replay never re-inserts); `end_to_end_id` is the
  originator's ISO 20022 reference, folded into the idempotency fingerprint
  `sha256(debit│credit│amount│kind│end_to_end_id)` so the same key with a
  different reference is a `422` mismatch. Together they are the **deterministic
  dedup key** a rail consumer (§2 #3) needs — already present, already stable.
- **`status_iso` incl. the reversed/ACSC/recall triple** — the ISO-20022
  projection (`iso_status()`, Rec 20) is **computed, never stored**, so it can be
  re-mapped without a migration. The load-bearing subtlety is the **reversal
  triple**: a `reversed` original stays `ACSC` (it *did* settle), the reversal
  transfer is its own `ACSC` row, and the interbank return is
  `disputes.recall_status`/`pacs.004` — exactly the asymmetric-saga shape (§2 #5).
  `posted → ACSC` is honest *today* (closed core: posting is settlement); when a
  rail arrives, an intermediate `pending → PDNG`-then-`ACSC` step slots in without
  the client relearning the vocabulary.
- **`events` as a same-txn projection seed** — `emit_event` writes the per-user
  feed **in the same transaction as its cause** (`transfer.posted`,
  `payment.incoming`, `transfer.held`; [`00014_events.sql`](../db/migrations/00014_events.sql)).
  That is the transactional-outbox *pattern* already in miniature: a durable,
  ordered projection written atomically with the ledger. A real outbox (§2 #1)
  generalises the same discipline to a rail instruction instead of a notification.
- **Per-owner idempotency namespace as a future rail-consumer dedup key** — the
  `idempotency_keys` PK is `(owner_id, key)` (Rec 3, docs/03 §3), not `key` alone.
  Cross-owner isolation is a client-safety property today, but the same namespaced
  key is exactly what a rail consumer would use to dedup **inbound** returns/recalls
  per originating principal without one principal's key colliding with another's.

---

## 5. The YAGNI ledger (deferred-as-YAGNI, with triggers)

Deliberately **not** built. Each is additive and safe to add later; none blocks
the closed core. The trigger is the condition that would flip it from YAGNI to
warranted.

| Deferred | What it would add | Build trigger |
|---|---|---|
| **Rec 7 — partial capture** | `post_transfer(amount_to_capture ≤ hold.amount_minor)`: post the captured legs, release the residual hold. Keeps the single-transaction shape. | A product need for authorize-now / capture-less-later (card-style incremental capture, tips/adjustments). No current flow captures less than it authorized. |
| **Rec 8 — ISO-4217 currency-metadata table** | A table carrying the minor-unit exponent per currency, so formatting/rounding are currency-driven rather than the hard-coded exponent-2 EUR assumption. Prerequisite for multi-currency / an FX-GL leg model. | The first non-EUR currency. Today `accounts.currency` is effectively single-valued and every amount is EUR minor units. |
| **Request-side `currency`** | Accepting `currency` on `CreateTransferRequest`. | Multi-currency (with Rec 8). **Deliberately omitted by design:** the server derives currency from the **debit account**, and `request_transfer` rejects a debit/credit currency mismatch — so a request-side currency would be redundant and a spoofing surface. `currency` now ships on money-bearing **responses** (Rec 19); requests **inherit** it. |

---

## 6. Cross-references

- Spec problem statement + resolution: [`specs/spec-banking-grade-hardening.md`](specs/spec-banking-grade-hardening.md) §2 (RESOLVED), §3.5 (`status_iso`, Rec 20), §3.8 (rail-readiness), §3.2 (Recs 7/8).
- Ledger lifecycle, `post_transfer(id, allow_from)`, reversal semantics: [`03-ledger-lifecycle-idempotency.md`](03-ledger-lifecycle-idempotency.md) §2.2 / §2.4 / §1.
- `transfers` schema, `uetr`/`end_to_end_id`, `status_iso` (computed): [`02-data-model.md`](02-data-model.md) §3.3.
- Client contract for `status_iso` + dispute `currency`: [`06-client-api.md`](06-client-api.md) §1 / §5.
- The DB objects: [`00008_transfers.sql`](../db/migrations/00008_transfers.sql) (`iso_status`, `post_transfer`, `EXTERNAL_CLEARING` wrappers), [`00013_disputes.sql`](../db/migrations/00013_disputes.sql) (`recall_status`, `set_dispute_recall`), [`00014_events.sql`](../db/migrations/00014_events.sql) (`emit_event`).
