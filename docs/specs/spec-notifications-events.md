# Spec — Notifications / events feed (`GET /me/events`)

> Status: **spec, ready to implement.** Phase 1 of the "Notifications / webhooks"
> P2 row in [`../09-fraudbank-integration.md`](../09-fraudbank-integration.md) §2
> ("`GET /me/events?cursor=` (poll) first; push (FCM/APNs token registry) later").
> Reuses the opaque composite cursor from the shipped ledger pagination
> (`internal/api/cursor.go`; as-built in [`../06-client-api.md`](../06-client-api.md)) —
> note lists return bare arrays, not a `…Page` envelope.

---

## 1. Summary & rationale

fraudbank clients (web/iOS/Android) poll on focus to discover incoming payments and transfer-status changes; there is no server feed, so every client re-fetches accounts + ledgers and diffs. This adds a **per-user append-only event feed** the clients poll once, newest-first, keyset-paginated:

| Event | Emitted when | Why a customer cares |
|-------|--------------|----------------------|
| `transfer.posted` | a transfer the user **debited** posts to the ledger | "Your €X payment to … completed" |
| `payment.incoming` | a transfer the user **credited** posts to the ledger | "You received €X from …" |
| `device.new` | a new refresh-token **family** is opened (a new login/device) | security: "New sign-in on …" |
| `dispute.updated` | a dispute on one of the user's transfers changes state | dispute tracking (forward-looking; see §3.4) |

Phase 1 is **poll-only**: `GET /me/events?cursor=` returns events for the JWT subject. Events are written **inside the same DB transaction** as the source state change (so an event never exists without its cause, and vice-versa), by the PL/pgSQL functions that already own those transitions — honoring [`../../CLAUDE.md`](../../CLAUDE.md) rule 1 (logic in the DB). Phase 2 (push/webhooks) is a note in §9, not built here.

The feed is a **derived projection** of state that already exists (transfers, refresh-token families); it is **not** a second source of truth for money. An event is a denormalized notification row, safe to lose without affecting the ledger.

---

## 2. API — OpenAPI 3.1

Edit `api/openapi.yaml`. Add one path and two schemas (client tag).

### 2.1 Operation (add under `paths`)

```yaml
  /me/events:
    get:
      operationId: listMyEvents
      tags: [client]
      summary: The caller's notification feed (security + money events), newest first
      parameters:
        - { $ref: "#/components/parameters/LedgerCursor" }
        - { $ref: "#/components/parameters/Limit" }
        - name: type
          in: query
          required: false
          description: Filter to one event type (e.g. payment.incoming).
          schema:
            type: string
            enum: [transfer.posted, payment.incoming, device.new, dispute.updated]
        - name: unread_only
          in: query
          required: false
          schema: { type: boolean }
      responses:
        "200":
          description: ok
          content:
            application/json:
              schema: { $ref: "#/components/schemas/EventPage" }
        "400": { $ref: "#/components/responses/Error" }
```

> `LedgerCursor` + `Limit` are the reusable params from the ledger spec (§2.2). If implementing this first, add `LedgerCursor` here.

### 2.2 Schemas (add under `components.schemas`)

```yaml
    Event:
      type: object
      required: [id, type, created_at]
      properties:
        id: { type: string, format: uuid }
        type:
          type: string
          enum: [transfer.posted, payment.incoming, device.new, dispute.updated]
        title: { type: string, description: "Short human-readable summary." }
        body: { type: string }
        related_transfer_id: { type: string, format: uuid, nullable: true }
        related_account_id: { type: string, format: uuid, nullable: true }
        data:
          type: object
          additionalProperties: true
          description: "Type-specific payload (amount_minor, counterparty_iban, user_agent, …)."
        read_at: { type: string, format: date-time, nullable: true }
        created_at: { type: string, format: date-time }
    EventPage:
      type: object
      required: [items, has_more, unread_count]
      properties:
        items:
          type: array
          items: { $ref: "#/components/schemas/Event" }
        next_cursor: { type: string, nullable: true }
        has_more: { type: boolean }
        unread_count:
          type: integer
          description: Total unread events for the caller (for a badge), independent of this page.
```

> **Optional mark-read companion** (recommended, keeps the badge usable). If included, add:
> ```yaml
>   /me/events/read:
>     post:
>       operationId: markEventsRead
>       tags: [client]
>       summary: Mark events read up to and including a cursor (or all)
>       requestBody:
>         required: false
>         content:
>           application/json:
>             schema:
>               type: object
>               properties:
>                 up_to_cursor: { type: string, description: "Mark read everything at/older than this cursor. Omit to mark all read." }
>       responses:
>         "200":
>           description: ok
>           content:
>             application/json:
>               schema:
>                 type: object
>                 properties: { marked: { type: integer } }
>         "400": { $ref: "#/components/responses/Error" }
> ```

After editing: `task generate:oapi` (build breaks until `ListMyEvents` — and `MarkEventsRead` if adopted — are implemented).

---

## 3. Data model — migration `00018_events.sql`

> **Migration number:** migrations are now a 9-file domain baseline (`00001_foundation.sql` / `00002_iban.sql` / `00003_users.sql` / `00004_accounts.sql` / `00005_transfers.sql` / `00006_maker_checker.sql` / `00007_maintenance.sql` / `00008_features.sql` / `00009_system_seed.sql`), so the next free slot on disk is `00010`, but several sibling specs in this directory also add a migration — [`../06-client-api.md`](../06-client-api.md) §6 flags new MFA tables, and `spec-disputes.md` / `spec-step-up-mfa.md` each claim "next free number" (self-registration + account opening already landed, folded into the domain baseline). **At implementation time, `ls db/migrations/` and take the actual next free number** (depending on merge order); update every illustrative `00018` reference in this file to match. Goose orders by the numeric prefix; never reuse one. If this lands **after** disputes, sequence the `dispute.updated` emission edit (§3.4) to `CREATE OR REPLACE` the *post-disputes* `resolve_dispute` body.

### 3.1 Enum + table

```sql
-- +goose Up
-- +goose StatementBegin

-- Per-user notification feed (docs/08 P2, phase 1). Append-only projection of
-- state that already exists (transfers, refresh-token families). NOT a money
-- source of truth: an event is a denormalized notification, safe to lose. Written
-- in the SAME txn as its cause by the functions that own that transition.
CREATE TYPE event_type AS ENUM (
    'transfer.posted',
    'payment.incoming',
    'device.new',
    'dispute.updated'
);

CREATE TABLE events (
    id                  UUID PRIMARY KEY DEFAULT uuidv7(),      -- UUIDv7: time-ordered, doubles as keyset tiebreak
    user_id             UUID NOT NULL REFERENCES users(id) ON DELETE CASCADE,
    type                event_type NOT NULL,
    title               TEXT NOT NULL DEFAULT '',
    body                TEXT NOT NULL DEFAULT '',
    related_transfer_id UUID REFERENCES transfers(id) ON DELETE SET NULL,
    related_account_id  UUID REFERENCES accounts(id)  ON DELETE SET NULL,
    data                JSONB NOT NULL DEFAULT '{}',
    read_at             TIMESTAMPTZ,
    created_at          TIMESTAMPTZ NOT NULL DEFAULT now(),
    -- Idempotent emission: at most one event per (user, type, transfer). NULL
    -- transfer_id (e.g. device.new) is exempt from this uniqueness (multiple NULLs
    -- allowed by Postgres), which is correct — device events dedupe on family below.
    UNIQUE (user_id, type, related_transfer_id)
);

-- Feed read path: keyset (created_at, id) DESC per user.
CREATE INDEX idx_events_user_created ON events (user_id, created_at DESC, id DESC);
-- Unread badge / unread_only filter (partial: unread rows only).
CREATE INDEX idx_events_user_unread  ON events (user_id) WHERE read_at IS NULL;

-- +goose StatementEnd
```

### 3.2 Append-only guard (match the ledger discipline)

Events are a record of things that happened; they may only be **inserted** and have `read_at` set. Block edits to the immutable columns and all deletes (cascade on user delete is the only removal), mirroring `ledger_block_mutation` in `00005`:

```sql
-- +goose StatementBegin
CREATE OR REPLACE FUNCTION events_block_mutation() RETURNS TRIGGER AS $$
BEGIN
    IF TG_OP = 'DELETE' THEN
        RAISE EXCEPTION 'events is append-only (DELETE blocked)' USING ERRCODE = 'restrict_violation';
    END IF;
    -- UPDATE: only read_at may change (mark-read). Everything else is frozen.
    IF NEW.id IS DISTINCT FROM OLD.id
       OR NEW.user_id IS DISTINCT FROM OLD.user_id
       OR NEW.type IS DISTINCT FROM OLD.type
       OR NEW.title IS DISTINCT FROM OLD.title
       OR NEW.body IS DISTINCT FROM OLD.body
       OR NEW.related_transfer_id IS DISTINCT FROM OLD.related_transfer_id
       OR NEW.related_account_id IS DISTINCT FROM OLD.related_account_id
       OR NEW.data IS DISTINCT FROM OLD.data
       OR NEW.created_at IS DISTINCT FROM OLD.created_at THEN
        RAISE EXCEPTION 'events rows are immutable except read_at' USING ERRCODE = 'restrict_violation';
    END IF;
    RETURN NEW;
END;
$$ LANGUAGE plpgsql;
-- +goose StatementEnd

CREATE TRIGGER trg_events_immutable
    BEFORE UPDATE OR DELETE ON events
    FOR EACH ROW EXECUTE FUNCTION events_block_mutation();
```

### 3.3 Emit helper + mark-read

```sql
-- +goose StatementBegin
-- emit_event: idempotent insert (the UNIQUE (user, type, transfer) absorbs a
-- re-emit on idempotent replay of the source money move). Returns the event id
-- (new or existing). Safe to call from inside the source function's txn.
CREATE OR REPLACE FUNCTION emit_event(
    p_user_id      UUID,
    p_type         event_type,
    p_title        TEXT,
    p_body         TEXT,
    p_transfer_id  UUID  DEFAULT NULL,
    p_account_id   UUID  DEFAULT NULL,
    p_data         JSONB DEFAULT '{}'
) RETURNS UUID AS $$
DECLARE v_id UUID;
BEGIN
    INSERT INTO events (user_id, type, title, body, related_transfer_id, related_account_id, data)
    VALUES (p_user_id, p_type, COALESCE(p_title,''), COALESCE(p_body,''), p_transfer_id, p_account_id, COALESCE(p_data,'{}'::jsonb))
    ON CONFLICT (user_id, type, related_transfer_id) DO NOTHING
    RETURNING id INTO v_id;
    IF v_id IS NULL THEN
        SELECT id INTO v_id FROM events
         WHERE user_id = p_user_id AND type = p_type AND related_transfer_id IS NOT DISTINCT FROM p_transfer_id;
    END IF;
    RETURN v_id;
END;
$$ LANGUAGE plpgsql;

-- mark_events_read: set read_at on the caller's unread events at/older than a
-- cursor position, or all when p_cursor_ts is NULL. Returns the count touched.
CREATE OR REPLACE FUNCTION mark_events_read(
    p_user_id   UUID,
    p_cursor_ts TIMESTAMPTZ DEFAULT NULL,
    p_cursor_id UUID        DEFAULT NULL
) RETURNS INT AS $$
DECLARE v_n INT;
BEGIN
    UPDATE events SET read_at = now()
     WHERE user_id = p_user_id AND read_at IS NULL
       AND (p_cursor_ts IS NULL OR (created_at, id) <= (p_cursor_ts, p_cursor_id));
    GET DIAGNOSTICS v_n = ROW_COUNT;
    RETURN v_n;
END;
$$ LANGUAGE plpgsql;
-- +goose StatementEnd
```

### 3.4 Write-points — where each event is emitted

Each emission goes **inside the function that already performs the transition**, in the same transaction, so the event and its cause commit or roll back together.

| Event | Function (file) | Insert site | Recipient(s) |
|-------|-----------------|-------------|--------------|
| `transfer.posted` | `post_transfer(p_transfer_id)` in `00005_transfers.sql` — **edit this function** (`CREATE OR REPLACE` in this spec's new migration, do **not** alter `00005`) | after the `UPDATE transfers SET status='posted'`, look up the **debit** account's `user_id` (skip if NULL/system); `emit_event(debit_owner, 'transfer.posted', …, transfer_id, debit_account_id, jsonb_build_object('amount_minor', v_t.amount_minor, 'counterparty', credit label))` | debit-side owner (the payer) |
| `payment.incoming` | same `post_transfer` | same site, for the **credit** account's `user_id` (skip if NULL/system) — for a `deposit` the credit owner is the customer; for an internal transfer it's the recipient | credit-side owner (the payee) |
| `device.new` | `issue_refresh_token(...)` in `00003_users.sql` — **edit via `CREATE OR REPLACE` in this spec's new migration** | after inserting the new family row, `emit_event(p_user_id, 'device.new', 'New sign-in', …, NULL, NULL, jsonb_build_object('user_agent', p_user_agent, 'ip', p_ip, 'family_id', v_family))`. Because `related_transfer_id` is NULL, the `(user,type,NULL)` uniqueness does not dedupe — dedupe instead on `family_id` by adding a partial guard (see note) | the logging-in user |
| `dispute.updated` | disputes has **shipped**: `resolve_dispute(p_dispute_id, p_resolver, p_status, p_note)` (status machine `open → under_review → resolved/rejected`) lives in `db/migrations/` (as-built: [`../05-admin-ui.md`](../05-admin-ui.md) §4.7). Edit it (via `CREATE OR REPLACE` in this migration, sequenced *after* disputes) to `emit_event(dispute_owner, 'dispute.updated', …, related_transfer_id := <disputed transfer>, data := jsonb_build_object('dispute_id', p_dispute_id, 'status', p_status))` after the status UPDATE. The dispute owner is the customer who filed it (`disputes.filed_by` / the disputed transfer's owning party — confirm the column name against the `disputes` table in `db/migrations/`). The `(user, type, related_transfer_id)` unique would collapse multiple status changes on one dispute into one row — for `dispute.updated`, **exclude it from that unique** (it is keyed per-transfer, but you want one event per *status change*); emit via a direct INSERT (no `ON CONFLICT`) rather than `emit_event`, or add `status` to the dedupe. Document the choice. | dispute filer |

> **`device.new` dedupe:** since `related_transfer_id` is NULL for device events, the `(user, type, related_transfer_id)` unique index won't prevent duplicates across logins (and shouldn't — each new family is a distinct device event). To make emission idempotent against a retried `issue_refresh_token`, store `family_id` in `data` and add a **partial unique index** on the extracted family id:
> ```sql
> CREATE UNIQUE INDEX uq_events_device_family
>   ON events ((data->>'family_id')) WHERE type = 'device.new';
> ```
> and use `ON CONFLICT DO NOTHING` against it in the device-event insert (a separate INSERT path in `issue_refresh_token`, not `emit_event`, since `emit_event`'s conflict target is the transfer-keyed constraint). Keep this index in the same migration.

### 3.5 Down

```sql
-- +goose Down
-- +goose StatementBegin
-- Restore post_transfer and issue_refresh_token to their pre-events bodies
-- (paste the original bodies from 00008 / 00017 here so down is a true revert).
-- ... CREATE OR REPLACE FUNCTION post_transfer ... (original) ...
-- ... CREATE OR REPLACE FUNCTION issue_refresh_token ... (original) ...
DROP TRIGGER IF EXISTS trg_events_immutable ON events;
DROP FUNCTION IF EXISTS events_block_mutation();
DROP FUNCTION IF EXISTS mark_events_read(UUID, TIMESTAMPTZ, UUID);
DROP FUNCTION IF EXISTS emit_event(UUID, event_type, TEXT, TEXT, UUID, UUID, JSONB);
DROP INDEX IF EXISTS uq_events_device_family;
DROP INDEX IF EXISTS idx_events_user_unread;
DROP INDEX IF EXISTS idx_events_user_created;
DROP TABLE IF EXISTS events;
DROP TYPE IF EXISTS event_type;
-- +goose StatementEnd
```

> The down section **must** restore the original `post_transfer` / `issue_refresh_token` bodies (copy them verbatim from `00008` / `00017`) so `migrate up→down→up` is clean per [`../../CLAUDE.md`](../../CLAUDE.md) "Testing against PostgreSQL 18".

---

## 4. Queries — `db/queries/events.sql` (new file)

Plain sqlc. The mark-read and emit functions are scalar calls; the feed is a keyset `:many`.

```sql
-- name: ListMyEvents :many
-- Caller's feed, newest first, composite (created_at, id) keyset. NULL filter
-- args = no filter. page_limit = limit + 1 for has_more.
SELECT id, user_id, type, title, body, related_transfer_id, related_account_id,
       data, read_at, created_at
FROM events
WHERE user_id = sqlc.arg(user_id)::uuid
  AND (sqlc.narg(cursor)::timestamptz IS NULL
       OR (created_at, id) < (sqlc.narg(cursor)::timestamptz, sqlc.narg(cursor_id)::uuid))
  AND (sqlc.narg(type)::event_type IS NULL OR type = sqlc.narg(type)::event_type)
  AND (NOT sqlc.arg(unread_only)::boolean OR read_at IS NULL)
ORDER BY created_at DESC, id DESC
LIMIT sqlc.arg(page_limit)::int;

-- name: CountMyUnreadEvents :one
SELECT COUNT(*)::int FROM events WHERE user_id = sqlc.arg(user_id)::uuid AND read_at IS NULL;

-- name: MarkEventsRead :one
SELECT mark_events_read(sqlc.arg(user_id)::uuid, sqlc.narg(cursor)::timestamptz, sqlc.narg(cursor_id)::uuid) AS marked;
```

`task generate:sqlc` → `ListMyEventsParams`, `ListMyEventsRow`, `CountMyUnreadEvents`, `MarkEventsRead`. The `data` column is `JSONB` → scan into `[]byte` / `json.RawMessage`; the handler re-marshals it into the `Event.data` object (or define a struct with `Data json.RawMessage`).

---

## 5. Handler logic — `internal/api/handlers_events.go` (new file)

`ListMyEvents` is read-only (no idempotency). Ownership is the `user_id = subject` predicate — there is no per-row 404; a feed only ever contains the caller's own events.

1. **Subject:** `subj, ok := clientSubject(ctx)`; `!ok` → **401** (defensive; behind `requireJWT`).
2. **Cursor:** reuse `decodeCursor` (ledger spec §5.1); malformed → **400**.
3. **`type` filter:** validate against `event_type`; invalid → **400**. Map to `*sqlc.EventType`.
4. **`unread_only`:** `*bool` from the wrapper; default false.
5. **Limit + has_more + envelope:** same mechanics as the ledger/transfers handlers — `eff = limitOr`, query `PageLimit = eff+1`, trim sentinel, `next_cursor = encodeCursor(last.CreatedAt, last.ID)`, **init `items` non-nil**.
6. **`unread_count`:** call `CountMyUnreadEvents(subj)` and put it on the envelope (one extra cheap indexed count; the partial unread index covers it).
7. **`data`:** pass through as `json.RawMessage` so the JSONB payload reaches the client unchanged.
8. **Errors:** `mapDBError`.

`MarkEventsRead` handler (if adopted): decode optional body (`up_to_cursor`); if present, `decodeCursor` → pass `(ts,id)`; else NULL/NULL = mark all. Call `MarkEventsRead`, return `{marked}`. Scope to `subj` (the function takes `p_user_id`). Malformed cursor → **400**.

Sketch:

```go
type eventPage struct {
	Items       []eventDTO `json:"items"`
	NextCursor  *string    `json:"next_cursor"`
	HasMore     bool       `json:"has_more"`
	UnreadCount int32      `json:"unread_count"`
}
type eventDTO struct {
	ID                uuid.UUID       `json:"id"`
	Type              string          `json:"type"`
	Title             string          `json:"title"`
	Body              string          `json:"body"`
	RelatedTransferID *uuid.UUID      `json:"related_transfer_id"`
	RelatedAccountID  *uuid.UUID      `json:"related_account_id"`
	Data              json.RawMessage `json:"data"`
	ReadAt            *time.Time      `json:"read_at"`
	CreatedAt         time.Time       `json:"created_at"`
}

func (s *Server) ListMyEvents(w http.ResponseWriter, r *http.Request, params genclient.ListMyEventsParams) {
	subj, ok := clientSubject(r.Context())
	if !ok { writeError(w, http.StatusUnauthorized, "unauthorized", "client token required"); return }

	q := sqlc.ListMyEventsParams{UserID: subj, UnreadOnly: params.UnreadOnly != nil && *params.UnreadOnly}
	if params.Cursor != nil && *params.Cursor != "" {
		c, err := decodeCursor(*params.Cursor)
		if err != nil { writeError(w, http.StatusBadRequest, "bad_request", "invalid cursor"); return }
		q.Cursor, q.CursorID = &c.ts, &c.id
	}
	if params.Type != nil {
		et := sqlc.EventType(*params.Type)
		if !validEventType(et) { writeError(w, http.StatusBadRequest, "bad_request", "invalid type"); return }
		q.Type = &et
	}
	eff := s.limitOr(params.Limit); q.PageLimit = eff + 1
	rows, err := s.pg.Queries.ListMyEvents(r.Context(), q)
	if err != nil { mapDBError(w, err); return }
	unread, err := s.pg.Queries.CountMyUnreadEvents(r.Context(), subj)
	if err != nil { mapDBError(w, err); return }

	hasMore := int32(len(rows)) > eff
	if hasMore { rows = rows[:eff] }
	items := make([]eventDTO, 0, len(rows))
	for _, row := range rows { items = append(items, toEventDTO(row)) }
	var next *string
	if hasMore && len(items) > 0 {
		last := rows[len(rows)-1]; c := encodeCursor(last.CreatedAt, last.ID); next = &c
	}
	writeJSON(w, http.StatusOK, eventPage{Items: items, NextCursor: next, HasMore: hasMore, UnreadCount: unread})
}
```

Add `validEventType` (switch over the four sqlc `EventType` constants) and `toEventDTO`.

---

## 6. Tests to add

### 6.1 DB integration (`internal/db/integration_test.go`)

- **`TestEmitEventOnPostTransfer`** — alice→bob posted transfer: assert one `transfer.posted` event for alice (debit owner) and one `payment.incoming` for bob (credit owner), each with `related_transfer_id` set and `data.amount_minor` correct. A **deposit** (`EXTERNAL_CLEARING -> customer`) yields a `payment.incoming` for the customer and **no** event for the system account (NULL user).
- **`TestEmitEventIdempotent`** — a replayed idempotent transfer (same `Idempotency-Key`) does **not** create a second `transfer.posted` (the `(user,type,transfer)` unique + `ON CONFLICT DO NOTHING`).
- **`TestEmitDeviceEventOnLogin`** — `issue_refresh_token` creates one `device.new`; a retried issue with the same `family_id` does not duplicate (the partial unique index).
- **`TestEventsAppendOnly`** — `DELETE FROM events` and an `UPDATE events SET type=…` both raise `restrict_violation`; `UPDATE events SET read_at=now()` succeeds.
- **`TestMarkEventsRead`** — `mark_events_read` with a cursor marks only at/older rows; with NULL marks all; returns the right count; `CountMyUnreadEvents` reflects it.
- **`TestEventsKeysetTies`** — many events for one user in one statement (shared `created_at`); page following `(cursor, cursor_id)`; every id seen once (UUIDv7 id tiebreak; mirrors `TestSearchKeysetPaginationTies`).
- **Migration reversibility:** `migrate up→down→up` clean (down restores `post_transfer`/`issue_refresh_token`).

### 6.2 API integration (`internal/api/integration_test.go`)

- **`TestHTTPMyEventsFeed`** — alice (client token) transfers to bob; `GET /me/events` as alice shows a `transfer.posted` item, as bob shows `payment.incoming`; `items` is `[]` (non-null) for a brand-new user; `unread_count` matches; `has_more`/`next_cursor` correct.
- **`TestHTTPMyEventsFilters`** — `?type=payment.incoming` narrows; `?type=bogus` → **400**; `?unread_only=true` returns only unread; `?cursor=not-base64` → **400**.
- **`TestHTTPMyEventsScoping`** — alice's feed never contains bob-only events (e.g. bob's `device.new`).
- **`TestHTTPMarkEventsRead`** (if adopted) — POST `/me/events/read` drops `unread_count` to 0; idempotent on re-POST.

---

## 7. Security considerations

- **Ownership is the predicate.** `GET /me/events` returns only `user_id = subject` rows; `mark_events_read` and `CountMyUnreadEvents` take `p_user_id = subject`. No id is accepted from the client to address another user's events — there is no IDOR surface (no `{id}` path param at all).
- **Append-only / tamper-evident.** The `events_block_mutation` trigger (mirroring the ledger guard) means a notification record can't be silently rewritten; only `read_at` is mutable. A security event (`device.new`) therefore can't be erased to hide an intrusion.
- **No new disclosure.** Event `data` carries only what the caller already sees elsewhere: their own transfer amount, the masked counterparty (use `mask_name` for any name in `body`/`data`, never the raw full name), and their own login `user_agent`/`ip`. Do **not** put the counterparty's balance, full name, or account internals in `data`.
- **Events follow money atomicity, not money authority.** Emission is in the source txn, so an event never advertises a transfer that didn't post. But the feed is a projection — losing an `events` row never corrupts the ledger (which remains the source of truth per rule 2).
- **Opaque cursor, bound.** Same guarantees as the ledger/transfers cursor: forged/malformed cursor cannot widen the `user_id`-scoped set; malformed → **400**.
- **Phase-2 push:** when FCM/APNs lands (§9), the device-token registry must store push tokens hashed/scoped per user and be revoked on `logout-all`; do not log push tokens. (Out of scope for phase 1.)

---

## 8. Acceptance criteria

- [ ] `GET /me/events` returns `{items, next_cursor, has_more, unread_count}`; `items` always an array (`[]`, never `null`).
- [ ] A posted transfer emits exactly one `transfer.posted` (payer) and one `payment.incoming` (payee), in the same txn as the post; a deposit emits `payment.incoming` for the customer and nothing for the system account.
- [ ] A new login emits exactly one `device.new`; idempotent replays of the source transitions never duplicate events.
- [ ] `events` is append-only (DELETE + non-`read_at` UPDATE rejected); only `read_at` is mutable.
- [ ] Feed is scoped to the JWT subject (no other user's events; no IDOR path).
- [ ] Filters `type` (valid enum) and `unread_only` apply server-side; invalid `type`/`cursor` → **400**.
- [ ] Composite `(created_at, id)` keyset: no tie-skip (DB test).
- [ ] `dispute.updated` enum member + feed plumbing exist; emitted from `resolve_dispute` iff `spec-disputes.md` has landed, otherwise wired and documented for when it does.
- [ ] `migrate up→down→up` clean (down restores `post_transfer`/`issue_refresh_token`); DB + API suites pass on PG18; `go build && go vet` clean.

---

## 9. Phase 2 note — push / webhooks (NOT in this spec)

Once the poll feed proves out:

- **Device-token registry:** `POST /me/push-tokens {platform: fcm|apns, token}` + `DELETE`; a `push_tokens` table scoped to `user_id`, revoked on `logout-all`. A maintenance/worker sweep (the existing `worker/`) fans out new `events` rows to registered tokens (poll the `events` table by `created_at > last_pushed`, or `LISTEN/NOTIFY` on insert).
- **Webhooks:** for B2B/BFF consumers, signed `POST` of new events to a per-tenant URL with HMAC + retry/backoff — BFF-side fan-out (docs/08 §3) over the same `events` table, no new core authority.
- **Composition with BFF:** the `GET /me/dashboard` aggregation (docs/09 §3) can include `unread_count` from `CountMyUnreadEvents` for a one-call cold start.

These are additive; the `events` table and `emit_event` write-points designed here are the substrate they build on.

---

## 10. Implementation order

1. Reuse the shipped `internal/api/cursor.go` (composite cursor + reusable params) — already in the baseline.
2. Write `db/migrations/00018_events.sql` (§3, `00018` illustrative) — **take the real next free number** after the 9-file domain baseline (and after any sibling spec migration that has already landed). Include the enum, table, indexes, append-only trigger, `emit_event`, `mark_events_read`, the device partial unique index, the `CREATE OR REPLACE` of `post_transfer` (original in `00005_transfers.sql`, +emit) and `issue_refresh_token` (original in `00003_users.sql`, +emit), and a down that restores those original bodies.
3. `task test:db` first against just the migration: confirm up→down→up and that the existing transfer/refresh tests still pass with the augmented functions.
4. Add `db/queries/events.sql` (§4); `task generate:sqlc`.
5. Edit `api/openapi.yaml` (§2): `/me/events` (+ optional `/me/events/read`), `Event` + `EventPage`. `task generate:oapi` (build breaks — expected).
6. Implement `internal/api/handlers_events.go` (§5) + `validEventType`/`toEventDTO`; `go build ./...`.
7. DB tests (§6.1) then API tests (§6.2); `task test:db`.
8. `go vet`; add a `Notifications` row to [`../06-client-api.md`](../06-client-api.md) §1; flip the docs/08 P2 "Notifications" row to "phase 1 shipped".
