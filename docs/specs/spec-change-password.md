# spec — `POST /me/password` (logged-in password change)

> ✅ **IMPLEMENTED (2026-06-13, `feat/bff`).** Migration `00018_change_password.sql`
> (`change_password` + `revoke_user_refresh_except_family`), DB methods in
> `internal/db/auth.go`, handler `internal/api/handlers_password.go`, tests
> `password_test.go`. As-built: [`../06-client-api.md`](../06-client-api.md) §1.
> Rate-limiting (the `429`) is still the open gap — see
> [`../10-security-review.md`](../10-security-review.md). Retained for rationale.

> Implementation-ready. Implement as written; no further design decisions.
> Closes the **P0** gap in [`09-fraudbank-bff-plan.md`](../09-fraudbank-bff-plan.md)
> ("Password change (logged-in customer)"). Companion auth surface:
> [`06-client-api.md`](../06-client-api.md) §2–3.

---

## 1. Summary & rationale

A banking client without self-service "change password" is not shippable. Today
only staff reset a password via the portal (`update_user_info`); the customer app
(fraudbank web/iOS/Android) has no path. This adds **one client endpoint**:

`POST /me/password` `{current_password, new_password}` → **204**.

It does two things atomically-enough for our threat model:

1. **Verify the current password** (bcrypt via pgcrypto), then **re-hash and store**
   the new one — reusing the existing `crypt()/gen_salt('bf',10)` discipline from
   `create_user` / `update_user_info` (migration `00006`). Wrong current password →
   **401**; weak new password → **422**.
2. **Revoke every refresh-token family for the caller EXCEPT the current session.**
   A password change is a credential rotation: any *other* device (and any attacker
   who phished the old password and is sitting on a refresh token) must be kicked.
   The device performing the change keeps working — no self-logout surprise. This
   reuses the `refresh_tokens` family model from migration `00017`
   ([`06-client-api.md`](../06-client-api.md) §3): the caller passes the refresh
   token of the session they're changing from; its `family_id` is the one we spare.

The access JWT is stateless and short (15m, `auth.jwt_ttl`), so we do **not** try to
invalidate live access tokens — the refresh-family revocation is what makes other
sessions die at their next rotate (≤15m later). This is the same trade-off the rest
of the auth surface already makes; call it out, don't fight it.

---

## 2. API — OpenAPI 3.1 operation

Add under `paths:` in `api/openapi.yaml` (client tag), and the request schema under
`components/schemas`. Style matches the existing file (compact flow-style refs).

```yaml
  /me/password:
    post:
      operationId: changePassword
      tags: [client]
      summary: Change the authenticated customer's password (revokes other sessions)
      requestBody:
        required: true
        content:
          application/json:
            schema: { $ref: "#/components/schemas/ChangePasswordRequest" }
      responses:
        "204": { description: password changed; other sessions revoked }
        "401": { $ref: "#/components/responses/Error" }   # wrong current password / no bearer
        "422": { $ref: "#/components/responses/Error" }   # new password fails policy
        "429": { $ref: "#/components/responses/Error" }   # too many attempts (see §6)
```

```yaml
    ChangePasswordRequest:
      type: object
      required: [current_password, new_password]
      properties:
        current_password: { type: string }
        new_password: { type: string, minLength: 12, maxLength: 256 }
        refresh_token:
          type: string
          description: >
            The opaque refresh token of the session performing the change. Its
            family is the one spared from revocation. Optional: if omitted (or
            unknown), ALL families are revoked and the caller must re-login.
```

Notes:
- No `Idempotency-Key` parameter: no money moves. A double-submit just re-verifies
  the (now-new) password and 401s the second time — harmless and self-correcting.
- `429` is documented so clients render it; enforcement is best-effort (§6).

---

## 3. Data model & migration

**No new tables.** Reuse `users.password_hash` (migration `00003`) and the
`refresh_tokens` family model (`00017`). We add **one PL/pgSQL function** that does
verify-and-rotate atomically, plus **one family-sparing revoke** helper. Next free
migration number is **`00018`** — but note spec B (`spec-step-up-mfa.md`) also claims
`00018`. **Coordinate**: if B lands first, this file becomes `00019`. The DDL is
order-independent; pick the next free number at implement time.

`db/migrations/00018_change_password.sql` (renumber if taken):

```sql
-- +goose Up
-- +goose StatementBegin

-- change_password: verify the current password (bcrypt) and store the new hash,
-- in one statement, so a concurrent reset can't interleave. Raises typed errors
-- the API maps to HTTP:
--   * wrong current password           -> 28P01 (mapped 401)
--   * new password fails server policy  -> check_violation / 23514 (mapped 422)
-- Password POLICY lives here (DB-first, like every other rule): >= 12 chars,
-- not equal to the current password. (Length/charset shape is also pre-checked in
-- Go for a friendlier message; the DB check is the authority.)
CREATE OR REPLACE FUNCTION change_password(
    p_user_id      UUID,
    p_current      TEXT,
    p_new          TEXT
) RETURNS VOID AS $$
DECLARE
    v_hash TEXT;
BEGIN
    SELECT password_hash INTO v_hash
      FROM users
     WHERE id = p_user_id AND status = 'active'
     FOR UPDATE;
    IF NOT FOUND THEN
        -- unknown / non-active user: same code as a bad password (no enumeration)
        RAISE EXCEPTION 'invalid current password' USING ERRCODE = '28P01';
    END IF;

    IF v_hash <> crypt(p_current, v_hash) THEN
        RAISE EXCEPTION 'invalid current password' USING ERRCODE = '28P01';
    END IF;

    -- policy (authority): length + must differ from current.
    IF length(p_new) < 12 THEN
        RAISE EXCEPTION 'new password too short (min 12 chars)' USING ERRCODE = 'check_violation';
    END IF;
    IF crypt(p_new, v_hash) = v_hash THEN
        RAISE EXCEPTION 'new password must differ from the current password' USING ERRCODE = 'check_violation';
    END IF;

    UPDATE users
       SET password_hash = crypt(p_new, gen_salt('bf', 10))
     WHERE id = p_user_id;
END;
$$ LANGUAGE plpgsql;

-- revoke_user_refresh_except_family: "log out everywhere else". Revokes every live
-- refresh family for the user EXCEPT the family the current session belongs to.
-- p_keep_family may be NULL (revoke all). Returns the count revoked. Reason
-- 'password_change' joins the existing revoked_reason vocabulary (00017).
CREATE OR REPLACE FUNCTION revoke_user_refresh_except_family(
    p_user_id     UUID,
    p_keep_family UUID DEFAULT NULL
) RETURNS INTEGER AS $$
DECLARE v_n INTEGER;
BEGIN
    UPDATE refresh_tokens
       SET revoked_at = now(), revoked_reason = 'password_change'
     WHERE user_id = p_user_id
       AND revoked_at IS NULL
       AND (p_keep_family IS NULL OR family_id <> p_keep_family);
    GET DIAGNOSTICS v_n = ROW_COUNT;
    RETURN v_n;
END;
$$ LANGUAGE plpgsql;

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DROP FUNCTION IF EXISTS revoke_user_refresh_except_family(UUID, UUID);
DROP FUNCTION IF EXISTS change_password(UUID, TEXT, TEXT);
-- +goose StatementEnd
```

Conventions honored:
- bcrypt via `crypt()` / `gen_salt('bf',10)` — identical to `00006`.
- Typed SQLSTATEs (`28P01`, `check_violation`) consumed by `mapDBError`.
- `password_change` extends the `revoked_reason` set (`'logout'|'reuse_detected'|
  'forced'|'expired'`) documented in `00017`.
- Append-only/idempotency untouched (no ledger, no money).

### 3.1 sqlc wiring

`change_password` and `revoke_user_refresh_except_family` both return scalar/void
and take known args, so they **could** be sqlc'd — but the refresh-token helpers in
`00017` are all hand-written `p.Pool.QueryRow`/`Exec` in `internal/db/auth.go`
(see `RevokeUserRefresh`). **Match that**: add hand-written methods, do not add to
`db/queries/`.

In `internal/db/auth.go`, append:

```go
// ChangePassword verifies the current password and stores the new hash. On a wrong
// current password it raises 28P01 (mapped 401); on a policy failure 23514 (mapped 422).
func (p *Postgres) ChangePassword(ctx context.Context, userID uuid.UUID, current, next string) error {
	_, err := p.Pool.Exec(ctx, `SELECT change_password($1::uuid, $2::text, $3::text)`, userID, current, next)
	return err
}

// RevokeUserRefreshExceptFamily revokes every live refresh family for the user
// except keepFamily (pass uuid.Nil to revoke all). Returns the count revoked.
func (p *Postgres) RevokeUserRefreshExceptFamily(ctx context.Context, userID, keepFamily uuid.UUID) (int, error) {
	var keep any
	if keepFamily != uuid.Nil {
		keep = keepFamily
	}
	var n int
	err := p.Pool.QueryRow(ctx,
		`SELECT revoke_user_refresh_except_family($1::uuid, $2::uuid)`, userID, keep).Scan(&n)
	return n, err
}
```

We also need to resolve the presented refresh token to its `family_id` (so we know
which family to spare). Add a tiny lookup — best-effort, returns ok=false if the
token is unknown/revoked (then we revoke everything):

```go
// RefreshFamilyByToken returns the family_id of a (still-live) refresh token hash.
// ok=false when the token is unknown or already revoked.
func (p *Postgres) RefreshFamilyByToken(ctx context.Context, tokenHash string) (uuid.UUID, bool, error) {
	var fam uuid.UUID
	err := p.Pool.QueryRow(ctx,
		`SELECT family_id FROM refresh_tokens WHERE id = $1 AND revoked_at IS NULL`, tokenHash).Scan(&fam)
	if errors.Is(err, pgx.ErrNoRows) {
		return uuid.Nil, false, nil
	}
	return fam, err == nil, err
}
```

(`internal/db/auth.go` already imports `pgx` and `errors`.)

---

## 4. Handler logic

New file `internal/api/handlers_password.go` (the handler satisfies the generated
`genclient.ServerInterface.ChangePassword` once the spec is regenerated):

```go
package api

import "net/http"

type changePasswordReq struct {
	CurrentPassword string `json:"current_password"`
	NewPassword     string `json:"new_password"`
	RefreshToken    string `json:"refresh_token"`
}

// ChangePassword implements genclient.ServerInterface. Client surface only (behind
// requireJWT). Verifies the current password, stores the new one, and revokes every
// OTHER refresh-token family for the caller (the session performing the change is
// spared via its refresh_token's family_id). 204 on success.
func (s *Server) ChangePassword(w http.ResponseWriter, r *http.Request) {
	subj, ok := clientSubject(r.Context())
	if !ok {
		writeError(w, http.StatusUnauthorized, "unauthorized", "authentication required")
		return
	}
	var req changePasswordReq
	if !decodeJSON(w, r, &req) {
		return
	}
	// Cheap client-side policy pre-check for a friendly message; the DB is the authority.
	if len(req.NewPassword) < 12 {
		writeError(w, http.StatusUnprocessableEntity, "weak_password", "new password must be at least 12 characters")
		return
	}
	if err := s.pg.ChangePassword(r.Context(), subj, req.CurrentPassword, req.NewPassword); err != nil {
		mapDBError(w, err) // 28P01 -> 401, 23514 -> 422
		return
	}
	// Spare the current session's family; revoke the rest. Best-effort: if the token
	// is missing/unknown we revoke everything (safer default).
	keep := uuid.Nil
	if req.RefreshToken != "" {
		if fam, found, ferr := s.pg.RefreshFamilyByToken(r.Context(), hashToken(req.RefreshToken)); ferr == nil && found {
			keep = fam
		}
	}
	if _, err := s.pg.RevokeUserRefreshExceptFamily(r.Context(), subj, keep); err != nil {
		// The password is already changed; log and still 204 (don't 500 a succeeded change).
		s.log.Error("revoke other families after password change", "err", err)
	}
	w.WriteHeader(http.StatusNoContent)
}
```

(Add the `"github.com/google/uuid"` import.)

### Error mapping (via existing `mapDBError`, `internal/api/respond.go`)

| Cause | DB SQLSTATE | HTTP |
|-------|-------------|------|
| Missing/invalid bearer | — (handler) | 401 `unauthorized` |
| Wrong current password / non-active user | `28P01` | 401 `unauthorized` |
| New password < 12 chars (Go pre-check) | — | 422 `weak_password` |
| New password fails DB policy (len / == current) | `23514` check_violation | 422 (mapped `unprocessable`) |
| Invalid JSON | — | 400 `bad_request` |

`28P01` already maps to **401** in `mapDBError`. `23514` already maps to **422**
(message contains neither "insufficient" nor "idempotency key", so the generic
`unprocessable` code is returned — acceptable; clients key off the 422 status).

### Edge cases
- **`current == new`** → DB raises check_violation → 422. (Pre-check can't catch this
  without hashing; let the DB own it.)
- **Refresh token belongs to a *different* user** than the bearer: `RefreshFamilyByToken`
  returns the family regardless of user, but `RevokeUserRefreshExceptFamily` is scoped
  to `user_id = subj`, so an attacker can't spare another user's family or revoke it.
  Worst case they fail to spare any of *their own* families → they get logged out too.
  Acceptable (no cross-user effect).
- **No `refresh_token` supplied** → `keep = uuid.Nil` → all families revoked → the
  caller's own current access token still works until expiry, but their refresh dies;
  they re-login at next rotate. Document this in client guides as "changing password
  without supplying your session token logs you out everywhere."
- **Concurrent change**: `FOR UPDATE` on the `users` row serializes; the second caller
  re-verifies against the already-updated hash and 401s if it used the old password.

### Routing
`/me/password` is `POST` on the client surface and **needs the subject**, so it lives
on the JWT-guarded subrouter — it is wired automatically by
`genclient.HandlerFromMux(s, cr)` in `server.go` once regenerated. **No** parent-router
registration (unlike the public `/auth/*` routes).

---

## 5. Tests to add

### DB integration (`internal/db/auth_test.go`, new or appended)
- `TestChangePasswordHappy`: create user, `ChangePassword(old,new)` → nil; `Login(new)`
  ok=true; `Login(old)` ok=false.
- `TestChangePasswordWrongCurrent`: `ChangePassword(wrong,new)` → PgError `28P01`.
- `TestChangePasswordTooShort`: `ChangePassword(old,"short")` → PgError `23514`.
- `TestChangePasswordSameAsCurrent`: `ChangePassword(old,old)` → PgError `23514`.
- `TestRevokeExceptFamily`: issue two families A,B for one user; `RevokeUserRefreshExceptFamily(user, A.family)` returns 1; B is revoked, A still live.
- `TestRevokeExceptFamilyNil`: keep=Nil revokes both.

Use the `mkCustomer` / `IssueRefreshToken` fixtures already in `internal/db/`.

### API integration (`internal/api/password_test.go`, new)
Reuse `clientLogin` / `doRefresh` from `refresh_test.go`:
- `TestHTTPChangePasswordRevokesOthers`:
  1. login session A (keep its refresh), login session B (separate family).
  2. `POST /me/password` with A's bearer + `{current_password, new_password, refresh_token: A.refresh}` → 204.
  3. `doRefresh(B.refresh)` → 401 (revoked). `doRefresh(A.refresh)` → 200 (spared).
  4. `clientLogin(name, old_pw)` → fails (401); `clientLogin(name, new_pw)` → 200.
- `TestHTTPChangePasswordWrongCurrent`: bearer + wrong current → 401, and a control session is **not** revoked (still refreshable).
- `TestHTTPChangePasswordWeak`: bearer + `new_password:"short"` → 422.
- `TestHTTPChangePasswordNoBearer`: no `Authorization` → 401.
- `TestHTTPChangePasswordNoRefreshToken`: omit `refresh_token` → 204, and a second session is revoked (all-families path).

---

## 6. Security considerations

- **No user enumeration**: unknown/non-active user and wrong password both raise
  `28P01` → identical 401.
- **Constant-time-ish compare**: bcrypt `crypt()` comparison, same as login.
- **Rate-limiting** (note, not built here): `/me/password` is a password *oracle*
  (it confirms the current password). It MUST be throttled per subject + IP, alongside
  `/auth/login` and `/auth/refresh` — this is the open checklist item in
  [`06-client-api.md`](../06-client-api.md) §6.4 ("Rate-limit … per subject + IP").
  Until the shared limiter middleware lands, document the gap; the endpoint returns
  **429** (already in the OpenAPI responses) when that middleware is added. Do **not**
  invent a bespoke limiter here.
- **Session hygiene**: revoking other families is the actual security win — a stolen
  refresh token on another device dies. The spared family is authenticated by
  possession of its opaque refresh token (only the real device has it), and the
  revoke is `user_id`-scoped so it can't touch another user.
- **No secrets logged**: never log `current_password`/`new_password`/`refresh_token`.
  The `s.log.Error` branch logs only the err.
- **Access token lag**: other devices' *access* tokens stay valid up to 15m
  (`auth.jwt_ttl`); their refresh is dead, so they cannot rotate. This matches the
  stateless-JWT trade-off already accepted across the surface (§1).

---

## 7. Acceptance criteria

- [ ] `POST /me/password` added to `api/openapi.yaml` under tag `client` with
      `ChangePasswordRequest`; `task generate:oapi` regenerates cleanly.
- [ ] Migration `00018` (or next free) adds `change_password` and
      `revoke_user_refresh_except_family`; `goose up`/`down` both succeed.
- [ ] `internal/db/auth.go` gains `ChangePassword`, `RevokeUserRefreshExceptFamily`,
      `RefreshFamilyByToken`.
- [ ] `internal/api/handlers_password.go` implements `ChangePassword`; the package
      still compiles (`genclient.ServerInterface` satisfied).
- [ ] Wrong current password → 401; weak new → 422; success → 204.
- [ ] After success: other refresh families revoked, current family (if `refresh_token`
      supplied) spared; old password no longer logs in, new password does.
- [ ] DB + API tests above pass under `task test:db`.
- [ ] No password/token values appear in logs.

---

## 8. Implementation order

1. Add the operation + `ChangePasswordRequest` schema to `api/openapi.yaml`.
2. `task generate:oapi` — `ChangePassword` now appears in `genclient.ServerInterface`;
   the build breaks (handler missing). Good.
3. Write `db/migrations/00018_change_password.sql` (next free number); `goose up` on a
   scratch DB; verify `goose down` drops both functions.
4. Append `ChangePassword`, `RevokeUserRefreshExceptFamily`, `RefreshFamilyByToken` to
   `internal/db/auth.go`.
5. Add `internal/api/handlers_password.go`; build passes.
6. Add DB tests (`internal/db/auth_test.go`) and API tests (`internal/api/password_test.go`).
7. `task test:db`; fix; done.
