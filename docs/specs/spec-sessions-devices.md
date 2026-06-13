# spec — `GET /me/sessions` + `DELETE /me/sessions/{family_id}` (device list & selective revoke)

> ✅ **IMPLEMENTED (2026-06-14, `improve/add-features`).** Migration
> `00021_session_device_label.sql` (device_label column, 6-arg `issue_refresh_token`,
> `list_user_sessions`, `revoke_refresh_family_scoped`); DB methods in
> `internal/db/auth.go` (`ListUserSessions`, `RevokeUserFamily`; the `current` flag
> reuses `RefreshFamilyByToken`); `FamilyOwnedBy` query for the precise-404 variant;
> handler `internal/api/handlers_sessions.go`; tests `internal/api/sessions_test.go`.
> The precise (404) variant was chosen.
>
> Implementation-ready. Implement as written; no further design decisions.
> Closes the **P2** gap in [`09-fraudbank-bff-plan.md`](../09-fraudbank-bff-plan.md)
> ("Session/device listing + selective revoke — the refresh-token families (`00017`)
> already model this; only listing is missing"). This is the *granular* version of the
> existing `POST /auth/logout-all`. Auth surface: [`06-client-api.md`](../06-client-api.md) §3.

---

## 1. Summary & rationale

"You're signed in on 3 devices." Customers expect to see active sessions and sign one
out without nuking all of them (`logout-all`). The `refresh_tokens` family model from
migration `00017` already *is* a device/session model:

- **one family = one login = one device** (`family_id`, opened by `issue_refresh_token`);
- rotation chains tokens within a family (`parent_id`), sliding `expires_at`;
- `user_agent` / `ip` are already captured per token.

So all the data exists; only two read/write endpoints and one optional column are
missing. **No money moves, no new auth model** — pure surfacing of `00017`.

- `GET /me/sessions` → the caller's **active** families: device label, created, last
  seen, and a `current` flag for the family making the request.
- `DELETE /me/sessions/{family_id}` → revoke one family (selective sign-out). Revoking
  the **current** family logs *this* device out (its next refresh 401s) — that's the
  expected "sign out this device" affordance, and we document it.

A family is **active** if it has at least one live token: `revoked_at IS NULL` and
`expires_at > now()` on its newest (un-rotated) token. We expose the family, not
individual rotated tokens.

---

## 2. API — OpenAPI 3.1 operations

Add under `paths:` (client tag), plus a `FamilyId` path parameter and two schemas.

```yaml
  /me/sessions:
    get:
      operationId: listSessions
      tags: [client]
      summary: List the caller's active sessions (refresh-token families = devices)
      responses:
        "200":
          description: ok
          content:
            application/json:
              schema:
                type: array
                items: { $ref: "#/components/schemas/Session" }
        "401": { $ref: "#/components/responses/Error" }

  /me/sessions/{family_id}:
    delete:
      operationId: revokeSession
      tags: [client]
      summary: Revoke one session/device (selective sign-out). Idempotent.
      parameters: [ { $ref: "#/components/parameters/FamilyId" } ]
      responses:
        "204": { description: revoked (idempotent — also 204 if already gone) }
        "401": { $ref: "#/components/responses/Error" }
        "404": { $ref: "#/components/responses/Error" }   # family not owned by caller
```

The caller learns which family is *theirs* by passing the refresh token they hold via a
header (so the response can flag `current`). Add a parameter and use it on `GET`:

```yaml
  parameters:
    # ... existing Id / IdempotencyKey / Cursor / Limit ...
    FamilyId:
      name: family_id
      in: path
      required: true
      schema: { type: string, format: uuid }
    SessionToken:
      name: X-Refresh-Token
      in: header
      required: false
      schema: { type: string }
      description: >
        The caller's current refresh token. Optional; when present, the matching
        family is flagged `current:true` in GET /me/sessions. Never logged.
```

Attach `SessionToken` to the `GET` op parameters:

```yaml
  /me/sessions:
    get:
      operationId: listSessions
      tags: [client]
      summary: List the caller's active sessions (refresh-token families = devices)
      parameters: [ { $ref: "#/components/parameters/SessionToken" } ]
      responses: { ... as above ... }
```

> Note on `current`: the BFF (web, [`08`](../09-fraudbank-bff-plan.md) §1.1) holds the
> refresh token in an HttpOnly cookie and can inject `X-Refresh-Token` on the proxied
> call; native apps hold it in Keychain/Keystore and pass it directly. If absent,
> `current` is `false` for all rows — harmless.

```yaml
    Session:
      type: object
      properties:
        family_id: { type: string, format: uuid }
        device_label: { type: string, description: "human label (see §3.1); falls back to a UA summary" }
        user_agent: { type: string }
        ip: { type: string }
        created_at: { type: string, format: date-time, description: "when the family/login was opened" }
        last_seen_at: { type: string, format: date-time, description: "issued_at of the newest token in the family (last rotate)" }
        current: { type: boolean, description: "true when this is the family of the presented X-Refresh-Token" }
```

---

## 3. Data model & migration

The families already exist (`00017`). One **optional** ergonomic column: a
client-supplied `device_label` so the UI can show "Pixel 8 / Chrome on macOS" instead
of a raw UA string. It is set once per family at login. Next free migration is
**`00018`** but `00018` is reserved by `spec-step-up-mfa.md` (`00018_mfa.sql`) and
`spec-change-password.md` eyes it too — **this file takes the next number after both**
(`00019` or `00020`); pick the next free at implement time. The DDL is independent.

`db/migrations/00019_session_device_label.sql` (renumber as needed):

```sql
-- +goose Up
-- +goose StatementBegin

-- Optional per-family device label, set at login from a client hint. Lives on the
-- family's FIRST token (the one issue_refresh_token inserted). Nullable; the API
-- falls back to a user_agent summary when absent.
ALTER TABLE refresh_tokens ADD COLUMN device_label TEXT;

-- issue_refresh_token gains an optional device label. Keep the OLD 5-arg signature
-- working (it's called by the existing Login/IssueRefreshToken) by ADDING a 6-arg
-- overload rather than breaking the contract; the Go layer migrates to the 6-arg form.
CREATE OR REPLACE FUNCTION issue_refresh_token(
    p_user_id      UUID,
    p_token_hash   TEXT,
    p_idle_seconds INT,
    p_user_agent   TEXT DEFAULT NULL,
    p_ip           TEXT DEFAULT NULL,
    p_device_label TEXT DEFAULT NULL
) RETURNS UUID AS $$
DECLARE v_family UUID;
BEGIN
    INSERT INTO refresh_tokens (id, user_id, expires_at, user_agent, ip, device_label)
    VALUES (p_token_hash, p_user_id, now() + make_interval(secs => p_idle_seconds),
            p_user_agent, p_ip, p_device_label)
    RETURNING family_id INTO v_family;
    RETURN v_family;
END;
$$ LANGUAGE plpgsql;

-- list_user_sessions: one row per ACTIVE family for a user. A family is active if its
-- newest (un-rotated, un-revoked, un-expired) token is live. device_label/user_agent/ip
-- come from the family's FIRST token (the login); last_seen is the newest token's
-- issued_at (the last rotate).
CREATE OR REPLACE FUNCTION list_user_sessions(p_user_id UUID)
RETURNS TABLE (
    family_id    UUID,
    device_label TEXT,
    user_agent   TEXT,
    ip           TEXT,
    created_at   TIMESTAMPTZ,
    last_seen_at TIMESTAMPTZ
) AS $$
    WITH live AS (
        SELECT rt.family_id
          FROM refresh_tokens rt
         WHERE rt.user_id = p_user_id
           AND rt.revoked_at IS NULL
           AND rt.rotated_at IS NULL          -- the current tip of the chain
           AND rt.expires_at > now()
    )
    SELECT
        f.family_id,
        first.device_label,
        first.user_agent,
        first.ip,
        first.issued_at                                   AS created_at,
        (SELECT max(rt2.issued_at) FROM refresh_tokens rt2
          WHERE rt2.family_id = f.family_id)              AS last_seen_at
    FROM live f
    JOIN LATERAL (
        SELECT rt3.device_label, rt3.user_agent, rt3.ip, rt3.issued_at
          FROM refresh_tokens rt3
         WHERE rt3.family_id = f.family_id
         ORDER BY rt3.issued_at ASC
         LIMIT 1
    ) first ON TRUE
    ORDER BY last_seen_at DESC;
$$ LANGUAGE sql STABLE;

-- revoke_refresh_family_scoped: revoke a family BY family_id, but only if it belongs
-- to p_user_id (ownership scoping). Returns the number of live tokens revoked; 0 means
-- the family doesn't exist or isn't the caller's (API maps 0 -> 404). Idempotent.
CREATE OR REPLACE FUNCTION revoke_refresh_family_scoped(p_user_id UUID, p_family_id UUID)
RETURNS INTEGER AS $$
DECLARE v_n INTEGER;
BEGIN
    -- ownership gate: the family must have at least one token owned by the caller.
    IF NOT EXISTS (SELECT 1 FROM refresh_tokens
                    WHERE family_id = p_family_id AND user_id = p_user_id) THEN
        RETURN 0;
    END IF;
    UPDATE refresh_tokens
       SET revoked_at = now(), revoked_reason = 'logout'
     WHERE family_id = p_family_id AND user_id = p_user_id AND revoked_at IS NULL;
    GET DIAGNOSTICS v_n = ROW_COUNT;
    RETURN v_n;
END;
$$ LANGUAGE plpgsql;

-- family_for_token: the family_id of a (live) refresh-token hash, for the `current`
-- flag. ok via NOT FOUND in the Go layer (returns NULL otherwise).
CREATE OR REPLACE FUNCTION family_for_token(p_token_hash TEXT)
RETURNS UUID LANGUAGE sql STABLE AS $$
    SELECT family_id FROM refresh_tokens WHERE id = p_token_hash;
$$;

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DROP FUNCTION IF EXISTS family_for_token(TEXT);
DROP FUNCTION IF EXISTS revoke_refresh_family_scoped(UUID, UUID);
DROP FUNCTION IF EXISTS list_user_sessions(UUID);
-- restore the original 5-arg-only issue_refresh_token from 00017
CREATE OR REPLACE FUNCTION issue_refresh_token(
    p_user_id UUID, p_token_hash TEXT, p_idle_seconds INT,
    p_user_agent TEXT DEFAULT NULL, p_ip TEXT DEFAULT NULL
) RETURNS UUID AS $$
DECLARE v_family UUID;
BEGIN
    INSERT INTO refresh_tokens (id, user_id, expires_at, user_agent, ip)
    VALUES (p_token_hash, p_user_id, now() + make_interval(secs => p_idle_seconds), p_user_agent, p_ip)
    RETURNING family_id INTO v_family;
    RETURN v_family;
END;
$$ LANGUAGE plpgsql;
ALTER TABLE refresh_tokens DROP COLUMN IF EXISTS device_label;
-- +goose StatementEnd
```

> **Function-overload caution**: Postgres distinguishes `issue_refresh_token/5` and
> `/6` as separate functions. The 6-arg version with all-defaulted trailing args makes
> a 5-positional-arg call ambiguous. **Avoid ambiguity**: have the Go layer *always*
> call the 6-arg form (passing the label or `NULL`) — see §3.1 — and drop the old 5-arg
> body in the Up migration. (The Down recreates the 5-arg original.) If you prefer to
> keep both, the Go layer must always pass 6 positional args so the call resolves
> unambiguously to `/6`. Either way: **Go always sends 6 args after this migration.**

Conventions honored: append-only/immutability untouched (we only set `revoked_at`,
already allowed by `00017`); `revoked_reason='logout'` is in the existing vocabulary;
ownership scoping done in SQL *and* re-checked in Go (defense in depth).

### 3.1 sqlc / db wiring

`list_user_sessions` RETURNS TABLE → **sqlc cannot expand it** (same limitation noted
for `transfer()`/`resolve_account_by_iban()` in `db/queries/beneficiaries.sql`).
Hand-write in `internal/db/auth.go` (alongside the other refresh helpers), matching the
`Reconcile` row-scan pattern in `bank.go`:

```go
// Session is one active refresh-token family (a device/login).
type Session struct {
	FamilyID    uuid.UUID  `json:"family_id"`
	DeviceLabel *string    `json:"device_label,omitempty"`
	UserAgent   *string    `json:"user_agent,omitempty"`
	IP          *string    `json:"ip,omitempty"`
	CreatedAt   time.Time  `json:"created_at"`
	LastSeenAt  time.Time  `json:"last_seen_at"`
	Current     bool       `json:"current"`
}

// ListUserSessions returns the caller's active sessions, newest activity first.
func (p *Postgres) ListUserSessions(ctx context.Context, userID uuid.UUID) ([]Session, error) {
	rows, err := p.Pool.Query(ctx,
		`SELECT family_id, device_label, user_agent, ip, created_at, last_seen_at
		   FROM list_user_sessions($1::uuid)`, userID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Session{} // non-nil so JSON marshals as [] not null (the 00079/item-7 rule)
	for rows.Next() {
		var s Session
		if err := rows.Scan(&s.FamilyID, &s.DeviceLabel, &s.UserAgent, &s.IP, &s.CreatedAt, &s.LastSeenAt); err != nil {
			return nil, err
		}
		out = append(out, s)
	}
	return out, rows.Err()
}

// RevokeUserFamily revokes one family if owned by userID. Returns count revoked;
// 0 => not found / not owned (API -> 404).
func (p *Postgres) RevokeUserFamily(ctx context.Context, userID, familyID uuid.UUID) (int, error) {
	var n int
	err := p.Pool.QueryRow(ctx,
		`SELECT revoke_refresh_family_scoped($1::uuid, $2::uuid)`, userID, familyID).Scan(&n)
	return n, err
}

// FamilyForToken returns the family of a refresh-token hash (for the `current` flag).
func (p *Postgres) FamilyForToken(ctx context.Context, tokenHash string) (uuid.UUID, bool, error) {
	var fam uuid.UUID
	err := p.Pool.QueryRow(ctx, `SELECT family_for_token($1::text)`, tokenHash).Scan(&fam)
	if errors.Is(err, pgx.ErrNoRows) {
		return uuid.Nil, false, nil
	}
	return fam, err == nil, err
}
```

**Device label at login**: update the existing `IssueRefreshToken` in
`internal/db/auth.go` to call the 6-arg function and add a `deviceLabel string` arg:

```go
func (p *Postgres) IssueRefreshToken(ctx context.Context, userID uuid.UUID, tokenHash string, idleSeconds int, userAgent, ip, deviceLabel string) (uuid.UUID, error) {
	var family uuid.UUID
	err := p.Pool.QueryRow(ctx,
		`SELECT issue_refresh_token($1::uuid, $2::text, $3::int, $4::text, $5::text, $6::text)`,
		userID, tokenHash, idleSeconds, userAgent, ip, nullIfEmpty(deviceLabel),
	).Scan(&family)
	return family, err
}
```

`nullIfEmpty` is a small helper to add to `internal/db/auth.go` (it does not yet exist
in the package) so an empty label is stored as SQL `NULL`, not `''`:

```go
// nullIfEmpty returns nil for an empty string so pgx binds SQL NULL (not '').
func nullIfEmpty(s string) any {
	if s == "" {
		return nil
	}
	return s
}
```

Update its two callers (`Login` in `handlers_users.go`, `MfaVerify` in the MFA spec)
to pass a device label — derive it from an optional `device_label` field in the login
request body, or `""`. (Adding `device_label` to `LoginRequest` is a tiny optional
follow-up; `""` is fine for v1.)

---

## 4. Handler logic

New file `internal/api/handlers_sessions.go`:

```go
package api

import (
	"net/http"

	openapi_types "github.com/oapi-codegen/runtime/types"
	"github.com/google/uuid"
	"github.com/minhtt159/bank0/internal/api/genclient"
)

// ListSessions implements genclient.ServerInterface. Client surface only. Lists the
// caller's active refresh-token families (devices), flagging the current one when the
// X-Refresh-Token header is present.
func (s *Server) ListSessions(w http.ResponseWriter, r *http.Request, params genclient.ListSessionsParams) {
	subj, ok := clientSubject(r.Context())
	if !ok {
		writeError(w, http.StatusUnauthorized, "unauthorized", "authentication required")
		return
	}
	sessions, err := s.pg.ListUserSessions(r.Context(), subj)
	if err != nil {
		mapDBError(w, err)
		return
	}
	// Flag the current family if the caller sent their refresh token.
	if params.XRefreshToken != nil && *params.XRefreshToken != "" {
		if fam, found, ferr := s.pg.FamilyForToken(r.Context(), hashToken(*params.XRefreshToken)); ferr == nil && found {
			for i := range sessions {
				if sessions[i].FamilyID == fam {
					sessions[i].Current = true
				}
			}
		}
	}
	writeJSON(w, http.StatusOK, sessions)
}

// RevokeSession implements genclient.ServerInterface. Selective sign-out: revoke one
// family if owned by the caller. Idempotent (204 even if already gone); 404 if the
// family isn't the caller's. Revoking the current family logs THIS device out.
func (s *Server) RevokeSession(w http.ResponseWriter, r *http.Request, familyID openapi_types.UUID) {
	subj, ok := clientSubject(r.Context())
	if !ok {
		writeError(w, http.StatusUnauthorized, "unauthorized", "authentication required")
		return
	}
	n, err := s.pg.RevokeUserFamily(r.Context(), subj, uuid.UUID(familyID))
	if err != nil {
		mapDBError(w, err)
		return
	}
	// n==0 with an existing-but-already-revoked family is also 0; distinguish "not
	// owned/never existed" -> 404 from "owned but already revoked" -> 204 by re-checking
	// ownership only when n==0.
	if n == 0 {
		// revoke_refresh_family_scoped returns 0 both for not-owned AND already-revoked.
		// To keep DELETE idempotent for the owner, treat not-owned as 404 via a cheap
		// ownership probe. (See §4.1 for the simpler all-204 alternative.)
		if owned, _ := s.pg.FamilyOwnedBy(r.Context(), subj, uuid.UUID(familyID)); !owned {
			writeError(w, http.StatusNotFound, "not_found", "session not found")
			return
		}
	}
	w.WriteHeader(http.StatusNoContent)
}
```

### 4.1 Ownership-vs-idempotency decision (pick one; default = the probe above)

`revoke_refresh_family_scoped` returns `0` for **both** "not the caller's family" and
"the caller's family but already revoked". To honor *both* ownership (404 for someone
else's / nonexistent family — never confirm its existence) **and** idempotency (204 on
a repeat revoke of your own), the handler does a cheap ownership probe on `n==0`:

`FamilyOwnedBy` — add a tiny query (sqlc-able) to `db/queries/ownership.sql`:

```sql
-- name: FamilyOwnedBy :one
SELECT EXISTS (
    SELECT 1 FROM refresh_tokens
     WHERE family_id = sqlc.arg(family)::uuid AND user_id = sqlc.arg(owner)::uuid
) AS owned;
```

> **Simpler alternative** (acceptable, less precise): always return **204** from
> DELETE regardless of `n` (pure best-effort/idempotent, matching `revoke_refresh_token`
> / `Logout`). This leaks nothing (revoking a non-owned family is a no-op) and drops the
> probe + the `FamilyOwnedBy` query. The spec defaults to the 404-precise version
> because the OpenAPI op documents a 404; if you take the all-204 route, remove the
> `404` from the op's `responses`.

### Error mapping
| Cause | HTTP |
|-------|------|
| No bearer | 401 `unauthorized` |
| Family not owned / nonexistent (precise variant) | 404 `not_found` |
| Bad UUID in path | 400 (generated wrapper / `mapDBError`) |
| Success (list) | 200 with `[]`-or-array |
| Success (revoke) | 204 |

### Edge cases
- **Empty list** (no active sessions — shouldn't happen for an authenticated caller, but
  e.g. right after logout-all + a still-valid access token): returns `[]`, never `null`
  (the slice is initialized non-nil — the same fix called out for the ledger in
  [`08`](../09-fraudbank-bff-plan.md) P0). The 15m access token can outlive its refresh.
- **Revoking the current family**: allowed; the response is 204 and this device's *next*
  `/auth/refresh` 401s (family revoked). This is the intended "sign out this device"
  behavior — document in client guides; the UI should treat a 204 on the `current` row
  as "you've signed yourself out, return to login on next refresh."
- **Cross-user**: `revoke_refresh_family_scoped` and `FamilyOwnedBy` are both
  `user_id`-scoped, and the handler re-derives the subject from the JWT — alice cannot
  list or revoke bob's sessions (consistent with the IDOR scoping in
  [`06`](../06-client-api.md) §4).
- **Rotated tokens within a family** are not separate rows in the list — `list_user_sessions`
  collapses to one row per active family.

### Routing
Both endpoints need the subject → JWT-guarded subrouter, wired by
`genclient.HandlerFromMux(s, cr)` in `server.go` after regeneration. No parent-router
registration.

---

## 5. Tests to add

### DB integration (`internal/db/auth_test.go`, appended)
- `TestListUserSessions`: open three families (three `IssueRefreshToken` calls, distinct
  UAs/labels); `ListUserSessions` returns 3 rows, ordered by `last_seen_at` desc, with
  the right labels. Rotate one family once (`RotateRefreshToken`) and assert it's still
  **one** row and its `last_seen_at` advanced.
- `TestListUserSessionsExcludesRevoked`: revoke one family; list returns 2.
- `TestRevokeUserFamilyScoped`: `RevokeUserFamily(user, fam)` returns ≥1; the family's
  tip token then fails `RotateRefreshToken` (401/28000-or-28P01). Revoking again returns 0.
- `TestRevokeUserFamilyCrossUser`: user B's family id passed with user A's id → returns 0,
  B's family still live.
- `TestFamilyForToken`: returns the family of a live token; unknown hash → ok=false.

### API integration (`internal/api/sessions_test.go`, new)
Reuse `clientLogin` / `doRefresh` / `get`:
- `TestHTTPListSessions`: login twice (two devices). `GET /me/sessions` with device-A's
  bearer + `X-Refresh-Token: A.refresh` → 200, 2 rows, the A family `current:true`,
  the B family `current:false`.
- `TestHTTPRevokeSelectiveSession`:
  1. login A, login B. From A, `GET /me/sessions`, find B's `family_id`.
  2. `DELETE /me/sessions/{B.family}` with A's bearer → 204.
  3. `doRefresh(B.refresh)` → 401; `doRefresh(A.refresh)` → 200 (A unaffected).
- `TestHTTPRevokeCurrentLogsOut`: from A, delete A's own family → 204; `doRefresh(A.refresh)` → 401.
- `TestHTTPRevokeOthersFamily404`: alice tries `DELETE /me/sessions/{bob.family}` → 404,
  bob still refreshable. (Or 204+no-op if the all-204 variant was chosen — assert bob still live.)
- `TestHTTPRevokeIdempotent`: delete the same owned family twice → 204 both times (precise variant).
- `TestHTTPListSessionsNoBearer`: no `Authorization` → 401.
- Assert the empty case marshals as `[]`: a fresh user with only an access token (force
  via a crafted scenario, or just assert the JSON body parses as an array, never `null`).

---

## 6. Security considerations

- **Ownership scoping (IDOR)**: every query is `user_id`-scoped *and* the subject is
  re-derived from the verified JWT; a non-owned `family_id` is 404 (never confirm it
  exists). Mirrors [`06`](../06-client-api.md) §4.
- **`X-Refresh-Token` is a credential**: only used to compute the `current` flag; never
  logged, never persisted, never echoed. It's optional — absence just means no `current`
  flag. (The opaque token is matched by sha256 hash, same as everywhere.)
- **No token material in the response**: `Session` exposes `family_id`, label, UA, IP,
  timestamps — never any token or hash. `family_id` is an opaque UUIDv7, safe to show.
- **IP/UA are operator-grade hints**, captured at login; surfacing them to the owner is
  fine (it's their own data) and aids fraud spotting ("a session from an IP I don't
  recognize"). They are nullable.
- **Revoke is append-only-safe**: only sets `revoked_at`/`revoked_reason` (allowed by
  `00017`'s model); no ledger interaction, no money.
- **Self-revoke**: revoking the current family is permitted and logs the device out at
  next refresh — a feature, not a footgun, but document it.

---

## 7. Acceptance criteria

- [ ] `api/openapi.yaml`: `GET /me/sessions` + `DELETE /me/sessions/{family_id}` under
      tag `client`; `FamilyId` + `SessionToken` parameters; `Session` schema.
      `task generate:oapi` clean.
- [ ] Migration adds `refresh_tokens.device_label`, `list_user_sessions`,
      `revoke_refresh_family_scoped`, `family_for_token`, and the 6-arg
      `issue_refresh_token`; `goose up`/`down` succeed; the Go layer always calls the
      6-arg form.
- [ ] `internal/db/auth.go`: `Session` type + `ListUserSessions`, `RevokeUserFamily`,
      `FamilyForToken`; `IssueRefreshToken` gains `deviceLabel`; callers updated.
- [ ] (precise variant) `FamilyOwnedBy` query added; `task generate:sqlc` clean.
- [ ] `internal/api/handlers_sessions.go` implements `ListSessions` + `RevokeSession`;
      package compiles (`genclient.ServerInterface` satisfied).
- [ ] `GET /me/sessions` lists active families newest-first, one row per family, with a
      correct `current` flag; body is `[]` (never `null`) when empty.
- [ ] `DELETE /me/sessions/{family_id}` revokes only the caller's family (404 for
      others, precise variant), is idempotent, and revoking the current family logs the
      device out at next refresh.
- [ ] Cross-user isolation verified (alice can't see/revoke bob).
- [ ] DB + API tests pass under `task test:db`.

---

## 8. Implementation order

1. `api/openapi.yaml`: add both operations, `FamilyId`/`SessionToken` params, `Session`
   schema. `task generate:oapi` (build breaks — handlers missing).
2. Migration `00019` (or next free): add `device_label`, the 6-arg `issue_refresh_token`,
   `list_user_sessions`, `revoke_refresh_family_scoped`, `family_for_token`. `goose up`/`down`.
3. `internal/db/auth.go`: `Session` + `ListUserSessions`/`RevokeUserFamily`/`FamilyForToken`;
   widen `IssueRefreshToken` to 6 args + `nullIfEmpty` helper; update callers (`Login`,
   and `MfaVerify` if the MFA spec landed).
4. (precise variant) add `FamilyOwnedBy` to `db/queries/ownership.sql`; `task generate:sqlc`.
5. `internal/api/handlers_sessions.go`; build passes.
6. DB tests (`internal/db/auth_test.go`) then API tests (`internal/api/sessions_test.go`).
7. `task test:db`; fix; done. Update fraudbank `docs/02-api-contract.md` with the
   sessions list + selective-revoke and the "X-Refresh-Token marks current" convention.

