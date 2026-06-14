# Spec — end-to-end test harness (two tiers)

> Status: **plan, not built.** This is the implementation plan for E2E coverage. It
> layers *on top of* the existing in-process integration tests (`internal/db`,
> `internal/api`) and the load harness (`load/`) — it does not replace them. Both
> tiers are **opt-in / gated** (like the DSN-gated integration tests and the `e2e`
> build tag) so a plain `go test ./...` and the per-PR CI stay fast.

## Why (the gap)

The current suite is deep at two layers, both single-process:
- **DB integration** (`internal/db`) — the PL/pgSQL invariants, incl. concurrency
  (`concurrency_test.go`) and ownership (`ownership_test.go`).
- **HTTP handler** (`internal/api`) — drives the real mux in `mode=all` via `httptest`.

Everything **above single-process Go** is untested: the **deployed split-mode
topology** (separate `portal` and `api` binaries), the **Cloudflare Worker `/api`
proxy** (header forwarding, path rewrite, SPA fallback), and the **browser** (the
PWA's token handling, optimistic UI, offline). `mode=all` in one process cannot
catch routing/shadowing differences between the two real binaries, and nothing
exercises the Worker or the SPA.

Onboarding is **operator-driven** (no self-registration endpoint), so every scenario
seeds via the operator console or the DB, not a signup flow.

---

## Tier A — Go, cross-surface (build first)

Black-box HTTP against the **real two-binary topology**. Highest value for the
lowest effort; pure Go, reuses the test Postgres.

**Location:** `internal/e2e/` behind `//go:build e2e`.

**Harness (`TestMain` / a `setup` helper):**
1. `go build -o $TMPDIR/bank0 ./cmd/app` once.
2. Migrate one throwaway DB (reuse `TEST_DATABASE_DSN`); optionally load `db/seed.sql`.
3. Grab two free ports (`net.Listen("tcp", ":0")` → close → reuse the numbers).
4. `os/exec` two processes:
   - `APP_SERVER_MODE=portal APP_SERVER_PORT=<p1>`
   - `APP_SERVER_MODE=api APP_SERVER_PORT=<p2>`
   both with `APP_DATABASE_DSN`, `APP_AUTH_JWT_SECRET`, etc.
5. Poll `GET /health` on each until ready (timeout ~10s).
6. `t.Cleanup`: `cmd.Process.Kill()` + wait, on both.

Drive purely over HTTP with thin local helpers (login → cookie jar for portal, login
→ bearer for api). Do **not** import the `internal/api` test fixtures — Tier A tests
the compiled binary as a black box.

**Scenarios:**
1. **Split-mode integrity.** Portal-only routes (`/console/*`, `GET /transfers/pending`,
   `/admin/*`) are reachable on the portal port and **absent (404)** on the api port;
   client routes (`/me`, `/transfers`, `/auth/*`) the reverse. A **client JWT cannot
   reach the admin JSON surface**; a **portal cookie cannot reach client routes**.
   (These are unit-tested in-process today, but only `mode=all` — never the split.)
2. **Cross-surface money flow.** Operator (portal, cookie) creates a user + account +
   funds it → customer (api, JWT) logs in, sees the balance, transfers, sees the debit
   in `/transfers` and the ledger. Assert balances against the api JSON, not HTML.

**CI:** a dedicated job (Postgres service) running `go test -tags e2e ./internal/e2e/`.
Manual `workflow_dispatch` or nightly — not per-PR.

**Effort:** ~1 day. **Risks:** process lifecycle (kill on cleanup, context timeouts),
port races (use `:0`), env wiring.

---

## Tier B — Playwright (browser) + the Worker

Exercises the customer surface a human actually uses, **through the Worker proxy**.

**Location:** `web/app/e2e/` (Playwright + TypeScript), `playwright.config.ts`.

**Stack under test:** PWA served by the **Worker via Miniflare / `wrangler dev --local`
(workerd)** — so the `/api/*` proxy is real, not mocked — talking to a live `api`
binary against a seeded Postgres.

**Orchestration:** Playwright `globalSetup` migrates + seeds Postgres, launches the api
binary and the Worker (or `docker-compose` for the backend + Playwright `webServer` for
the Worker); `globalTeardown` stops them. `storageState` persists the operator session
and per-test JWTs.

**Scenarios** (from the audit's test plan):
1. **Operator-provisioned onboarding → first transfer** — operator provisions in the
   console; customer logs into the PWA, resolves a payee (confirmation-of-payee masked
   name), saves a beneficiary, transfers with an Idempotency-Key, sees both ledger sides.
2. **Maker-checker approval across surfaces** — above-threshold console credit routes to
   Approvals (not posted); maker can't self-approve; a second admin approves; the customer
   sees it land in the PWA.
3. **Dispute lifecycle** — customer raises a dispute in the PWA → operator walks
   open→under_review→resolved (auditor blocked) → customer sees the resolution.
4. **Token rotation + theft** — silent refresh (rotation); a replayed OLD refresh token
   → 401 + family revoked; operator force-revoke → next refresh 401s; the SPA routes to
   login (no infinite refresh loop).

**Proxy assertions (the point of Tier B):** on the network tab, confirm `Authorization`
and `Idempotency-Key` **survive the proxy hop** unchanged, `/api/*` rewrites to the api
binary, and non-`/api` paths serve the SPA shell. Cross-check all balances/statuses
against the DB or JSON API — **never against rendered HTML alone**, so a UI change can't
mask a ledger bug.

**Cheaper intermediate (do alongside Tier A):** a **Miniflare + vitest** unit test in
`worker/` for the proxy contract only (header forwarding, path rewrite, SPA fallback,
CORS) — far lighter than full browser E2E and catches the highest-risk Worker bugs.

**CI:** a separate job (`npx playwright install` for browsers); traces/video on failure.
Nightly/manual.

**Effort:** ~2–3 days (browser infra + flake-hardening). **Risks:** flakiness (use
Playwright retries + traces), Worker/binary lifecycle in CI.

---

## Sequencing & uniqueness

1. **Tier A** (Go split-mode) — most value per effort.
2. **Worker proxy unit test** (Miniflare/vitest) — cheap, high-risk surface.
3. **Tier B** (Playwright) — the full browser journeys.

Generate unique usernames/IBANs per test (uuid-derived, as the suite already does) so
tests share one DB without truncation, except globally-scoped tables (disputes,
guided_scenarios, bank_settings) which need reset/cleanup per test.
