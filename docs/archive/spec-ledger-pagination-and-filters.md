# Spec — Ledger pagination envelope, composite cursor, and server-side filters

> ✅ **IMPLEMENTED (2026-06-14, branch `improve/add-features`)** — the **bare-array**
> variant. The `{items, next_cursor, has_more}` envelope was **dropped** (decision
> 2026-06-13, [`../09-fraudbank-bff-plan.md`](../09-fraudbank-bff-plan.md) §0.2); list
> endpoints stay bare arrays (always `[]`, never `null`; end-of-data = a short page).
> What shipped on `GET /accounts/{id}/ledger`: the **composite-keyset cursor
> `(posted_at, id)`** (pass `cursor` + `cursor_id` from the last row — fixes the
> tie-skip bug) and the **server-side filters** `from`/`to`/`direction`/`q`/`min_minor`/
> `max_minor`. No migration (reuses `enriched_ledger` + the `(account_id, posted_at
> DESC, id DESC)` index). Query in `db/queries/transfers.sql` (`GetAccountLedger`),
> handler in `internal/api/handlers_accounts.go`; tests
> `internal/db/ledger_test.go` (`TestLedgerKeysetCoversTies`) +
> `internal/api/ledger_test.go` (`TestHTTPLedgerFilters`). **Read the envelope
> sections below as superseded** — the cursor + filter design stands.

> Status: **spec, ready to implement.** Three related changes to
> `GET /accounts/{id}/ledger`, specced together because they all touch the same
> handler, query, and response shape. Completes the two ledger P0 rows and the
> "server-side ledger filters" P1 row in
> [`../09-fraudbank-bff-plan.md`](../09-fraudbank-bff-plan.md) §2. Builds on the
> composite-keyset pattern already shipped for the console
> (`AccountStatement` in `db/queries/transfers.sql`) and `SearchTransfers`.

---

## 1. Summary & rationale

Today `GET /accounts/{id}/ledger` returns a **bare JSON array** of `LedgerEntry`,
paginated with a `cursor` that is *only* `posted_at` (`WHERE posted_at < $cursor`).
Three defects, all hit by the fraudbank clients (web/iOS/Android):

| # | Defect | Impact |
|---|--------|--------|
| 1 | An exhausted cursor or an empty account returns HTTP 200 with body **`null`** (Go marshals a nil slice as `null`). | Typed clients (`[LedgerEntry]` / `List<LedgerEntry>`) throw a decode error; fraudbank added per-client null-tolerance as a workaround (item 7, 2026-06-13). |
| 2 | `posted_at`-only cursor **skips ledger rows tied on the same millisecond** at a page boundary. | A transfer's two legs and any same-`now()` batch share a `posted_at`; the statement silently loses rows. Already proven by the console keyset test (`TestSearchKeysetPaginationTies`). |
| 3 | No server-side filters. | Date-range / direction / free-text / amount search means fetching every page and filtering client-side — wrong for 70-row accounts, hopeless for real ones. |

This spec replaces the bare array with a **pagination envelope** (`{items, next_cursor, has_more}`), switches to an **opaque composite `(posted_at, id)` keyset cursor**, and adds **server-side filters** (`from`, `to`, `direction`, `q`, `min_minor`, `max_minor`). `items` is always a non-nil array, so defect 1 dies with the bare array. The envelope is a **breaking response-shape change**; §8 specifies the versioning / migration path so the live fraudbank clients don't break.

Per [`../../CLAUDE.md`](../../CLAUDE.md) rule 1, no business logic moves into Go: filtering and keyset paging stay in SQL (a new sqlc query against `enriched_ledger`); the handler only decodes the cursor, scopes ownership, and shapes the envelope.

---

## 2. API — OpenAPI 3.1

Edit `api/openapi.yaml`. Replace the `GetAccountLedger` operation, add three reusable parameters, and add two schemas. Copy-pasteable, matching the file's style.

### 2.1 Operation (replace the existing `/accounts/{id}/ledger` block)

```yaml
  /accounts/{id}/ledger:
    get:
      operationId: getAccountLedger
      tags: [client]
      summary: Account statement (keyset-paginated, running balance, filterable)
      parameters:
        - { $ref: "#/components/parameters/Id" }
        - { $ref: "#/components/parameters/LedgerCursor" }
        - { $ref: "#/components/parameters/Limit" }
        - { $ref: "#/components/parameters/FilterFrom" }
        - { $ref: "#/components/parameters/FilterTo" }
        - { $ref: "#/components/parameters/FilterDirection" }
        - { $ref: "#/components/parameters/FilterQ" }
        - { $ref: "#/components/parameters/FilterMinMinor" }
        - { $ref: "#/components/parameters/FilterMaxMinor" }
      responses:
        "200":
          description: ok
          content:
            application/json:
              schema: { $ref: "#/components/schemas/LedgerPage" }
        "400": { $ref: "#/components/responses/Error" }
        "404": { $ref: "#/components/responses/Error" }
```

### 2.2 Parameters (add under `components.parameters`)

> **Do not reuse the existing `Cursor` parameter** (`schema: {type: string, format: date-time}`). The ledger cursor is now an opaque base64 token, not a timestamp. A new, ledger-local `LedgerCursor` keeps the admin `Cursor` (still `date-time`, used by `/transfers/pending`) untouched and avoids changing the generated `Cursor = time.Time` alias other handlers depend on.

```yaml
    LedgerCursor:
      name: cursor
      in: query
      required: false
      description: >-
        Opaque keyset cursor returned as next_cursor by a previous page.
        Encodes (posted_at, id). Treat as opaque; do not parse.
      schema: { type: string }
    FilterFrom:
      name: from
      in: query
      required: false
      description: Only entries posted at or after this instant (inclusive).
      schema: { type: string, format: date-time }
    FilterTo:
      name: to
      in: query
      required: false
      description: Only entries posted strictly before this instant (exclusive).
      schema: { type: string, format: date-time }
    FilterDirection:
      name: direction
      in: query
      required: false
      schema: { type: string, enum: [debit, credit] }
    FilterQ:
      name: q
      in: query
      required: false
      description: Free-text match over description, counterparty IBAN, and counterparty owner.
      schema: { type: string }
    FilterMinMinor:
      name: min_minor
      in: query
      required: false
      description: Minimum absolute amount in minor units (inclusive).
      schema: { type: integer, format: int64, minimum: 0 }
    FilterMaxMinor:
      name: max_minor
      in: query
      required: false
      description: Maximum absolute amount in minor units (inclusive).
      schema: { type: integer, format: int64, minimum: 0 }
```

### 2.3 Schemas (add under `components.schemas`)

```yaml
    LedgerPage:
      type: object
      required: [items, has_more]
      properties:
        items:
          type: array
          items: { $ref: "#/components/schemas/LedgerEntry" }
        next_cursor:
          type: string
          nullable: true
          description: Pass as ?cursor= to fetch the next page. null/absent at end of data.
        has_more:
          type: boolean
          description: True iff another page exists (a next_cursor is present).
```

`LedgerEntry` is unchanged.

After editing: `task generate:oapi` (regenerates `genclient`; `GetAccountLedgerParams` gains the new optional fields). The build breaks until the handler matches the new signature — that is the intended contract-first guardrail.

---

## 3. Data model

**No migration required.** Everything reads the existing `enriched_ledger` view (`db/migrations/00010_views.sql`) and the existing index `idx_ledger_account_posted (account_id, posted_at DESC, id DESC)` (`00004_init_indexes.sql`) — which is exactly the keyset order, so the new query is index-covered.

`q` free-text uses **ILIKE** plus trigram `word_similarity`, mirroring `SearchTransfers`. The trigram GIN index on `transfers.description` already exists (`00013_search.sql`); `enriched_ledger.description` is `transfers.description`, so `q` against the description is index-assisted. Counterparty IBAN/owner are matched with ILIKE (no dedicated index; bounded by the `account_id` predicate, which the composite index already satisfies — acceptable, the result set per account is small).

> If a future profiler shows `q` on counterparty fields is hot, add trigram GIN indexes in a later migration. Out of scope here; do **not** add an unused index.

---

## 4. Query — `db/queries/transfers.sql`

Add a new sqlc query next to `AccountStatement` (which it closely mirrors). Plain sqlc, `:many`.

```sql
-- name: GetAccountLedgerPage :many
-- Client statement page: composite (posted_at, id) keyset cursor (fixes the
-- same-millisecond tie-skip) plus optional server-side filters. NULL/'' filter
-- args mean "no filter" (sqlc.narg). Mirrors AccountStatement; reads the single
-- source of truth (enriched_ledger). Caller passes page_limit = limit + 1 to
-- detect has_more.
SELECT id, transfer_id, account_id, account_iban, direction, amount_minor, signed_amount,
       balance_after, currency, posted_at, transfer_kind, transfer_status, description,
       counterparty_iban, counterparty_owner
FROM enriched_ledger
WHERE account_id = sqlc.arg(account_id)::uuid
  AND (sqlc.narg(cursor)::timestamptz IS NULL
       OR (posted_at, id) < (sqlc.narg(cursor)::timestamptz, sqlc.narg(cursor_id)::uuid))
  AND (sqlc.narg(from_ts)::timestamptz   IS NULL OR posted_at >= sqlc.narg(from_ts)::timestamptz)
  AND (sqlc.narg(to_ts)::timestamptz     IS NULL OR posted_at <  sqlc.narg(to_ts)::timestamptz)
  AND (sqlc.narg(direction)::entry_direction IS NULL OR direction = sqlc.narg(direction)::entry_direction)
  AND (sqlc.narg(min_minor)::bigint      IS NULL OR amount_minor >= sqlc.narg(min_minor)::bigint)
  AND (sqlc.narg(max_minor)::bigint      IS NULL OR amount_minor <= sqlc.narg(max_minor)::bigint)
  AND (sqlc.narg(q)::text IS NULL OR sqlc.narg(q)::text = ''
       OR description                  ILIKE '%' || sqlc.narg(q) || '%'
       OR COALESCE(counterparty_iban::text, '')  ILIKE '%' || sqlc.narg(q) || '%'
       OR COALESCE(counterparty_owner, '')        ILIKE '%' || sqlc.narg(q) || '%'
       OR word_similarity(sqlc.narg(q)::text, description) > 0.3)
ORDER BY posted_at DESC, id DESC
LIMIT sqlc.arg(page_limit)::int;
```

`task generate:sqlc` produces `GetAccountLedgerPageParams` (fields `AccountID`, `Cursor *time.Time`, `CursorID *uuid.UUID`, `FromTs`, `ToTs`, `Direction *EntryDirection`, `MinMinor`, `MaxMinor *int64`, `Q *string`, `PageLimit int32`) and a `GetAccountLedgerPageRow` (identical columns to `AccountStatementRow`). This is a plain `SELECT`, so sqlc expands it — **no** hand-written pgx needed.

> Keep the old `GetAccountLedger` query in the file only if something else uses it; grep first. Nothing else does — **delete `GetAccountLedger`** and its generated method to avoid dead code (per repo discipline). The console uses `AccountStatement`, untouched here.

---

## 5. Handler logic — `internal/api/handlers_accounts.go`

### 5.1 Cursor codec (new file `internal/api/cursor.go`)

Match the existing console codec (`base64.RawURLEncoding`, `posted_at` as `RFC3339Nano`, `|`-joined with the id — see `console_handlers.go` `buildPageURL`/`pagerLinks`). Centralize it so spec B reuses it verbatim.

```go
package api

import (
	"encoding/base64"
	"errors"
	"strings"
	"time"

	"github.com/google/uuid"
)

// ledgerCursor is the opaque keyset position (posted_at, id). The wire form is
// base64url("<RFC3339Nano>|<uuid>") — same layout as the console pager, so the
// two stay debuggable with one decoder.
type ledgerCursor struct {
	ts time.Time
	id uuid.UUID
}

func encodeCursor(ts time.Time, id uuid.UUID) string {
	raw := ts.UTC().Format(time.RFC3339Nano) + "|" + id.String()
	return base64.RawURLEncoding.EncodeToString([]byte(raw))
}

// decodeCursor parses an opaque cursor. An empty string is the first page
// (zero value, ok=false-equivalent handled by caller passing nil). A malformed
// cursor is an error -> 400 (never a silent full-table scan).
func decodeCursor(s string) (ledgerCursor, error) {
	b, err := base64.RawURLEncoding.DecodeString(s)
	if err != nil {
		return ledgerCursor{}, errors.New("invalid cursor")
	}
	parts := strings.SplitN(string(b), "|", 2)
	if len(parts) != 2 {
		return ledgerCursor{}, errors.New("invalid cursor")
	}
	ts, err := time.Parse(time.RFC3339Nano, parts[0])
	if err != nil {
		return ledgerCursor{}, errors.New("invalid cursor")
	}
	id, err := uuid.Parse(parts[1])
	if err != nil {
		return ledgerCursor{}, errors.New("invalid cursor")
	}
	return ledgerCursor{ts: ts, id: id}, nil
}
```

### 5.2 Rewrite `GetAccountLedger`

Validation, scoping, idempotency (none — read-only), error mapping, edge cases:

1. **Ownership scoping (unchanged):** if `clientSubject(ctx)` is present, `AccountOwner(id)` then `ownsAccount` → **404** on mismatch. (The portal surface never reaches this client-tag handler, but the guard is harmless and matches `GetAccount`.)
2. **Cursor:** if `params.Cursor != nil && *params.Cursor != ""`, `decodeCursor`; on error → `writeError(w, 400, "bad_request", "invalid cursor")`. Set `cur.ts`/`cur.id` as `*time.Time`/`*uuid.UUID` query args; else leave both nil (first page).
3. **`direction`:** validate against `entry_direction` (`debit`/`credit`) like `SetAccountStatus` validates `account_status`; invalid → **400** `bad_request`. Map to `*sqlc.EntryDirection`.
4. **`from`/`to`:** already parsed to `*time.Time` by the generated wrapper (RFC3339). If both present and `from >= to` → **400** `bad_request` ("from must be before to").
5. **`min_minor`/`max_minor`:** `*int64`. If both present and `min > max` → **400**. (Negative values can't occur — schema `minimum: 0`; the wrapper rejects malformed ints with 400 already.)
6. **`q`:** pass through as `*string` (empty string treated as no-filter by the SQL).
7. **Limit + has_more:** `eff := s.limitOr(params.Limit)`; query with `PageLimit: eff + 1`. If `len(rows) > eff`: `hasMore = true`, drop the extra row, build `next_cursor = encodeCursor(last.PostedAt, last.ID)` from the **last kept** row. Else `hasMore = false`, `next_cursor = nil`.
8. **Envelope, never null:** initialize `items := make([]sqlc.GetAccountLedgerPageRow, 0, len(rows))` **before** appending, so an empty page marshals as `[]`, not `null` (defect 1). Respond `LedgerPage{Items: items, NextCursor: next, HasMore: hasMore}`.
9. **DB errors:** `mapDBError(w, err)` (an unknown `account_id` surfaces via the ownership lookup as 404; if scoping is absent, an empty result is a valid empty page, not 404 — matches today's behavior).

Sketch:

```go
type ledgerPage struct {
	Items      []sqlc.GetAccountLedgerPageRow `json:"items"`
	NextCursor *string                        `json:"next_cursor"`
	HasMore    bool                           `json:"has_more"`
}

func (s *Server) GetAccountLedger(w http.ResponseWriter, r *http.Request, id openapi_types.UUID, params genclient.GetAccountLedgerParams) {
	if subj, ok := clientSubject(r.Context()); ok {
		owner, err := s.pg.Queries.AccountOwner(r.Context(), uuid.UUID(id))
		if err != nil { mapDBError(w, err); return }
		if !ownsAccount(subj, owner) {
			writeError(w, http.StatusNotFound, "not_found", "account not found"); return
		}
	}

	q := sqlc.GetAccountLedgerPageParams{AccountID: uuid.UUID(id)}

	if params.Cursor != nil && *params.Cursor != "" {
		c, err := decodeCursor(*params.Cursor)
		if err != nil { writeError(w, http.StatusBadRequest, "bad_request", "invalid cursor"); return }
		q.Cursor, q.CursorID = &c.ts, &c.id
	}
	if params.Direction != nil {
		d := sqlc.EntryDirection(*params.Direction)
		if d != sqlc.EntryDirectionDebit && d != sqlc.EntryDirectionCredit {
			writeError(w, http.StatusBadRequest, "bad_request", "invalid direction"); return
		}
		q.Direction = &d
	}
	if params.From != nil && params.To != nil && !params.From.Before(*params.To) {
		writeError(w, http.StatusBadRequest, "bad_request", "from must be before to"); return
	}
	if params.MinMinor != nil && params.MaxMinor != nil && *params.MinMinor > *params.MaxMinor {
		writeError(w, http.StatusBadRequest, "bad_request", "min_minor must be <= max_minor"); return
	}
	q.FromTs, q.ToTs, q.MinMinor, q.MaxMinor, q.Q = params.From, params.To, params.MinMinor, params.MaxMinor, params.Q

	eff := s.limitOr(params.Limit)
	q.PageLimit = eff + 1

	rows, err := s.pg.Queries.GetAccountLedgerPage(r.Context(), q)
	if err != nil { mapDBError(w, err); return }

	hasMore := int32(len(rows)) > eff
	if hasMore { rows = rows[:eff] }
	items := make([]sqlc.GetAccountLedgerPageRow, 0, len(rows))
	items = append(items, rows...)

	var next *string
	if hasMore && len(items) > 0 {
		last := items[len(items)-1]
		c := encodeCursor(last.PostedAt, last.ID)
		next = &c
	}
	writeJSON(w, http.StatusOK, ledgerPage{Items: items, NextCursor: next, HasMore: hasMore})
}
```

> Field-name note: confirm generated params field names after `task generate:oapi` (oapi-codegen camelCases `min_minor` → `MinMinor`, `direction` → `Direction`, etc.). `*params.Direction` is a `*string`; cast to `sqlc.EntryDirection`.

---

## 6. Tests to add

### 6.1 DB integration (`internal/db/integration_test.go`)

- **`TestGetAccountLedgerPageTieKeyset`** — model on `TestSearchKeysetPaginationTies`. Post N transfers into one account inside a single statement so several `enriched_ledger` rows share `posted_at`; page with `PageLimit: 3` following `(Cursor, CursorID)`; assert every entry id is seen exactly once (no skip, no dup). This is the regression test for defect 2.
- **`TestGetAccountLedgerPageFilters`** — seed a mix of debits/credits/amounts/descriptions; assert: `direction=debit` returns only debits; `from`/`to` bound the window (inclusive/exclusive per §2.2); `min_minor`/`max_minor` bound amount; `q` matches description and counterparty owner (case-insensitive) and excludes non-matches; all-NULL args return the full set.
- **`TestGetAccountLedgerPageEmpty`** — account with zero entries returns `len(rows) == 0` and no error (the `[]` shaping is asserted at the HTTP layer, 6.2).

### 6.2 API integration (`internal/api/integration_test.go`)

Add to the client-JWT suite (reuse `clientToken`, `mkUser`, `mkAcct`, `get`, `body`):

- **`TestHTTPLedgerEnvelopeNotNull`** — `GET /accounts/{ownAcct}/ledger` on a freshly funded account: body parses as `{items, next_cursor, has_more}`; `items` is `[]` (never `null`) for an account/cursor with no rows; `has_more=false`, `next_cursor` absent/null. Assert the **raw body contains `"items":[]`**, not `null` (defect 1).
- **`TestHTTPLedgerKeysetPaging`** — fund + create > `limit` entries; first page `?limit=2` returns `has_more=true` + a `next_cursor`; follow `?cursor=<next_cursor>&limit=2`; assert no id repeats and the last page has `has_more=false`.
- **`TestHTTPLedgerFiltersAndBadCursor`** — `?direction=credit` returns only credits; `?direction=bogus` → 400; `?cursor=not-base64` → 400; `?from=...&to=...` with `from>=to` → 400.
- **`TestHTTPLedgerOwnership`** — alice cannot read bob's ledger → **404** (extend the existing IDOR test).

---

## 7. Security considerations

- **Ownership unchanged:** still scoped to `clientSubject` → 404 on non-owned accounts (the IDOR fix in [`../06-client-api.md`](../06-client-api.md) §4 is preserved; tested in 6.2).
- **Opaque cursor, no trust:** the cursor only encodes `(posted_at, id)`; it is **not** account-scoped, so a forged/replayed cursor cannot widen the result set — the `WHERE account_id = $1` predicate and the ownership check bound every page regardless of cursor contents. A malformed cursor is a hard **400**, never a silent unbounded scan.
- **No injection:** all filters bind as parameters via sqlc; `q` is a bound ILIKE pattern (`%`/`_` in user input only broaden the LIKE within the caller's own account — not an injection or a cross-tenant leak).
- **DoS bound:** `limit` is clamped by `limitOr` (≤ 200); `q` ILIKE is bounded by the per-account predicate.
- **No new PII:** `enriched_ledger` already exposes counterparty IBAN/owner to a statement the caller is entitled to; `q` searches the same fields, exposing nothing new.

---

## 8. Backward-compatibility & migration path

This changes a **response shape three live clients depend on** (fraudbank web/iOS/Android currently expect a *bare array* and were patched to tolerate `null`). Ship without breaking them:

**Recommended: additive envelope behind an opt-in, then flip the default.**

1. **Phase 1 (compatible).** Keep the default response a **bare `[]` array** (just apply defect-1's non-nil-slice fix immediately — it is strictly compatible and unblocks removing the client null-workarounds). Gate the envelope behind a request signal:
   - Accept `Accept: application/vnd.bank0.ledger+json;v=2` **or** a query flag `?envelope=1`. When present, return `LedgerPage`; otherwise return the bare (non-nil) array.
   - The composite keyset cursor is **opaque** — adopt it in *both* modes immediately. v1 clients pass back whatever `cursor` they received; the new opaque token is still just a `?cursor=` string to them. (The old clients sent an RFC3339 `cursor`; `decodeCursor` must therefore **fall back**: if base64 decode fails, try `time.Parse(RFC3339)` and treat it as `(ts, uuid.Max)` so a legacy timestamp cursor still pages correctly. Document this fallback in `cursor.go`.)
2. **Phase 2 (clients migrate).** Update fraudbank web/iOS/Android to send the `v=2` Accept (or `?envelope=1`) and consume `{items, next_cursor, has_more}`; drop the per-client null tolerance and short-page end-of-data inference.
3. **Phase 3 (flip default).** Once all three clients ship v2 (track via the client `version` header / app store rollout), make the envelope the default and treat the bare-array path as deprecated; remove it in the next major (`info.version` bump, note in `06-client-api.md`).

**Document the version bump:** bump `openapi.yaml` `info.version` (0.1.0 → 0.2.0) when the envelope becomes default, and add a row to [`../06-client-api.md`](../06-client-api.md) §1 + a changelog note. The opaque-cursor + non-null-array changes are safe to land in 0.1.x; the envelope-as-default is the 0.2.0 line.

> If the team prefers a clean break (acceptable since all three clients are first-party and version-locked to this backend): skip the Accept-header gate, ship the envelope as the only shape, and land the matching client changes in the **same** release train. Choose this only if the clients can be deployed atomically with the backend; otherwise use the phased path above.

---

## 9. Acceptance criteria

- [ ] `GET /accounts/{id}/ledger` (envelope mode) returns `{items, next_cursor, has_more}`; `items` is **always** a JSON array, `[]` when empty — never `null`.
- [ ] Two ledger entries tied on the same `posted_at` straddling a page boundary are **both** returned (no tie-skip); covered by a DB test mirroring `TestSearchKeysetPaginationTies`.
- [ ] `next_cursor` is opaque base64; round-trips through `?cursor=` to the exact next page; absent/null exactly when `has_more=false`.
- [ ] Filters `from`, `to`, `direction`, `q`, `min_minor`, `max_minor` apply server-side; all-absent = full statement.
- [ ] `direction` ∉ {debit,credit}, `from>=to`, `min_minor>max_minor`, and malformed `cursor` each return **400** with `{error,message}`.
- [ ] Ownership: a caller cannot read another user's ledger (**404**).
- [ ] Backward-compat path implemented per §8 (no live fraudbank client breaks on deploy).
- [ ] `go build ./... && go vet ./...` clean; DB + API suites pass against PG18; migrate up→down→up clean (no schema change, but confirm the dropped `GetAccountLedger` doesn't break callers).

---

## 10. Implementation order

1. **Defect-1 hotfix first (shippable alone):** in the current `GetAccountLedger`, initialize the slice non-nil before responding (`make([]T, 0, ...)`), so the bare-array endpoint returns `[]`. Land + deploy; fraudbank can drop null-workarounds independently of the rest.
2. Edit `api/openapi.yaml` (§2): new `LedgerCursor` + filter parameters, `LedgerPage` schema, rewritten operation. `task generate:oapi` (build now broken — expected).
3. Add `GetAccountLedgerPage` to `db/queries/transfers.sql` (§4); delete the unused `GetAccountLedger` query; `task generate:sqlc`.
4. Add `internal/api/cursor.go` (§5.1) with the legacy-timestamp fallback from §8.
5. Rewrite `GetAccountLedger` handler (§5.2) to satisfy the regenerated signature; `go build ./...`.
6. Implement the §8 phase-1 gate (Accept header / `?envelope=1`) if taking the phased path.
7. Add DB tests (§6.1) then API tests (§6.2); `task test:db`.
8. `go vet`, update [`../06-client-api.md`](../06-client-api.md) §1 statement row + changelog; bump `info.version` per the chosen §8 phase.
