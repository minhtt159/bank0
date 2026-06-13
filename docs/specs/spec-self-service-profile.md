# Spec — Self-service profile edit (`PATCH /me`)

> ✅ **IMPLEMENTED (2026-06-13, `feat/bff`).** No migration (reuses `update_user_info`
> with password/status pinned nil); handler `UpdateMe` in
> `internal/api/handlers_users.go`, tests `me_test.go`. As-built:
> [`../06-client-api.md`](../06-client-api.md) §1. Retained for design rationale only.

> Implementation spec. Closes the P1 "Profile edit (client)" gap in
> [`../09-fraudbank-bff-plan.md`](../09-fraudbank-bff-plan.md): a logged-in customer
> can fix their own `full_name` / `email` / `phone_number`. The DB function
> (`update_user_info`) already exists and is portal-only today; this exposes a
> **scoped, password-and-status-locked** client `PATCH /me` over it.
>
> **Scope boundary — this spec does NOT duplicate its neighbours on the `/me`
> surface.** Password change is [`spec-change-password.md`](spec-change-password.md)
> (`POST /me/password`). Session/device listing + selective revoke is
> [`spec-sessions-devices.md`](spec-sessions-devices.md) (`GET`/`DELETE
> /me/sessions`). Step-up/MFA is [`spec-step-up-mfa.md`](spec-step-up-mfa.md).
> This file is **only** the profile PATCH; it references those for everything else.

---

## 1. Summary & rationale

`GET /me` is read-only. `update_user_info` (migration `00006`) is a partial update
that re-hashes the password only when one is supplied and can also set `status` — it is
used by the portal console. fraudbank clients need to fix a mistyped email/phone or a
changed name without calling staff. This adds **one client endpoint**:

```
PATCH /me {full_name?, email?, phone_number?}  →  200 User
```

The whole point is the **guardrail**: the handler reuses `update_user_info` but passes
`NULL` for both `password` and `status`, so self-service can **never** change a
password (that is the current-password-gated [`spec-change-password.md`](spec-change-password.md)),
unlock a locked account, or escalate role. No new DB code is required beyond a thin
sqlc wrapper.

**Extends an existing domain** (users) — no new tables, no new functions.

---

## 2. API — OpenAPI 3.1 operation

Add a `patch` to the existing `/me` path (tag `client`, `bearerAuth`). Style matches
the existing `/me` `get`.

```yaml
  /me:
    # existing GET stays unchanged; ADD:
    patch:
      operationId: updateMe
      tags: [client]
      summary: Update the caller's own profile (partial; name/email/phone only)
      requestBody:
        required: true
        content:
          application/json:
            schema: { $ref: "#/components/schemas/UpdateMeRequest" }
      responses:
        "200":
          description: updated
          content:
            application/json:
              schema: { $ref: "#/components/schemas/User" }
        "409": { $ref: "#/components/responses/Error" }   # email/phone now taken
        "422": { $ref: "#/components/responses/Error" }   # invalid email shape
```

Add to `components/schemas:`

```yaml
    UpdateMeRequest:
      type: object
      properties:
        full_name:    { type: string }
        email:        { type: string }
        phone_number: { type: string }
      # All optional. An absent field is left unchanged (COALESCE in update_user_info).
      # An empty {} body is a valid no-op -> 200 with the unchanged User.
      # role / status / password are deliberately NOT accepted here.
```

> `email`/`phone_number` use the same validation as `users` already enforces (the
> `email ~* ...` CHECK; phone `VARCHAR(16)` UNIQUE). Sending `""` clears nothing —
> `update_user_info` NULLIFs empty strings to "leave unchanged", matching `create_user`.
> To support **clearing** an optional contact field later, add an explicit
> `clear_email`/`clear_phone` flag then; v1 does not clear (avoids accidental wipes).

---

## 3. Data model — migration

**No new tables, no new functions.** `update_user_info(p_user_id, p_full_name,
p_email, p_phone_number, p_password, p_status)` already exists (migration `00006`,
[`../06-client-api.md`](../06-client-api.md)) and does exactly what's needed: COALESCE
partial update, NULLIF on empty email/phone, password re-hash **only** when supplied,
status set **only** when supplied. We call it with `password=NULL, status=NULL`.

The only artifact is a sqlc query (`db/queries/users.sql`, alongside the existing user
queries):

```sql
-- name: UpdateMe :exec
-- Self-service profile edit. password + status are pinned NULL so the client
-- surface can never change them (escalation guard). full_name/email/phone are
-- optional (narg) — absent => COALESCE leaves the column unchanged.
SELECT update_user_info(
    sqlc.arg(user_id)::uuid,
    sqlc.narg(full_name)::text,
    sqlc.narg(email)::citext,
    sqlc.narg(phone_number)::varchar,
    NULL,   -- password: never settable here
    NULL    -- status:   never settable here
);
```

> If [`spec-change-password.md`](spec-change-password.md) or
> [`spec-step-up-mfa.md`](spec-step-up-mfa.md) introduces a new migration, this spec
> still adds **no migration of its own** — coordinate nothing. It is purely an API +
> handler + query change.

---

## 4. Handler logic

New handler in `internal/api/handlers_users.go` (next to `GetMe`), implementing the
generated `UpdateMe`.

1. `subj, ok := clientSubject(r.Context())`; `!ok` → 401 `unauthorized`
   (client-only — operators edit users via the portal console, unscoped).
2. Decode `UpdateMeRequest` into a struct of `*string` pointers (so "absent" is
   distinguishable from "empty"). Empty body decodes to all-nil → valid no-op.
3. Optional Go-side validation: if `email` present and non-empty, a cheap format check
   before the DB (the `users` CHECK is the real gate, but a Go check gives a 422 with a
   clearer message than the P0001 path).
4. `UpdateMe(user_id=subj, full_name?, email?, phone_number?)`. Map errors via
   `mapDBError`:
   - `23505` (unique_violation on email/phone) → **409** `already_exists`.
   - `23514`/P0001 from the email CHECK → **422** `invalid_email`.
   - non-existent user (can't happen for a valid JWT, but) → 404.
5. Re-read `GetUserByID(subj)` (the no-password-hash projection) and return **200**
   `User` — so the client gets the canonical post-update state, including the unchanged
   `onboarding_status` if [`spec-self-registration.md`](spec-self-registration.md) has
   landed.

### Ownership / scoping

The update target is **always `subj`** — there is no `user_id` in the request or path,
so a caller can only ever edit themselves. This is structurally IDOR-proof (matches the
`GET /me` pattern). No `Idempotency-Key`: no money moves; the operation is naturally
idempotent (re-applying the same values is a no-op).

### Escalation guard (the load-bearing detail)

The handler passes `NULL` for `update_user_info`'s `p_password` and `p_status`
arguments **unconditionally** — they are hard-coded `NULL` in the sqlc query, not wired
from the request. Even if a client sends `{"role":"admin","status":"active",
"password":"x"}`, those fields are not in `UpdateMeRequest`, are ignored by the
decoder, and could not reach the DB regardless. Profile edit therefore cannot unlock an
account, set a password (bypassing the current-password gate), or change role.

---

## 5. Tests to add

**DB integration (`internal/db/users_test.go`, extend)**:

- [ ] `update_user_info(id, full_name)` with `password=NULL` leaves `password_hash`
      byte-identical (the password re-hash branch is skipped).
- [ ] `update_user_info(id, ..., status=NULL)` leaves `status` unchanged even for a
      `locked` user.
- [ ] partial update: setting only `email` leaves `full_name`/`phone_number` intact
      (COALESCE).
- [ ] duplicate email/phone → `23505`.

**API integration (`internal/api/me_test.go`)** (existing server harness):

- [ ] `PATCH /me {email:"new@x.com"}` → 200; `GET /me` reflects it; password login
      still works afterwards (hash untouched).
- [ ] `PATCH /me {}` (empty) → 200, unchanged `User`.
- [ ] duplicate email (already another user's) → 409.
- [ ] invalid email shape → 422.
- [ ] `PATCH /me {role:"admin", status:"active"}` → role/status **unchanged** (fields
      ignored; assert via `GET /me` and a portal `GET /users/{id}`).
- [ ] no bearer → 401.

---

## 6. Security considerations

- [ ] Scoped to `subj`; no `user_id` accepted → IDOR-proof by construction.
- [ ] `password` and `status` are pinned `NULL` in the query — self-service cannot
      change a password (use [`spec-change-password.md`](spec-change-password.md)),
      unlock a locked/closed account, or otherwise alter lifecycle.
- [ ] `role` is not a parameter of `update_user_info` at all and is never touched —
      no role escalation surface.
- [ ] Email/phone uniqueness is DB-enforced (`23505` → 409); a user can't claim
      another's contact details.
- [ ] **Email/phone change does not re-verify** the new value. If
      [`spec-self-registration.md`](spec-self-registration.md)'s contact verification is
      live, a changed email/phone should be **marked unverified** (clear the relevant
      `*_verified_at`) and a re-verification challenge dispatched — note this as the
      integration point; if registration/verification is not yet built, accept the
      change as-is and revisit. (Do not silently keep a stale `verified_at` on a changed
      address.)
- [ ] No rate-limit needed beyond the general client limiter (no oracle, no money).

---

## 7. Acceptance criteria

- [ ] `oapi-codegen` regenerates `genclient` with `UpdateMe`; the handler implements it
      (build green). No migration added.
- [ ] `PATCH /me` updates only `full_name`/`email`/`phone_number`, scoped to the JWT
      subject; returns the updated `User`.
- [ ] Password hash and `status` are provably unchanged by any `PATCH /me` call.
- [ ] Duplicate email/phone → 409; invalid email → 422; missing bearer → 401.
- [ ] If contact verification exists, a changed email/phone is flagged unverified (or
      the integration point is documented as deferred).
- [ ] `reconcile()` unaffected (no ledger surface).

---

## 8. Step-by-step implementation order

1. Add `UpdateMe :exec` to `db/queries/users.sql` with `password`/`status` pinned
   `NULL`; run `sqlc generate`.
2. Add the `patch` to `/me` + `UpdateMeRequest` schema in `api/openapi.yaml`; run
   `oapi-codegen`; fix the compiler.
3. Write the `UpdateMe` handler in `internal/api/handlers_users.go` (subject-scoped,
   `*string` pointer decode, re-read + return `User`).
4. If [`spec-self-registration.md`](spec-self-registration.md) is live, wire the
   "changed contact → unverified + re-challenge" hook; otherwise leave a TODO referencing
   that spec.
5. Write DB + API tests (§5). `go test ./...` green with and without
   `TEST_DATABASE_DSN`.
6. Update [`../06-client-api.md`](../06-client-api.md) §1 surface table and the P1 row
   in [`../09-fraudbank-bff-plan.md`](../09-fraudbank-bff-plan.md).
