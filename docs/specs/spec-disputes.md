# Spec — Disputes ("I don't recognise this")

> ✅ **IMPLEMENTED (2026-06-13, `feat/bff`).** Migration `00020_disputes.sql`, queries
> `db/queries/disputes.sql`, handlers `internal/api/handlers_disputes.go`, tests
> `disputes_test.go`. As-built: [`../06-client-api.md`](../06-client-api.md) §1 (client)
> + [`05-admin-ui.md`](../05-admin-ui.md) §4.7 (admin: JSON API **and** console screen). Flag-only fraud hook
> (no auto-freeze); illegal transitions → 409 via plain `P0001`. Retained for rationale.

> **Status: spec.** Implementation-ready. Completes the P-list in
> [`09-fraudbank-bff-plan.md`](../09-fraudbank-bff-plan.md) and the client stub in
> fraudbank (the receipt's "I don't recognise this" button currently only fires a
> local risk event — `risk.generalEvent("dispute", transfer_id)`; see §6). Adds a
> real disputes domain on both surfaces (client raise/track, admin queue/resolve).

---

## 1. Summary & rationale

A retail banking client needs a "**I don't recognise this transaction**" path: the
customer flags a transfer they believe is fraudulent or wrong, the bank opens a
**dispute case**, an operator triages it, and either resolves or rejects it. For
fraudbank this is the natural *down-stream* of Guided mode (the APP-scam victim
later realises and disputes) and the place a **server-side fraud signal** is
emitted.

This spec adds:

| Surface | Method | Path | Purpose |
|---|---|---|---|
| client | POST | `/transfers/{id}/dispute` | Raise a dispute on a transfer the caller is a party to → 201 |
| client | GET | `/disputes` | List the caller's disputes (cursor-paginated) |
| client | GET | `/disputes/{id}` | Track one of the caller's disputes |
| admin | GET | `/disputes` | Operator triage queue (filter by status, cursor-paginated) |
| admin | POST | `/disputes/{id}/resolve` | Resolve or reject a dispute (state machine + note) |

A `disputes` table holds case state; a status state machine governs transitions;
ownership (party-to-the-transfer) is enforced on the client surface; idempotency
prevents duplicate **open** disputes on the same transfer by the same user; and
opening a dispute **emits a fraud signal** (and can flag/freeze — §6). All business
rules live in PL/pgSQL; the Go layer maps typed errors to HTTP (project discipline,
[`01-overview.md`](../01-overview.md) P2/P5; same pattern as `00008`/`00014`).

The ledger stays **append-only**: a dispute never edits a posted transfer. Remedy
(refund / reversal) remains the operator's existing `reverse_transfer` action
(`00008`) — a resolution *may* trigger it, but the dispute row itself is the only
new mutable state, and only its own status/resolution fields change.

---

## 2. API — OpenAPI 3.1

Add to `api/openapi.yaml`. Note `/disputes` is defined under **both** tags but they
are different operations resolved per surface (mode `api` vs `portal`), exactly
like `/transfers/pending` (admin) coexisting with client transfer routes —
register the admin `GET /disputes` on the portal subrouter and the client
`GET /disputes` on the client subrouter. Use distinct `operationId`s so the two
generated interfaces don't collide (`listMyDisputes` vs `listDisputes`).

```yaml
  /transfers/{id}/dispute:
    post:
      operationId: raiseDispute
      tags: [client]
      summary: >-
        Raise a dispute on a transfer the caller is a party to ("I don't
        recognise this"). One open dispute per (transfer, caller).
      parameters: [ { $ref: "#/components/parameters/Id" } ]
      requestBody:
        required: false
        content:
          application/json:
            schema: { $ref: "#/components/schemas/RaiseDisputeRequest" }
      responses:
        "201":
          description: dispute opened
          content:
            application/json:
              schema: { $ref: "#/components/schemas/Dispute" }
        "404": { $ref: "#/components/responses/Error" }   # not a party / unknown transfer
        "409": { $ref: "#/components/responses/Error" }   # an open dispute already exists
        "422": { $ref: "#/components/responses/Error" }   # invalid category, transfer not disputable

  /disputes:
    get:
      operationId: listMyDisputes
      tags: [client]
      summary: List the caller's disputes (newest first, cursor-paginated)
      parameters:
        - { $ref: "#/components/parameters/Cursor" }
        - { $ref: "#/components/parameters/Limit" }
      responses:
        "200":
          description: ok
          content:
            application/json:
              schema:
                type: array
                items: { $ref: "#/components/schemas/Dispute" }

  /disputes/{id}:
    get:
      operationId: getDispute
      tags: [client]
      summary: Track one of the caller's disputes (scoped to raiser)
      parameters: [ { $ref: "#/components/parameters/Id" } ]
      responses:
        "200":
          description: ok
          content:
            application/json:
              schema: { $ref: "#/components/schemas/Dispute" }
        "404": { $ref: "#/components/responses/Error" }

  /admin/disputes:
    get:
      operationId: listDisputes
      tags: [admin]
      summary: Operator dispute triage queue (filter by status, cursor-paginated)
      parameters:
        - name: status
          in: query
          required: false
          schema: { type: string, enum: [open, under_review, resolved, rejected] }
        - { $ref: "#/components/parameters/Cursor" }
        - { $ref: "#/components/parameters/Limit" }
      responses:
        "200":
          description: ok
          content:
            application/json:
              schema:
                type: array
                items: { $ref: "#/components/schemas/DisputeQueueItem" }

  /admin/disputes/{id}/resolve:
    post:
      operationId: resolveDispute
      tags: [admin]
      summary: Move a dispute to resolved/rejected (or under_review) with a note
      parameters: [ { $ref: "#/components/parameters/Id" } ]
      requestBody:
        required: true
        content:
          application/json:
            schema: { $ref: "#/components/schemas/ResolveDisputeRequest" }
      responses:
        "200":
          description: updated
          content:
            application/json:
              schema: { $ref: "#/components/schemas/Dispute" }
        "404": { $ref: "#/components/responses/Error" }
        "409": { $ref: "#/components/responses/Error" }   # illegal state transition
```

> **Path note.** The admin operations live under `/admin/disputes` (mirroring the
> existing `/admin/reconcile`, `/admin/expire-holds`) to keep them clearly on the
> portal surface and avoid any `/disputes` vs `/disputes/{id}` cross-tag ambiguity
> in `mode=all`. The client keeps the unprefixed `/disputes` reads.

```yaml
    RaiseDisputeRequest:
      type: object
      properties:
        category: { type: string, enum: [unrecognised, fraud, wrong_amount, duplicate, other] }
        reason:   { type: string, description: "Free-text customer explanation (optional)." }
    Dispute:
      type: object
      properties:
        id:              { type: string, format: uuid }
        transfer_id:     { type: string, format: uuid }
        status:          { type: string, enum: [open, under_review, resolved, rejected] }
        category:        { type: string, enum: [unrecognised, fraud, wrong_amount, duplicate, other] }
        reason:          { type: string }
        resolution_note: { type: string }
        created_at:      { type: string, format: date-time }
        updated_at:      { type: string, format: date-time }
    DisputeQueueItem:
      type: object
      description: Admin queue row — joins transfer + raiser context for triage.
      properties:
        id:             { type: string, format: uuid }
        transfer_id:    { type: string, format: uuid }
        status:         { type: string }
        category:       { type: string }
        reason:         { type: string }
        amount_minor:   { type: integer, format: int64 }
        currency:       { type: string }
        raised_by:      { type: string, description: "Raiser username." }
        debit_iban:     { type: string }
        credit_iban:    { type: string }
        created_at:     { type: string, format: date-time }
    ResolveDisputeRequest:
      type: object
      required: [status]
      properties:
        status:          { type: string, enum: [under_review, resolved, rejected] }
        resolution_note: { type: string }
```

---

## 3. Data model

### 3.1 Enums + `disputes` table

Two new enums (added in the migration, mirroring `00002` style). `disputes` is the
only new mutable table; it references `transfers` and `users` but holds **no money
state** and never edits the ledger.

| Column | Type | Notes |
|---|---|---|
| `id` | `UUID` PK | `uuidv7()` |
| `transfer_id` | `UUID` FK → `transfers(id)` `ON DELETE RESTRICT` | the disputed transfer |
| `raised_by_user_id` | `UUID` FK → `users(id)` `ON DELETE RESTRICT` | must be a party to the transfer |
| `status` | `dispute_status` NOT NULL DEFAULT `'open'` | `open` \| `under_review` \| `resolved` \| `rejected` |
| `category` | `dispute_category` NOT NULL DEFAULT `'unrecognised'` | `unrecognised` \| `fraud` \| `wrong_amount` \| `duplicate` \| `other` |
| `reason` | `TEXT` NOT NULL DEFAULT `''` | customer free text |
| `resolver_user_id` | `UUID` FK → `users(id)`, NULL | operator who last transitioned it |
| `resolution_note` | `TEXT` NOT NULL DEFAULT `''` | operator note |
| `created_at` | `TIMESTAMPTZ` NOT NULL DEFAULT `now()` | |
| `updated_at` | `TIMESTAMPTZ` NOT NULL DEFAULT `now()` | bumped by the shared `set_updated_at` trigger (`00005`) |

**Idempotency / no-duplicate-open invariant:** a partial unique index ensures **at
most one non-terminal dispute** per (transfer, raiser):

```sql
CREATE UNIQUE INDEX uq_disputes_one_open
  ON disputes (transfer_id, raised_by_user_id)
  WHERE status IN ('open', 'under_review');
```

So a second raise while one is still open hits `23505` → **409**; once a dispute is
`resolved`/`rejected`, the same user may raise a fresh one (e.g. it recurred).

### 3.2 Status state machine

```
            raise_dispute
                 │
                 ▼
   ┌──────────► open ──────────┐
   │             │             │
   │  resolve(under_review)    │ resolve(resolved)/resolve(rejected)
   │             ▼             ▼
   └────── under_review ──► resolved | rejected   (terminal)
                 │
       resolve(resolved/rejected)
```

| From | Allowed `resolve` targets |
|---|---|
| `open` | `under_review`, `resolved`, `rejected` |
| `under_review` | `resolved`, `rejected` |
| `resolved` | — (terminal; any transition → 409) |
| `rejected` | — (terminal; any transition → 409) |

Transitioning to `resolved`/`rejected` sets `resolver_user_id` and
`resolution_note`. Re-issuing the *same* target while already there is a no-op
**only** for non-terminal idempotence is not required; illegal transitions raise
`check_violation` → 409 (`mapDBError` maps the "cannot " message to `invalid_state`
409, see `respond.go` P0001 branch).

### 3.3 Migration `00018_disputes.sql`

> **Numbering:** the next free number is `00018`. If
> [`spec-guided-transfer-suggestion.md`](spec-guided-transfer-suggestion.md) lands
> first as `00018_guided_scenarios.sql`, this becomes **`00019_disputes.sql`**.
> Whichever merges second takes the next integer — they do not share a file.

```sql
-- +goose Up
-- +goose StatementBegin

CREATE TYPE dispute_status   AS ENUM ('open', 'under_review', 'resolved', 'rejected');
CREATE TYPE dispute_category AS ENUM ('unrecognised', 'fraud', 'wrong_amount', 'duplicate', 'other');

-- disputes: a customer "I don't recognise this" case against a transfer they are a
-- party to. NOT money state — the ledger is append-only; remedy stays operator-side
-- (reverse_transfer, 00008). Only this row's status/resolution fields mutate.
CREATE TABLE disputes (
    id                UUID PRIMARY KEY DEFAULT uuidv7(),
    transfer_id       UUID NOT NULL REFERENCES transfers(id) ON DELETE RESTRICT,
    raised_by_user_id UUID NOT NULL REFERENCES users(id)     ON DELETE RESTRICT,
    status            dispute_status   NOT NULL DEFAULT 'open',
    category          dispute_category NOT NULL DEFAULT 'unrecognised',
    reason            TEXT NOT NULL DEFAULT '',
    resolver_user_id  UUID REFERENCES users(id),
    resolution_note   TEXT NOT NULL DEFAULT '',
    created_at        TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at        TIMESTAMPTZ NOT NULL DEFAULT now()
);

-- at most one non-terminal dispute per (transfer, raiser) -> 23505 -> 409
CREATE UNIQUE INDEX uq_disputes_one_open
  ON disputes (transfer_id, raised_by_user_id)
  WHERE status IN ('open', 'under_review');

CREATE INDEX idx_disputes_raiser ON disputes (raised_by_user_id, created_at DESC);
CREATE INDEX idx_disputes_queue  ON disputes (created_at DESC) WHERE status IN ('open', 'under_review');

-- updated_at maintenance: reuse the project's shared trigger fn from 00005.
CREATE TRIGGER trg_disputes_updated_at
  BEFORE UPDATE ON disputes
  FOR EACH ROW EXECUTE FUNCTION set_updated_at();

-- ─────────────────────────────────────────────────────────────────────────────
-- raise_dispute: open a case. Caller must be a party to the transfer (debit or
-- credit owner). Records a fraud signal in admin_actions (the server-side hook,
-- §6). The partial unique index enforces "no duplicate open dispute" (23505).
-- ─────────────────────────────────────────────────────────────────────────────
CREATE OR REPLACE FUNCTION raise_dispute(
    p_transfer_id UUID,
    p_raiser      UUID,
    p_category    dispute_category DEFAULT 'unrecognised',
    p_reason      TEXT DEFAULT ''
) RETURNS UUID AS $$
DECLARE
    v_t  transfers%ROWTYPE;
    v_id UUID;
    v_is_party BOOLEAN;
BEGIN
    SELECT * INTO v_t FROM transfers WHERE id = p_transfer_id;
    IF NOT FOUND THEN
        RAISE EXCEPTION 'transfer % not found', p_transfer_id;     -- -> 404
    END IF;

    -- Party check: raiser owns either side of the transfer.
    SELECT EXISTS (
        SELECT 1 FROM accounts a
        WHERE a.id IN (v_t.debit_account_id, v_t.credit_account_id)
          AND a.user_id = p_raiser
    ) INTO v_is_party;
    IF NOT v_is_party THEN
        RAISE EXCEPTION 'transfer % not found', p_transfer_id;     -- -> 404 (don't reveal existence)
    END IF;

    -- Only a settled (posted/reversed) transfer is disputable; a pending one is
    -- cancellable instead.
    IF v_t.status NOT IN ('posted', 'reversed') THEN
        RAISE EXCEPTION 'cannot dispute a transfer in state %', v_t.status
            USING ERRCODE = 'check_violation';                    -- -> 422
    END IF;

    INSERT INTO disputes (transfer_id, raised_by_user_id, category, reason)
    VALUES (p_transfer_id, p_raiser, p_category, COALESCE(p_reason, ''))
    RETURNING id INTO v_id;                                        -- dup open -> 23505 -> 409

    -- Server-side fraud hook: an auditable signal alongside the ledger (§6).
    INSERT INTO admin_actions (actor_user_id, action, target_id, detail)
    VALUES (p_raiser, 'dispute_raised', p_transfer_id,
            jsonb_build_object('dispute_id', v_id, 'category', p_category));

    RETURN v_id;
END;
$$ LANGUAGE plpgsql;

-- ─────────────────────────────────────────────────────────────────────────────
-- resolve_dispute: operator transition (state machine in §3.2). Records the
-- resolver + note; appends an admin_actions audit row. Illegal transitions raise
-- check_violation (-> 409 invalid_state).
-- ─────────────────────────────────────────────────────────────────────────────
CREATE OR REPLACE FUNCTION resolve_dispute(
    p_dispute_id UUID,
    p_resolver   UUID,
    p_status     dispute_status,
    p_note       TEXT DEFAULT ''
) RETURNS dispute_status AS $$
DECLARE v_cur dispute_status;
BEGIN
    SELECT status INTO v_cur FROM disputes WHERE id = p_dispute_id FOR UPDATE;
    IF NOT FOUND THEN RAISE EXCEPTION 'dispute % not found', p_dispute_id; END IF;  -- -> 404

    IF p_status NOT IN ('under_review', 'resolved', 'rejected') THEN
        RAISE EXCEPTION 'cannot set dispute to %', p_status USING ERRCODE = 'check_violation';
    END IF;
    IF v_cur IN ('resolved', 'rejected') THEN
        RAISE EXCEPTION 'cannot transition a % dispute', v_cur USING ERRCODE = 'check_violation';
    END IF;
    IF v_cur = 'under_review' AND p_status = 'under_review' THEN
        RETURN v_cur;  -- no-op
    END IF;

    UPDATE disputes
       SET status           = p_status,
           resolver_user_id = p_resolver,
           resolution_note  = CASE WHEN p_status IN ('resolved','rejected')
                                   THEN COALESCE(NULLIF(p_note,''), resolution_note)
                                   ELSE resolution_note END
     WHERE id = p_dispute_id;

    INSERT INTO admin_actions (actor_user_id, action, target_id, detail)
    VALUES (p_resolver, 'dispute_' || p_status::text, p_dispute_id,
            jsonb_build_object('note', COALESCE(p_note,'')));

    RETURN p_status;
END;
$$ LANGUAGE plpgsql;

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DROP FUNCTION IF EXISTS resolve_dispute(UUID, UUID, dispute_status, TEXT);
DROP FUNCTION IF EXISTS raise_dispute(UUID, UUID, dispute_category, TEXT);
DROP TABLE IF EXISTS disputes;
DROP TYPE IF EXISTS dispute_category;
DROP TYPE IF EXISTS dispute_status;
-- +goose StatementEnd
```

> Verify `set_updated_at()` exists in `00005_triggers.sql` (it maintains
> `users.updated_at`/`accounts.updated_at`). If the function name differs, match it;
> the trigger is otherwise the only external dependency.

### 3.4 sqlc queries (`db/queries/disputes.sql`)

`raise_dispute`/`resolve_dispute` are scalar-returning so sqlc can wrap them; the
list/get/queue reads are plain selects. (No `RETURNS TABLE` here, so unlike
`transfer()` nothing needs hand-written pgx.)

```sql
-- name: RaiseDispute :one
SELECT raise_dispute(
    sqlc.arg(transfer_id)::uuid,
    sqlc.arg(raiser)::uuid,
    sqlc.arg(category)::dispute_category,
    sqlc.arg(reason)::text
) AS id;

-- name: ResolveDispute :one
SELECT resolve_dispute(
    sqlc.arg(dispute_id)::uuid,
    sqlc.arg(resolver)::uuid,
    sqlc.arg(status)::dispute_status,
    sqlc.arg(note)::text
) AS status;

-- name: GetDisputeForRaiser :one
SELECT id, transfer_id, status, category, reason, resolution_note, created_at, updated_at
FROM disputes
WHERE id = sqlc.arg(id)::uuid AND raised_by_user_id = sqlc.arg(raiser)::uuid;

-- name: ListDisputesForRaiser :many
SELECT id, transfer_id, status, category, reason, resolution_note, created_at, updated_at
FROM disputes
WHERE raised_by_user_id = sqlc.arg(raiser)::uuid
  AND (sqlc.narg(cursor)::timestamptz IS NULL OR created_at < sqlc.narg(cursor)::timestamptz)
ORDER BY created_at DESC
LIMIT sqlc.arg(page_limit)::int;

-- name: GetDisputeAdmin :one
SELECT id, transfer_id, status, category, reason, resolution_note, created_at, updated_at
FROM disputes WHERE id = sqlc.arg(id)::uuid;

-- name: ListDisputesAdmin :many
SELECT d.id, d.transfer_id, d.status, d.category, d.reason,
       t.amount_minor, t.currency,
       COALESCE(u.username::text, '')        AS raised_by,
       COALESCE(da.iban, da.system_code, '') AS debit_iban,
       COALESCE(ca.iban, ca.system_code, '') AS credit_iban,
       d.created_at
FROM disputes d
JOIN transfers t  ON t.id  = d.transfer_id
LEFT JOIN users u ON u.id  = d.raised_by_user_id
JOIN accounts da  ON da.id = t.debit_account_id
JOIN accounts ca  ON ca.id = t.credit_account_id
WHERE (sqlc.narg(status)::dispute_status IS NULL OR d.status = sqlc.narg(status)::dispute_status)
  AND (sqlc.narg(cursor)::timestamptz  IS NULL OR d.created_at < sqlc.narg(cursor)::timestamptz)
ORDER BY d.created_at DESC
LIMIT sqlc.arg(page_limit)::int;
```

---

## 4. Handler logic

New file `internal/api/handlers_disputes.go`. Implements the four client/admin
generated methods. No money moves, so **no idempotency-key header** — the
"no duplicate open" invariant is the relevant idempotence and lives in the unique
index (`23505` → 409 via `mapDBError`). Helpers reused: `clientSubject`,
`ownsAccount`-style party check is done in PL/pgSQL; `mapDBError`; `s.limitOr`;
the cursor parse pattern from `handlers_transfers.go`/search helpers.

### 4.1 `RaiseDispute` (client)

```
func (s *Server) RaiseDispute(w, r, id openapi_types.UUID)
```
1. `subj, ok := clientSubject(ctx)`; `!ok` → 401.
2. `decodeOptionalJSON(r, &req)` (body optional). Validate `category` against the
   enum set if non-empty; unknown → 422 (`bad_request`/`unprocessable`); empty →
   default `unrecognised` (pass `""`? No — pass the validated value or the literal
   `"unrecognised"`).
3. `s.pg.Queries.RaiseDispute(ctx, {TransferID:id, Raiser:subj, Category:..., Reason:req.Reason})`.
4. `mapDBError` covers: not-a-party / unknown transfer → **404** (the function
   RAISEs `"... not found"`), non-disputable state → **422** (`check_violation`),
   duplicate open → **409** (`23505`).
5. On success read back with `GetDisputeForRaiser(disputeID, subj)` and
   `writeJSON(w, 201, dispute)`.

### 4.2 `ListMyDisputes` / `GetDispute` (client)

- `ListMyDisputes(w, r, params)`: subject-scoped; parse `cursor`/`limit`; call
  `ListDisputesForRaiser`. **Initialise the slice non-nil** so an empty list
  marshals as `[]`, not `null` (the bug called out in
  [`09-fraudbank-bff-plan.md`](../09-fraudbank-bff-plan.md) P0 — do not reintroduce
  it; mirror `ListBeneficiaries`'s `if rows == nil { rows = []... }`).
- `GetDispute(w, r, id)`: `GetDisputeForRaiser(id, subj)` → `pgx.ErrNoRows` →
  **404** (also covers "exists but not yours" — never reveals another user's
  dispute).

### 4.3 `ListDisputes` / `ResolveDispute` (admin)

- `ListDisputes(w, r, params)`: portal surface (behind `requireSession`); optional
  `status` filter + cursor/limit → `ListDisputesAdmin`. Non-nil slice.
- `ResolveDispute(w, r, id)`: `decodeJSON` → `{status, resolution_note}`. Validate
  `status` ∈ {`under_review`,`resolved`,`rejected`} (else 422). Resolver id =
  the session user (operator), obtained the same way the console handlers do
  (`s.requireRole`/session user — see `console_handlers.go` `consoleActionContext`;
  for the JSON admin handler take the session subject). Call `ResolveDispute`;
  illegal transition → `check_violation` → **409** (`invalid_state`); unknown id →
  **404**. Read back with `GetDisputeAdmin` → `writeJSON(w, 200, dispute)`.

### 4.4 Edge cases

| Case | Behaviour |
|---|---|
| Raiser not a party to the transfer | 404 (existence not revealed) |
| Transfer pending/canceled/failed | 422 (`cannot dispute a transfer in state ...`) |
| Second raise while one is open/under_review | 409 (`already_exists`) |
| Raise again after resolved/rejected | allowed (partial index only guards non-terminal) |
| Both parties dispute the same transfer | allowed — index is per (transfer, raiser); two distinct rows |
| Operator resolves an already-resolved dispute | 409 (`invalid_state`) |
| `GET /disputes/{id}` for another user's dispute | 404 |
| Empty list (client or admin) | `200` with `[]` (never `null`) |

---

## 5. Tests to add

### 5.1 DB integration (`internal/db/integration_test.go` style)

- `TestRaiseDisputePartyOnly`: posted transfer alice→bob. alice (debit owner) and
  bob (credit owner) can each raise; a third user → "not found" error.
- `TestRaiseDisputeNonDisputableState`: pending transfer → `check_violation`.
- `TestRaiseDisputeNoDuplicateOpen`: second raise by same user while open →
  `23505`; after `resolve_dispute(... 'resolved')`, a fresh raise succeeds.
- `TestRaiseDisputeBothPartiesAllowed`: alice and bob both raise → two rows.
- `TestResolveDisputeStateMachine`: `open→under_review→resolved` OK;
  `resolved→rejected` raises `check_violation`; `open→rejected` OK directly.
- `TestRaiseDisputeEmitsSignal`: after `raise_dispute`, an `admin_actions` row with
  `action='dispute_raised'` and matching `target_id` exists (the §6 hook).

### 5.2 API (`internal/api/*_test.go`, Bearer + session harness)

Client (Bearer):
- `TestHTTPRaiseDisputeRequiresAuth`: no token → 401.
- `TestHTTPRaiseDisputeHappyPath`: alice posts a transfer to bob, then
  `POST /transfers/{id}/dispute {category:"fraud","reason":"..."}` → 201 with
  `status:"open"`.
- `TestHTTPRaiseDisputeNotParty`: a third user disputes alice↔bob's transfer → 404.
- `TestHTTPRaiseDisputeDuplicate409`: raise twice → 201 then 409.
- `TestHTTPListMyDisputesScoped`: alice lists → sees only her own; empty case is
  `[]` not `null`; bob can't `GET /disputes/{aliceDisputeId}` → 404.

Admin (session): in the portal-mode test server (see `integration_test.go` /
`helpers_test.go` session helpers):
- `TestHTTPAdminDisputeQueueAndResolve`: queue lists the open dispute; filter
  `?status=open` returns it, `?status=resolved` empty; `POST
  /admin/disputes/{id}/resolve {status:"resolved","resolution_note":"refunded"}` →
  200, then a repeat → 409; the client `GET /disputes/{id}` now shows `resolved`
  with the note.

### 5.3 Pure-unit
- Category-validation helper (`validDisputeCategory`) table test, mirroring
  `TestValidRole` in `helpers_test.go`.

---

## 6. Security considerations & the server-side fraud hook

- **Ownership / party scoping.** Raising, getting, and listing are all scoped to
  the JWT subject; non-party or foreign-id access returns **404**, never revealing
  another customer's transfer or dispute. The party check is in PL/pgSQL
  (`raise_dispute`), not client-trusted.
- **Append-only ledger preserved.** A dispute never edits a posted transfer or a
  ledger entry. The only mutation is the dispute row's own status/resolution.
  Monetary remedy stays the operator's existing `reverse_transfer` (`00008`,
  idempotency-key) — a resolution flow *may* call it, but that is a separate,
  audited money move, not part of the dispute write.
- **Where the server-side fraud hook lives (cross-reference).** The fraudbank
  clients today only fire a *local* `risk.generalEvent("dispute", transfer_id)`
  (web `Transfer.tsx`/receipt, iOS `RiskSdk`, Android `RiskSdk`). The
  authoritative, server-side equivalent is the `admin_actions` insert inside
  `raise_dispute` (`action='dispute_raised'`, `detail={dispute_id, category}`).
  This is the single seam a real fraud engine subscribes to. Two non-breaking
  escalation options, both isolated to the function body so handlers/contract don't
  change:
  - **Flag (default, recommended):** the `admin_actions` row *is* the flag; an
    operator/risk job watches `action='dispute_raised'`. No customer-visible
    side-effect.
  - **Auto-freeze (opt-in, demo):** within `raise_dispute`, for
    `category IN ('fraud','unrecognised')` also `PERFORM set_account_status(<the
    raiser's account on this transfer>, 'frozen')` (or the relevant
    counterparty/mule account, for the APP-scam demo). Gate behind a config/flag so
    production stays flag-only; document that freezing the mule account is the
    demo-satisfying behaviour and uses the existing `accounts.status` machinery, not
    a new one.
- **No new authority on the admin surface.** `resolve` is portal-session-gated like
  every other `/admin/*` op; the resolver id is taken from the session, audited in
  `admin_actions`. (A 4-eyes requirement, if wanted, would reuse the `00014`
  maker-checker pattern — out of scope here.)
- **Input validation.** `category` is enum-checked (422 on garbage); `reason`/`note`
  are free text stored as-is and only ever rendered escaped by the console
  templates — no SQL concat (parameterised throughout).
- **No idempotency-key needed** (no money move); replay safety is the partial unique
  index (one open dispute per transfer/raiser).

---

## 7. Acceptance criteria

- [ ] `api/openapi.yaml` has the five operations + `Dispute`, `DisputeQueueItem`,
      `RaiseDisputeRequest`, `ResolveDisputeRequest` schemas; `oapi-codegen`
      regenerates with no drift across `genclient` + `genadmin`.
- [ ] Migration (`00018_disputes.sql` or `00019_` if guided-scenarios merged first)
      creates the two enums, `disputes`, the partial-unique + queue indexes, the
      `updated_at` trigger, and `raise_dispute`/`resolve_dispute`; `down` drops all.
- [ ] `db/queries/disputes.sql` added; sqlc regenerates clean.
- [ ] Client: raise (201), list (200, `[]` not `null`), get (200/404) — all
      subject-scoped; non-party → 404; non-disputable state → 422; duplicate open →
      409.
- [ ] Admin: queue with optional `status` filter + cursor; `resolve` enforces the
      §3.2 state machine (illegal → 409), records resolver + note + audit row.
- [ ] `raise_dispute` emits an `admin_actions` `dispute_raised` row (the fraud hook).
- [ ] Ledger untouched; no money moves in any dispute write.
- [ ] DB + API tests in §5 pass under `task test`.
- [ ] `docs/06-client-api.md` §1 surface table + `docs/05-admin-ui.md` (admin queue)
      gain rows; `08-...` P-table notes the gap closed.

---

## 8. Step-by-step implementation order

1. **Migration.** Add the disputes migration (next free number). Confirm the shared
   `set_updated_at()` trigger name in `00005`; apply up/down cleanly via the test
   harness.
2. **Queries.** Add `db/queries/disputes.sql` (§3.4); `task gen` (sqlc) → new
   methods in `internal/db/sqlc`.
3. **Contract.** Add the five operations + schemas to `api/openapi.yaml` (§2);
   `task gen` (oapi-codegen). Build fails until handlers exist (the drift check).
4. **Handlers.** `internal/api/handlers_disputes.go` (§4): the two client methods,
   the two admin methods, and a `validDisputeCategory` helper. Wire the admin
   routes — they fall out of `genadmin.HandlerFromMux` on the portal subrouter; no
   manual route registration needed (paths are `/admin/disputes...`, no client
   collision). Build passes.
5. **Tests.** DB integration (§5.1), then API client + admin (§5.2), then the unit
   helper (§5.3). `task test`.
6. **Optional fraud escalation.** If the demo wants auto-freeze, add the gated
   `PERFORM set_account_status(...)` inside `raise_dispute` (§6) behind a config
   flag; add a test asserting the account is frozen for `category='fraud'` and not
   otherwise.
7. **Docs.** Rows in `docs/06-client-api.md` §1 and `docs/05-admin-ui.md`; tick the
   gap in `docs/09-fraudbank-bff-plan.md`.
8. **Client cutover (fraudbank, separate repo).** Replace the local-only
   `risk.generalEvent("dispute", …)` stub on the receipt with a real
   `POST /transfers/{id}/dispute` call (keep firing the risk event too), and add a
   "My disputes" list backed by `GET /disputes`. **Not part of this backend PR.**
```
