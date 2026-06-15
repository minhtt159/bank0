# bank0

A **core-banking backend**: a double-entry ledger where correctness is a property of
the database, fronted by a thin Go API, an operator console, and a customer PWA. It
holds account balances and moves money between them without ever losing a cent,
double-spending, or double-posting on a retry.

Four invariants shape everything (see [`docs/01-overview.md`](docs/01-overview.md)):

1. **The ledger is the source of truth.** `accounts.balance_minor` is a
   trigger-maintained cache of `SUM(ledger_entries)`, always reconcilable; money is
   never created from nowhere.
2. **Money/auth logic lives in the database.** PL/pgSQL functions + triggers own every
   money movement and auth transition; the API is thin transport.
3. **Idempotency is enforced by the database.** Replays return the original result;
   they never double-post.
4. **Append-only and auditable.** The ledger can't be updated or deleted — corrections
   are new reversing entries.

## Three surfaces, three hosts

Two surfaces are the same Go binary in different `server.mode`s (separated in the app,
not just at the edge); the third is a Cloudflare Worker.

| Host | Surface | Tech | Auth |
|------|---------|------|------|
| `portal.bank0.hnimn.art` | admin API + operator console | Go `mode=portal` (Templ/HTMX) | DB cookie session (staff roles, 30-min idle) |
| `api.bank0.hnimn.art` | customer JSON API | Go `mode=api`, behind Cloudflare | JWT bearer + rotating refresh tokens (ownership-scoped) |
| `bank0.hnimn.art` | customer PWA | Cloudflare Worker (Preact/Vite, ~15 KB gzip) | proxies `/api/*` to the client API |

`server.mode=all` serves both Go surfaces in one container for local development.

## Quick start (local)

```bash
docker compose -f deploy/docker-compose.dev.yml up --build -d   # Postgres + migrate + admin (:8080) + client (:8090)
task seed                                                       # load the dev seed (db/seed.sql); migrate ran above
open http://localhost:8080/        # operator console (Templ + HTMX)
open http://localhost:8090/docs    # client API reference (Scalar)
```

Seeded logins (dev passwords): staff `admin`/`admin`, `operator1`/`operator`,
`auditor1`/`auditor`; customers `alice`/`password` … (no console access). The default
seed (`db/seed.sql`, idempotent) loads 91 customers / 215 accounts (valid NL IBANs) /
714 transfers, with pending/canceled/reversed lifecycle coverage and three dedicated
guided-transfer "mule" accounts; `task seed:demo` loads a larger randomized set, and
`task dev:reset` rebuilds the stack from a clean DB and seeds it in one step. **Change
the admin password before exposing the portal.**

Without Docker: `task install && task generate && task migrate:up && psql "$APP_DATABASE_DSN" -f db/seed.sql && task run`.

## Deploy

Same image, two supported paths:

```bash
helm install bank0 deploy/helm/bank0 --set database.existingSecret=bank0-db
```

creates `bank0-api` (mode=api, HPA) and `bank0-portal` (mode=portal) behind Gateway
API/Envoy, with a pre-upgrade migrate job ([`docs/04-deployment.md`](docs/04-deployment.md)).
A serverless Supabase + Cloud Run + Cloudflare path is in
[`docs/08-deployment-cloud-run-supabase.md`](docs/08-deployment-cloud-run-supabase.md).

## Tech stack

Go 1.26 · PostgreSQL 18 (native `uuidv7()`; a polyfill keeps PG17/Supabase working) ·
pgx/v5 + sqlc · goose migrations · slog · BIGINT minor units · bcrypt (pgcrypto) ·
Templ + HTMX (console) · OpenAPI 3.1 contract-first (oapi-codegen + Scalar) · Helm.

## Documentation

Start at [`docs/01-overview.md`](docs/01-overview.md) — it frames the design and walks
through how you use bank0 (the customer money-move and the operator journeys). The
reference docs ([`docs/02`](docs/02-data-model.md)–[`docs/11`](docs/11-iban-verification.md))
cover the data model, ledger lifecycle, deployment, the two surfaces, the PWA, fraudbank
integration, security, and IBAN handling. The product roadmap is in
[`docs/specs/`](docs/specs/).
