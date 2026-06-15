# Spec — Customer account opening & transfer-limit-change requests

> Implementation spec. Closes two P1 gaps in
> [`../09-fraudbank-integration.md`](../09-fraudbank-integration.md): **customer account
> opening** (open a second/savings account without staff) and **transfer-limit change
> requests** (a customer asks to raise the €500/transfer cap; an operator approves via
> the existing maker-checker). Both **extend the existing account model**, not new
> domains. A model implementing this needs no further design decisions.
>
> Related: the savings/"pots" product vision is the architecture in
> [`spec-p3-roadmap.md`](spec-p3-roadmap.md) §2; this spec ships the plumbing
> (self-open + server-allocated IBAN) that pots will reuse.

---

## 1. Summary & rationale

Today `createAccount` is `admin`-tagged and takes a **staff-chosen IBAN + PIN**.
A customer with one account can't open a second. And the `transfer_limit_minor`
(€500 default) is account data only staff can change. fraudbank needs:

```
POST /me/accounts {kind?}            → server allocates IBAN, opens account, 201
POST /accounts/{id}/limit-requests   → pending limit-change, surfaces in portal queue
```

Two design problems this spec solves concretely:

1. **Server-side IBAN allocation.** `create_account` requires the caller to pass an
   IBAN; a customer can't. We add a deterministic IBAN generator (a `bank0`-scheme
   pseudo-IBAN with a check, drawn from a sequence) so the server mints one. This is
   the missing piece both this spec and pots need.
2. **Limit changes as maker-checker.** A customer-initiated limit raise is exactly the
   4-eyes pattern already built for high-value credits (the maker-checker functions in `00006_maker_checker.sql`):
   record a pending request in `admin_actions`, a *different* operator approves
   (applies `update_transfer_limit`) or rejects. No new approval machinery — one new
   `action` value + two thin functions.

Design stance:

- **Reuse `create_account`** for the open; only add an IBAN allocator in front of it.
- **Reuse `admin_actions` + `update_transfer_limit`** for limit requests — the
  approval queue, the `approved_by` 4-eyes guard, and the audit row already exist.
- **No new account kind is required** for "second account" — it is still
  `kind='customer'`. (A distinct `savings` kind is the pots roadmap, §2 there.)

---

## 2. API — OpenAPI 3.1 operations

### Client surface (tag `client`, `bearerAuth`)

```yaml
  /me/accounts:
    post:
      operationId: openMyAccount
      tags: [client]
      summary: Open a new account for the caller (server allocates IBAN). Idempotent.
      parameters: [ { $ref: "#/components/parameters/IdempotencyKey" } ]
      requestBody:
        required: false
        content:
          application/json:
            schema: { $ref: "#/components/schemas/OpenAccountRequest" }
      responses:
        "201":
          description: opened
          content:
            application/json:
              schema: { $ref: "#/components/schemas/Account" }
        "409": { $ref: "#/components/responses/Error" }   # account cap reached
        "422": { $ref: "#/components/responses/Error" }

  /accounts/{id}/limit-requests:
    post:
      operationId: requestLimitChange
      tags: [client]
      summary: Request a transfer-limit change on an owned account (operator approves)
      parameters: [ { $ref: "#/components/parameters/Id" } ]
      requestBody:
        required: true
        content:
          application/json:
            schema: { $ref: "#/components/schemas/LimitRequest" }
      responses:
        "201":
          description: pending approval
          content:
            application/json:
              schema: { $ref: "#/components/schemas/LimitRequestResponse" }
        "403": { $ref: "#/components/responses/Error" }   # not the caller's account
        "404": { $ref: "#/components/responses/Error" }
        "422": { $ref: "#/components/responses/Error" }
```

### Admin surface (tag `admin`, `sessionCookie`) — approval queue

```yaml
  /admin/limit-requests:
    get:
      operationId: listLimitRequests
      tags: [admin]
      summary: Pending transfer-limit-change requests (cursor-paginated)
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
                items: { $ref: "#/components/schemas/LimitRequestItem" }

  /admin/limit-requests/{id}/approve:
    post:
      operationId: approveLimitRequest
      tags: [admin]
      summary: Approve a limit request (applies the new limit; 4-eyes — not your own)
      parameters: [ { $ref: "#/components/parameters/Id" } ]
      responses:
        "200":
          description: applied
          content:
            application/json:
              schema: { $ref: "#/components/schemas/StatusResponse" }
        "403": { $ref: "#/components/responses/Error" }   # own request / not operator
        "409": { $ref: "#/components/responses/Error" }   # already handled

  /admin/limit-requests/{id}/reject:
    post:
      operationId: rejectLimitRequest
      tags: [admin]
      summary: Reject a pending limit request
      parameters: [ { $ref: "#/components/parameters/Id" } ]
      requestBody:
        required: false
        content:
          application/json:
            schema: { $ref: "#/components/schemas/ReasonRequest" }
      responses:
        "200":
          description: rejected
          content:
            application/json:
              schema: { $ref: "#/components/schemas/StatusResponse" }
        "409": { $ref: "#/components/responses/Error" }
```

### Schemas

```yaml
    OpenAccountRequest:
      type: object
      properties:
        kind:  { type: string, enum: [customer], default: customer, description: "reserved for future kinds (savings); only 'customer' accepted today" }
        label: { type: string, description: "optional display label (not the IBAN)" }
    LimitRequest:
      type: object
      required: [transfer_limit_minor]
      properties:
        transfer_limit_minor: { type: integer, format: int64, minimum: 0 }
        reason:               { type: string }
    LimitRequestResponse:
      type: object
      properties:
        request_id:           { type: string, format: uuid }
        account_id:           { type: string, format: uuid }
        requested_limit_minor:{ type: integer, format: int64 }
        status:               { type: string, example: pending }
    LimitRequestItem:
      type: object
      properties:
        request_id:           { type: string, format: uuid }
        account_id:           { type: string, format: uuid }
        account_iban:         { type: string }
        user_id:              { type: string, format: uuid }
        current_limit_minor:  { type: integer, format: int64 }
        requested_limit_minor:{ type: integer, format: int64 }
        reason:               { type: string }
        requested_at:         { type: string, format: date-time }
```

> A PIN is **not** taken at self-open: the client uses bearer auth, not a card PIN.
> `create_account` requires a PIN, so the allocator sets a server-generated random PIN
> (the customer never sees/uses it on the client surface; PIN is for the card/ATM path
> which is out of scope here). If a future flow needs a customer-set PIN, add
> `set_account_pin` then — do not block this spec on it.

---

## 3. Data model — migration `00020_account_self_service.sql`

Migrations are now a consolidated 9-file domain baseline (`00001_foundation.sql`,
`00002_iban.sql`, `00003_users.sql`, `00004_accounts.sql`, `00005_transfers.sql`,
`00006_maker_checker.sql`, `00007_maintenance.sql`, `00008_features.sql`,
`00009_system_seed.sql`); the next free number is `00010`.
Several sibling specs in this directory also add new migrations (mfa, change-password,
events, disputes, self-registration). **This spec's migration is independent of all of
them** — assign it the next free number at land time (`00020` is illustrative). What
matters is goose ordering, not the suffix. Adds: IBAN allocation, a per-user account cap, and the
limit-request maker-checker functions. **No new tables** — limit requests live in
`admin_actions` (like the existing approval flow).

```sql
-- +goose Up
-- +goose StatementBegin

-- IBAN allocation: bank0 has no national IBAN registry, so we mint a deterministic
-- internal pseudo-IBAN that satisfies the accounts CHECK (^[A-Z0-9]{15,34}$) and is
-- unique. Format: 'BANK0' + 'EUR' + 16-digit zero-padded sequence value = 24 chars.
-- A real BBAN/MOD-97 check digit is out of scope (single virtual bank); the sequence
-- guarantees uniqueness, and the UNIQUE index on accounts.iban is the backstop.
CREATE SEQUENCE iban_seq START 1000000000000001;

CREATE OR REPLACE FUNCTION allocate_iban() RETURNS VARCHAR AS $$
    SELECT 'BANK0EUR' || lpad(nextval('iban_seq')::text, 16, '0');
$$ LANGUAGE sql;

-- Cap self-opened accounts to keep abuse bounded; staff createAccount is uncapped.
-- 5 is a product default; lives here so the rule is DB-enforced.
CREATE OR REPLACE FUNCTION open_customer_account(
    p_user_id              UUID,
    p_transfer_limit_minor BIGINT DEFAULT 50000,
    p_max_accounts         INT    DEFAULT 5
) RETURNS UUID AS $$
DECLARE v_count INT; v_iban VARCHAR; v_pin TEXT; v_id UUID;
BEGIN
    PERFORM 1 FROM users WHERE id = p_user_id AND status = 'active' FOR UPDATE;
    IF NOT FOUND THEN RAISE EXCEPTION 'user % not found or not active', p_user_id; END IF;

    SELECT count(*) INTO v_count FROM accounts
     WHERE user_id = p_user_id AND status <> 'closed';
    IF v_count >= p_max_accounts THEN
        RAISE EXCEPTION 'account limit reached (% of %)', v_count, p_max_accounts
            USING ERRCODE = 'check_violation';   -- -> 422 (or treat as 409, see handler)
    END IF;

    v_iban := allocate_iban();
    v_pin  := lpad((floor(random() * 1000000))::int::text, 6, '0'); -- server PIN; unused on client surface
    -- create_account makes the first account default; subsequent ones non-default.
    v_id := create_account(p_user_id, v_iban, v_pin, p_transfer_limit_minor);
    RETURN v_id;
END;
$$ LANGUAGE plpgsql;

-- ── Limit-change maker-checker (reuses admin_actions + update_transfer_limit) ──
-- The request stores the desired limit in admin_actions.detail; approve_request-style
-- 4-eyes applies it. action = 'limit_request'.

-- request_limit_change: record a pending limit-change for an account. p_maker is the
-- requesting *customer* (the maker need not be staff here — the customer asks, an
-- operator approves; the 4-eyes guard is operator-vs-operator on approve).
CREATE OR REPLACE FUNCTION request_limit_change(
    p_account_id   UUID,
    p_maker        UUID,                 -- requesting user (customer)
    p_new_limit    BIGINT,
    p_reason       TEXT DEFAULT ''
) RETURNS UUID AS $$
DECLARE v_id UUID; v_cur BIGINT;
BEGIN
    IF p_new_limit < 0 THEN RAISE EXCEPTION 'limit must be >= 0'; END IF;
    SELECT transfer_limit_minor INTO v_cur FROM accounts
     WHERE id = p_account_id AND kind = 'customer';
    IF NOT FOUND THEN RAISE EXCEPTION 'account % not found', p_account_id; END IF;

    INSERT INTO admin_actions (actor_user_id, action, target_id, detail)
    VALUES (p_maker, 'limit_request', p_account_id,
            jsonb_build_object('current_limit_minor', v_cur,
                               'requested_limit_minor', p_new_limit,
                               'reason', COALESCE(p_reason, '')))
    RETURNING id INTO v_id;
    RETURN v_id;
END;
$$ LANGUAGE plpgsql;

-- approve_limit_change: a staff member (different from… the customer maker is not
-- staff, so the only 4-eyes constraint is the approver must be an operator/admin;
-- additionally an operator cannot approve a request they themselves filed on behalf
-- of a customer). Applies the new limit via update_transfer_limit. Idempotent-guarded
-- by approved_by.
CREATE OR REPLACE FUNCTION approve_limit_change(p_request_id UUID, p_approver UUID)
RETURNS UUID AS $$
DECLARE v_req admin_actions%ROWTYPE; v_new BIGINT;
BEGIN
    SELECT * INTO v_req FROM admin_actions
     WHERE id = p_request_id AND action = 'limit_request' FOR UPDATE;
    IF NOT FOUND THEN RAISE EXCEPTION 'limit request % not found', p_request_id; END IF;
    IF v_req.approved_by IS NOT NULL THEN
        RAISE EXCEPTION 'request already handled' USING ERRCODE = 'check_violation';
    END IF;
    IF v_req.actor_user_id = p_approver THEN
        RAISE EXCEPTION 'cannot approve your own request' USING ERRCODE = '42501';
    END IF;
    v_new := (v_req.detail->>'requested_limit_minor')::bigint;
    PERFORM update_transfer_limit(v_req.target_id, v_new);
    UPDATE admin_actions SET approved_by = p_approver WHERE id = p_request_id;
    RETURN v_req.target_id;
END;
$$ LANGUAGE plpgsql;

-- reject_limit_change: mark handled without applying. Mirrors reject_request.
CREATE OR REPLACE FUNCTION reject_limit_change(p_request_id UUID, p_approver UUID, p_reason TEXT DEFAULT '')
RETURNS UUID AS $$
DECLARE v_req admin_actions%ROWTYPE;
BEGIN
    SELECT * INTO v_req FROM admin_actions
     WHERE id = p_request_id AND action = 'limit_request' FOR UPDATE;
    IF NOT FOUND THEN RAISE EXCEPTION 'limit request % not found', p_request_id; END IF;
    IF v_req.approved_by IS NOT NULL THEN
        RAISE EXCEPTION 'request already handled' USING ERRCODE = 'check_violation';
    END IF;
    UPDATE admin_actions
       SET approved_by = p_approver,
           detail = detail || jsonb_build_object('rejected', true, 'reject_reason', COALESCE(p_reason,''))
     WHERE id = p_request_id;
    RETURN v_req.target_id;
END;
$$ LANGUAGE plpgsql;

CREATE INDEX IF NOT EXISTS idx_admin_actions_pending_limit
    ON admin_actions (created_at DESC)
    WHERE action = 'limit_request' AND approved_by IS NULL;

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DROP INDEX IF EXISTS idx_admin_actions_pending_limit;
DROP FUNCTION IF EXISTS reject_limit_change(UUID, UUID, TEXT);
DROP FUNCTION IF EXISTS approve_limit_change(UUID, UUID);
DROP FUNCTION IF EXISTS request_limit_change(UUID, UUID, BIGINT, TEXT);
DROP FUNCTION IF EXISTS open_customer_account(UUID, BIGINT, INT);
DROP FUNCTION IF EXISTS allocate_iban();
DROP SEQUENCE IF EXISTS iban_seq;
-- +goose StatementEnd
```

sqlc queries (`db/queries/account_self_service.sql`):

```sql
-- name: OpenCustomerAccount :one
SELECT open_customer_account(sqlc.arg(user_id)::uuid,
    sqlc.arg(transfer_limit_minor)::bigint) AS id;

-- name: RequestLimitChange :one
SELECT request_limit_change(sqlc.arg(account_id)::uuid, sqlc.arg(maker)::uuid,
    sqlc.arg(new_limit)::bigint, sqlc.arg(reason)::text) AS id;

-- name: ListLimitRequests :many
SELECT aa.id AS request_id, aa.target_id AS account_id, a.iban AS account_iban,
       a.user_id, (aa.detail->>'current_limit_minor')::bigint   AS current_limit_minor,
       (aa.detail->>'requested_limit_minor')::bigint AS requested_limit_minor,
       COALESCE(aa.detail->>'reason','') AS reason, aa.created_at AS requested_at
FROM admin_actions aa
JOIN accounts a ON a.id = aa.target_id
WHERE aa.action = 'limit_request' AND aa.approved_by IS NULL
  AND (sqlc.narg(cursor)::timestamptz IS NULL OR aa.created_at < sqlc.narg(cursor)::timestamptz)
ORDER BY aa.created_at DESC
LIMIT sqlc.arg(page_limit)::int;

-- name: ApproveLimitChange :one
SELECT approve_limit_change(sqlc.arg(request_id)::uuid, sqlc.arg(approver)::uuid) AS account_id;

-- name: RejectLimitChange :one
SELECT reject_limit_change(sqlc.arg(request_id)::uuid, sqlc.arg(approver)::uuid,
    sqlc.arg(reason)::text) AS account_id;
```

---

## 4. Handler logic

### 4.1 `OpenMyAccount` (POST /me/accounts) — client

1. `subj, ok := clientSubject(...)`; `!ok` → 401.
2. Decode optional `OpenAccountRequest`. If `kind` present and not `"customer"` → 422
   `unsupported_kind` (savings is roadmap §2; reject explicitly so clients fail loud).
3. **Idempotency:** the header is required (generated wrapper binds it). Wrap the open
   in the `idempotency_keys` gate with `scope='open_account'` (the
   `request_transfer` pattern): a replayed key returns the originally-opened account
   id instead of opening a second. Store `account_id` in the key row's `response`.
4. `OpenCustomerAccount(subj)`. `check_violation` ("account limit reached") → **409**
   `account_limit` (cap is a conflict, not bad input — handler maps it explicitly even
   though the SQLSTATE is 23514; special-case on the message, like `mapDBError` does
   for "insufficient").
5. `GetAccount(account_id)`, return 201 `Account` (includes the allocated IBAN).

> Ownership is implicit: the account is opened **for `subj`** — the caller can't open
> for anyone else (no `user_id` in the request).

### 4.2 `RequestLimitChange` (POST /accounts/{id}/limit-requests) — client

1. `subj`; parse account `id`. **Ownership:** `AccountOwner(id)` then
   `ownsAccount(subj, owner)`; not owned → **403** (matches the transfer-debit
   convention). Not found → 404.
2. Decode `LimitRequest`; require `transfer_limit_minor >= 0` (422). Optionally clamp:
   reject a requested limit equal to the current one (422 `no_change`) — nice-to-have.
3. `RequestLimitChange(account_id, maker=subj, new_limit, reason)`. Returns
   `request_id`.
4. 201 `LimitRequestResponse{request_id, account_id, requested_limit_minor, status:"pending"}`.

### 4.3 Admin approve/reject/list

- `ListLimitRequests` → `respond`-style cursor pagination (copy
  `respondPending`/`ListPendingTransfers` in `handlers_transfers.go`). Slice
  initialized non-nil → `[]` not `null`.
- `ApproveLimitChange(request_id, approver=operatorSubject)`: the approver is the
  **portal session user** (operators are unscoped; get the actor id from the session,
  as the existing maker-checker console does). `42501` → 403 `forbidden`
  (own request); `check_violation` ("already handled") → 409 `invalid_state`.
- `RejectLimitChange(...)`: same mapping; 200 `StatusResponse`.
- Record the operator action: the `admin_actions` row IS the audit (`approved_by` set
  on approve/reject) — consistent with the existing approval flow. Optionally also
  call the console audit helper (`audit.go`) for the operator's action log.

### 4.4 Error mapping

`mapDBError` already covers `23505`→409, `23514`→422, `42501`→? — check: `42501`
(insufficient_privilege) is **not** currently in `mapDBError`. Add:

```go
case "42501": // insufficient_privilege (own-approval 4-eyes guard)
    writeError(w, http.StatusForbidden, "forbidden", msg)
    return
```

For the account-cap, special-case the message in the existing `23514` branch
(`strings.Contains(msg, "account limit")` → 409 `account_limit`) **or** map it 422 and
let the client treat it as terminal — pick 409 for product clarity.

### 4.5 Edge cases

| Case | Behavior |
|------|----------|
| Open with `kind:"savings"` | 422 `unsupported_kind` (roadmap §2 not built) |
| 6th self-opened account | 409 `account_limit` |
| Replayed open (same Idempotency-Key) | returns the same account, no dup |
| Limit request on someone else's account | 403 |
| Limit request on a closed account | 404 (function filters `kind='customer'`; add status check if desired) |
| Operator approves own request | 403 (`42501`) |
| Approve an already-approved/rejected request | 409 (`check_violation`) |
| Concurrent IBAN allocation | sequence is concurrency-safe; UNIQUE index is the backstop |

---

## 5. Tests to add

**DB integration (`internal/db/account_self_service_test.go`)**:

- [ ] `allocate_iban` returns distinct, CHECK-matching IBANs across calls (concurrent
      `nextval` never collides).
- [ ] `open_customer_account` opens a non-default account for a user who already has
      one; first-ever account is default.
- [ ] account cap: opening the (cap+1)th → `check_violation`.
- [ ] `request_limit_change` writes a `limit_request` row with current+requested in
      `detail`; `approve_limit_change` applies it (`update_transfer_limit` ran);
      `transfers` then accept an amount above the *old* limit.
- [ ] approve by the same actor → `42501`; approve twice → `check_violation`.
- [ ] `reject_limit_change` marks handled without changing the limit.

**API integration (`internal/api/account_self_service_test.go`)**:

- [ ] `POST /me/accounts` (empty body) → 201 with a fresh `BANK0EUR…` IBAN; appears in
      `GET /users/{id}/accounts`.
- [ ] replay with same `Idempotency-Key` → same account id, only one new account.
- [ ] `POST /me/accounts {kind:"savings"}` → 422.
- [ ] `POST /accounts/{owned}/limit-requests` → 201; `POST` on a non-owned account → 403.
- [ ] portal `GET /admin/limit-requests` lists it; `approve` by a different operator →
      200 and the account's `transfer_limit_minor` is raised; `approve` by the maker
      operator → 403; second approve → 409.

---

## 6. Security considerations

- [ ] Self-open is scoped to `subj` — a caller can only open accounts for themselves
      (no `user_id` field on the request).
- [ ] Account cap (5) bounds resource-exhaustion abuse from self-registration; staff
      `createAccount` stays uncapped for legitimate ops.
- [ ] Limit raises are **never self-applied** — they go through operator 4-eyes
      (`approve_limit_change`'s own-request guard), so a compromised customer token
      can't lift its own transfer ceiling. This is the whole point of routing it
      through maker-checker rather than a direct `PATCH`.
- [ ] The server-generated PIN is random and never returned; the client surface uses
      bearer auth, so the PIN is irrelevant there (it exists only to satisfy
      `create_account`).
- [ ] Allocated IBANs are internal/non-routable (`BANK0EUR…`) — they don't collide
      with real-world IBANs and can't be used to spoof an external bank. If real SEPA
      IBANs are ever needed that is a separate, regulated workstream (note it).
- [ ] No direct ledger surface in either flow; `reconcile()` is unaffected. Opening an
      account creates a zero-balance row (the create path mints no money).

---

## 7. Acceptance criteria

- [ ] `00020_account_self_service.sql` applies up/down cleanly; reuses `admin_actions`
      (no new table); sequence + functions created.
- [ ] `oapi-codegen` regenerates both tags (`openMyAccount`, `requestLimitChange` on
      client; `listLimitRequests`/`approveLimitRequest`/`rejectLimitRequest` on admin);
      handlers implement the interfaces (build green).
- [ ] A customer opens a second account via `POST /me/accounts` with a server-allocated
      IBAN; replays are idempotent; the cap is enforced (409 at the 6th).
- [ ] A limit request is created (201), surfaces in the portal queue, is applied only
      by a *different* operator (403 on self-approval), and the new limit takes effect
      (a previously-over-limit transfer now succeeds).
- [ ] `mapDBError` gains the `42501`→403 case; account-cap maps to 409.
- [ ] `reconcile()` healthy throughout.

---

## 8. Step-by-step implementation order

1. Write `db/migrations/00020_account_self_service.sql` (sequence + `allocate_iban` +
   `open_customer_account` + the three limit-change functions + index). `goose
   up`/`down` on a scratch DB; verify IBANs match the accounts CHECK.
2. Add `db/queries/account_self_service.sql`; `sqlc generate`.
3. Add the two client + three admin operations and schemas to `api/openapi.yaml`. Run
   `oapi-codegen` (both tags); fix the compiler.
4. Add `mapDBError` cases: `42501`→403; account-cap message→409.
5. Write client handlers (`OpenMyAccount`, `RequestLimitChange`) — ownership-scoped,
   idempotency-gated for the open. Add the `idempotency_keys` `scope='open_account'`
   gate (mirror `request_transfer`).
6. Write admin handlers (`ListLimitRequests`, `ApproveLimitRequest`,
   `RejectLimitRequest`) — actor = portal session user; copy the cursor pagination from
   `respondPending`.
7. Surface the queue in the portal console (`05-admin-ui.md`): a "Limit requests" list
   with approve/reject, next to the existing approvals queue.
8. Write DB then API tests (§5). `go test ./...` green with/without `TEST_DATABASE_DSN`.
9. Update [`../06-client-api.md`](../06-client-api.md) §1 surface table,
   [`../05-admin-ui.md`](../05-admin-ui.md) (new queue), and the P1 rows in
   [`../09-fraudbank-integration.md`](../09-fraudbank-integration.md).
```
