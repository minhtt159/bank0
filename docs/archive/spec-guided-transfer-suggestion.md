# Spec — Guided transfer suggestion (`GET /transfers/suggestion`)

> ⤴ **Superseded for the endpoint shape** by guided-transfer v2
> ([`../specs/spec-banking-grade-hardening.md`](../specs/spec-banking-grade-hardening.md) §5,
> migration `00032`): the endpoint now returns `{"options": [...]}` with **up to 3
> third-party mule candidates** drawn at random from the active `guided_scenarios`
> short-list, and the own-account fallback moved to the client. `guided_scenarios`
> (00019) and the security model below are unchanged — retained for that rationale.

> ✅ **IMPLEMENTED (2026-06-13, `feat/bff`).** Migration `00019_guided_scenarios.sql`,
> DB wrapper `internal/db/bank.go` (`SuggestTransferDestination`), handler
> `internal/api/handlers_suggestion.go`, tests `suggestion_test.go`. As-built surface:
> [`../06-client-api.md`](../06-client-api.md) §1. Retained for design rationale only.

> **Status: spec.** Implementation-ready. Completes the P-list in
> [`09-fraudbank-bff-plan.md`](../09-fraudbank-bff-plan.md) and the client seam in
> fraudbank `apps/web/src/lib/guided.ts` (mirrors in iOS `GuidedSuggestion.swift`,
> Android `GuidedSuggestion.kt`). It does **not** move money — selecting a
> suggestion still goes through `POST /transfers` unchanged (idempotency-key).

---

## 1. Summary & rationale

fraudbank's clients descend from a fraud-risk-SDK demo. "Guided transaction" mode
simulates an **authorised-push-payment (APP) / social-engineering scam**: instead
of the victim typing a destination, the app *suggests* a recipient and pre-selects
it, steering the user toward an attacker-chosen account (the "mule"). Today the
client **fakes** the suggestion (`suggestGuidedRecipient` picks one of the user's
own other accounts) because no backend endpoint exists.

This spec adds the real endpoint: `GET /transfers/suggestion` (client tag) returns
**one** suggested credit account — `account_id`, `iban`, a **masked** owner name
(exactly the field set `/beneficiaries/resolve` already exposes), and a
human-facing `reason`/`label`. The suggestion is **scenario-driven**: a
configurable `guided_scenarios` table maps an active scenario to a target account
(the simulated mule), optionally targeted per-user. When **no** scenario applies,
the endpoint falls back to the **safe default**: suggest one of the caller's *own
other active accounts* — identical behaviour to today's client stand-in, so the
demo always has something concrete and harmless to pre-select.

Design constraints (all enforced below):

| Constraint | Why |
|---|---|
| Read-only; no money moves | The transfer itself stays on `POST /transfers` (idempotency-key). This endpoint only *names a destination*. |
| Never leaks more than `/beneficiaries/resolve` | Same masked-name + iban + account_id triple, via the same `mask_name()`. No balance, no full name, no owner id. |
| Suggested target must be a valid credit target | An `active` `customer` account that is **not** the `from_account`. Mirrors the `request_transfer` credit-side validation so a suggestion never dead-ends at a 422. |
| Safe default when no scenario | Caller's own other active account — the suggestion is always something the caller could legitimately send to in the demo. |
| Scenario seam is operator/seed-controlled | The "mule" target is configured in `guided_scenarios` (seed/console), never chosen by the client. The client only toggles *whether* it asks. |

### 1.1 How the client uses it & how it feeds the risk score

- The client's Guided toggle (default ON in fraudbank web) calls
  `GET /transfers/suggestion?from_account=<id>&amount_minor=<n>` when the **From**
  account / amount changes, replacing the local `suggestGuidedRecipient` stand-in.
  On `200` it pre-selects the returned account and shows `reason` in the guided
  banner; on `204` it falls back to manual entry (no suggestion).
- The response carries `scenario` (the active scenario name, or `null` for the
  safe default) and `source` (`scenario` | `own_account`). The client passes these
  into its risk-SDK seam: a transfer that proceeds from a `source=scenario`
  suggestion is the APP-scam-shaped event the demo wants to score
  (`risk.generalEvent("transfer","guided")` today; the `scenario` name gives the
  risk SDK a concrete label). The backend does **not** itself score — it only
  surfaces the signal; server-side scoring is out of scope here (see
  [`spec-disputes.md`](spec-disputes.md) §6 for where a server-side fraud hook
  would live).

---

## 2. API — OpenAPI 3.1

Add to `api/openapi.yaml`. New path under `paths:`, two new schemas under
`components.schemas`. Matches the file's terse flow style.

```yaml
  /transfers/suggestion:
    get:
      operationId: suggestTransferDestination
      tags: [client]
      summary: >-
        Guided-transfer demo: suggest a destination account for the caller.
        Scenario-driven (simulated mule) when an active guided_scenario applies,
        else the caller's own other active account. Read-only; never moves money.
      parameters:
        - name: from_account
          in: query
          required: false
          description: The intended debit account (must be owned by the caller). Excluded from the suggestion.
          schema: { type: string, format: uuid }
        - name: amount_minor
          in: query
          required: false
          description: Intended amount in minor units; lets a scenario gate on a threshold.
          schema: { type: integer, format: int64 }
      responses:
        "200":
          description: a suggested destination
          content:
            application/json:
              schema: { $ref: "#/components/schemas/TransferSuggestion" }
        "204": { description: no suggestion available (client falls back to manual) }
        "403": { $ref: "#/components/responses/Error" }
```

```yaml
    TransferSuggestion:
      type: object
      properties:
        account_id:        { type: string, format: uuid }
        iban:              { type: string }
        owner_name_masked: { type: string, description: "Masked, exactly as /beneficiaries/resolve. Never the full name." }
        reason:            { type: string, description: "Human-facing label shown in the guided banner, e.g. 'Recommended payee' or 'your savings account'." }
        scenario:          { type: string, description: "Active scenario name, or omitted/null for the safe-default own-account suggestion." }
        source:            { type: string, enum: [scenario, own_account] }
```

Notes:
- `from_account` is **optional** so the client can ask before an account is chosen
  (the default account is then used by the handler). `amount_minor` is advisory —
  only scenario gating reads it.
- `204` (no body) is returned when there is no scenario *and* no eligible own
  account (e.g. single-account user). The client's existing "return null →
  fall back to manual" contract maps cleanly onto `204`.
- The endpoint is registered behind `requireJWT` like every other client route
  (`server.go`: `genclient.HandlerFromMux(s, cr)`); no router special-casing is
  needed because the path is static and does not collide with `/transfers/{id}`
  (mux matches the literal segment first; if collision is observed in `all` mode,
  register `/transfers/suggestion` on the parent ahead of the subrouter exactly as
  `/transfers/pending` is — see `server.go` §portalOn).

---

## 3. Data model

### 3.1 `guided_scenarios` table

Demo/config state only — **no money state**. One row = one named scenario mapping
to a target ("mule") account, optionally scoped to a single caller. Multiple rows
may be active; the resolver picks deterministically (see §4).

| Column | Type | Notes |
|---|---|---|
| `id` | `UUID` PK | `uuidv7()` |
| `name` | `TEXT` UNIQUE | scenario label, e.g. `app_scam_invoice`; surfaced as `scenario` in the response |
| `target_account_id` | `UUID` FK → `accounts(id)` `ON DELETE CASCADE` | the simulated mule; validated active+customer at resolve time |
| `reason` | `TEXT` NOT NULL DEFAULT `'Recommended payee'` | shown in the guided banner |
| `active` | `BOOLEAN` NOT NULL DEFAULT `TRUE` | toggle without deleting |
| `target_user_id` | `UUID` FK → `users(id)` `ON DELETE CASCADE`, NULL | when set, the scenario applies **only** when the caller is this user; NULL = applies to any caller |
| `min_amount_minor` | `BIGINT` NOT NULL DEFAULT `0` | scenario fires only when `amount_minor >= min_amount_minor` (0 = always) |
| `priority` | `INT` NOT NULL DEFAULT `0` | higher wins when several scenarios match |
| `created_at` | `TIMESTAMPTZ` NOT NULL DEFAULT `now()` | |

Indexed for the resolve query: a partial index on `active`.

### 3.2 Migration `00018_guided_scenarios.sql`

Next free number (current max is `00017`). Goose, PL/pgSQL resolver in the file
(handler calls one function and maps typed errors — project discipline,
[`01-overview.md`](../01-overview.md) P2/P5). `mask_name()` already exists
(`00016`).

```sql
-- +goose Up
-- +goose StatementBegin

-- guided_scenarios: demo/config for fraudbank's "Guided transaction" mode (an
-- APP-scam simulation). A scenario maps an active demo to a target ("mule")
-- account that GET /transfers/suggestion will suggest. NO money state lives here.
-- Operator/seed-controlled; the client only toggles whether it ASKS for a
-- suggestion, never which account is returned.
CREATE TABLE guided_scenarios (
    id                UUID PRIMARY KEY DEFAULT uuidv7(),
    name              TEXT NOT NULL UNIQUE,
    target_account_id UUID NOT NULL REFERENCES accounts(id) ON DELETE CASCADE,
    reason            TEXT NOT NULL DEFAULT 'Recommended payee',
    active            BOOLEAN NOT NULL DEFAULT TRUE,
    target_user_id    UUID REFERENCES users(id) ON DELETE CASCADE,   -- NULL = any caller
    min_amount_minor  BIGINT NOT NULL DEFAULT 0,
    priority          INT NOT NULL DEFAULT 0,
    created_at        TIMESTAMPTZ NOT NULL DEFAULT now(),
    CHECK (name <> ''),
    CHECK (min_amount_minor >= 0)
);
CREATE INDEX idx_guided_scenarios_active ON guided_scenarios (priority DESC) WHERE active;

-- suggest_transfer_destination: the resolver.
--   1. Try an active scenario matching this caller + amount (per-user beats
--      global; higher priority wins; ties broken by newest). The target must be an
--      active customer account and must differ from p_from_account.
--   2. Else fall back to the caller's own OTHER active customer account (the safe
--      default — same behaviour as the client stand-in being replaced).
-- Returns the masked owner name via mask_name() (00016) — never a full name or
-- balance. Returns zero rows when nothing is eligible (handler -> 204).
CREATE OR REPLACE FUNCTION suggest_transfer_destination(
    p_caller       UUID,
    p_from_account UUID,           -- may be NULL; resolver substitutes the caller's default account
    p_amount_minor BIGINT DEFAULT 0
)
RETURNS TABLE (
    account_id        UUID,
    iban              VARCHAR,
    owner_name_masked TEXT,
    reason            TEXT,
    scenario          TEXT,
    source            TEXT
) AS $$
DECLARE
    v_from   UUID := p_from_account;
    v_acct   accounts%ROWTYPE;
    v_scn    guided_scenarios%ROWTYPE;
BEGIN
    -- Resolve the effective debit account: explicit, else the caller's default.
    IF v_from IS NULL THEN
        SELECT id INTO v_from FROM accounts
         WHERE user_id = p_caller AND kind = 'customer' AND is_default
         LIMIT 1;
    END IF;

    -- (1) Scenario match. Per-user targeting beats global; priority then recency.
    SELECT gs.* INTO v_scn
    FROM guided_scenarios gs
    JOIN accounts ta ON ta.id = gs.target_account_id
    WHERE gs.active
      AND COALESCE(p_amount_minor, 0) >= gs.min_amount_minor
      AND (gs.target_user_id IS NULL OR gs.target_user_id = p_caller)
      AND ta.kind = 'customer' AND ta.status = 'active'
      AND (v_from IS NULL OR ta.id <> v_from)        -- never suggest the debit account
    ORDER BY (gs.target_user_id IS NOT NULL) DESC, gs.priority DESC, gs.created_at DESC
    LIMIT 1;

    IF FOUND THEN
        SELECT * INTO v_acct FROM accounts WHERE id = v_scn.target_account_id;
        RETURN QUERY SELECT v_acct.id, v_acct.iban,
                            mask_name((SELECT full_name FROM users WHERE id = v_acct.user_id)),
                            v_scn.reason, v_scn.name, 'scenario'::text;
        RETURN;
    END IF;

    -- (2) Safe default: the caller's own OTHER active customer account.
    SELECT a.* INTO v_acct
    FROM accounts a
    WHERE a.user_id = p_caller AND a.kind = 'customer' AND a.status = 'active'
      AND (v_from IS NULL OR a.id <> v_from)
    ORDER BY a.is_default DESC, a.created_at
    LIMIT 1;

    IF FOUND THEN
        RETURN QUERY SELECT v_acct.id, v_acct.iban,
                            mask_name((SELECT full_name FROM users WHERE id = v_acct.user_id)),
                            ('your ' || v_acct.kind || ' account')::text,
                            NULL::text, 'own_account'::text;
        RETURN;
    END IF;

    -- Nothing eligible -> empty result set -> handler returns 204.
    RETURN;
END;
$$ LANGUAGE plpgsql STABLE;

-- +goose StatementEnd

-- +goose Down
-- +goose StatementBegin
DROP FUNCTION IF EXISTS suggest_transfer_destination(UUID, UUID, BIGINT);
DROP INDEX IF EXISTS idx_guided_scenarios_active;
DROP TABLE IF EXISTS guided_scenarios;
-- +goose StatementEnd
```

### 3.3 Optional seed (`db/migrations/00011_seed_system.sql` style, or console)

For the demo, a scenario can be seeded once a mule account exists. Keep it OUT of
the migration if seed customers don't exist at migrate time; instead document a
console/SQL one-liner:

```sql
INSERT INTO guided_scenarios (name, target_account_id, reason, min_amount_minor)
VALUES ('app_scam_invoice',
        (SELECT id FROM accounts WHERE iban = '<mule_iban>'),
        'Verified payee — safe to send', 10000);   -- fires for amounts >= €100
```

---

## 4. Handler logic

New file `internal/api/handlers_suggestion.go` (or append to
`handlers_transfers.go`). Implements the generated
`genclient.ServerInterface.SuggestTransferDestination`.

```
func (s *Server) SuggestTransferDestination(w, r, params genclient.SuggestTransferDestinationParams)
```

1. **Auth/scope.** `subj, ok := clientSubject(r.Context())`; `!ok` → 401
   (`"authentication required"`). This endpoint is client-only.
2. **Ownership of `from_account` (when supplied).** If `params.FromAccount != nil`:
   `owner, err := s.pg.Queries.AccountOwner(ctx, *params.FromAccount)`; on err →
   `mapDBError`. If `!ownsAccount(subj, owner)` → **403** (`forbidden`,
   `"from_account not owned by caller"`). Reuses the exact check `CreateTransfer`
   does for the debit side, so a suggestion can't be used to probe the from-side of
   accounts the caller doesn't own.
3. **Resolve.** Call a hand-written pgx wrapper `s.pg.SuggestTransferDestination`
   (the function `RETURNS TABLE`, which sqlc can't expand — same treatment as
   `Transfer`/`ResolveAccountByIban` in `internal/db/bank.go`). Pass `subj`, the
   optional `from_account`, and `amount_minor` (default 0 when nil).
4. **Empty → 204.** If the wrapper returns `pgx.ErrNoRows` (no eligible
   suggestion), `w.WriteHeader(http.StatusNoContent)` and return. The wrapper uses
   `QueryRow(...).Scan(...)`, so an empty result set surfaces as `ErrNoRows`.
5. **200.** Otherwise `writeJSON(w, 200, suggestion)`. `scenario` is omitted when
   NULL (pointer field, `omitempty`).
6. **Errors.** Any other DB error → `mapDBError`.

### 4.1 `internal/db/bank.go` addition

```go
// TransferSuggestion mirrors suggest_transfer_destination()'s RETURNS TABLE.
// Read-only; never exposes a full name or balance (mask_name, same as CoP).
type TransferSuggestion struct {
    AccountID       uuid.UUID `json:"account_id"`
    Iban            string    `json:"iban"`
    OwnerNameMasked string    `json:"owner_name_masked"`
    Reason          string    `json:"reason"`
    Scenario        *string   `json:"scenario,omitempty"`
    Source          string    `json:"source"`
}

func (p *Postgres) SuggestTransferDestination(
    ctx context.Context, caller uuid.UUID, from *uuid.UUID, amountMinor int64,
) (TransferSuggestion, error) {
    const q = `SELECT account_id, iban, owner_name_masked, reason, scenario, source
               FROM suggest_transfer_destination($1::uuid, $2::uuid, $3::bigint)`
    var sg TransferSuggestion
    err := p.Pool.QueryRow(ctx, q, caller, from, amountMinor).
        Scan(&sg.AccountID, &sg.Iban, &sg.OwnerNameMasked, &sg.Reason, &sg.Scenario, &sg.Source)
    return sg, err
}
```

(`from *uuid.UUID` binds to `$2::uuid` as SQL NULL when nil — pgx handles this.)

### 4.2 Edge cases

| Case | Behaviour |
|---|---|
| `from_account` not owned by caller | 403 (before any resolve) |
| `from_account` omitted, caller has a default | resolver uses the default account as the exclusion |
| Single-account user, no scenario | 204 (own-account fallback finds nothing but the from-account) |
| Active scenario whose target is now frozen/closed | excluded by `ta.status = 'active'`; falls through to own-account default or 204 |
| Scenario `target_account_id` equals the from-account | excluded by `ta.id <> v_from`; falls through |
| Multiple matching scenarios | per-user beats global, then `priority DESC`, then newest |
| `amount_minor` below a scenario's `min_amount_minor` | that scenario does not fire; lower/zero-threshold scenarios or the default may still apply |

---

## 5. Tests to add

### 5.1 DB integration (`internal/db/integration_test.go` style; TestMain applies migrations)

- `TestSuggestOwnAccountDefault`: a user with two active accounts and **no**
  scenario → resolver returns the *other* account, `source='own_account'`,
  `scenario IS NULL`, masked name matches `mask_name(full_name)`.
- `TestSuggestSingleAccountUserEmpty`: one-account user, no scenario, `from` = that
  account → zero rows.
- `TestSuggestScenarioGlobal`: insert an active global scenario pointing at a mule
  account → resolver returns the mule, `source='scenario'`, `scenario=<name>`,
  `reason` from the row.
- `TestSuggestScenarioPerUserBeatsGlobal`: a global scenario + a `target_user_id`
  scenario for caller X → X gets the per-user target; a different user gets the
  global one.
- `TestSuggestScenarioAmountGate`: scenario with `min_amount_minor=10000` →
  `amount_minor=5000` falls back to own-account/204; `amount_minor=20000` returns
  the scenario target.
- `TestSuggestScenarioExcludesFromAndInactive`: scenario target == from-account →
  excluded; frozen target → excluded.

### 5.2 API (`internal/api/*_test.go`, Bearer-token harness — see `beneficiaries_test.go`)

- `TestHTTPSuggestionRequiresAuth`: no token → 401.
- `TestHTTPSuggestionOwnAccount`: alice with two accounts, no scenario →
  `200`, body `source=own_account`, returned `account_id` is alice's *other*
  account, no full name / balance fields present.
- `TestHTTPSuggestionForbiddenFromAccount`: alice asks with
  `from_account=<bob's account>` → 403.
- `TestHTTPSuggestionScenario`: seed a scenario targeting bob's account (the mule),
  active, global → alice gets `200`, `source=scenario`, `owner_name_masked` masked,
  `iban` = bob's iban, and **no** `balance_minor`/`full_name` in the body.
- `TestHTTPSuggestionNoneIs204`: single-account user, no scenario → `204`,
  empty body.
- `TestHTTPSuggestionThenTransferUnchanged`: take the suggested `account_id`, POST
  `/transfers` with an `Idempotency-Key` debit=own/credit=suggested → `200`
  (proves the suggestion endpoint did not alter the money path).

---

## 6. Security considerations

- **No data leak beyond CoP.** The response is exactly the
  `{account_id, iban, owner_name_masked}` triple `/beneficiaries/resolve` already
  returns, plus demo metadata (`reason`, `scenario`, `source`). `mask_name()`
  (`00016`) is reused — never the full name; never the balance, owner id, or
  account status.
- **Suggestion is not authority.** The endpoint cannot move money or grant any
  capability; it only names a public-confirmation-of-payee destination. The actual
  transfer re-validates everything in `request_transfer` (ownership of the debit
  side in the handler; active accounts, limits, funds in PL/pgSQL).
- **Mule target is operator/seed-controlled.** The client picks neither the target
  nor the scenario — only whether to *ask*. A compromised client cannot point the
  suggestion at an arbitrary account it doesn't already know.
- **`from_account` ownership is checked first**, so the endpoint can't be used as
  an oracle over accounts the caller doesn't own (no 403-vs-204 timing distinction
  on foreign ids — 403 is returned purely from the ownership check, before resolve).
- **Demo-only by construction.** `guided_scenarios` is empty by default; with no
  rows the endpoint behaves exactly like the safe client stand-in it replaces. In a
  real deployment the table simply stays empty.
- The endpoint is read-only `GET`; no idempotency key, no CSRF surface (bearer
  auth, no cookies on the client surface).

---

## 7. Acceptance criteria

- [ ] `api/openapi.yaml` has `GET /transfers/suggestion` (tag `client`) +
      `TransferSuggestion` schema; `oapi-codegen` regenerates with no drift.
- [ ] Migration `00018_guided_scenarios.sql` creates the table, partial index, and
      `suggest_transfer_destination(...)`; `down` drops all three.
- [ ] `internal/db/bank.go` has `SuggestTransferDestination` (hand-written pgx,
      `RETURNS TABLE`).
- [ ] Handler enforces JWT auth, `from_account` ownership (403), 204-on-empty, and
      masks the owner name; returns `source` ∈ {`scenario`,`own_account`}.
- [ ] With no scenario rows, the endpoint suggests the caller's own other active
      account (safe default) or 204 for single-account users.
- [ ] With an active scenario, the endpoint suggests the configured mule, with
      per-user > global, priority, and amount-gate ordering.
- [ ] Response never contains a full name, balance, owner id, or any account the
      resolver wasn't asked to expose.
- [ ] `POST /transfers` is byte-for-byte unchanged; a suggested account flows
      through it normally with an `Idempotency-Key`.
- [ ] DB + API tests in §5 pass under `task test` (throwaway-DB harness).
- [ ] `docs/06-client-api.md` §1 surface table gains the row; `08-...` P-table notes
      the gap closed.

---

## 8. Step-by-step implementation order

1. **Migration.** Add `db/migrations/00018_guided_scenarios.sql` (§3.2). Run
   `task migrate` (or the test harness) to confirm up/down apply cleanly.
2. **Contract.** Add the path + schema to `api/openapi.yaml` (§2). Regenerate:
   `task gen` (oapi-codegen). The build now fails until the handler exists — the
   intended drift check.
3. **DB wrapper.** Add `SuggestTransferDestination` + `TransferSuggestion` to
   `internal/db/bank.go` (§4.1).
4. **Handler.** Implement `SuggestTransferDestination` in
   `internal/api/handlers_suggestion.go` (§4). Build passes.
5. **DB tests** (§5.1), then **API tests** (§5.2). `task test`.
6. **Docs.** One row in `docs/06-client-api.md` §1; tick the gap in
   `docs/09-fraudbank-bff-plan.md`.
7. **Client cutover (fraudbank, separate repo).** Replace the
   `suggestGuidedRecipient` stand-in (`apps/web/src/lib/guided.ts`, plus
   `GuidedSuggestion.swift` / `GuidedSuggestion.kt`) with a call to
   `GET /transfers/suggestion`; map `200→pre-select + banner(reason)`,
   `204→manual`. Pass `scenario`/`source` into the risk-SDK seam. **Not part of
   this backend PR.**
```
