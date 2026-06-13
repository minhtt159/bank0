# Spec — `GET /transfers` : list the caller's transfers across all their accounts

> Status: **spec, ready to implement.** Completes the "List my transfers (across
> accounts)" P1 row in [`../09-fraudbank-bff-plan.md`](../09-fraudbank-bff-plan.md)
> §2. Depends on the pagination envelope + opaque composite cursor defined in
> [`spec-ledger-pagination-and-filters.md`](spec-ledger-pagination-and-filters.md)
> (§2.3 `…Page` envelope, §5.1 `cursor.go`); implement that first or in the same
> train, and **reuse its `cursor.go` codec verbatim**.

---

## 1. Summary & rationale

Today a customer can fetch a transfer only by a **known id** (`GET /transfers/{id}`) or reconstruct history by walking each account's ledger (`GET /accounts/{id}/ledger`) and merging client-side. fraudbank's "recent activity" and receipt-lookup screens need a single, cross-account, newest-first transfer list — without the client knowing every account id or doing N ledger fetches and a merge.

`GET /transfers` returns every transfer where the **caller owns either the debit or the credit account**, newest first, keyset-paginated with the same envelope + opaque cursor as the ledger. Filters: `status`, `kind`, `from`/`to` (by `requested_at`), `direction` *relative to the caller* (`out` = caller debits / `in` = caller credits), and `q` free-text. This is distinct from the operator-only `GET /transfers/pending` (admin tag, unscoped) — `GET /transfers` is **client-tag, ownership-scoped**.

Per [`../../CLAUDE.md`](../../CLAUDE.md) rule 1, scoping and paging live in SQL: the ownership predicate (`caller owns debit OR credit`) is in the query, not assembled in Go. This is a set-returning query whose ownership join sqlc expands fine (plain `SELECT`), so it is **plain sqlc**, not hand-written pgx.

---

## 2. API — OpenAPI 3.1

Edit `api/openapi.yaml`. **There is already a `/transfers` path with a `post` (createTransfer).** Add a `get` to that same path object — do not create a second `/transfers`. Add the filter parameters and the page schema.

### 2.1 Operation (add a `get:` under the existing `/transfers:` path)

```yaml
  /transfers:
    get:
      operationId: listMyTransfers
      tags: [client]
      summary: List the caller's transfers across all their accounts (keyset-paginated)
      parameters:
        - { $ref: "#/components/parameters/LedgerCursor" }
        - { $ref: "#/components/parameters/Limit" }
        - { $ref: "#/components/parameters/FilterFrom" }
        - { $ref: "#/components/parameters/FilterTo" }
        - { $ref: "#/components/parameters/TransferStatusFilter" }
        - { $ref: "#/components/parameters/TransferKindFilter" }
        - { $ref: "#/components/parameters/TransferDirectionFilter" }
        - { $ref: "#/components/parameters/FilterQ" }
      responses:
        "200":
          description: ok
          content:
            application/json:
              schema: { $ref: "#/components/schemas/TransferPage" }
        "400": { $ref: "#/components/responses/Error" }
    post:
      # ... existing createTransfer operation, unchanged ...
```

> `LedgerCursor`, `FilterFrom`, `FilterTo`, `FilterQ`, `Limit` are the reusable parameters defined in the ledger spec (§2.2). `LedgerCursor` is generic ("opaque keyset cursor"); reuse it. If implementing this spec before the ledger spec, add those four parameter definitions here instead.

### 2.2 New parameters (add under `components.parameters`)

```yaml
    TransferStatusFilter:
      name: status
      in: query
      required: false
      schema: { type: string, enum: [pending, posted, failed, canceled, reversed] }
    TransferKindFilter:
      name: kind
      in: query
      required: false
      schema: { type: string, enum: [transfer, deposit, withdrawal, reversal] }
    TransferDirectionFilter:
      name: direction
      in: query
      required: false
      description: >-
        Relative to the caller: out = caller's account is the debit side;
        in = caller's account is the credit side.
      schema: { type: string, enum: [out, in] }
```

> Confirm the `transfer_status` / `transfer_kind` enum members against `db/migrations/00002_init_enums.sql` before finalizing the `enum:` lists (the spec must match the DB enum exactly). Adjust if the DB defines additional members.

### 2.3 Schemas (add under `components.schemas`)

`TransferListItem` is a self-describing row: the transfer plus the caller-relative direction and the counterparty label, so the client renders "recent activity" without a second lookup.

```yaml
    TransferListItem:
      type: object
      properties:
        id: { type: string, format: uuid }
        debit_account_id: { type: string, format: uuid }
        credit_account_id: { type: string, format: uuid }
        amount_minor: { type: integer, format: int64 }
        currency: { type: string }
        status: { type: string }
        kind: { type: string }
        description: { type: string }
        direction: { type: string, enum: [out, in], description: "Relative to the caller." }
        counterparty_iban: { type: string }
        counterparty_owner: { type: string, description: "Masked owner name of the other party." }
        requested_at: { type: string, format: date-time }
        posted_at: { type: string, format: date-time }
    TransferPage:
      type: object
      required: [items, has_more]
      properties:
        items:
          type: array
          items: { $ref: "#/components/schemas/TransferListItem" }
        next_cursor: { type: string, nullable: true }
        has_more: { type: boolean }
```

After editing: `task generate:oapi`. The build breaks until `ListMyTransfers` is implemented — intended.

> **Routing note (`mode=all`).** `GET /transfers` (client) and `GET /transfers/pending` (admin) must not shadow each other. Today `/transfers/pending` is registered on the **parent** router behind `requireSession` *before* the client subrouter precisely so the greedy client `/transfers/{id}` doesn't swallow it ([`../../CLAUDE.md`](../../CLAUDE.md) "Three surfaces"). `GET /transfers` has **no** path variable, so it does not collide with `/transfers/pending` or `/transfers/{id}` — gorilla/mux matches the exact path. No special registration needed; verify in the routing test (§6.2).

---

## 3. Data model

**No migration required.** Reads `transfers` joined to `accounts` (for ownership and counterparty IBAN) and `users` (for masked counterparty name, via the existing `mask_name()` from `00016`). Indexes already present and used:

- `idx_transfers_debit (debit_account_id, created_at DESC)` and `idx_transfers_credit (credit_account_id, created_at DESC)` (`00004`) — the per-side history indexes; the query's `OR` over the two sides hits each.
- Keyset order is `(requested_at, id) DESC` — matching `SearchTransfers`. There is no composite index on `(requested_at, id)`; for a single customer the transfer set is small and bounded by the ownership predicate, so a sort is cheap. **Do not add an index speculatively.** If a heavy-account profile later shows it hot, add `CREATE INDEX idx_transfers_requested ON transfers (requested_at DESC, id DESC)` in a follow-up migration.

> **System / GL accounts:** a deposit is `EXTERNAL_CLEARING -> customer`; the customer owns the credit side, the system account has `user_id IS NULL`. The ownership predicate keys on `user_id = caller`, so system legs never match as "owned" — a deposit correctly appears as a single `in` row for the customer, never duplicated. Counterparty for such a row is the system account (IBAN NULL); surface `counterparty_owner = ''` and `counterparty_iban` from `system_code` per the `enriched_ledger` convention.

---

## 4. Query — `db/queries/transfers.sql`

Add next to `SearchTransfers` (same keyset + filter idiom). Plain sqlc `:many`. The caller's user id (`p_subject`) is the scoping arg; `direction` is computed relative to it.

```sql
-- name: ListMyTransfers :many
-- Cross-account transfer history for one customer (the JWT subject). A row is
-- visible iff the caller owns the debit OR the credit account. direction is
-- caller-relative ('out' = caller debits, 'in' = caller credits). Composite
-- (requested_at, id) keyset cursor (same as SearchTransfers); NULL/'' filter
-- args = no filter. Caller passes page_limit = limit + 1 to detect has_more.
SELECT t.id, t.debit_account_id, t.credit_account_id, t.amount_minor, t.currency,
       t.status, t.kind, t.description, t.requested_at, t.posted_at,
       CASE WHEN da.user_id = sqlc.arg(subject)::uuid THEN 'out' ELSE 'in' END AS direction,
       CASE WHEN da.user_id = sqlc.arg(subject)::uuid
            THEN COALESCE(ca.iban, ca.system_code, '')
            ELSE COALESCE(da.iban, da.system_code, '') END AS counterparty_iban,
       CASE WHEN da.user_id = sqlc.arg(subject)::uuid
            THEN mask_name(cu.full_name)
            ELSE mask_name(du.full_name) END AS counterparty_owner
FROM transfers t
JOIN accounts da ON da.id = t.debit_account_id
JOIN accounts ca ON ca.id = t.credit_account_id
LEFT JOIN users du ON du.id = da.user_id
LEFT JOIN users cu ON cu.id = ca.user_id
WHERE (da.user_id = sqlc.arg(subject)::uuid OR ca.user_id = sqlc.arg(subject)::uuid)
  AND (sqlc.narg(cursor)::timestamptz IS NULL
       OR (t.requested_at, t.id) < (sqlc.narg(cursor)::timestamptz, sqlc.narg(cursor_id)::uuid))
  AND (sqlc.narg(status)::transfer_status IS NULL OR t.status = sqlc.narg(status)::transfer_status)
  AND (sqlc.narg(kind)::transfer_kind     IS NULL OR t.kind   = sqlc.narg(kind)::transfer_kind)
  AND (sqlc.narg(from_ts)::timestamptz    IS NULL OR t.requested_at >= sqlc.narg(from_ts)::timestamptz)
  AND (sqlc.narg(to_ts)::timestamptz      IS NULL OR t.requested_at <  sqlc.narg(to_ts)::timestamptz)
  AND (sqlc.narg(dir)::text IS NULL
       OR (sqlc.narg(dir)::text = 'out' AND da.user_id = sqlc.arg(subject)::uuid)
       OR (sqlc.narg(dir)::text = 'in'  AND ca.user_id = sqlc.arg(subject)::uuid))
  AND (sqlc.narg(q)::text IS NULL OR sqlc.narg(q)::text = ''
       OR t.description ILIKE '%' || sqlc.narg(q) || '%'
       OR COALESCE(da.iban::text, '') ILIKE '%' || sqlc.narg(q) || '%'
       OR COALESCE(ca.iban::text, '') ILIKE '%' || sqlc.narg(q) || '%'
       OR word_similarity(sqlc.narg(q)::text, t.description) > 0.3)
ORDER BY t.requested_at DESC, t.id DESC
LIMIT sqlc.arg(page_limit)::int;
```

`task generate:sqlc` produces `ListMyTransfersParams` (`Subject uuid.UUID`, `Cursor *time.Time`, `CursorID *uuid.UUID`, `Status *TransferStatus`, `Kind *TransferKind`, `FromTs`, `ToTs *time.Time`, `Dir *string`, `Q *string`, `PageLimit int32`) and a `ListMyTransfersRow`.

> **Self-transfer edge case:** if the caller owns *both* sides (a transfer between their own two accounts), `da.user_id = subject` is true, so the row is classed `direction='out'` and the counterparty is the credit (their other) account. That is one row, correctly. A `direction=in` filter then **excludes** it (it is an `out` row); a `direction=out` filter includes it. Document this in the query comment; it matches the "relative to the caller, debit side wins the tie" rule.

---

## 5. Handler logic — `internal/api/handlers_transfers.go`

Add `ListMyTransfers`. There is **no idempotency** (read-only). Ownership is enforced in the query (the `subject` predicate) — there is no per-row 404; a non-owned transfer simply doesn't appear.

1. **Subject required:** `subj, ok := clientSubject(r.Context())`. This is a client-tag-only operation; if `!ok` (shouldn't happen behind `requireJWT`), `writeError(w, 401, "unauthorized", "client token required")`.
2. **Cursor:** reuse `decodeCursor` from `cursor.go` (ledger spec §5.1). `params.Cursor` non-empty → decode; on error → **400** `bad_request` "invalid cursor". Set `q.Cursor`/`q.CursorID`.
3. **Enum filters:** validate `status` against `transfer_status` and `kind` against `transfer_kind` (the same switch idiom `SetAccountStatus` uses); invalid → **400**. Map to `*sqlc.TransferStatus` / `*sqlc.TransferKind`.
4. **`direction`:** must be `out` or `in` if present; else **400**. Pass as `*string` to `Dir`.
5. **`from`/`to`:** `*time.Time` from the wrapper; both present and `from >= to` → **400**.
6. **`q`:** pass through.
7. **Limit + has_more + envelope:** identical mechanics to the ledger handler — `eff = s.limitOr(params.Limit)`, query `PageLimit = eff+1`, trim the sentinel, `next_cursor = encodeCursor(last.RequestedAt, last.ID)`, **init `items` non-nil** so an empty list is `[]` not `null`.
8. **DB errors:** `mapDBError`.

Sketch:

```go
type transferPage struct {
	Items      []sqlc.ListMyTransfersRow `json:"items"`
	NextCursor *string                   `json:"next_cursor"`
	HasMore    bool                      `json:"has_more"`
}

func (s *Server) ListMyTransfers(w http.ResponseWriter, r *http.Request, params genclient.ListMyTransfersParams) {
	subj, ok := clientSubject(r.Context())
	if !ok {
		writeError(w, http.StatusUnauthorized, "unauthorized", "client token required"); return
	}
	q := sqlc.ListMyTransfersParams{Subject: subj}

	if params.Cursor != nil && *params.Cursor != "" {
		c, err := decodeCursor(*params.Cursor)
		if err != nil { writeError(w, http.StatusBadRequest, "bad_request", "invalid cursor"); return }
		q.Cursor, q.CursorID = &c.ts, &c.id
	}
	if params.Status != nil {
		st := sqlc.TransferStatus(*params.Status)
		if !validTransferStatus(st) { writeError(w, http.StatusBadRequest, "bad_request", "invalid status"); return }
		q.Status = &st
	}
	if params.Kind != nil {
		k := sqlc.TransferKind(*params.Kind)
		if !validTransferKind(k) { writeError(w, http.StatusBadRequest, "bad_request", "invalid kind"); return }
		q.Kind = &k
	}
	if params.Direction != nil && *params.Direction != "out" && *params.Direction != "in" {
		writeError(w, http.StatusBadRequest, "bad_request", "invalid direction"); return
	}
	if params.From != nil && params.To != nil && !params.From.Before(*params.To) {
		writeError(w, http.StatusBadRequest, "bad_request", "from must be before to"); return
	}
	q.Dir, q.FromTs, q.ToTs, q.Q = params.Direction, params.From, params.To, params.Q

	eff := s.limitOr(params.Limit)
	q.PageLimit = eff + 1
	rows, err := s.pg.Queries.ListMyTransfers(r.Context(), q)
	if err != nil { mapDBError(w, err); return }

	hasMore := int32(len(rows)) > eff
	if hasMore { rows = rows[:eff] }
	items := make([]sqlc.ListMyTransfersRow, 0, len(rows))
	items = append(items, rows...)
	var next *string
	if hasMore && len(items) > 0 {
		last := items[len(items)-1]
		c := encodeCursor(last.RequestedAt, last.ID)
		next = &c
	}
	writeJSON(w, http.StatusOK, transferPage{Items: items, NextCursor: next, HasMore: hasMore})
}
```

Add small `validTransferStatus` / `validTransferKind` helpers (switch over the sqlc enum constants), mirroring the `SetAccountStatus` validation pattern. `posted_at` is nullable in the row (`*time.Time`); JSON omits it for pending/failed/canceled — matches the existing `Transfer` schema.

---

## 6. Tests to add

### 6.1 DB integration (`internal/db/integration_test.go`)

- **`TestListMyTransfersScoping`** — alice & bob each with an account; create transfers alice→bob, bob→alice, and bob→carol. Query with `Subject=alice`: returns the first two, **not** bob→carol. Assert `direction` is `out` for alice→bob and `in` for bob→alice; counterparty fields point at the *other* party.
- **`TestListMyTransfersFilters`** — seed varied `status`/`kind`/amounts/timestamps; assert `status=posted`, `kind=deposit`, `from`/`to`, `direction=out`/`in`, and `q` each narrow correctly; all-NULL returns everything owned.
- **`TestListMyTransfersKeysetTies`** — create many transfers for one subject inside one statement (shared `requested_at`); page with `PageLimit: 5` following `(Cursor, CursorID)`; assert every id seen exactly once (the tie-skip regression, mirroring `TestSearchKeysetPaginationTies`).
- **`TestListMyTransfersSelfTransfer`** — alice→alice (two own accounts): one row, `direction='out'`; included under `direction=out`, excluded under `direction=in`.

### 6.2 API integration (`internal/api/integration_test.go`)

- **`TestHTTPListMyTransfers`** — alice creates a transfer to bob (reusing the existing `mkTransfer` helper in `TestHTTPClientJWTOwnership`); `GET /transfers` as alice returns it in `items` with `direction:"out"`; as bob returns it with `direction:"in"`; a third user sees `items:[]` (non-null) and `has_more:false`.
- **`TestHTTPListMyTransfersPaging`** — > `limit` transfers, `?limit=2`, follow `next_cursor`, assert no id repeats, last page `has_more:false`.
- **`TestHTTPListMyTransfersRouting`** — assert in `mode=all` that `GET /transfers` (client bearer) returns 200 and `GET /transfers/pending` (admin session) still returns the pending queue — neither shadows the other.
- **`TestHTTPListMyTransfersBadFilters`** — `?status=bogus`, `?kind=bogus`, `?direction=sideways`, `?cursor=not-base64` each → **400**.

---

## 7. Security considerations

- **Ownership is the WHERE clause.** A row is returned only if `da.user_id = subject OR ca.user_id = subject`; there is no way to page into another user's transfers regardless of cursor, filters, or limit — the predicate is unconditional and parameter-bound. (Same guarantee class as the ledger's per-account 404, expressed as a set predicate.)
- **Opaque, non-authoritative cursor.** Same as the ledger: the cursor encodes only `(requested_at, id)`; a forged cursor cannot bypass the ownership predicate. Malformed → **400**, never an unbounded scan.
- **Counterparty masking.** `counterparty_owner` is `mask_name(full_name)` (the same masking used for confirmation-of-payee in `00016`), never the raw name; balances are never exposed. This matches what the caller can already learn from `enriched_ledger`/`resolve_account_by_iban`, so no new disclosure.
- **No injection.** All filters and `q` are bound parameters; `q` ILIKE is scoped to the caller's own transfers.
- **DoS bound.** `limit ≤ 200` via `limitOr`; result set bounded by the per-subject ownership predicate.

---

## 8. Acceptance criteria

- [ ] `GET /transfers` returns `{items, next_cursor, has_more}`; `items` always a JSON array (`[]`, never `null`).
- [ ] Only transfers where the caller owns the debit or credit side are returned; a non-party sees none (verified alice/bob/carol).
- [ ] `direction` on each item is **caller-relative** (`out`/`in`); the `direction` *filter* matches that semantics, with the self-transfer tie resolved to `out`.
- [ ] Filters `status`, `kind`, `from`, `to`, `direction`, `q` apply server-side; all-absent = full owned history newest-first.
- [ ] Composite `(requested_at, id)` keyset: no tie-skip across page boundaries (DB test).
- [ ] Invalid `status`/`kind`/`direction`/`cursor`/`from>=to` → **400** `{error,message}`.
- [ ] In `mode=all`, `GET /transfers` and `GET /transfers/pending` coexist (routing test).
- [ ] `go build ./... && go vet ./...` clean; DB + API suites pass on PG18.

---

## 9. Implementation order

1. Land (or co-deliver) [`spec-ledger-pagination-and-filters.md`](spec-ledger-pagination-and-filters.md) so `cursor.go` and the reusable filter parameters exist.
2. Confirm `transfer_status` / `transfer_kind` enum members in `db/migrations/00002_init_enums.sql`; finalize the §2.2 `enum:` lists to match exactly.
3. Edit `api/openapi.yaml` (§2): add `get` to `/transfers`, the three filter params, `TransferListItem` + `TransferPage`. `task generate:oapi` (build breaks — expected).
4. Add `ListMyTransfers` to `db/queries/transfers.sql` (§4); `task generate:sqlc`.
5. Implement `ListMyTransfers` handler + `validTransferStatus`/`validTransferKind` (§5); `go build ./...`.
6. DB tests (§6.1) then API tests (§6.2); `task test:db`.
7. `go vet`; add a `GET /transfers` row to [`../06-client-api.md`](../06-client-api.md) §1 (Transfers area).
