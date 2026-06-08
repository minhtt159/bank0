# bank0

A **core-banking backend proof of concept**: a double-entry ledger with a strict
state machine, database-enforced idempotency, and a server-rendered operator
console. Forked in spirit from `tf-backend`, redesigned from scratch around four
principles (see [`docs/01-overview.md`](docs/01-overview.md)):

1. **The ledger is the source of truth.** Balances are a maintained cache, always
   reconcilable to `SUM(ledger_entries)`. Money is never created from nowhere.
2. **State transitions live in the database.** PL/pgSQL functions and triggers own
   every money movement. The API is a thin, dumb transport.
3. **Idempotency is enforced by the database**, not by hope. Replays return the
   original result; they never double-post.
4. **Everything is append-only and auditable.** The ledger cannot be updated or
   deleted — corrections are new reversing entries.

## Documentation

| Doc | What it covers |
|-----|----------------|
| [`docs/01-overview.md`](docs/01-overview.md) | Scope, design principles, tech stack, request lifecycle |
| [`docs/02-data-model.md`](docs/02-data-model.md) | Consolidated ERD, every table, invariants, indexes |
| [`docs/03-ledger-lifecycle-idempotency.md`](docs/03-ledger-lifecycle-idempotency.md) | Transfer state machine, DB functions, idempotency, triggers |
| [`docs/04-admin-ui.md`](docs/04-admin-ui.md) | Operator console UX, roles, dashboards, maker-checker |
| [`docs/05-deployment.md`](docs/05-deployment.md) | Modes, two domains, Helm, Gateway API/Envoy, HA, migrations, OpenAPI contract |
| [`docs/06-customer-app-plan.md`](docs/06-customer-app-plan.md) | Deferred plan for the customer-facing surface (auth, IDOR fix, roadmap) |
| [`docs/07-auth-refresh-mfa.md`](docs/07-auth-refresh-mfa.md) | Client auth hardening plan: refresh-token rotation, TOTP MFA, step-up |
| [`docs/08-customer-web-app.md`](docs/08-customer-web-app.md) | Customer web app build plan (SPA/PWA + JWT, Cloudflare Worker, beneficiaries) |

## Tech stack (carried from tf-backend, refined)

| Concern | Choice | Change vs tf-backend |
|---------|--------|----------------------|
| Language | Go 1.26 | — |
| Database | PostgreSQL 18 | native `uuidv7()` (no helper function needed) |
| Driver / pool | pgx/v5 | — |
| Queries | sqlc | — |
| Migrations | goose | — |
| Logging | **slog** (stdlib) | replaces zap (the planned migration) |
| Money | **BIGINT minor units** | replaces `NUMERIC(12,2)` |
| Passwords/PINs | **hashed** (bcrypt via pgcrypto) | replaces plaintext |
| Admin UI | Templ + HTMX | role-based sessions instead of single BasicAuth user |
| API docs | **OpenAPI 3.1, contract-first** (oapi-codegen + Scalar UI) | replaces swaggo annotations |
| Deploy | **Helm chart + container image** | api/portal split, HPA, in-cluster migrations |

## Two surfaces, one image

`server.mode` selects the route surface — separated **in the app**, not just at the edge:

| Mode | Serves | Auth | Domain |
|------|--------|------|--------|
| `api` | client JSON API + `/docs` | **JWT bearer** (login issues HS256 token; ownership-scoped) | `api.bank0.hnimn.art` (HA, autoscaled) |
| `portal` | admin API + operator console + `/docs` | **DB-backed cookie session** (staff roles, 30-min idle) | `portal.bank0.hnimn.art` |
| `all` | everything | both | local docker-compose |

## Quick start (local)

```bash
docker compose -f deploy/docker-compose.dev.yml up --build
# stack: postgres + migrate (one-shot) + seed (one-shot) + admin + client
open http://localhost:8080/        # admin portal + operator console (Templ + HTMX)
open http://localhost:8090/docs    # client API reference (Scalar)
```

- **admin portal** → http://localhost:8080  (mode=portal, cookie sessions)
- **client API**  → http://localhost:8090  (mode=api, JWT bearer)

Seeded logins (dev passwords): `admin`/`admin`, `operator1`/`operator`,
`auditor1`/`auditor` (staff); customers `alice`/`password` … `frank` (no console
access). The seed (`db/seed.sql`, idempotent) loads 6 customers, 7 accounts, and
16 transfers (3 pending). **Change the admin password immediately.**

Without Docker: `task install && task generate && task migrate:up && psql "$APP_DATABASE_DSN" -f db/seed.sql && task run`.

## Deploy (Kubernetes)

```bash
helm install bank0 deploy/helm/bank0 --set database.existingSecret=bank0-db
```

Creates `bank0-api` (mode=api, HPA 3–10) and `bank0-portal` (mode=portal) from one
image, **Gateway API HTTPRoutes (Envoy Gateway)** for the two hosts, and a
pre-upgrade migrate Job. See [`docs/05-deployment.md`](docs/05-deployment.md).

## Status

**Working scaffold, validated end-to-end.** Migrations apply/rollback/re-apply on
PostgreSQL 18; the ledger, idempotency, holds, reversals, the balance tamper
guard, and `reconcile()` are exercised by [`db/smoke_test.sql`](db/smoke_test.sql)
and by live HTTP runs. The contract-first API (oapi-codegen, both surfaces), the
mode split, advisory-locked HA maintenance, embedded migrations, the Helm chart
(Gateway API/Envoy; `helm lint`/`template` clean), and the **operator console**
all build and run. The portal console is behind **DB-backed session auth** (login/logout, SHA-256
token, 30-min sliding idle, staff-role check) and shows the dashboard (reconcile
badge), accounts, and pending queue. The client API is behind **JWT bearer auth**
(login issues an HS256 token) with **ownership scoping** that closes the IDOR gap —
verified end-to-end: missing/bad token→401, and a customer cannot read or debit
another customer's account/ledger/transfers (404/403). `go build`/`go vet` clean;
`helm lint`/`template` clean.

**Next:** console actions (post/cancel/reverse, credit/debit) with confirm modals
+ idempotency keys, the maker-checker approvals queue, role-gating on actions, and search.
